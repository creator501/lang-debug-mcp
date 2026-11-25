package dap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

const (
	defaultOpenTimeout = 20 * time.Second
	defaultStopTimeout = 20 * time.Second
	// maxFrames 是单次停止收集的栈帧上限，避免深递归把返回体撑爆。
	maxFrames = 30
	// maxScopeFrames 是需要预取作用域的栈帧数：更深的帧仍可按需求值。
	maxScopeFrames = 10
)

// Dialect 描述一种语言适配器的差异。
//
// 协议层（帧读写、请求关联、停止事件、变量树）完全共用；本接口只承载“启动哪
// 个适配器、initialize/launch/attach 传什么参数”这类语言侧知识。
type Dialect interface {
	// Language 返回后端服务的语言。
	Language() backend.Language
	// Backend 返回后端名称，如 debugpy。
	Backend() string
	// Capability 返回能力清单。
	Capability() model.Capability
	// AdapterCommand 返回启动适配器进程的命令、追加环境变量与工作目录。
	AdapterCommand(spec backend.LaunchSpec) (argv []string, env []string, dir string, err error)
	// Initialize 返回 initialize 请求参数。
	Initialize() map[string]any
	// Launch 返回 launch 请求参数（目标由适配器启动）。
	Launch(spec backend.LaunchSpec) (map[string]any, error)
	// Attach 返回 attach 请求参数（目标已在运行）。
	Attach(spec backend.LaunchSpec) (map[string]any, error)
}

// ListenDialect 是可选的扩展：适配器自己在端口上承载 DAP，而不是走标准输入输出。
//
// 实现该接口后，launch 模式会先启动适配器、等它报告监听地址，再连入端口。
// dlv dap 属于这种形态，python 的 debugpy（stdio）则不需要实现。
type ListenDialect interface {
	ListenCommand(spec backend.LaunchSpec) (ListenSpec, error)
}

// EntryStopper 是可选的扩展：入口停止落在这些函数上，而不是适配器自己的占位停止。
//
// dlv 的 stopOnEntry 在目标还没初始化 Go 运行时之前就报 stopped，线程是一个占位
// 线程，栈与变量都取不到。实现本接口后，会话会在 configurationDone 之前下一个
// 内部函数断点，收到占位停止后再继续一次，把入口停止推进到有栈帧的用户代码上。
type EntryStopper interface {
	EntryFunctions() []string
}

// bpState 是一个断点在会话内的状态。
type bpState struct {
	info  backend.Breakpoint
	dapID int
	// known 表示适配器已为该断点分配标识；0 也是合法标识，不能用零值判断。
	known bool
}

// Session 实现 backend.Debugger，是 DAP 后端的通用实现。
type Session struct {
	client  *Client
	dialect Dialect
	out     *outputBuffer
	mode    string
	timeout time.Duration
	// cwd 用于把相对断点路径补全为绝对路径，DAP 断点按源码绝对路径匹配。
	cwd string

	mu         sync.Mutex
	bps        []*bpState
	bpSeq      int
	stop       *backend.Stop
	running    bool
	closed     bool
	terminated bool
	exitCode   int
	stopThread int
	threads    map[int]string
	// configPending 表示 attach 会话尚未发送 configurationDone。
	configPending bool
	// pendingStart 保存 attach 请求，其响应随 configurationDone 一起到达。
	pendingStart *Pending
	// entryBps 是内部入口断点的 DAP 标识，命中时映射为 entry 原因。
	entryBps map[int]bool

	initOnce sync.Once
	initCh   chan struct{}
	stopCh   chan *backend.Stop
	doneOnce sync.Once
	doneCh   chan struct{}
}

// Open 建立 DAP 调试会话。
//
// launch 模式由适配器启动目标；attach 模式连接目标内嵌的适配器端口。
func Open(ctx context.Context, dialect Dialect, spec backend.LaunchSpec) (backend.Debugger, error) {
	if dialect == nil {
		return nil, model.New(model.CodeInternalProtocol, "调试方言为空")
	}
	mode := spec.Mode
	if mode == "" {
		mode = "launch"
	}
	switch mode {
	case "launch":
		if len(spec.Command) == 0 {
			return nil, model.New(model.CodeInputInvalid, "launch 模式必须提供启动命令")
		}
	case "attach":
		if spec.Port <= 0 {
			return nil, model.New(model.CodeInputInvalid, "attach 模式必须提供调试端口").
				WithFix("给出目标已开启的调试端口")
		}
	default:
		return nil, model.New(model.CodeInputInvalid, "不支持的会话模式: %s", mode).
			WithFix("mode 只能是 launch 或 attach")
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultOpenTimeout
	}
	openCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		client *Client
		err    error
	)
	if mode == "attach" {
		client, err = Dial(openCtx, fmt.Sprintf("127.0.0.1:%d", spec.Port))
	} else if ld, ok := dialect.(ListenDialect); ok {
		lspec, lerr := ld.ListenCommand(spec)
		if lerr != nil {
			return nil, lerr
		}
		if lspec.Timeout <= 0 {
			lspec.Timeout = timeout
		}
		client, err = StartListening(openCtx, lspec)
	} else {
		argv, env, dir, aerr := dialect.AdapterCommand(spec)
		if aerr != nil {
			return nil, aerr
		}
		client, err = Start(argv, env, dir)
	}
	if err != nil {
		return nil, err
	}

	cwd := spec.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	s := &Session{
		client:  client,
		dialect: dialect,
		out:     newOutputBuffer(500),
		mode:    mode,
		timeout: timeout,
		cwd:     cwd,
		running: true,
		threads: make(map[int]string),
		stopCh:  make(chan *backend.Stop, 8),
		initCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go s.eventLoop()

	if err := s.handshake(openCtx, spec); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// handshake 完成 initialize → launch/attach → 断点配置 → configurationDone。
//
// 适配器可能把 launch/attach 的响应推迟到 configurationDone 之后（debugpy 就是
// 如此），所以请求先异步发出，等 initialized 事件到达后再配置断点，最后回收
// launch 的结果。
//
// attach 模式的收尾不同：configurationDone 会立刻放行目标，而调用方此时还没
// 有机会设断点。因此 attach 故意把 configurationDone 推迟到第一次恢复执行时，
// 让目标保持等待状态（对应 debugpy 的 --wait-for-client）。
func (s *Session) handshake(ctx context.Context, spec backend.LaunchSpec) error {
	if err := s.client.Request(ctx, "initialize", s.dialect.Initialize(), nil); err != nil {
		return err
	}

	command := "launch"
	var (
		args map[string]any
		err  error
	)
	if s.mode == "attach" {
		command = "attach"
		args, err = s.dialect.Attach(spec)
	} else {
		args, err = s.dialect.Launch(spec)
	}
	if err != nil {
		return err
	}

	pending, err := s.client.Send(command, args)
	if err != nil {
		return err
	}

	if s.mode == "attach" {
		select {
		case <-s.initCh:
		case <-ctx.Done():
			return model.New(model.CodeInternalProtocol, "等待适配器初始化事件超时").
				WithFix("确认目标以 debugpy 监听了该端口并处于等待客户端状态")
		case <-s.client.Done():
			return model.New(model.CodeStateDeadSession, "调试连接在初始化阶段断开").
				WithFix("确认目标仍在监听调试端口")
		}
		s.mu.Lock()
		s.pendingStart = pending
		s.configPending = true
		s.mu.Unlock()
		return nil
	}

	launchDone := make(chan error, 1)
	go func() { launchDone <- pending.Wait(ctx, nil) }()

	confirmed := false
	select {
	case <-s.initCh:
	case err := <-launchDone:
		if err != nil {
			return err
		}
		confirmed = true
	case <-ctx.Done():
		return model.New(model.CodeInternalProtocol, "等待适配器初始化事件超时").
			WithFix("确认启动命令可独立运行；若目标需要交互输入，请改用 attach 模式")
	case <-s.client.Done():
		return model.New(model.CodeEnvAdapterMissing, "调试适配器在初始化阶段退出").
			WithFix("确认适配器可执行，并查看目标输出定位启动失败原因")
	}

	// 空 filters 表示默认不在异常上中断：异常排查依赖显式断点。
	// 该请求必须在 initialized 之后，否则适配器会以“会话未初始化”拒绝。
	_ = s.client.Request(ctx, "setExceptionBreakpoints", map[string]any{"filters": []string{}}, nil)
	// 内部入口断点同样要在 configurationDone 之前下好，否则目标已经放行、来不及拦。
	s.installEntryBreakpoints(ctx)

	if err := s.client.Request(ctx, "configurationDone", map[string]any{}, nil); err != nil {
		if model.CodeOf(err) != model.CodeCapUnsupported {
			return err
		}
	}

	if !confirmed {
		select {
		case err := <-launchDone:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return model.New(model.CodeInternalProtocol, "等待 %s 响应超时", command)
		case <-s.client.Done():
			return model.New(model.CodeEnvAdapterMissing, "调试适配器已退出")
		}
	}

	if s.mode == "launch" {
		// 入口停止让调用方拿到会话后即可设断点；未收到停止不视为失败——
		// 适配器可能不支持 entry 停止，此时会话仍是可用的运行态。
		stop, werr := s.waitStop(ctx, remaining(ctx, s.timeout))
		if werr != nil {
			if errors.Is(werr, backend.ErrStillRunning) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			return werr
		}
		s.resumeToEntry(ctx, stop)
	}
	return nil
}

// installEntryBreakpoints 在 configurationDone 之前下内部入口断点。
//
// 只有声明了 EntryStopper 的方言（当前是 Go）才需要：它们适配器自带的入口停止
// 落在运行时初始化之前，那里既没有栈帧也没有变量。断点没验证通过时回退到适配器
// 自己的入口停止，不多做一次继续。
func (s *Session) installEntryBreakpoints(ctx context.Context) {
	if s.mode != "launch" {
		return
	}
	stopper, ok := s.dialect.(EntryStopper)
	if !ok {
		return
	}
	names := stopper.EntryFunctions()
	if len(names) == 0 {
		return
	}
	req := make([]map[string]any, 0, len(names))
	for _, name := range names {
		req = append(req, map[string]any{"name": name})
	}
	var resp struct {
		Breakpoints []struct {
			ID       int  `json:"id"`
			Verified bool `json:"verified"`
		} `json:"breakpoints"`
	}
	if err := s.client.Request(ctx, "setFunctionBreakpoints", map[string]any{"breakpoints": req}, &resp); err != nil {
		// 适配器不支持函数断点：保持它自己的入口停止。
		return
	}
	ids := make(map[int]bool, len(resp.Breakpoints))
	for _, bp := range resp.Breakpoints {
		if bp.Verified && bp.ID > 0 {
			ids[bp.ID] = true
		}
	}
	if len(ids) == 0 {
		// 入口函数不存在（如库或插件目标）：清掉未验证断点，避免残留状态。
		_ = s.client.Request(ctx, "setFunctionBreakpoints", map[string]any{"breakpoints": []map[string]any{}}, nil)
		return
	}
	s.mu.Lock()
	s.entryBps = ids
	s.mu.Unlock()
}

// resumeToEntry 把占位式的入口停止推进到用户代码入口。
//
// 适配器报的 entry 停止可能早于运行时初始化（dlv 就是如此），此时没有栈帧可展示；
// 已确认入口断点就绪时，主动继续一次，让入口停止落在 main.main 这类真实位置。
func (s *Session) resumeToEntry(ctx context.Context, stop *backend.Stop) {
	if stop == nil || stop.Reason != "entry" {
		return
	}
	if stop.Snapshot != nil && len(stop.Snapshot.Frames) > 0 {
		return
	}
	s.mu.Lock()
	ready := len(s.entryBps) > 0
	s.mu.Unlock()
	if !ready {
		return
	}
	if err := s.client.Request(ctx, "continue", map[string]any{"threadId": s.currentThread()}, nil); err != nil {
		return
	}
	if _, err := s.waitStop(ctx, remaining(ctx, s.timeout)); err != nil {
		// 入口断点没命中：目标已退出或入口函数不在本次调试范围内。
		s.out.appendLine("[提示] 未命中入口断点，目标可能已退出或入口函数不在本次调试范围内")
	}
}

// eventLoop 分发适配器事件，直到连接结束。
func (s *Session) eventLoop() {
	for {
		select {
		case msg := <-s.client.Events():
			s.handleEvent(msg)
		case <-s.client.Done():
			// 关闭前把已缓冲的事件处理掉，否则最后一次停止/退出可能丢失。
			for {
				select {
				case msg := <-s.client.Events():
					s.handleEvent(msg)
					continue
				default:
				}
				break
			}
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			return
		}
	}
}

func (s *Session) handleEvent(msg Message) {
	switch msg.Event {
	case "initialized":
		s.initOnce.Do(func() { close(s.initCh) })
	case "stopped":
		s.onStopped(msg.Body)
	case "continued":
		s.markRunning()
	case "output":
		s.onOutput(msg.Body)
	case "exited":
		s.onExited(msg.Body)
	case "terminated":
		s.onTerminated()
	}
}

// onOutput 记录目标输出，跳过适配器自身的遥测事件。
func (s *Session) onOutput(raw json.RawMessage) {
	var body struct {
		Category string `json:"category"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Output == "" {
		return
	}
	if body.Category == "telemetry" {
		return
	}
	s.out.appendChunk(body.Output)
}

// onExited 记录目标退出码，供后续错误与输出说明使用。
func (s *Session) onExited(raw json.RawMessage) {
	var body struct {
		ExitCode int `json:"exitCode"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return
	}
	s.mu.Lock()
	s.exitCode = body.ExitCode
	s.mu.Unlock()
	s.out.appendLine(fmt.Sprintf("[目标退出] exitCode=%d", body.ExitCode))
}

// onTerminated 标记目标已结束，唤醒所有等待停止的调用。
func (s *Session) onTerminated() {
	s.mu.Lock()
	s.terminated = true
	s.running = false
	s.stop = nil
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.doneCh) })
}

// markRunning 标记目标已恢复运行。
func (s *Session) markRunning() {
	s.mu.Lock()
	s.running = true
	s.stop = nil
	s.mu.Unlock()
}

// attachGrace 是 attach 会话首次恢复执行后等待“立刻命中”的时间。
//
// 目标通常在 configurationDone 之后自行开跑，因此这里不发送 continue（对未
// 暂停的目标发送 continue 会永远等不到响应），而是给断点一个短暂窗口。
const attachGrace = 3 * time.Second

// Continue 恢复执行并等待下一次停止；超时返回 backend.ErrStillRunning。
func (s *Session) Continue(ctx context.Context, timeout time.Duration) (*backend.Stop, error) {
	if err := s.ensureUsable(); err != nil {
		return nil, err
	}
	if s.configPendingNow() {
		if err := s.ensureConfigured(ctx); err != nil {
			return nil, err
		}
		s.drainStops()
		return s.waitStop(ctx, attachGrace)
	}
	if s.CurrentStop() == nil {
		return nil, model.New(model.CodeStateNotStopped, "目标正在运行，没有可继续的停止点").
			WithFix("先设置断点等待命中，或调用 pause 主动中断")
	}
	s.drainStops()
	s.markRunning()
	tid := s.currentThread()
	if err := s.client.Request(ctx, "continue", map[string]any{"threadId": tid}, nil); err != nil {
		return nil, err
	}
	return s.waitStop(ctx, timeout)
}

// Step 单步执行。
func (s *Session) Step(ctx context.Context, kind backend.StepKind, timeout time.Duration) (*backend.Stop, error) {
	if err := s.ensureUsable(); err != nil {
		return nil, err
	}
	if s.configPendingNow() {
		// 目标还没开始执行，单步没有落点；先放行，由断点决定第一次停止。
		if err := s.ensureConfigured(ctx); err != nil {
			return nil, err
		}
		return nil, model.New(model.CodeStateNotStopped, "目标尚未暂停，无法单步").
			WithFix("先设置断点并 continue，等命中后再单步")
	}
	if s.CurrentStop() == nil {
		return nil, model.New(model.CodeStateNotStopped, "目标正在运行，无法单步").
			WithFix("先调用 pause 中断目标")
	}
	command := "next"
	switch kind {
	case backend.StepInto:
		command = "stepIn"
	case backend.StepOut:
		command = "stepOut"
	}
	s.drainStops()
	s.markRunning()
	tid := s.currentThread()
	if err := s.client.Request(ctx, command, map[string]any{"threadId": tid}, nil); err != nil {
		return nil, err
	}
	return s.waitStop(ctx, timeout)
}

// Pause 主动中断运行中的目标。
func (s *Session) Pause(ctx context.Context) (*backend.Stop, error) {
	if err := s.ensureUsable(); err != nil {
		return nil, err
	}
	if err := s.ensureConfigured(ctx); err != nil {
		return nil, err
	}
	s.drainStops()
	if err := s.client.Request(ctx, "pause", map[string]any{"threadId": s.currentThread()}, nil); err != nil {
		return nil, err
	}
	return s.waitStop(ctx, defaultStopTimeout)
}

// configPendingNow 报告 attach 会话是否还没发送 configurationDone。
func (s *Session) configPendingNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configPending
}

// ensureConfigured 为 attach 会话补发 configurationDone。
//
// 这是放行目标（以及收到 attach 响应）的前提，因此每次恢复执行前都要确认。
func (s *Session) ensureConfigured(ctx context.Context) error {
	s.mu.Lock()
	pending := s.configPending
	start := s.pendingStart
	s.mu.Unlock()
	if !pending {
		return nil
	}

	if err := s.client.Request(ctx, "configurationDone", map[string]any{}, nil); err != nil {
		if model.CodeOf(err) != model.CodeCapUnsupported {
			return err
		}
	}
	if start != nil {
		if err := start.Wait(ctx, nil); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.configPending = false
	s.pendingStart = nil
	s.mu.Unlock()
	return nil
}

// CurrentStop 返回当前生效的停止信息。
func (s *Session) CurrentStop() *backend.Stop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

// drainStops 清空尚未消费的停止事件，避免把上一次停止当成本次结果。
func (s *Session) drainStops() {
	for {
		select {
		case <-s.stopCh:
			continue
		default:
		}
		return
	}
}

// currentThread 返回最近一次停止的线程标识。
func (s *Session) currentThread() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopThread > 0 {
		return s.stopThread
	}
	return 1
}

// waitStop 等待下一次停止。
func (s *Session) waitStop(ctx context.Context, timeout time.Duration) (*backend.Stop, error) {
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case stop := <-s.stopCh:
		if stop == nil {
			return nil, model.New(model.CodeStateNotStopped, "未获得有效的停止信息")
		}
		return stop, nil
	case <-timer.C:
		return nil, backend.ErrStillRunning
	case <-s.doneCh:
		return nil, s.targetExitedError()
	case <-s.client.Done():
		return nil, model.New(model.CodeStateDeadSession, "调试连接已断开").
			WithFix("重建会话")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ensureUsable 在会话不可用时给出稳定错误码。
func (s *Session) ensureUsable() error {
	s.mu.Lock()
	closed := s.closed
	terminated := s.terminated
	s.mu.Unlock()
	switch {
	case closed:
		return model.New(model.CodeStateDeadSession, "调试会话已断开").
			WithFix("重建会话")
	case terminated:
		return s.targetExitedError()
	}
	return nil
}

func (s *Session) targetExitedError() error {
	s.mu.Lock()
	code := s.exitCode
	s.mu.Unlock()
	return model.New(model.CodeTargetExited, "目标进程已退出（exitCode=%d）", code).
		WithFix("查看 debug_output 确认退出原因，然后重建会话")
}

// AddBreakpoint 新增断点。
func (s *Session) AddBreakpoint(spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if strings.TrimSpace(spec.File) == "" {
		return backend.Breakpoint{}, model.New(model.CodeInputInvalid, "断点必须给出源文件路径")
	}
	if spec.Line <= 0 {
		return backend.Breakpoint{}, model.New(model.CodeInputInvalid, "断点行号必须为正数")
	}
	if err := s.ensureUsable(); err != nil {
		return backend.Breakpoint{}, err
	}

	s.mu.Lock()
	s.bpSeq++
	st := &bpState{info: backend.Breakpoint{ID: fmt.Sprintf("bp%d", s.bpSeq), Spec: spec}}
	s.bps = append(s.bps, st)
	s.mu.Unlock()

	if err := s.syncFile(spec.File); err != nil {
		s.removeLocal(st.info.ID)
		return backend.Breakpoint{}, err
	}
	return s.breakpointByID(st.info.ID)
}

// UpdateBreakpoint 替换断点定义，并保持调用方持有的标识不变。
func (s *Session) UpdateBreakpoint(id string, spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if _, err := s.breakpointByID(id); err != nil {
		return backend.Breakpoint{}, err
	}
	if err := s.RemoveBreakpoint(id); err != nil {
		return backend.Breakpoint{}, err
	}
	created, err := s.AddBreakpoint(spec)
	if err != nil {
		return backend.Breakpoint{}, err
	}
	// 标识漂移会让模型手里的断点引用凭空失效，因此这里换回原标识。
	s.mu.Lock()
	for _, bp := range s.bps {
		if bp.info.ID == created.ID {
			bp.info.ID = id
			break
		}
	}
	s.mu.Unlock()
	return s.breakpointByID(id)
}

// RemoveBreakpoint 移除断点。
func (s *Session) RemoveBreakpoint(id string) error {
	s.mu.Lock()
	var target *bpState
	kept := s.bps[:0]
	for _, bp := range s.bps {
		if bp.info.ID == id {
			target = bp
			continue
		}
		kept = append(kept, bp)
	}
	s.bps = kept
	s.mu.Unlock()
	if target == nil {
		return model.New(model.CodeInputNotFound, "断点 %s 不存在", id).
			WithFix("调用 debug_breakpoint 的 list 动作查看当前断点")
	}
	if err := s.syncFile(target.info.Spec.File); err != nil {
		// 同步失败时恢复本地状态，避免本地记录与适配器不一致。
		s.mu.Lock()
		s.bps = append(s.bps, target)
		s.mu.Unlock()
		return err
	}
	return nil
}

// ListBreakpoints 列出全部断点。
func (s *Session) ListBreakpoints() []backend.Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]backend.Breakpoint, 0, len(s.bps))
	for _, bp := range s.bps {
		out = append(out, bp.info)
	}
	return out
}

func (s *Session) breakpointByID(id string) (backend.Breakpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bp := range s.bps {
		if bp.info.ID == id {
			return bp.info, nil
		}
	}
	return backend.Breakpoint{}, model.New(model.CodeInputNotFound, "断点 %s 不存在", id).
		WithFix("调用 debug_breakpoint 的 list 动作查看当前断点")
}

func (s *Session) removeLocal(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.bps[:0]
	for _, bp := range s.bps {
		if bp.info.ID != id {
			kept = append(kept, bp)
		}
	}
	s.bps = kept
}

// dapBreakpoint 是 setBreakpoints 响应中的单项。
type dapBreakpoint struct {
	ID       int    `json:"id"`
	Verified bool   `json:"verified"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
}

// syncFile 按文件整批重发断点。
//
// DAP 的 setBreakpoints 是“替换该文件的全部断点”，因此每次变更都要把同一文件
// 的断点全量重发，不能逐条增删。
func (s *Session) syncFile(file string) error {
	s.mu.Lock()
	targets := make([]*bpState, 0, len(s.bps))
	for _, bp := range s.bps {
		if platform.SamePath(bp.info.Spec.File, file) {
			targets = append(targets, bp)
		}
	}
	s.mu.Unlock()
	if len(targets) == 0 {
		return nil
	}

	request := make([]map[string]any, 0, len(targets))
	lines := make([]int, 0, len(targets))
	for _, bp := range targets {
		item := map[string]any{"line": bp.info.Spec.Line}
		if bp.info.Spec.Column > 0 {
			item["column"] = bp.info.Spec.Column
		}
		if bp.info.Spec.Condition != "" {
			item["condition"] = bp.info.Spec.Condition
		}
		if bp.info.Spec.HitCondition != "" {
			item["hitCondition"] = bp.info.Spec.HitCondition
		}
		if bp.info.Spec.LogMessage != "" {
			item["logMessage"] = bp.info.Spec.LogMessage
		}
		request = append(request, item)
		lines = append(lines, bp.info.Spec.Line)
	}

	path := platform.NormalizePath(file)
	if !filepath.IsAbs(path) && s.cwd != "" {
		path = filepath.Join(s.cwd, path)
	}

	var resp struct {
		Breakpoints []dapBreakpoint `json:"breakpoints"`
	}
	err := s.client.Request(context.Background(), "setBreakpoints", map[string]any{
		"source":         map[string]any{"path": path},
		"breakpoints":    request,
		"lines":          lines,
		"sourceModified": false,
	}, &resp)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i, target := range targets {
		if i >= len(resp.Breakpoints) {
			target.info.Verified = false
			target.info.Note = "适配器未返回该断点的状态"
			target.known = false
			continue
		}
		got := resp.Breakpoints[i]
		target.info.Verified = got.Verified
		target.dapID = got.ID
		target.known = true
		target.info.Note = got.Message
		if !got.Verified && target.info.Note == "" {
			target.info.Note = "该行暂无可用代码或文件尚未加载，断点在条件满足后自动生效"
		}
	}
	return nil
}

// Language 实现 backend.Debugger。
func (s *Session) Language() backend.Language { return s.dialect.Language() }

// Capability 实现 backend.Debugger。
func (s *Session) Capability() model.Capability { return s.dialect.Capability() }

// Output 实现 backend.Debugger。
func (s *Session) Output(maxLines int) string { return s.out.Join(maxLines) }

// Closed 报告后端是否已不可用。
func (s *Session) Closed() bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	return closed || s.client.Closed()
}

// Close 断开连接；attach 模式不终止目标进程。
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	attach := s.mode == "attach"
	s.mu.Unlock()

	if !s.client.Closed() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.client.Request(ctx, "disconnect", map[string]any{"terminateDebuggee": !attach}, nil)
		cancel()
	}
	return s.client.Close()
}

// 确保 Session 满足 backend.Debugger。
var _ backend.Debugger = (*Session)(nil)
