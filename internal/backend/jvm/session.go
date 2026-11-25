package jvm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/textbuf"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// backendName 是 JVM 后端的名称。
const backendName = "jdwp"

// maxScopeFrames 是预取作用域的最大帧数。
const maxScopeFrames = 10

// Session 是一个 JVM 调试会话。
type Session struct {
	lang  backend.Language
	mode  string
	wire  *wire
	proc  *platform.Proc
	out   *textbuf.Buffer
	cache *cache

	mu         sync.Mutex
	bps        []*bp
	bpSeq      int
	stop       *backend.Stop
	running    bool
	closed     bool
	terminated bool
	exitCode   int
	stopThread uint64
	threads    map[uint64]string
	sourceDirs []string
	cpReq      requestID
	cpActive   bool
	stepReq    requestID
	stepActive bool

	stopCh   chan *backend.Stop
	doneCh   chan struct{}
	doneOnce sync.Once
}

// Language 实现 backend.Debugger。
func (s *Session) Language() backend.Language { return s.lang }

// Backend 返回后端名。
func (s *Session) Backend() string { return backendName }

// Capability 实现 backend.Debugger。
func (s *Session) Capability() model.Capability {
	return model.Capability{
		Backend: backendName,
		Read:    true,
		Write:   true,
		Eval:    false,
		Limits: []string{
			"JDWP 不支持表达式求值与条件断点，条件需要写进代码或用断点命中次数在客户端过滤",
			"集合按后端识别的结构展示：ArrayList/List.of、HashMap/LinkedHashMap、HashSet、LinkedList 会被展开，" +
				"其他集合（TreeMap、并发集合、自定义集合）按内部字段展示",
			"包装类型（Integer/Long/Boolean 等）展开为对应的原始值，lambda 捕获变量按普通字段展示",
			"静态字段放在 statics 作用域（按声明类分组），可读可写；实例字段里不会混入静态成员",
			"对象一律按字段渲染，不调用 toString()：它可能带副作用，返回的字符串也不可信",
			"没有局部变量表（javac 未加 -g）时读不到 locals/args，只能看 this",
			"数组与集合单次最多取回 100 项，超出部分需要区间取值",
			"方法调用、异常断点与监视点未实现",
		},
	}
}

// Output 实现 backend.Debugger。
func (s *Session) Output(maxLines int) string { return s.out.Tail(maxLines) }

// Closed 实现 backend.Debugger。
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || s.terminated
}

// Close 实现 backend.Debugger。
//
// attach 只断开（目标继续运行），launch 会连目标进程树一起清理。
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	mode := s.mode
	proc := s.proc
	s.mu.Unlock()

	if mode == "attach" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.wire.dispose(ctx)
		cancel()
	}
	_ = s.wire.Close()
	s.doneOnce.Do(func() { close(s.doneCh) })
	if proc != nil {
		return proc.Close()
	}
	return nil
}

// CurrentStop 实现 backend.Debugger。
func (s *Session) CurrentStop() *backend.Stop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

// markRunning 清空当前停止并标记运行中。
func (s *Session) markRunning() {
	s.mu.Lock()
	s.running = true
	s.stop = nil
	s.mu.Unlock()
}

// drainStops 清空未消费的停止事件。
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

// ensureUsable 检查会话是否还能执行命令。
func (s *Session) ensureUsable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return model.New(model.CodeStateDeadSession, "会话已关闭").
			WithFix("重新调用 debug_session 的 open 建立会话")
	}
	if s.terminated {
		return model.New(model.CodeTargetExited, "目标已退出（exitCode=%d）", s.exitCode).
			WithFix("查看 debug_output 与 debug_state；需要继续排查时重建会话")
	}
	return nil
}

// Continue 实现 backend.Debugger。
func (s *Session) Continue(ctx context.Context, timeout time.Duration) (*backend.Stop, error) {
	if err := s.ensureUsable(); err != nil {
		return nil, err
	}
	if s.CurrentStop() == nil {
		return nil, model.New(model.CodeStateNotStopped, "目标正在运行，没有可继续的停止点").
			WithFix("先设置断点等待命中，或调用 pause 主动中断")
	}
	s.drainStops()
	s.clearStepRequest(ctx)
	s.markRunning()
	if err := s.wire.resumeAll(ctx); err != nil {
		return nil, err
	}
	return s.waitStop(ctx, timeout)
}

// Step 实现 backend.Debugger。
func (s *Session) Step(ctx context.Context, kind backend.StepKind, timeout time.Duration) (*backend.Stop, error) {
	if err := s.ensureUsable(); err != nil {
		return nil, err
	}
	stop := s.CurrentStop()
	if stop == nil {
		return nil, model.New(model.CodeStateNotStopped, "目标正在运行，无法单步").
			WithFix("先调用 pause 中断目标")
	}
	thread := s.currentThread()

	depth := int32(stepOver)
	switch kind {
	case backend.StepInto:
		depth = stepInto
	case backend.StepOut:
		depth = stepOut
	}
	s.clearStepRequest(ctx)
	id, err := s.wire.setEvent(ctx, evSingleStep, suspendAll, []eventModifier{
		{kind: modStep, thread: thread, size: stepLine, depth: depth},
	})
	if err != nil {
		return nil, wrapJDWP(err)
	}
	s.mu.Lock()
	s.stepReq, s.stepActive = id, true
	s.mu.Unlock()

	s.drainStops()
	s.markRunning()
	if err := s.wire.resumeAll(ctx); err != nil {
		return nil, err
	}
	return s.waitStop(ctx, timeout)
}

// Pause 实现 backend.Debugger。
//
// JDWP 没有“主动挂起事件”，因此挂起后由本端挑选线程并合成一次停止。
func (s *Session) Pause(ctx context.Context) (*backend.Stop, error) {
	if err := s.ensureUsable(); err != nil {
		return nil, err
	}
	if err := s.wire.suspendAll(ctx); err != nil {
		return nil, wrapJDWP(err)
	}
	thread := s.pickPauseThread(ctx)
	stop := s.buildStop(ctx, thread, "pause", 0)
	s.publishStop(stop)
	return stop, nil
}

// currentThread 返回最近一次停止的线程，默认主线程。
func (s *Session) currentThread() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopThread != 0 {
		return s.stopThread
	}
	return 1
}

// waitStop 等待下一次停止。
func (s *Session) waitStop(ctx context.Context, timeout time.Duration) (*backend.Stop, error) {
	if stop := s.CurrentStop(); stop != nil {
		return stop, nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case stop := <-s.stopCh:
			return stop, nil
		case <-timer.C:
			return nil, backend.ErrStillRunning
		case <-ctx.Done():
			return nil, backend.ErrStillRunning
		case <-s.doneCh:
			s.mu.Lock()
			code := s.exitCode
			s.mu.Unlock()
			return nil, model.New(model.CodeTargetExited, "目标已退出（exitCode=%d）", code).
				WithFix("查看 debug_output 与 debug_state；需要继续排查时重建会话")
		}
	}
}

// publishStop 记录一次停止并唤醒等待者。
func (s *Session) publishStop(stop *backend.Stop) {
	if stop == nil {
		return
	}
	s.mu.Lock()
	if s.closed || s.terminated {
		s.mu.Unlock()
		return
	}
	if stop.Thread != "" {
		// 线程名只用于展示，按名字回填 stopThread 由调用方传入的线程标识决定。
	}
	s.stop = stop
	s.running = false
	s.mu.Unlock()

	select {
	case s.stopCh <- stop:
	default:
	}
}

// buildStop 收集线程栈帧并生成快照。
func (s *Session) buildStop(ctx context.Context, threadID uint64, reason string, reqID requestID) *backend.Stop {
	threadName := s.threadNameOf(ctx, threadID)
	frames := s.loadFrames(ctx, threadID)
	snap := model.NewSnapshot("", 0, reason, threadName, frames)
	stop := &backend.Stop{Reason: reason, Thread: threadName, Snapshot: snap}
	if bp := s.breakpointByRequest(reqID); bp != nil {
		stop.BreakpointID = bp.id
	}
	s.mu.Lock()
	if threadID != 0 {
		s.stopThread = threadID
	}
	s.mu.Unlock()
	return stop
}

// threadNameOf 返回线程名，带缓存。
func (s *Session) threadNameOf(ctx context.Context, threadID uint64) string {
	if threadID == 0 {
		return ""
	}
	s.mu.Lock()
	name, ok := s.threads[threadID]
	s.mu.Unlock()
	if ok {
		return name
	}
	name, err := s.wire.threadName(ctx, threadID)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	s.threads[threadID] = name
	s.mu.Unlock()
	return name
}

// loadFrames 读取线程的调用栈并预取前若干帧的作用域。
func (s *Session) loadFrames(ctx context.Context, threadID uint64) []model.Frame {
	infos, err := s.wire.frames(ctx, threadID, 0, -1)
	if err != nil || len(infos) == 0 {
		return nil
	}
	thread := s.threadNameOf(ctx, threadID)
	frames := make([]model.Frame, 0, len(infos))
	for i, f := range infos {
		name := "<unknown>"
		file := ""
		if ci, cerr := s.info(ctx, f.loc.classID); cerr == nil {
			name = s.methodName(ctx, f.loc.classID, f.loc.methodID)
			file = s.sourcePath(ci.source)
		}
		frame := model.Frame{
			Index:     i,
			Name:      name,
			Where:     model.Position{File: file, Line: s.lineAt(ctx, f.loc.classID, f.loc.methodID, f.loc.index), Func: name},
			Thread:    thread,
			BackendID: u2s(threadID) + ":" + u2s(f.id),
		}
		if i < maxScopeFrames {
			frame.Scopes = s.loadScopes(ctx, threadID, f)
		}
		frames = append(frames, frame)
	}
	return frames
}

// sourcePath 把 JDWP 给出的源文件名补全为可点击的路径。
//
// JDWP 只提供文件名（不含目录），这里用已知源码目录（会话工作目录与断点所在
// 目录）拼出完整路径；都找不到时退回文件名本身。
func (s *Session) sourcePath(base string) string {
	if base == "" {
		return ""
	}
	if filepath.IsAbs(base) {
		return base
	}
	s.mu.Lock()
	dirs := append([]string{}, s.sourceDirs...)
	s.mu.Unlock()
	for _, dir := range dirs {
		candidate := filepath.Join(dir, base)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return base
}

// addSourceDir 登记一个源码目录。
func (s *Session) addSourceDir(file string) {
	dir := filepath.Dir(file)
	if dir == "" || dir == "." {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.sourceDirs {
		if existing == dir {
			return
		}
	}
	s.sourceDirs = append(s.sourceDirs, dir)
}

// slotRef 是一个可读的局部变量槽。
type slotRef struct {
	name string
	sig  string
	slot int
}

// loadScopes 构造 locals / args / this / statics 四个作用域（均为惰性展开）。
//
// statics 不依赖局部变量表，因此即使方法没带 -g 也能看类状态；其余三个作用域
// 需要变量表才能定位槽位。
func (s *Session) loadScopes(ctx context.Context, threadID uint64, f frameInfo) map[string]*model.Value {
	scopes := make(map[string]*model.Value, 4)
	if st := s.staticScope(ctx, f.loc.classID); st != nil {
		scopes[model.ScopeStatics] = st
	}
	if _, ok := s.methodOf(ctx, f.loc.classID, f.loc.methodID); !ok {
		return scopes
	}
	vars, err := s.varTableOf(ctx, f.loc.classID, f.loc.methodID)
	if err != nil || vars == nil {
		return scopes
	}

	var args, locals []slotRef
	for _, slot := range vars.slots {
		if slot.name == "this" || slot.name == "" {
			continue
		}
		// 变量表里同一槽位在不同区间会重复出现，只保留命中当前代码位置的条目。
		if f.loc.index < slot.codeIndex || f.loc.index >= slot.codeIndex+uint64(slot.length) {
			continue
		}
		ref := slotRef{name: slot.name, sig: slot.signature, slot: slot.slot}
		if slot.slot < vars.argCount {
			args = append(args, ref)
		}
		// locals 与其它后端保持一致：包含参数在内的全部可见变量，args 只列参数。
		locals = append(locals, ref)
	}

	if this, terr := s.wire.thisObject(ctx, threadID, f.id); terr == nil && this != 0 {
		scopes[model.ScopeThis] = s.makeValue(ctx, "Ljava/lang/Object;", value{tag: tagObject, ref: this},
			"obj:"+u2s(this))
	}
	if len(locals) > 0 {
		scopes[model.ScopeLocals] = s.slotScope(threadID, f.id, locals, "locals")
	}
	if len(args) > 0 {
		scopes[model.ScopeArgs] = s.slotScope(threadID, f.id, args, "args")
	}
	// 没有变量表时仍提供空作用域，让调用方看到“可读但没有变量”而不是路径不存在。
	if _, ok := scopes[model.ScopeLocals]; !ok {
		scopes[model.ScopeLocals] = &model.Value{Type: "locals", Kind: model.KindObject,
			Fields: map[string]*model.Value{}}
	}
	return scopes
}

// slotScope 构造一个惰性读取槽位值的作用域节点。
func (s *Session) slotScope(threadID, frameID uint64, slots []slotRef, label string) *model.Value {
	node := &model.Value{Type: label, Kind: model.KindObject}
	node.Expand = func() (*model.Value, error) {
		ctx := context.Background()
		want := make([]frameSlot, 0, len(slots))
		for _, sl := range slots {
			want = append(want, frameSlot{slot: sl.slot, tag: sigTag(sl.sig)})
		}
		vals, err := s.wire.frameValues(ctx, threadID, frameID, want)
		if err != nil {
			return nil, wrapJDWP(err)
		}
		out := &model.Value{Type: label, Kind: model.KindObject, Fields: make(map[string]*model.Value, len(slots))}
		for i, sl := range slots {
			if i >= len(vals) {
				break
			}
			handle := "slot:" + u2s(threadID) + ":" + u2s(frameID) + ":" + strconv.Itoa(sl.slot) + ":" + sl.sig
			out.Fields[sl.name] = s.makeValue(ctx, sl.sig, vals[i], handle)
			out.Order = append(out.Order, sl.name)
		}
		return out, nil
	}
	return node
}

// pickPauseThread 在挂起后挑选一个用户线程用于展示。
func (s *Session) pickPauseThread(ctx context.Context) uint64 {
	threads, err := s.wire.allThreads(ctx)
	if err != nil || len(threads) == 0 {
		return s.currentThread()
	}
	mainThread := uint64(0)
	for _, t := range threads {
		name := s.threadNameOf(ctx, t)
		if name == "main" {
			mainThread = t
			break
		}
	}
	if mainThread != 0 {
		return mainThread
	}
	for _, t := range threads {
		if isSystemThread(s.threadNameOf(ctx, t)) {
			continue
		}
		if frames, ferr := s.wire.frames(ctx, t, 0, 1); ferr == nil && len(frames) > 0 {
			return t
		}
	}
	return threads[0]
}

// isSystemThread 报告线程是否为 JVM 内部线程。
func isSystemThread(name string) bool {
	prefixes := []string{
		"Reference Handler", "Finalizer", "Signal Dispatcher", "Attach Listener",
		"Common-Cleaner", "Notification Thread", "Service Thread", "Monitor Deflation",
		"Sweeper thread", "DestroyJavaVM", "C1 CompilerThread", "C2 CompilerThread",
	}
	for _, p := range prefixes {
		if name == p || (len(name) > len(p) && name[:len(p)] == p) {
			return true
		}
	}
	return false
}

// Eval 实现 backend.Debugger：JDWP 没有表达式求值能力。
func (s *Session) Eval(in backend.EvalInput) (backend.EvalOutput, error) {
	return backend.EvalOutput{}, model.New(model.CodeCapUnsupported,
		"后端 %s 不支持表达式求值", backendName).
		WithFix("用 debug_inspect 读取具体变量；需要计算结果时在目标代码里加一行，或用 debug_mutate 改值后观察")
}

// wrapJDWP 把协议错误转换为统一错误。
func wrapJDWP(err error) error {
	if err == nil {
		return nil
	}
	if je, ok := err.(*jdwpError); ok {
		return je.toModel()
	}
	return err
}

// frameOf 解析帧句柄中的线程与帧标识。
func frameOf(backendID string) (threadID, frameID uint64, ok bool) {
	var t, f string
	if _, err := fmt.Sscanf(backendID, "%s", &t); err != nil {
		return 0, 0, false
	}
	for i := 0; i < len(backendID); i++ {
		if backendID[i] == ':' {
			t, f = backendID[:i], backendID[i+1:]
			break
		}
	}
	tid, err1 := strconv.ParseUint(t, 10, 64)
	fid, err2 := strconv.ParseUint(f, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return tid, fid, true
}
