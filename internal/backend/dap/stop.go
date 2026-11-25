package dap

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// stopEvent 是 stopped 事件的参数。
type stopEvent struct {
	Reason           string `json:"reason"`
	ThreadID         int    `json:"threadId"`
	HitBreakpointIDs []int  `json:"hitBreakpointIds"`
	Description      string `json:"description"`
	Text             string `json:"text"`
}

// stackFrame 是 stackTrace 响应中的一帧。
type stackFrame struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Source *struct {
		Path string `json:"path"`
	} `json:"source"`
}

// onStopped 把一次停止转换为统一模型的快照并唤醒等待者。
//
// 停止事件本身只带线程与原因，栈帧与作用域需要额外两轮请求；这些请求在事件
// 循环内顺序完成，保证调用方拿到停止时快照已经可用。
func (s *Session) onStopped(raw json.RawMessage) {
	var e stopEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return
	}
	stop := s.buildStop(e)

	s.mu.Lock()
	if s.terminated {
		s.mu.Unlock()
		return
	}
	s.stop = stop
	s.running = false
	if e.ThreadID > 0 {
		s.stopThread = e.ThreadID
	}
	s.mu.Unlock()

	select {
	case s.stopCh <- stop:
	default:
		// 已有未消费的停止，保留最新结果由 CurrentStop 提供。
	}
}

// buildStop 收集一帧以上的停止信息。
func (s *Session) buildStop(e stopEvent) *backend.Stop {
	reason := mapStopReason(e.Reason)
	thread := s.threadName(e.ThreadID)
	frames := s.loadFrames(e.ThreadID, thread)

	bpID := ""
	if len(e.HitBreakpointIDs) > 0 {
		if s.isEntryBreakpoint(e.HitBreakpointIDs[0]) {
			// 内部入口断点：对调用方仍然是「入口停止」，不外露内部断点标识。
			reason = "entry"
		} else {
			reason = "breakpoint"
			bpID = s.hitBreakpoint(e.HitBreakpointIDs[0])
		}
	}
	if e.Reason == "exception" {
		reason = "exception"
		if e.Description != "" {
			s.out.appendLine("[异常] " + e.Description)
		} else if e.Text != "" {
			s.out.appendLine("[异常] " + e.Text)
		}
	}
	snap := model.NewSnapshot("", 0, reason, thread, frames)
	return &backend.Stop{Reason: reason, Thread: thread, Snapshot: snap, BreakpointID: bpID}
}

// loadFrames 收集调用栈，并为头部若干帧预取作用域。
func (s *Session) loadFrames(threadID int, thread string) []model.Frame {
	if threadID <= 0 {
		// 有的适配器（dlv）在入口停止事件里不带 threadId，但线程列表已经可用；
		// 直接放弃会让调用方拿到没有栈帧的停止快照。
		threadID = s.firstThread()
		if threadID <= 0 {
			return nil
		}
	}
	frames, err := s.stackFrames(threadID, thread)
	if err != nil {
		return nil
	}
	return frames
}

// stackFrames 请求一次栈并转换为统一模型。
func (s *Session) stackFrames(threadID int, thread string) ([]model.Frame, error) {
	var resp struct {
		StackFrames []stackFrame `json:"stackFrames"`
	}
	err := s.client.Request(context.Background(), "stackTrace", map[string]any{
		"threadId":   threadID,
		"startFrame": 0,
		"levels":     maxFrames,
	}, &resp)
	if err != nil {
		return nil, err
	}
	frames := make([]model.Frame, 0, len(resp.StackFrames))
	for i, sf := range resp.StackFrames {
		file := ""
		if sf.Source != nil {
			file = platform.NormalizePath(sf.Source.Path)
		}
		frame := model.Frame{
			Index:     i,
			Name:      sf.Name,
			Where:     model.Position{File: file, Line: sf.Line, Func: sf.Name},
			Thread:    thread,
			BackendID: strconv.Itoa(sf.ID),
		}
		if i < maxScopeFrames {
			frame.Scopes = s.loadScopes(sf.ID)
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

// firstThread 取线程列表里的第一个线程，用于停止事件没给 threadId 的适配器。
func (s *Session) firstThread() int {
	var resp struct {
		Threads []struct {
			ID int `json:"id"`
		} `json:"threads"`
	}
	if err := s.client.Request(context.Background(), "threads", nil, &resp); err != nil {
		return 0
	}
	for _, t := range resp.Threads {
		if t.ID > 0 {
			return t.ID
		}
	}
	return 0
}

// scopeInfo 是 scopes 响应中的作用域。
type scopeInfo struct {
	Name               string `json:"name"`
	PresentationHint   string `json:"presentationHint"`
	VariablesReference int    `json:"variablesReference"`
	Expensive          bool   `json:"expensive"`
}

// loadScopes 取回某帧的作用域，每个作用域都是按需展开的节点。
func (s *Session) loadScopes(frameID int) map[string]*model.Value {
	var resp struct {
		Scopes []scopeInfo `json:"scopes"`
	}
	if err := s.client.Request(context.Background(), "scopes", map[string]any{"frameId": frameID}, &resp); err != nil {
		return nil
	}
	scopes := make(map[string]*model.Value, len(resp.Scopes))
	for _, sc := range resp.Scopes {
		if sc.VariablesReference == 0 {
			continue
		}
		name := scopeName(sc)
		if _, exists := scopes[name]; exists {
			// 同名作用域（如多个 Locals）加序号区分，避免后者覆盖前者。
			name = name + "-" + strconv.Itoa(len(scopes)+1)
		}
		scopes[name] = s.scopeValue(sc.VariablesReference, name)
	}
	return scopes
}

// scopeName 把 DAP 作用域映射为统一模型的稳定名称。
func scopeName(sc scopeInfo) string {
	switch strings.ToLower(strings.TrimSpace(sc.PresentationHint)) {
	case "locals":
		return model.ScopeLocals
	case "arguments", "parameters":
		return model.ScopeArgs
	case "globals":
		return "globals"
	}
	name := strings.ToLower(strings.TrimSpace(sc.Name))
	switch name {
	case "locals", "local":
		return model.ScopeLocals
	case "arguments", "args", "parameters":
		return model.ScopeArgs
	case "globals", "global":
		return "globals"
	}
	if name == "" {
		name = "scope"
	}
	return name
}

// threadName 返回线程名，必要时拉取一次线程列表。
func (s *Session) threadName(id int) string {
	if id <= 0 {
		return ""
	}
	s.mu.Lock()
	name, ok := s.threads[id]
	s.mu.Unlock()
	if ok && name != "" {
		return name
	}
	var resp struct {
		Threads []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"threads"`
	}
	if err := s.client.Request(context.Background(), "threads", map[string]any{}, &resp); err != nil {
		return ""
	}
	s.mu.Lock()
	for _, t := range resp.Threads {
		s.threads[t.ID] = t.Name
	}
	name = s.threads[id]
	s.mu.Unlock()
	return name
}

// isEntryBreakpoint 报告该 DAP 断点是否是内部入口断点。
func (s *Session) isEntryBreakpoint(dapID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entryBps[dapID]
}

// hitBreakpoint 记录命中并返回本会话内的断点标识。
//
// 命中条件由适配器在目标侧执行，条件未满足的命中不会通知客户端，因此 Hits 是
// “观察到的中断次数”，不等于该行真实执行次数。
func (s *Session) hitBreakpoint(dapID int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bp := range s.bps {
		if bp.known && bp.dapID == dapID {
			bp.info.Hits++
			return bp.info.ID
		}
	}
	return ""
}

// mapStopReason 把 DAP 的停止原因映射为统一模型的稳定取值。
func mapStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "entry":
		return "entry"
	case "breakpoint", "function breakpoint", "data breakpoint", "instruction breakpoint":
		return "breakpoint"
	case "step":
		return "step"
	case "pause", "debugger statement":
		return "pause"
	case "exception", "exception caught", "exception uncaught":
		return "exception"
	case "":
		return "pause"
	}
	return strings.ToLower(strings.TrimSpace(reason))
}

// remaining 返回 ctx 的剩余时间；没有截止时间时使用 fallback。
func remaining(ctx context.Context, fallback time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left > 0 {
			return left
		}
		return time.Millisecond
	}
	return fallback
}
