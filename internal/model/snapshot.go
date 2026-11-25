package model

import (
	"fmt"
	"sync"
	"time"
)

// 作用域名称。
const (
	ScopeLocals = "locals"
	ScopeArgs   = "args"
	ScopeThis   = "this"
	// ScopeStatics 是类级静态成员作用域（JVM 等有类静态字段的后端提供）。
	ScopeStatics = "statics"
)

// Position 是源码位置。
type Position struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Func string `json:"func,omitempty"`
}

// String 返回 file:line 形式的位置描述。
func (p Position) String() string {
	return fmt.Sprintf("%s:%d", p.File, p.Line)
}

// Frame 是一个调用栈帧。
type Frame struct {
	Index  int               `json:"index"`
	Name   string            `json:"name,omitempty"`
	Where  Position          `json:"at"`
	Thread string            `json:"thread,omitempty"`
	Scopes map[string]*Value `json:"-"`
	// BackendID 是后端私有的帧句柄（如 CDP 的 callFrameId），供求值与求值使用。
	BackendID string `json:"-"`
}

// Snapshot 是目标停止态的不可变视图。
//
// 只有目标处于停止态时才存在有效快照；目标恢复运行后快照立即失效，且不得
// 用新状态的数据顶替，否则调用方会把改前与改后混为一谈。
type Snapshot struct {
	ID      string
	Seq     int
	Reason  string
	Thread  string
	Frames  []Frame
	Created time.Time

	mu      sync.Mutex
	refs    map[string]*Value
	nextRef int
	valid   bool
	invalid State
}

// NewSnapshot 创建一个有效快照。
func NewSnapshot(id string, seq int, reason, thread string, frames []Frame) *Snapshot {
	return &Snapshot{
		ID:      id,
		Seq:     seq,
		Reason:  reason,
		Thread:  thread,
		Frames:  frames,
		Created: time.Now(),
		refs:    make(map[string]*Value),
		valid:   true,
	}
}

// IsValid 报告快照是否仍然有效。
func (s *Snapshot) IsValid() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.valid
}

// Invalidate 作废快照并清空引用表。
func (s *Snapshot) Invalidate(state State) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.valid = false
	if state == "" {
		state = StateRunning
	}
	s.invalid = state
	s.refs = make(map[string]*Value)
}

// AddRef 登记一个待展开节点并返回引用标识。
func (s *Snapshot) AddRef(v *Value) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.valid {
		return "", s.staleError()
	}
	s.nextRef++
	id := fmt.Sprintf("r%d", s.nextRef)
	s.refs[id] = v
	return id, nil
}

// Ref 按标识取回引用节点。
func (s *Snapshot) Ref(id string) (*Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.valid {
		return nil, s.staleError()
	}
	v, ok := s.refs[id]
	if !ok {
		return nil, New(CodeInputNotFound, "引用 %s 不属于快照 %s", id, s.ID).
			WithFix("重新取一次值以获得新的引用标识")
	}
	return v, nil
}

// Frame 按序号取栈帧。
func (s *Snapshot) Frame(idx int) (*Frame, error) {
	if idx < 0 || idx >= len(s.Frames) {
		return nil, New(CodeInputNotFound, "栈帧 %d 不存在，当前仅有 %d 帧", idx, len(s.Frames)).
			WithFix("检查 frame 序号，最大可用值为 " + fmt.Sprint(len(s.Frames)-1))
	}
	return &s.Frames[idx], nil
}

// Scope 取某帧内指定作用域的值。
func (s *Snapshot) Scope(frame int, scope string) (*Value, error) {
	f, err := s.Frame(frame)
	if err != nil {
		return nil, err
	}
	v, ok := f.Scopes[scope]
	if !ok || v == nil {
		return nil, New(CodeInputNotFound, "栈帧 %d 不存在作用域 %s", frame, scope).
			WithFix("该后端只提供 locals / args / this 中的部分作用域")
	}
	return v, nil
}

// StaleSnapshotError 表示引用了已失效的快照。
type StaleSnapshotError struct {
	ID    string
	State State
	// Note 说明失效原因，如“已被新快照替代”。
	Note string
}

func (e *StaleSnapshotError) Error() string {
	if e.Note != "" {
		return fmt.Sprintf("快照 %s 已失效（%s）：目标当前处于 %s 状态", e.ID, e.Note, e.State)
	}
	return fmt.Sprintf("快照 %s 已失效：目标当前处于 %s 状态", e.ID, e.State)
}

// Code 实现错误码接口。
func (e *StaleSnapshotError) Code() Code { return CodeStateStaleSnapshot }

func (s *Snapshot) staleError() error {
	return &StaleSnapshotError{ID: s.ID, State: s.invalid}
}
