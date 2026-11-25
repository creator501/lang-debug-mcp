package native

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// gdbBackendName 是降级后端的名称。
const gdbBackendName = "gdb-mi"

// gdbInfo 是本机 gdb 的探测结果。
type gdbInfo struct {
	path    string
	version string
}

var (
	gdbOnce sync.Once
	gdbEnv  gdbInfo
	gdbErr  error
)

// discoverGDB 查找可用的 gdb，进程内只执行一次。
func discoverGDB() (gdbInfo, error) {
	gdbOnce.Do(func() {
		path, err := exec.LookPath("gdb")
		if err != nil {
			gdbErr = model.New(model.CodeEnvAdapterMissing, "PATH 中没有 gdb").
				WithFix("安装 gdb（MSYS2: pacman -S mingw-w64-ucrt-x86_64-gdb），或安装 lldb-dap 使用完整后端")
			return
		}
		out, _ := exec.Command(path, "--version").CombinedOutput()
		gdbEnv = gdbInfo{path: path, version: parseVersion(string(out))}
	})
	return gdbEnv, gdbErr
}

// miBreakpoint 是断点在 MI 会话内的状态。
type miBreakpoint struct {
	info   backend.Breakpoint
	number int
}

// MISession 是 gdb MI 后端会话，用于没有 lldb-dap 时的降级路径。
//
// 只实现基础能力：断点、继续/单步、栈帧与变量读取。改值明确不支持，
// 因为 MI 的写值语义（varobj 生命周期、类型转换）与 DAP 差异过大，
// 半支持会给出不可信的成功报告。
type MISession struct {
	lang   backend.Language
	client *miClient

	// opMu 串行化“切线程/切帧 + 命令”的组合，避免并发展开互相踩上下文。
	opMu sync.Mutex

	mu       sync.Mutex
	bps      []*miBreakpoint
	bpSeq    int
	stop     *backend.Stop
	running  bool
	closed   bool
	exitCode int
	// lastThread 是最近一次停止所在的线程号：MI 的帧操作需要它。
	lastThread int
}

// openMISession 用 gdb MI 建立降级会话。
func openMISession(ctx context.Context, lang backend.Language, spec backend.LaunchSpec) (backend.Debugger, error) {
	info, err := discoverGDB()
	if err != nil {
		return nil, err
	}
	if len(spec.Command) == 0 {
		return nil, model.New(model.CodeInputInvalid, "launch 模式必须提供启动命令")
	}
	if spec.Cwd != "" {
		if st, statErr := os.Stat(spec.Cwd); statErr != nil || !st.IsDir() {
			return nil, model.New(model.CodeInputInvalid, "工作目录不存在: %s", spec.Cwd).
				WithFix("确认 cwd 参数")
		}
	}
	client, err := startMIClient(info.path, spec.Command, spec.Cwd, spec.Env)
	if err != nil {
		return nil, err
	}
	session := &MISession{lang: lang, client: client}
	if err := session.prepare(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return session, nil
}

// prepare 完成 gdb 初始化并让目标停在入口。
func (s *MISession) prepare(ctx context.Context) error {
	for _, command := range []string{
		"-gdb-set confirm off",
		"-gdb-set pagination off",
		"-gdb-set breakpoint pending on",
	} {
		if _, err := s.client.exec(ctx, command); err != nil {
			return err
		}
	}
	// 入口停止：临时断点 main，命中后自动删除，与 DAP 的 stopOnEntry 对齐。
	_, _ = s.client.exec(ctx, "-break-insert -t main")
	if _, err := s.client.exec(ctx, "-exec-run"); err != nil {
		return err
	}
	rec, err := s.client.waitEvent(ctx, 15*time.Second, isStopEvent)
	if err != nil {
		return err
	}
	if rec.class == "" {
		// 没能在入口停下来不算失败：会话仍可设断点后继续。
		s.mu.Lock()
		s.running = true
		s.mu.Unlock()
		return nil
	}
	stop, serr := s.buildStop(rec)
	if serr != nil {
		return serr
	}
	s.setStop(stop)
	return nil
}

// isStopEvent 报告记录是否为停止事件。
func isStopEvent(rec miRecord) bool {
	return rec.kind == '*' && rec.class == "stopped"
}

// setStop 记录当前停止并标记为非运行态。
func (s *MISession) setStop(stop *backend.Stop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop = stop
	s.running = false
}

// buildStop 由 *stopped 记录构造统一停止信息。
func (s *MISession) buildStop(rec miRecord) (*backend.Stop, error) {
	reason, code := miStopReason(rec)
	if reason == "exited" {
		s.mu.Lock()
		s.exitCode = code
		s.mu.Unlock()
		return nil, model.New(model.CodeTargetExited, "目标进程已退出（exitCode=%d）", code).
			WithFix("查看 debug_output 确认退出原因，然后重建会话")
	}
	threadID := rec.payload["thread-id"].int()
	thread := fmt.Sprintf("thread %d", threadID)
	s.mu.Lock()
	s.lastThread = threadID
	s.mu.Unlock()
	frames := s.stackFrames(threadID, thread)
	snapshot := model.NewSnapshot("", 0, reason, thread, frames)
	return &backend.Stop{Reason: reason, Snapshot: snapshot, Thread: thread, BreakpointID: s.bpID(rec)}, nil
}

// miStopReason 把 MI 的停止原因映射为统一原因与退出码。
func miStopReason(rec miRecord) (string, int) {
	raw := rec.payload["reason"].str()
	switch raw {
	case "breakpoint-hit":
		return "breakpoint", 0
	case "end-stepping-range", "function-finished", "location-reached":
		return "step", 0
	case "signal-received":
		return "exception", 0
	case "exited-normally":
		return "exited", 0
	case "exited", "exited-signalled":
		return "exited", rec.payload["exit-code"].int()
	}
	if raw == "" {
		return "pause", 0
	}
	return raw, 0
}

// bpID 把命中编号映射为断点标识。
func (s *MISession) bpID(rec miRecord) string {
	number := rec.payload["bkptno"].int()
	if number <= 0 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bp := range s.bps {
		if bp.number == number {
			bp.info.Hits++
			return bp.info.ID
		}
	}
	return ""
}

// stackFrames 取回调用栈，并为头部若干帧预取作用域。
func (s *MISession) stackFrames(threadID int, thread string) []model.Frame {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	rec, err := s.client.exec(context.Background(), "-stack-list-frames")
	if err != nil {
		return nil
	}
	stack := rec.payload["stack"]
	frames := make([]model.Frame, 0, len(stack.list))
	for i, item := range stack.list {
		raw := item.get("frame")
		file := raw.get("fullname").str()
		if file == "" {
			file = raw.get("file").str()
		}
		line := raw.get("line").int()
		fn := raw.get("func").str()
		frame := model.Frame{
			Index:     i,
			Name:      fn,
			Where:     model.Position{File: platform.NormalizePath(file), Line: line, Func: fn},
			Thread:    thread,
			BackendID: strconv.Itoa(i),
		}
		if i < maxMIScopeFrames {
			frame.Scopes = s.loadScopes(threadID, i)
		}
		frames = append(frames, frame)
	}
	return frames
}

// maxMIScopeFrames 是预取作用域的栈帧数。
const maxMIScopeFrames = 10

// loadScopes 组装一帧的作用域（MI 只提供 locals）。
func (s *MISession) loadScopes(threadID, frame int) map[string]*model.Value {
	scope := &model.Value{Type: "locals", Kind: model.KindObject}
	scope.Expand = func() (*model.Value, error) { return s.expandLocals(threadID, frame) }
	return map[string]*model.Value{"locals": scope}
}

// expandLocals 取回一帧的局部变量。
func (s *MISession) expandLocals(threadID, frame int) (*model.Value, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.selectFrame(threadID, frame); err != nil {
		return nil, err
	}
	rec, err := s.client.exec(context.Background(), "-stack-list-variables --simple-values")
	if err != nil {
		return nil, err
	}
	out := &model.Value{Type: "locals", Kind: model.KindObject, Fields: map[string]*model.Value{}}
	vars := rec.payload["variables"]
	for _, item := range vars.list {
		name := item.get("name").str()
		if name == "" {
			continue
		}
		typ := item.get("type").str()
		display := item.get("value").str()
		out.Order = append(out.Order, name)
		if display == "" {
			out.Fields[name] = s.containerValue(threadID, frame, name, typ)
			continue
		}
		out.Fields[name] = &model.Value{Type: typ, Kind: model.KindScalar, Display: display, Handle: name}
	}
	out.Total = len(out.Order)
	return out, nil
}

// containerValue 构造惰性展开的复合值。
func (s *MISession) containerValue(threadID, frame int, expr, typ string) *model.Value {
	val := &model.Value{Type: typ, Kind: model.KindObject, Handle: expr}
	if typ == "" {
		val.Type = "object"
	}
	val.Expand = func() (*model.Value, error) { return s.expandExpr(threadID, frame, expr, typ) }
	return val
}

// expandExpr 用 gdb varobj 展开一个复合值。
func (s *MISession) expandExpr(threadID, frame int, expr, typ string) (*model.Value, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.selectFrame(threadID, frame); err != nil {
		return nil, err
	}
	create, err := s.client.exec(context.Background(), "-var-create - * "+quoteMI(expr))
	if err != nil {
		return nil, err
	}
	name := create.payload["name"].str()
	if name == "" {
		return nil, model.New(model.CodeInternalProtocol, "gdb 未返回变量对象名: %s", expr)
	}
	defer func() {
		_, _ = s.client.exec(context.Background(), "-var-delete "+name)
	}()

	kind := create.payload["type"].str()
	if kind == "" {
		kind = typ
	}
	if create.payload["numchild"].int() == 0 {
		return &model.Value{Type: kind, Kind: model.KindScalar, Display: create.payload["value"].str()}, nil
	}
	list, err := s.client.exec(context.Background(), "-var-list-children --all-values "+name)
	if err != nil {
		return nil, err
	}
	return s.childrenValue(threadID, frame, expr, kind, list.payload["children"]), nil
}

// childrenValue 把 varobj 子项组装为序列或对象。
func (s *MISession) childrenValue(threadID, frame int, expr, typ string, children miValue) *model.Value {
	out := &model.Value{Type: typ, Kind: model.KindObject}
	array := isArrayType(typ)
	if array {
		out.Kind = model.KindList
	}
	for _, item := range children.list {
		child := item.get("child")
		exp := child.get("exp").str()
		if exp == "" {
			continue
		}
		childType := child.get("type").str()
		display := child.get("value").str()
		childExpr := joinExpr(expr, exp, array)
		var val *model.Value
		switch {
		case child.get("numchild").int() > 0 && display == "":
			val = s.containerValue(threadID, frame, childExpr, childType)
		default:
			val = &model.Value{Type: childType, Kind: model.KindScalar, Display: display, Handle: childExpr}
		}
		if array {
			out.Items = append(out.Items, val)
			continue
		}
		name := memberName(exp)
		if name == "" {
			continue
		}
		out.Order = append(out.Order, name)
		out.Fields[name] = val
	}
	if array {
		out.Total = len(out.Items)
	} else {
		out.Total = len(out.Order)
		if out.Fields == nil {
			out.Fields = map[string]*model.Value{}
		}
	}
	return out
}

// isArrayType 报告 gdb 的类型名是否为数组（如 int [2]）。
func isArrayType(typ string) bool {
	return strings.HasSuffix(strings.TrimSpace(typ), "]")
}

// joinExpr 把子项表达式拼到父表达式上。
func joinExpr(parent, child string, array bool) string {
	if array {
		return parent + "[" + child + "]"
	}
	return parent + "." + child
}

// memberName 取成员名，去掉 C++ 访问修饰前缀（如 public:x）。
func memberName(exp string) string {
	if idx := strings.LastIndex(exp, ":"); idx >= 0 && idx+1 < len(exp) {
		return exp[idx+1:]
	}
	return exp
}

// selectFrame 切换 gdb 的当前线程与帧。
func (s *MISession) selectFrame(threadID, frame int) error {
	ctx := context.Background()
	if threadID > 0 {
		if _, err := s.client.exec(ctx, fmt.Sprintf("-thread-select %d", threadID)); err != nil {
			return err
		}
	}
	if _, err := s.client.exec(ctx, fmt.Sprintf("-stack-select-frame %d", frame)); err != nil {
		return err
	}
	return nil
}

// quoteMI 把参数转义成 MI 的 C 字符串。
func quoteMI(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(text) + `"`
}

// Language 实现 backend.Debugger。
func (s *MISession) Language() backend.Language { return s.lang }

// Capability 实现 backend.Debugger，如实声明降级能力子集。
func (s *MISession) Capability() model.Capability {
	return model.Capability{
		Backend: gdbBackendName,
		Read:    true,
		Write:   false,
		Eval:    true,
		Limits: []string{
			"lldb-dap 不可用时的降级后端：只保证断点、栈与变量读取",
			"不支持改值：改用 debug_eval 执行赋值表达式",
			"logpoint 与命中条件仅支持 >=N 形式",
		},
	}
}

// AddBreakpoint 实现 backend.Debugger。
func (s *MISession) AddBreakpoint(spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if spec.File == "" || spec.Line <= 0 {
		return backend.Breakpoint{}, model.New(model.CodeInputInvalid, "断点需要文件与行号").
			WithFix(`示例: {"file": "app.c", "line": 14}`)
	}
	command := "-break-insert -f"
	if spec.Condition != "" {
		command += " -c " + quoteMI(spec.Condition)
	}
	ignore, ok := parseHitCondition(spec.HitCondition)
	if ok {
		command += " -i " + strconv.Itoa(ignore)
	}
	location := fmt.Sprintf("%s:%d", spec.File, spec.Line)
	rec, err := s.client.exec(context.Background(), command+" "+quoteMI(location))
	if err != nil {
		return backend.Breakpoint{}, err
	}
	number := rec.payload["bkpt"].get("number").int()

	s.mu.Lock()
	s.bpSeq++
	bp := &miBreakpoint{number: number, info: backend.Breakpoint{
		ID:       fmt.Sprintf("bp%d", s.bpSeq),
		Spec:     spec,
		Verified: rec.payload["bkpt"].get("addr").str() != "",
	}}
	switch {
	case !bp.info.Verified:
		bp.info.Note = "断点待定：目标加载到该位置后生效"
	case spec.LogMessage != "":
		bp.info.Note = "gdb MI 降级后端不支持 logpoint，该断点会中断执行"
	case spec.HitCondition != "" && !ok:
		bp.info.Note = "仅支持 >=N 形式的命中条件，其余写法按普通断点处理"
	}
	s.bps = append(s.bps, bp)
	info := bp.info
	s.mu.Unlock()
	return info, nil
}

// parseHitCondition 把 ">=N" 转成 gdb 的 ignore count。
func parseHitCondition(cond string) (int, bool) {
	trimmed := strings.TrimSpace(cond)
	trimmed = strings.TrimPrefix(trimmed, ">=")
	trimmed = strings.TrimPrefix(trimmed, "≥")
	n, err := strconv.Atoi(strings.TrimSpace(trimmed))
	if err != nil || n < 2 {
		return 0, false
	}
	return n - 1, true
}

// UpdateBreakpoint 实现 backend.Debugger：MI 没有就地修改，删除后重建。
func (s *MISession) UpdateBreakpoint(id string, spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if err := s.RemoveBreakpoint(id); err != nil {
		return backend.Breakpoint{}, err
	}
	return s.AddBreakpoint(spec)
}

// RemoveBreakpoint 实现 backend.Debugger。
func (s *MISession) RemoveBreakpoint(id string) error {
	s.mu.Lock()
	var target *miBreakpoint
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
			WithFix("先调用 debug_breakpoint 的 list 动作确认标识")
	}
	if target.number > 0 {
		if _, err := s.client.exec(context.Background(), fmt.Sprintf("-break-delete %d", target.number)); err != nil {
			return err
		}
	}
	return nil
}

// ListBreakpoints 实现 backend.Debugger。
func (s *MISession) ListBreakpoints() []backend.Breakpoint {
	s.mu.Lock()
	out := make([]backend.Breakpoint, 0, len(s.bps))
	for _, bp := range s.bps {
		out = append(out, bp.info)
	}
	s.mu.Unlock()
	return out
}

// Continue 实现 backend.Debugger。
func (s *MISession) Continue(ctx context.Context, timeout time.Duration) (*backend.Stop, error) {
	return s.resume(ctx, timeout, "-exec-continue")
}

// Step 实现 backend.Debugger。
func (s *MISession) Step(ctx context.Context, kind backend.StepKind, timeout time.Duration) (*backend.Stop, error) {
	command := "-exec-next"
	switch kind {
	case backend.StepInto:
		command = "-exec-step"
	case backend.StepOut:
		command = "-exec-finish"
	}
	return s.resume(ctx, timeout, command)
}

// resume 执行一条恢复命令并等待下一次停止。
func (s *MISession) resume(ctx context.Context, timeout time.Duration, command string) (*backend.Stop, error) {
	if err := s.ensureStopped(); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	if _, err := s.client.exec(ctx, command); err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return nil, err
	}
	rec, err := s.client.waitEvent(ctx, timeout, isStopEvent)
	if err != nil {
		return nil, err
	}
	if rec.class == "" {
		return nil, backend.ErrStillRunning
	}
	stop, serr := s.buildStop(rec)
	if serr != nil {
		return nil, serr
	}
	s.setStop(stop)
	return stop, nil
}

// ensureStopped 确认目标处于停止态。
func (s *MISession) ensureStopped() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return model.New(model.CodeStateDeadSession, "gdb 会话已关闭").WithFix("重建会话")
	}
	if s.running || s.stop == nil {
		return model.New(model.CodeStateNotStopped, "目标正在运行，命令需要停止态").
			WithFix("先调用 debug_run 的 pause 中断目标")
	}
	return nil
}

// Pause 实现 backend.Debugger。
func (s *MISession) Pause(ctx context.Context) (*backend.Stop, error) {
	if _, err := s.client.exec(ctx, "-exec-interrupt"); err != nil {
		return nil, err
	}
	rec, err := s.client.waitEvent(ctx, 10*time.Second, isStopEvent)
	if err != nil {
		return nil, err
	}
	if rec.class == "" {
		return nil, backend.ErrStillRunning
	}
	stop, serr := s.buildStop(rec)
	if serr != nil {
		return nil, serr
	}
	s.setStop(stop)
	return stop, nil
}

// CurrentStop 实现 backend.Debugger。
func (s *MISession) CurrentStop() *backend.Stop {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	return s.stop
}

// Mutate 实现 backend.Debugger：降级后端明确不支持写值。
func (s *MISession) Mutate(ops []backend.MutateOp) []backend.MutateResult {
	out := make([]backend.MutateResult, 0, len(ops))
	for _, op := range ops {
		out = append(out, backend.MutateResult{
			Path:   op.Path,
			Code:   model.CodeCapUnsupported,
			Reason: "gdb MI 降级后端不支持改值；用 debug_eval 执行赋值表达式",
		})
	}
	return out
}

// Eval 实现 backend.Debugger。
func (s *MISession) Eval(in backend.EvalInput) (backend.EvalOutput, error) {
	if in.Snapshot == nil {
		return backend.EvalOutput{}, model.New(model.CodeStateNotStopped, "目标未停止，无法求值")
	}
	frame, err := in.Snapshot.Frame(in.Frame)
	if err != nil {
		return backend.EvalOutput{}, err
	}
	level, convErr := strconv.Atoi(frame.BackendID)
	if convErr != nil {
		return backend.EvalOutput{}, model.New(model.CodeCapUnsupported, "当前栈帧不支持求值")
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	threadID := s.stopThread()
	if err := s.selectFrame(threadID, level); err != nil {
		return backend.EvalOutput{}, err
	}
	rec, err := s.client.exec(context.Background(), "-data-evaluate-expression "+quoteMI(in.Expression))
	if err != nil {
		return backend.EvalOutput{}, err
	}
	return backend.EvalOutput{
		Type:       "expr",
		Value:      rec.payload["value"].str(),
		SideEffect: strings.Contains(in.Expression, "=") || strings.Contains(in.Expression, "("),
	}, nil
}

// stopThread 取当前停止所在线程号。
func (s *MISession) stopThread() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastThread
}

// Output 实现 backend.Debugger。
func (s *MISession) Output(maxLines int) string {
	return strings.Join(s.client.out.tail(maxLines), "\n")
}

// Closed 实现 backend.Debugger。
func (s *MISession) Closed() bool {
	select {
	case <-s.client.Done():
		return true
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close 实现 backend.Debugger。
func (s *MISession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.client.Close()
}
