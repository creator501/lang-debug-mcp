package model

import (
	"errors"
	"fmt"
)

// Code 是跨后端稳定的错误码。
//
// 同一类失败在不同语言后端上必须映射到同一错误码，使调用方可据码分支处理。
type Code string

// 错误码分组：ENV 环境、STATE 状态、CAP 能力、INPUT 入参、TARGET 目标、INTERNAL 协议。
const (
	CodeEnvAdapterMissing Code = "ENV_ADAPTER_MISSING"
	CodeEnvToolMissing    Code = "ENV_TOOL_MISSING"
	CodeEnvPermission     Code = "ENV_PERMISSION"
	CodeEnvDebugInfo      Code = "ENV_DEBUG_INFO"

	CodeStateInvalidTransition Code = "STATE_INVALID_TRANSITION"
	CodeStateNotStopped        Code = "STATE_NOT_STOPPED"
	CodeStateNotRunning        Code = "STATE_NOT_RUNNING"
	CodeStateStaleSnapshot     Code = "STATE_STALE_SNAPSHOT"
	CodeStateDeadSession       Code = "STATE_DEAD_SESSION"
	CodeStateUnknownSession    Code = "STATE_UNKNOWN_SESSION"

	CodeCapUnsupported Code = "CAP_UNSUPPORTED"

	CodeInputInvalid  Code = "INPUT_INVALID"
	CodeInputNotFound Code = "INPUT_NOT_FOUND"

	CodeTargetExited    Code = "TARGET_EXITED"
	CodeTargetCrashed   Code = "TARGET_CRASHED"
	CodeTargetOptimized Code = "TARGET_OPTIMIZED_OUT"
	CodeTargetReadOnly  Code = "TARGET_READ_ONLY"
	CodeTargetImmutable Code = "TARGET_IMMUTABLE"

	CodeInternalProtocol Code = "INTERNAL_PROTOCOL"
)

// Error 是统一错误结构：错误码、可读原因、可执行的下一步、上下文。
type Error struct {
	Code    Code              `json:"code"`
	Message string            `json:"message"`
	Fix     string            `json:"fix,omitempty"`
	Context map[string]string `json:"context,omitempty"`
	Wrapped error             `json:"-"`
}

// New 构造一个统一错误。
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithFix 附加可执行的下一步。
func (e *Error) WithFix(fix string) *Error {
	e.Fix = fix
	return e
}

// WithContext 附加一条上下文。
func (e *Error) WithContext(key, value string) *Error {
	if e.Context == nil {
		e.Context = make(map[string]string, 2)
	}
	e.Context[key] = value
	return e
}

// Wrap 附加底层错误。
func (e *Error) Wrap(err error) *Error {
	e.Wrapped = err
	return e
}

func (e *Error) Error() string {
	msg := string(e.Code) + ": " + e.Message
	if e.Fix != "" {
		msg += "；建议: " + e.Fix
	}
	return msg
}

// Unwrap 支持 errors.Is / errors.As。
func (e *Error) Unwrap() error { return e.Wrapped }

// CodeOf 提取错误码；非统一错误返回 INTERNAL_PROTOCOL。
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var coded interface{ Code() Code }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return CodeInternalProtocol
}

// FailureKind 是后端适配层上报的失败类别。
//
// 适配层只描述“发生了什么”，由本表统一映射为稳定错误码，避免各后端各自
// 发明错误码导致调用方无法跨语言分支。
type FailureKind string

// 后端失败类别。
const (
	FailAdapterMissing FailureKind = "adapter_missing"
	FailToolMissing    FailureKind = "tool_missing"
	FailPermission     FailureKind = "permission"
	FailNotStopped     FailureKind = "not_stopped"
	FailNotRunning     FailureKind = "not_running"
	FailUnsupported    FailureKind = "unsupported"
	FailTargetExited   FailureKind = "target_exited"
	FailTargetCrashed  FailureKind = "target_crashed"
	FailOptimizedOut   FailureKind = "optimized_out"
	FailReadOnly       FailureKind = "read_only"
	FailImmutable      FailureKind = "immutable"
	FailProtocol       FailureKind = "protocol"
)

var failureCodes = map[FailureKind]Code{
	FailAdapterMissing: CodeEnvAdapterMissing,
	FailToolMissing:    CodeEnvToolMissing,
	FailPermission:     CodeEnvPermission,
	FailNotStopped:     CodeStateNotStopped,
	FailNotRunning:     CodeStateNotRunning,
	FailUnsupported:    CodeCapUnsupported,
	FailTargetExited:   CodeTargetExited,
	FailTargetCrashed:  CodeTargetCrashed,
	FailOptimizedOut:   CodeTargetOptimized,
	FailReadOnly:       CodeTargetReadOnly,
	FailImmutable:      CodeTargetImmutable,
	FailProtocol:       CodeInternalProtocol,
}

// MapFailure 把后端失败类别转换为统一错误。
func MapFailure(kind FailureKind, detail string) *Error {
	code, ok := failureCodes[kind]
	if !ok {
		code = CodeInternalProtocol
	}
	return &Error{Code: code, Message: detail}
}

// CodeForFailure 返回失败类别对应的错误码，未知类别归入 INTERNAL_PROTOCOL。
func CodeForFailure(kind FailureKind) Code {
	if code, ok := failureCodes[kind]; ok {
		return code
	}
	return CodeInternalProtocol
}
