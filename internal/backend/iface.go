package backend

import (
	"context"
	"errors"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// ErrStillRunning 表示在超时内目标未停止，仍处于运行状态。
//
// 这不是失败：调用方应据会话状态继续等待或改为查询，而不是重建会话。
var ErrStillRunning = errors.New("目标仍在运行，未在超时内停止")

// StepKind 是单步执行的方式。
type StepKind string

// 单步方式。
const (
	StepInto StepKind = "into"
	StepOver StepKind = "over"
	StepOut  StepKind = "out"
)

// BreakpointSpec 是断点定义。
type BreakpointSpec struct {
	File         string `json:"file"`
	Line         int    `json:"line"`
	Column       int    `json:"column,omitempty"`
	Condition    string `json:"condition,omitempty"`
	HitCondition string `json:"hitCondition,omitempty"`
	// LogMessage 非空时该断点不中断执行，只记录一条带插值的消息（logpoint）。
	LogMessage string `json:"logMessage,omitempty"`
}

// Breakpoint 是断点在会话内的状态。
type Breakpoint struct {
	ID       string         `json:"id"`
	Spec     BreakpointSpec `json:"spec"`
	Verified bool           `json:"verified"`
	Hits     int            `json:"hits"`
	Note     string         `json:"note,omitempty"`
}

// MutateOp 是一条改值操作。
type MutateOp struct {
	Snapshot string `json:"-"`
	Frame    int    `json:"-"`
	Path     string `json:"path"`
	Value    string `json:"value"`
}

// MutateResult 是单条改值操作的结果。
//
// 批量改值不承诺原子性：逐条执行、逐条报告，成功时必须回传原值以便恢复现场。
type MutateResult struct {
	Path     string `json:"path"`
	OK       bool   `json:"ok"`
	OldValue string `json:"oldValue,omitempty"`
	Type     string `json:"t,omitempty"`
	// Code 是失败时的稳定错误码；成功时为空。
	Code   model.Code `json:"code,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// EvalInput 是求值请求。
type EvalInput struct {
	Snapshot         *model.Snapshot
	Frame            int
	Expression       string
	AllowSideEffects bool
}

// EvalOutput 是求值结果。
type EvalOutput struct {
	Type       string `json:"t,omitempty"`
	Value      string `json:"v,omitempty"`
	SideEffect bool   `json:"sideEffect,omitempty"`
}

// Stop 描述一次停止。
type Stop struct {
	Reason   string          `json:"reason"`
	Thread   string          `json:"thread,omitempty"`
	Snapshot *model.Snapshot `json:"-"`
	// BreakpointID 是命中的断点标识（若因断点停止）。
	BreakpointID string `json:"breakpointId,omitempty"`
}

// Debugger 是工具层依赖的调试后端接口。
//
// 实现者只负责协议翻译与能力边界，不负责参数校验、预算裁剪与错误码归一——
// 那些属于工具层与 model 层。
type Debugger interface {
	// Language 返回后端服务的语言。
	Language() Language
	// Capability 返回能力清单。
	Capability() model.Capability

	AddBreakpoint(spec BreakpointSpec) (Breakpoint, error)
	UpdateBreakpoint(id string, spec BreakpointSpec) (Breakpoint, error)
	RemoveBreakpoint(id string) error
	ListBreakpoints() []Breakpoint

	// Continue 恢复执行并等待下一次停止；超时返回 ErrStillRunning。
	Continue(ctx context.Context, timeout time.Duration) (*Stop, error)
	// Step 单步执行。
	Step(ctx context.Context, kind StepKind, timeout time.Duration) (*Stop, error)
	// Pause 主动中断运行中的目标。
	Pause(ctx context.Context) (*Stop, error)
	// CurrentStop 返回当前生效的停止信息；目标运行中时返回 nil。
	CurrentStop() *Stop

	// Mutate 批量改值。
	Mutate(ops []MutateOp) []MutateResult
	// Eval 在指定栈帧内求值。
	Eval(in EvalInput) (EvalOutput, error)

	// Output 返回程序标准输出与标准错误的尾部。
	Output(maxLines int) string
	// Closed 报告后端是否已不可用。
	Closed() bool
	// Close 释放后端资源（attach 模式仅断开，launch 模式终止目标）。
	Close() error
}

// Launcher 是可由工具层创建的后端。
type Launcher interface {
	// Open 建立连接，返回就绪的调试后端。
	Open(ctx context.Context, spec LaunchSpec) (Debugger, error)
}

// LaunchSpec 是建立调试会话所需的信息。
type LaunchSpec struct {
	// Language 是目标语言。
	Language Language
	// Mode 是 launch 或 attach。
	Mode string
	// Command 是目标程序原本的启动命令行（不含任何调试参数）。
	Command []string
	Cwd     string
	Env     []string
	// Port 是调试端口，attach 模式为必填。
	Port int
	// Timeout 是启动与连接的总超时。
	Timeout time.Duration
	// Proc 由 launch 模式填充，供会话回收时清理进程树。
	Proc *platform.Proc
}
