package tool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// DefaultIdleTimeout 是会话空闲多久后释放后端进程。
const DefaultIdleTimeout = 10 * time.Minute

// Session 是一次调试会话。
//
// 会话持有后端、状态机、当前快照与断点定义。后端进程可被空闲回收后再按需
// 重建，因此断点定义保存在本结构上而不是后端里。
type Session struct {
	ID       string
	Language backend.Language
	Mode     string
	Spec     backend.LaunchSpec

	launcher backend.Launcher

	mu       sync.Mutex
	dbg      backend.Debugger
	machine  *model.Machine
	snapshot *model.Snapshot
	seq      int
	lastUsed time.Time
	mutated  bool
	// tracer 是采集驱动，非空表示该会话用过采集点。
	tracer *tracer
	// profile 是最近一次中断采样结果，供双源时间线对齐使用。
	profile *sampleReport
	// runningProfile 是进行中的外部剖析；非空时不允许重复开始。
	runningProfile *profileRun
}

// SessionInfo 是会话的对外描述。
type SessionInfo struct {
	ID          string           `json:"id"`
	Language    backend.Language `json:"language"`
	Mode        string           `json:"mode"`
	State       model.State      `json:"state"`
	Snapshot    string           `json:"snapshot,omitempty"`
	Breakpoints int              `json:"breakpoints"`
	Mutated     bool             `json:"mutated,omitempty"`
	LastUsed    time.Time        `json:"lastUsed"`
}

// Manager 管理全部会话。
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	seq      int
	idle     time.Duration
}

// Sessions 是进程内的会话管理器。
var Sessions = NewManager(DefaultIdleTimeout)

// NewManager 创建一个会话管理器。
func NewManager(idle time.Duration) *Manager {
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	return &Manager{sessions: make(map[string]*Session), idle: idle}
}

// Open 建立会话。
func (m *Manager) Open(ctx context.Context, language backend.Language, launcher backend.Launcher, spec backend.LaunchSpec) (*Session, error) {
	if launcher == nil {
		return nil, model.New(model.CodeEnvAdapterMissing, "%s 没有可用的调试后端", language).
			WithFix("已实现的调试后端：Node.js、Python、JVM、native；先调 debug_env 看该语言缺什么")
	}
	dbg, err := launcher.Open(ctx, spec)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.seq++
	session := &Session{
		ID:       fmt.Sprintf("sess%d", m.seq),
		Language: language,
		Mode:     spec.Mode,
		Spec:     spec,
		launcher: launcher,
		dbg:      dbg,
		machine:  model.NewMachine(model.StateRunning),
		lastUsed: time.Now(),
	}
	if stop := dbg.CurrentStop(); stop != nil {
		session.adoptStopLocked(stop)
	}
	m.sessions[session.ID] = session
	m.mu.Unlock()
	return session, nil
}

// Get 取回会话。
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		if len(m.sessions) == 1 {
			for _, s := range m.sessions {
				s.touch()
				return s, nil
			}
		}
		ids := make([]string, 0, len(m.sessions))
		for key := range m.sessions {
			ids = append(ids, key)
		}
		return nil, model.New(model.CodeStateUnknownSession, "存在 %d 个会话，必须显式指定 session", len(ids)).
			WithFix("传入 session 参数，可选值: " + fmt.Sprint(ids))
	}
	s, ok := m.sessions[id]
	if !ok {
		return nil, model.New(model.CodeStateUnknownSession, "会话 %s 不存在", id).
			WithFix("调用 debug_session 的 list 动作查看可用会话")
	}
	s.touch()
	return s, nil
}

// List 列出全部会话。
func (m *Manager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Info())
	}
	return out
}

// Close 关闭会话并清理后端进程树。
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return model.New(model.CodeStateUnknownSession, "会话 %s 不存在", id)
	}
	return s.Close()
}

// CloseAll 关闭所有会话。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()
	for _, s := range list {
		_ = s.Close()
	}
}

// Reap 释放空闲超时的会话后端进程，保留会话本身以便按需重建。
func (m *Manager) Reap(now time.Time) int {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	idle := m.idle
	m.mu.Unlock()

	released := 0
	for _, s := range list {
		if s.releaseIfIdle(now, idle) {
			released++
		}
	}
	return released
}

// ---- Session 方法 ----

func (s *Session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// Info 返回会话描述。
func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := SessionInfo{
		ID:       s.ID,
		Language: s.Language,
		Mode:     s.Mode,
		State:    s.machine.State(),
		Mutated:  s.mutated,
		LastUsed: s.lastUsed,
	}
	if s.snapshot != nil && s.snapshot.IsValid() {
		info.Snapshot = s.snapshot.ID
	}
	if s.dbg != nil {
		info.Breakpoints = len(s.dbg.ListBreakpoints())
	}
	return info
}

// State 返回当前状态。
func (s *Session) State() model.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.machine.State()
}

// Capability 返回后端能力。
func (s *Session) Capability() model.Capability {
	dbg, err := s.Debugger(context.Background())
	if err != nil {
		return model.Capability{}
	}
	return dbg.Capability()
}

// Debugger 返回可用的后端；被空闲回收后按需重建。
func (s *Session) Debugger(ctx context.Context) (backend.Debugger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.debuggerLocked(ctx)
}

func (s *Session) debuggerLocked(ctx context.Context) (backend.Debugger, error) {
	if s.dbg != nil && !s.dbg.Closed() {
		return s.dbg, nil
	}
	dbg, err := s.launcher.Open(ctx, s.Spec)
	if err != nil {
		return nil, err
	}
	s.dbg = dbg
	if s.machine.State() == model.StateDead || s.machine.State() == model.StateDetached {
		s.machine = model.NewMachine(model.StateRunning)
	}
	if stop := dbg.CurrentStop(); stop != nil {
		s.adoptStopLocked(stop)
	}
	return dbg, nil
}

// adoptStopLocked 接管一次停止并生成新快照。
func (s *Session) adoptStopLocked(stop *backend.Stop) *model.Snapshot {
	if stop == nil || stop.Snapshot == nil {
		return nil
	}
	// 同一次停止被重复接管时必须返回同一快照，否则调用方手里的标识会凭空失效。
	if s.snapshot != nil && s.snapshot == stop.Snapshot && s.snapshot.IsValid() {
		return s.snapshot
	}
	if s.snapshot != nil {
		s.snapshot.Invalidate(model.StateRunning)
	}
	s.seq++
	snap := stop.Snapshot
	snap.ID = fmt.Sprintf("s%d", s.seq)
	snap.Seq = s.seq
	s.snapshot = snap
	if s.machine.State() != model.StateStopped {
		_ = s.machine.Transition(model.StateStopped)
	}
	return snap
}

// ApplyStop 把后端返回的停止转换为会话当前快照。
func (s *Session) ApplyStop(stop *backend.Stop) *model.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptStopLocked(stop)
}

// MarkRunning 在目标恢复运行后作废快照。
func (s *Session) MarkRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markRunningLocked()
}

func (s *Session) markRunningLocked() {
	if s.snapshot != nil {
		s.snapshot.Invalidate(model.StateRunning)
		s.snapshot = nil
	}
	if s.machine.State() == model.StateStopped {
		_ = s.machine.Transition(model.StateRunning)
	}
}

// Snapshot 返回当前有效快照。
func (s *Session) Snapshot() (*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot != nil && s.snapshot.IsValid() {
		return s.snapshot, nil
	}
	return nil, model.New(model.CodeStateNotStopped, "目标当前未停止，没有可用的快照").
		WithFix("先设置断点并运行到命中，或调用 debug_run 的 pause 动作")
}

// RefreshAfterMutation 在改值后生成新快照并作废旧快照。
func (s *Session) RefreshAfterMutation(reason string) (*model.Snapshot, error) {
	s.mu.Lock()
	dbg := s.dbg
	s.mu.Unlock()
	if dbg == nil {
		return nil, model.New(model.CodeStateNotStopped, "目标未停止，无法刷新快照")
	}
	stop := dbg.CurrentStop()
	if stop == nil || stop.Snapshot == nil {
		return nil, model.New(model.CodeStateNotStopped, "目标未停止，无法刷新快照")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.snapshot
	s.seq++
	snap := model.NewSnapshot(fmt.Sprintf("s%d", s.seq), s.seq, stop.Reason, stop.Thread, stop.Snapshot.Frames)
	s.snapshot = snap
	s.mutated = true
	if old != nil {
		old.Invalidate(model.StateStopped)
	}
	return snap, nil
}

// releaseIfIdle 在空闲超时后释放后端进程。
func (s *Session) releaseIfIdle(now time.Time, idle time.Duration) bool {
	if t := s.tracerSnapshot(); t != nil && t.Running() {
		// 采集期间目标在跑，回收后端会让采集静默中断。
		return false
	}
	s.mu.Lock()
	last := s.lastUsed
	dbg := s.dbg
	s.mu.Unlock()
	if dbg == nil || now.Sub(last) < idle {
		return false
	}
	s.mu.Lock()
	if s.dbg != dbg {
		s.mu.Unlock()
		return false
	}
	_ = s.dbg.Close()
	s.dbg = nil
	if s.snapshot != nil {
		s.snapshot.Invalidate(model.StateRunning)
		s.snapshot = nil
	}
	_ = s.machine.Transition(model.StateDetached)
	s.mu.Unlock()
	return true
}

// setProfile 记录最近一次中断采样结果，供时间线对齐使用。
func (s *Session) setProfile(report *sampleReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profile = report
}

// lastProfile 返回最近一次采样结果；没有时返回 nil。
func (s *Session) lastProfile() *sampleReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profile
}

// tracerOf 取（必要时创建）会话的采集驱动。
func (s *Session) tracerOf() *tracer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracer == nil {
		s.tracer = newTracer(s)
	}
	return s.tracer
}

// Tracing 报告采集循环是否正在驱动目标。
func (s *Session) Tracing() bool {
	s.mu.Lock()
	t := s.tracer
	s.mu.Unlock()
	if t == nil {
		return false
	}
	return t.Running()
}

// stopTracing 停止采集循环；未在采集时是空操作。
func (s *Session) stopTracing() {
	if t := s.tracerSnapshot(); t != nil {
		t.stop()
	}
}

// tracerSnapshot 取采集驱动；未用过采集点时返回 nil。
func (s *Session) tracerSnapshot() *tracer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracer
}

// Close 关闭会话。
func (s *Session) Close() error {
	if t := s.tracerSnapshot(); t != nil {
		t.stop()
	}
	// 外部剖析进程必须随会话一起结束，否则会留下附着在被调试进程上的采样器。
	if run := s.takeProfilerRun(); run != nil {
		run.markPartial()
		run.kill()
		select {
		case <-run.done:
		case <-time.After(stopGrace):
		}
	}
	s.mu.Lock()
	dbg := s.dbg
	s.dbg = nil
	if s.snapshot != nil {
		s.snapshot.Invalidate(model.StateDead)
		s.snapshot = nil
	}
	if s.machine.State() != model.StateDead {
		_ = s.machine.Transition(model.StateDetached)
		_ = s.machine.Transition(model.StateDead)
	}
	s.mu.Unlock()
	if dbg != nil {
		return dbg.Close()
	}
	return nil
}

// PeekDebugger 返回当前后端但不重建；用于读取目标退出后仍需保留的信息（如输出）。
func (s *Session) PeekDebugger() backend.Debugger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dbg
}

// BreakpointSpecs 返回当前断点定义，用于重启后重放。
func (s *Session) BreakpointSpecs() []backend.BreakpointSpec {
	s.mu.Lock()
	dbg := s.dbg
	s.mu.Unlock()
	if dbg == nil {
		return nil
	}
	list := dbg.ListBreakpoints()
	out := make([]backend.BreakpointSpec, 0, len(list))
	for _, bp := range list {
		out = append(out, bp.Spec)
	}
	return out
}

// Restart 终止当前后端并重新启动目标，返回新的停止快照。
func (s *Session) Restart(ctx context.Context) (*model.Snapshot, error) {
	s.mu.Lock()
	old := s.dbg
	s.dbg = nil
	if s.snapshot != nil {
		s.snapshot.Invalidate(model.StateRunning)
		s.snapshot = nil
	}
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	s.mu.Lock()
	s.machine = model.NewMachine(model.StateRunning)
	s.mu.Unlock()

	dbg, err := s.Debugger(ctx)
	if err != nil {
		return nil, err
	}
	if stop := dbg.CurrentStop(); stop != nil {
		return s.ApplyStop(stop), nil
	}
	return nil, model.New(model.CodeStateNotStopped, "重启后目标未在入口停止").
		WithFix("检查启动命令是否需要交互输入")
}

// MarkDead 标记会话失效。
func (s *Session) MarkDead() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot != nil {
		s.snapshot.Invalidate(model.StateDead)
		s.snapshot = nil
	}
	if s.machine.State() != model.StateDead {
		_ = s.machine.Transition(model.StateDead)
	}
}
