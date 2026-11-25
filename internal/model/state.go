package model

import (
	"fmt"
	"sync"
)

// State 是调试会话的状态。
type State string

// 会话状态取值。
const (
	// StateRunning 表示目标正在运行，此时不存在有效快照。
	StateRunning State = "running"
	// StateStopped 表示目标已停止，可进行取值、改值、求值。
	StateStopped State = "stopped"
	// StateDetached 表示会话已断开（attach 模式）或目标已释放。
	StateDetached State = "detached"
	// StateDead 表示会话已失效，需重建。
	StateDead State = "dead"
)

// transitions 是合法状态转换表。死状态为终态。
var transitions = map[State][]State{
	StateRunning:  {StateStopped, StateDetached, StateDead},
	StateStopped:  {StateRunning, StateDetached, StateDead},
	StateDetached: {StateDead},
	StateDead:     {},
}

// StateNames 按稳定顺序返回全部状态名。
func StateNames() []State {
	return []State{StateRunning, StateStopped, StateDetached, StateDead}
}

// CanTransitionTo 报告从当前状态是否可以转换到目标状态。
func (s State) CanTransitionTo(to State) bool {
	for _, allowed := range transitions[s] {
		if allowed == to {
			return true
		}
	}
	return false
}

// AllowedTargets 返回当前状态允许到达的状态。
func (s State) AllowedTargets() []State {
	out := make([]State, len(transitions[s]))
	copy(out, transitions[s])
	return out
}

// TransitionError 描述一次非法状态转换。
type TransitionError struct {
	From   State
	To     State
	Reason string
}

func (e *TransitionError) Error() string {
	msg := fmt.Sprintf("非法的状态转换: %s -> %s", e.From, e.To)
	if e.Reason != "" {
		msg += "（" + e.Reason + "）"
	}
	return msg
}

// Code 实现错误码接口，使非法状态转换统一映射为 STATE_INVALID_TRANSITION。
func (e *TransitionError) Code() Code { return CodeStateInvalidTransition }

// Machine 是带并发保护的会话状态机。
type Machine struct {
	mu    sync.RWMutex
	state State
	seq   int
}

// NewMachine 创建一个从 initial 开始的状体机。
func NewMachine(initial State) *Machine {
	return &Machine{state: initial}
}

// State 返回当前状态。
func (m *Machine) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Seq 返回已完成的转换次数，用于诊断。
func (m *Machine) Seq() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.seq
}

// Transition 尝试转换到目标状态，非法转换返回 *TransitionError。
func (m *Machine) Transition(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.CanTransitionTo(to) {
		return &TransitionError{From: m.state, To: to}
	}
	m.state = to
	m.seq++
	return nil
}

// MustTransition 在非法转换时返回错误，行为同 Transition，供上层简化调用。
func (m *Machine) MustTransition(from, to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != from {
		return &TransitionError{From: m.state, To: to, Reason: fmt.Sprintf("期望起点为 %s", from)}
	}
	if !m.state.CanTransitionTo(to) {
		return &TransitionError{From: m.state, To: to}
	}
	m.state = to
	m.seq++
	return nil
}
