package nodejs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// defaultStopTimeout 是建立会话时等待目标首次停止的上限。
const defaultStopTimeout = 20 * time.Second

// Launcher 创建 Node.js 调试后端。
type Launcher struct{}

// Adapter 实现 backend.Debugger。
type Adapter struct {
	client *cdpClient
	proc   *platform.Proc
	out    *outputBuffer
	mode   string
	port   int

	mu      sync.Mutex
	bps     []*bpState
	bpSeq   int
	stop    *backend.Stop
	running bool
	closed  bool
	// finished 标记主脚本已执行完毕（Node 会停在等待调试器断开）。
	finished bool

	stopCh chan *backend.Stop

	// scripts 记录 scriptId 到 URL 的映射。
	// CDP 的 CallFrame.url 在某些版本/场景为空，需回退到 scriptParsed 上报的地址。
	scripts map[string]string
}

type bpState struct {
	info  backend.Breakpoint
	cdpID string
}

// pausedParams 是 Debugger.paused 的参数。
type pausedParams struct {
	CallFrames []struct {
		CallFrameID  string `json:"callFrameId"`
		FunctionName string `json:"functionName"`
		URL          string `json:"url"`
		Location     struct {
			ScriptID     string `json:"scriptId"`
			LineNumber   int    `json:"lineNumber"`
			ColumnNumber int    `json:"columnNumber"`
		} `json:"location"`
		ScopeChain []struct {
			Type   string       `json:"type"`
			Object remoteObject `json:"object"`
		} `json:"scopeChain"`
		This remoteObject `json:"this"`
	} `json:"callFrames"`
	Reason         string   `json:"reason"`
	HitBreakpoints []string `json:"hitBreakpoints"`
}

// Open 实现 backend.Launcher。
func (Launcher) Open(ctx context.Context, spec backend.LaunchSpec) (backend.Debugger, error) {
	port := spec.Port
	if port == 0 {
		p, err := pickFreePort()
		if err != nil {
			return nil, err
		}
		port = p
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}

	a := &Adapter{
		mode:    spec.Mode,
		port:    port,
		out:     newOutputBuffer(500),
		stopCh:  make(chan *backend.Stop, 64),
		running: true,
		scripts: make(map[string]string),
	}

	if err := a.attachOrLaunch(ctx, spec, port, timeout); err != nil {
		a.Close()
		return nil, err
	}

	go a.eventLoop()

	if err := a.enableDomains(ctx); err != nil {
		a.Close()
		return nil, err
	}

	if spec.Mode != "attach" {
		// 解除 --inspect-brk 的等待状态，目标会停在第一条可执行语句。
		_ = a.client.call(ctx, "Runtime.runIfWaitingForDebugger", nil, nil)
		if _, err := a.waitStop(ctx, timeout); err != nil {
			a.Close()
			return nil, err
		}
	}
	return a, nil
}

func (a *Adapter) attachOrLaunch(ctx context.Context, spec backend.LaunchSpec, port int, timeout time.Duration) error {
	if spec.Mode == "attach" {
		target, err := discoverTarget(ctx, port)
		if err != nil {
			return err
		}
		client, err := dialCDP(ctx, target.WebSocketDebugURL)
		if err != nil {
			return err
		}
		a.client = client
		return nil
	}

	args, err := injectInspectArgs(spec.Command, port)
	if err != nil {
		return err
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = spec.Cwd
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return model.New(model.CodeInternalProtocol, "获取目标输出管道失败: %v", err).Wrap(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return model.New(model.CodeInternalProtocol, "获取目标错误管道失败: %v", err).Wrap(err)
	}
	proc, err := platform.StartProc(cmd)
	if err != nil {
		return model.New(model.CodeTargetCrashed, "启动目标失败: %v", err).Wrap(err)
	}
	a.proc = proc
	a.out.capture(stdout)
	a.out.capture(stderr)

	target, err := waitForTarget(ctx, port, timeout, proc)
	if err != nil {
		return err
	}
	client, err := dialCDP(ctx, target.WebSocketDebugURL)
	if err != nil {
		return err
	}
	a.client = client
	return nil
}

func (a *Adapter) enableDomains(ctx context.Context) error {
	if err := a.client.call(ctx, "Runtime.enable", nil, nil); err != nil {
		return err
	}
	if err := a.client.call(ctx, "Debugger.enable", nil, nil); err != nil {
		return err
	}
	// 默认不因异常停下：异常的排查依赖显式断点，否则模型会被无关异常打断。
	_ = a.client.call(ctx, "Debugger.setPauseOnExceptions", map[string]any{"state": "none"}, nil)
	// 主脚本跑完后 Node 不会自行退出，而是停在“等待调试器断开”。
	// 不订阅该通知，调用方会一直以为目标在运行（采集循环尤其明显）。
	_ = a.client.call(ctx, "NodeRuntime.notifyWhenWaitingForDisconnect", map[string]any{"enabled": true}, nil)
	return nil
}

func (a *Adapter) eventLoop() {
	for {
		select {
		case msg := <-a.client.Events():
			a.handleEvent(msg)
		case <-a.client.Done():
			a.mu.Lock()
			a.closed = true
			a.mu.Unlock()
			return
		}
	}
}

func (a *Adapter) handleEvent(msg cdpMessage) {
	switch msg.Method {
	case "Debugger.paused":
		a.onPaused(msg.Params)
	case "Debugger.resumed":
		a.mu.Lock()
		a.running = true
		a.stop = nil
		a.mu.Unlock()
	case "NodeRuntime.waitingForDisconnect":
		// 主脚本已执行完毕：断开连接让目标收尾退出，等待方据此判定目标结束。
		a.mu.Lock()
		a.finished = true
		a.mu.Unlock()
		a.client.close()
	case "Debugger.scriptParsed":
		var p struct {
			ScriptID string `json:"scriptId"`
			URL      string `json:"url"`
		}
		if err := json.Unmarshal(msg.Params, &p); err == nil && p.ScriptID != "" && p.URL != "" {
			a.mu.Lock()
			a.scripts[p.ScriptID] = p.URL
			a.mu.Unlock()
		}
	}
}

// scriptURL 返回脚本地址。
func (a *Adapter) scriptURL(scriptID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scripts[scriptID]
}

func (a *Adapter) onPaused(raw json.RawMessage) {
	var p pausedParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	stop := a.buildStop(p)
	if stop.BreakpointID != "" {
		if bp := a.findBP(stop.BreakpointID); bp != nil && bp.info.Spec.LogMessage != "" {
			a.emitLogpoint(bp, p)
			_ = a.resume(context.Background())
			return
		}
	}
	a.mu.Lock()
	a.stop = stop
	a.running = false
	a.mu.Unlock()
	select {
	case a.stopCh <- stop:
	default:
	}
}

func (a *Adapter) buildStop(p pausedParams) *backend.Stop {
	frames := make([]model.Frame, 0, len(p.CallFrames))
	for i, cf := range p.CallFrames {
		scopes := make(map[string]*model.Value, len(cf.ScopeChain)+1)
		for _, sc := range cf.ScopeChain {
			scopes[scopeName(sc.Type)] = a.remoteToValue(sc.Object)
		}
		if _, ok := scopes[model.ScopeThis]; !ok {
			scopes[model.ScopeThis] = a.remoteToValue(cf.This)
		}
		file := urlToPath(cf.URL)
		if file == "" || file == "." {
			file = urlToPath(a.scriptURL(cf.Location.ScriptID))
		}
		frames = append(frames, model.Frame{
			Index:     i,
			Name:      cf.FunctionName,
			Where:     model.Position{File: file, Line: cf.Location.LineNumber + 1, Func: cf.FunctionName},
			Thread:    "main",
			Scopes:    scopes,
			BackendID: cf.CallFrameID,
		})
	}

	reason := mapReason(p.Reason)
	bpID := ""
	if len(p.HitBreakpoints) > 0 {
		// 部分 CDP 版本对断点命中上报 reason=other，命中列表才可信。
		reason = "breakpoint"
		if bp := a.findBPByCDPID(p.HitBreakpoints[0]); bp != nil {
			a.mu.Lock()
			bp.info.Hits++
			a.mu.Unlock()
			bpID = bp.info.ID
		}
	}
	snap := model.NewSnapshot("", 0, reason, "main", frames)
	return &backend.Stop{Reason: reason, Thread: "main", Snapshot: snap, BreakpointID: bpID}
}

func (a *Adapter) emitLogpoint(bp *bpState, p pausedParams) {
	message := bp.info.Spec.LogMessage
	if len(p.CallFrames) > 0 {
		frameID := p.CallFrames[0].CallFrameID
		message = a.interpolate(message, frameID)
	}
	where := ""
	if len(p.CallFrames) > 0 {
		where = fmt.Sprintf("%s:%d", urlToPath(p.CallFrames[0].URL), p.CallFrames[0].Location.LineNumber+1)
	}
	prefix := "[trace] " + bp.info.ID
	if where != "" {
		prefix += " " + where
	}
	a.out.append(prefix + " " + message)
}

// interpolate 把日志模板中的 {表达式} 替换为帧内求值结果。
func (a *Adapter) interpolate(template, callFrameID string) string {
	re := regexp.MustCompile(`\{([^{}]+)\}`)
	return re.ReplaceAllStringFunc(template, func(match string) string {
		expr := match[1 : len(match)-1]
		val, err := a.evaluate(context.Background(), callFrameID, expr)
		if err != nil {
			return "<" + expr + " 求值失败>"
		}
		return val
	})
}

func (a *Adapter) evaluate(ctx context.Context, callFrameID, expr string) (string, error) {
	var res struct {
		Result           remoteObject `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := a.client.call(ctx, "Debugger.evaluateOnCallFrame", map[string]any{
		"callFrameId":     callFrameID,
		"expression":      expr,
		"silent":          true,
		"returnByValue":   false,
		"generatePreview": false,
	}, &res); err != nil {
		return "", err
	}
	if res.ExceptionDetails != nil {
		return "", model.New(model.CodeInputInvalid, "求值异常: %s", res.ExceptionDetails.Text)
	}
	return a.remoteToValue(res.Result).Display, nil
}

func (a *Adapter) waitStop(ctx context.Context, timeout time.Duration) (*backend.Stop, error) {
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case stop := <-a.stopCh:
		return stop, nil
	case <-timer.C:
		return nil, backend.ErrStillRunning
	case <-a.client.Done():
		return nil, a.exitErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// exitErr 统一“目标已结束”的错误语义：正常跑完与异常退出要能区分。
func (a *Adapter) exitErr() error {
	a.mu.Lock()
	finished := a.finished
	a.mu.Unlock()
	if finished {
		return model.New(model.CodeTargetExited, "目标已执行完毕").
			WithFix("会话已结束；如需继续调试请重新建立会话")
	}
	return model.New(model.CodeTargetExited, "目标进程已退出").
		WithFix("查看 debug_output 中是否有崩溃信息，然后重建会话")
}

func (a *Adapter) resume(ctx context.Context) error {
	err := a.client.call(ctx, "Debugger.resume", nil, nil)
	if err == nil {
		return nil
	}
	// 连接已断开通常是目标跑完或退出，按目标结束上报而非协议错误。
	if model.CodeOf(err) == model.CodeStateDeadSession {
		return a.exitErr()
	}
	// 目标已处于运行态时 CDP 会报错，这不是失败。
	if strings.Contains(err.Error(), "not paused") || strings.Contains(err.Error(), "Can only perform operation while paused") {
		return nil
	}
	return err
}

// Continue 实现 backend.Debugger。
func (a *Adapter) Continue(ctx context.Context, timeout time.Duration) (*backend.Stop, error) {
	if a.Closed() {
		return nil, model.New(model.CodeStateDeadSession, "调试连接已断开").
			WithFix("重建会话")
	}
	deadline := time.Now().Add(timeout)
	if err := a.resume(ctx); err != nil {
		return nil, err
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, backend.ErrStillRunning
		}
		stop, err := a.waitStop(ctx, remaining)
		if err != nil {
			return nil, err
		}
		if stop.BreakpointID != "" && !a.hitSatisfied(stop.BreakpointID) {
			if err := a.resume(ctx); err != nil {
				return nil, err
			}
			continue
		}
		return stop, nil
	}
}

// Step 实现 backend.Debugger。
func (a *Adapter) Step(ctx context.Context, kind backend.StepKind, timeout time.Duration) (*backend.Stop, error) {
	method := "Debugger.stepOver"
	switch kind {
	case backend.StepInto:
		method = "Debugger.stepInto"
	case backend.StepOut:
		method = "Debugger.stepOut"
	}
	if err := a.client.call(ctx, method, nil, nil); err != nil {
		return nil, err
	}
	return a.waitStop(ctx, timeout)
}

// Pause 实现 backend.Debugger。
func (a *Adapter) Pause(ctx context.Context) (*backend.Stop, error) {
	if err := a.client.call(ctx, "Debugger.pause", nil, nil); err != nil {
		return nil, err
	}
	return a.waitStop(ctx, defaultStopTimeout)
}

// CurrentStop 实现 backend.Debugger。
func (a *Adapter) CurrentStop() *backend.Stop {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stop
}

// AddBreakpoint 实现 backend.Debugger。
func (a *Adapter) AddBreakpoint(spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if spec.Line <= 0 {
		return backend.Breakpoint{}, model.New(model.CodeInputInvalid, "断点行号必须为正数")
	}
	params := map[string]any{
		"lineNumber": spec.Line - 1,
		"urlRegex":   urlRegexFor(spec.File),
	}
	if spec.Condition != "" {
		params["condition"] = spec.Condition
	}
	var res struct {
		BreakpointID string `json:"breakpointId"`
		Locations    []struct {
			LineNumber int `json:"lineNumber"`
		} `json:"locations"`
	}
	if err := a.client.call(context.Background(), "Debugger.setBreakpointByUrl", params, &res); err != nil {
		return backend.Breakpoint{}, err
	}

	a.mu.Lock()
	a.bpSeq++
	info := backend.Breakpoint{
		ID:       fmt.Sprintf("bp%d", a.bpSeq),
		Spec:     spec,
		Verified: len(res.Locations) > 0,
	}
	if !info.Verified {
		info.Note = "脚本尚未加载或行号无可执行语句，断点将在脚本加载后生效"
	}
	a.bps = append(a.bps, &bpState{info: info, cdpID: res.BreakpointID})
	a.mu.Unlock()
	return info, nil
}

// UpdateBreakpoint 实现 backend.Debugger。
func (a *Adapter) UpdateBreakpoint(id string, spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if err := a.RemoveBreakpoint(id); err != nil {
		return backend.Breakpoint{}, err
	}
	created, err := a.AddBreakpoint(spec)
	if err != nil {
		return backend.Breakpoint{}, err
	}
	// 保持调用方持有的标识不变：标识漂移会让模型手里的断点引用凭空失效。
	a.mu.Lock()
	for _, bp := range a.bps {
		if bp.info.ID == created.ID {
			bp.info.ID = id
			created.ID = id
			break
		}
	}
	a.mu.Unlock()
	return created, nil
}

// RemoveBreakpoint 实现 backend.Debugger。
func (a *Adapter) RemoveBreakpoint(id string) error {
	a.mu.Lock()
	var target *bpState
	kept := a.bps[:0]
	for _, bp := range a.bps {
		if bp.info.ID == id {
			target = bp
			continue
		}
		kept = append(kept, bp)
	}
	a.bps = kept
	a.mu.Unlock()
	if target == nil {
		return model.New(model.CodeInputNotFound, "断点 %s 不存在", id).
			WithFix("调用 debug_breakpoint 的 list 动作查看当前断点")
	}
	if target.cdpID != "" {
		_ = a.client.call(context.Background(), "Debugger.removeBreakpoint", map[string]any{
			"breakpointId": target.cdpID,
		}, nil)
	}
	return nil
}

// ListBreakpoints 实现 backend.Debugger。
func (a *Adapter) ListBreakpoints() []backend.Breakpoint {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]backend.Breakpoint, 0, len(a.bps))
	for _, bp := range a.bps {
		out = append(out, bp.info)
	}
	return out
}

func (a *Adapter) findBP(id string) *bpState {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, bp := range a.bps {
		if bp.info.ID == id {
			return bp
		}
	}
	return nil
}

func (a *Adapter) findBPByCDPID(cdpID string) *bpState {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, bp := range a.bps {
		if bp.cdpID == cdpID {
			return bp
		}
	}
	return nil
}

func (a *Adapter) hitSatisfied(id string) bool {
	bp := a.findBP(id)
	if bp == nil || strings.TrimSpace(bp.info.Spec.HitCondition) == "" {
		return true
	}
	a.mu.Lock()
	hits := bp.info.Hits
	a.mu.Unlock()
	return hitConditionMet(bp.info.Spec.HitCondition, hits)
}

// hitConditionMet 判断命中次数是否满足条件，支持 >=n、>n、<=n、<n、==n、n。
func hitConditionMet(expr string, hits int) bool {
	e := strings.TrimSpace(expr)
	if e == "" {
		return true
	}
	ops := []string{">=", "<=", "==", ">", "<"}
	for _, op := range ops {
		if strings.HasPrefix(e, op) {
			n, err := strconv.Atoi(strings.TrimSpace(e[len(op):]))
			if err != nil {
				return true
			}
			switch op {
			case ">=":
				return hits >= n
			case "<=":
				return hits <= n
			case "==":
				return hits == n
			case ">":
				return hits > n
			case "<":
				return hits < n
			}
		}
	}
	n, err := strconv.Atoi(e)
	if err != nil {
		return true
	}
	return hits == n
}

// Language 实现 backend.Debugger。
func (a *Adapter) Language() backend.Language { return backend.NodeJS }

// Capability 实现 backend.Debugger。
func (a *Adapter) Capability() model.Capability {
	return model.Capability{
		Backend: "cdp",
		Read:    true,
		Write:   true,
		Eval:    true,
		Limits: []string{
			"不提供独立的参数作用域名，参数位于 locals 作用域",
			"大型数组展开受预算限制，请用区间取值",
		},
	}
}

// Output 实现 backend.Debugger。
func (a *Adapter) Output(maxLines int) string {
	if a.out == nil {
		return ""
	}
	return a.out.Join(maxLines)
}

// Closed 实现 backend.Debugger。
func (a *Adapter) Closed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

// Close 实现 backend.Debugger。
func (a *Adapter) Close() error {
	a.mu.Lock()
	if a.closed && a.client == nil {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()

	if a.client != nil {
		a.client.close()
	}
	if a.proc != nil && a.mode != "attach" {
		_ = a.proc.Close()
	}
	return nil
}

// ---- 平台与命令注入辅助 ----

// pickFreePort 申请一个空闲的本地端口。
func pickFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, model.New(model.CodeEnvPermission, "无法申请空闲端口: %v", err).Wrap(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// injectInspectArgs 把调试监听参数插入到 node 解释器之后。
//
// 调用方只提供原本的启动命令；若命令不是直接以 node 启动（如 npm/pnpm），
// 因为 Node 不允许在 NODE_OPTIONS 中设置 --inspect，只能给出明确指引。
func injectInspectArgs(cmd []string, port int) ([]string, error) {
	if len(cmd) == 0 {
		return nil, model.New(model.CodeInputInvalid, "启动命令为空").
			WithFix(`示例: ["node", "app.js"]`)
	}
	base := strings.ToLower(filepath.Base(cmd[0]))
	if base != "node" && base != "node.exe" {
		return nil, model.New(model.CodeEnvDebugInfo, "无法为 %s 注入调试参数", cmd[0]).
			WithFix("请直接用 node 启动入口脚本（如 node dist/main.js），或让进程带 --inspect-brk 启动后改用 attach 模式")
	}
	args := make([]string, 0, len(cmd)+1)
	args = append(args, cmd[0])
	args = append(args, fmt.Sprintf("--inspect-brk=127.0.0.1:%d", port))
	args = append(args, cmd[1:]...)
	return args, nil
}

// waitForTarget 轮询等待调试端点就绪。
func waitForTarget(ctx context.Context, port int, timeout time.Duration, proc *platform.Proc) (targetInfo, error) {
	deadline := time.Now().Add(timeout)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		target, err := discoverTarget(probeCtx, port)
		cancel()
		if err == nil {
			return target, nil
		}
		if proc != nil && !platform.IsAlive(proc.Pid()) {
			return targetInfo{}, model.New(model.CodeTargetExited, "目标进程在建立调试连接前已退出").
				WithFix("查看 debug_output 的输出确认启动失败原因")
		}
		if time.Now().After(deadline) {
			return targetInfo{}, model.New(model.CodeEnvAdapterMissing, "等待调试端口 %d 超时", port).
				WithFix("确认目标未被其他进程抢占端口，且启动命令正确")
		}
		select {
		case <-ctx.Done():
			return targetInfo{}, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// urlRegexFor 构造按文件名匹配脚本的表达式。
func urlRegexFor(file string) string {
	base := filepath.Base(platform.NormalizePath(file))
	if base == "" || base == "." {
		base = file
	}
	return ".*" + regexp.QuoteMeta(base) + "$"
}

// urlToPath 把脚本 URL 转回本地路径。
func urlToPath(url string) string {
	if url == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(url, "file://")
	trimmed = strings.TrimPrefix(trimmed, "file:")
	if idx := strings.IndexAny(trimmed, "?#"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	// Windows 上 file:///C:/a/b.js 会留下 /C:/a/b.js 形式。
	if len(trimmed) >= 3 && trimmed[0] == '/' && trimmed[2] == ':' {
		trimmed = trimmed[1:]
	}
	return platform.NormalizePath(trimmed)
}

func scopeName(cdpType string) string {
	switch cdpType {
	case "local":
		return model.ScopeLocals
	case "closure":
		return "closure"
	case "global":
		return "global"
	case "block":
		return "block"
	case "script":
		return "script"
	case "module":
		return "module"
	case "with":
		return "with"
	case "catch":
		return "catch"
	case "eval":
		return "eval"
	}
	return cdpType
}

func mapReason(cdpReason string) string {
	switch cdpReason {
	case "breakpoint":
		return "breakpoint"
	case "step":
		return "step"
	case "exception", "promiseRejection":
		return "exception"
	case "debugCommand":
		return "pause"
	case "entry":
		return "entry"
	}
	return "pause"
}

// 确保 Adapter 满足 backend.Debugger。
var _ backend.Debugger = (*Adapter)(nil)

// 允许在测试中直接注入错误。
var errNotStopped = errors.New("目标未停止")
