package model

import "strings"

// Action 是受后端能力约束的动作。
type Action string

// 受能力约束的动作。
const (
	ActionRead        Action = "read"
	ActionWrite       Action = "write"
	ActionEval        Action = "eval"
	ActionTracepoint  Action = "tracepoint"
	ActionStackSample Action = "stackSample"
	ActionProfile     Action = "profile"
)

// actionLabels 用于生成人类可读的能力名。
var actionLabels = map[Action]string{
	ActionRead:        "读取变量",
	ActionWrite:       "修改变量",
	ActionEval:        "表达式求值",
	ActionTracepoint:  "采集点追踪",
	ActionStackSample: "栈采样",
	ActionProfile:     "性能剖析",
}

// Label 返回动作的中文名。
func (a Action) Label() string {
	if label, ok := actionLabels[a]; ok {
		return label
	}
	return string(a)
}

// Capability 描述某后端支持的动作集合及其限制。
type Capability struct {
	Backend     string `json:"backend,omitempty"`
	Read        bool   `json:"read"`
	Write       bool   `json:"write"`
	Eval        bool   `json:"eval"`
	Tracepoint  bool   `json:"tracepoint"`
	StackSample bool   `json:"stackSample"`
	Profile     bool   `json:"profile"`
	// Limits 是能力边界说明，如“不支持修改只读内存段”“仅支持同步栈”。
	Limits []string `json:"limits,omitempty"`
}

// Supports 报告是否支持某动作。
func (c Capability) Supports(a Action) bool {
	switch a {
	case ActionRead:
		return c.Read
	case ActionWrite:
		return c.Write
	case ActionEval:
		return c.Eval
	case ActionTracepoint:
		return c.Tracepoint
	case ActionStackSample:
		return c.StackSample
	case ActionProfile:
		return c.Profile
	}
	return false
}

// Ensure 在能力缺失时返回 CAP_UNSUPPORTED 错误，并附带后端名与限制说明。
func (c Capability) Ensure(a Action) error {
	if c.Supports(a) {
		return nil
	}
	name := a.Label()
	who := "当前后端"
	if c.Backend != "" {
		who = "后端 " + c.Backend
	}
	err := New(CodeCapUnsupported, "%s不支持%s", who, name)
	if c.Backend != "" {
		err = err.WithContext("backend", c.Backend)
	}
	fix := "改用支持该动作的后端，或放弃" + name
	if len(c.Limits) > 0 {
		fix += "；已知限制: " + strings.Join(c.Limits, "；")
	}
	return err.WithFix(fix)
}

// SupportedActions 返回该后端支持的动作列表，用于能力查询响应。
func (c Capability) SupportedActions() []Action {
	out := make([]Action, 0, 6)
	for _, a := range []Action{ActionRead, ActionWrite, ActionEval, ActionTracepoint, ActionStackSample, ActionProfile} {
		if c.Supports(a) {
			out = append(out, a)
		}
	}
	return out
}

// UnsupportedActions 返回该后端不支持的动作列表。
func (c Capability) UnsupportedActions() []Action {
	out := make([]Action, 0, 6)
	for _, a := range []Action{ActionRead, ActionWrite, ActionEval, ActionTracepoint, ActionStackSample, ActionProfile} {
		if !c.Supports(a) {
			out = append(out, a)
		}
	}
	return out
}
