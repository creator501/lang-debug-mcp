package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMapFailureIsBackendIndependent(t *testing.T) {
	// 同一个失败类别，无论由哪个后端上报，都必须落到同一个错误码。
	kinds := []FailureKind{
		FailAdapterMissing, FailToolMissing, FailPermission, FailNotStopped,
		FailNotRunning, FailUnsupported, FailTargetExited, FailTargetCrashed, FailProtocol,
	}
	for _, kind := range kinds {
		backends := []string{"lldb-dap", "debugpy", "cdp", "jdi"}
		var want Code
		for i, be := range backends {
			err := MapFailure(kind, fmt.Sprintf("%s 上报 %s", be, kind))
			got := CodeOf(err)
			if i == 0 {
				want = got
				continue
			}
			if got != want {
				t.Fatalf("失败类别 %s 在 %s 上映射为 %s，与首个后端 %s 不一致", kind, be, got, want)
			}
		}
		if want != CodeForFailure(kind) {
			t.Fatalf("失败类别 %s 的错误码不一致", kind)
		}
	}
}

func TestUnknownFailureFallsBackToInternal(t *testing.T) {
	err := MapFailure(FailureKind("从未定义"), "细节")
	if CodeOf(err) != CodeInternalProtocol {
		t.Fatalf("未知类别应归入 %s，实际 %s", CodeInternalProtocol, CodeOf(err))
	}
}

func TestErrorCarriesFix(t *testing.T) {
	err := New(CodeEnvAdapterMissing, "缺少 debugpy").
		WithFix("pip install debugpy").
		WithContext("language", "python")
	if !strings.Contains(err.Error(), string(CodeEnvAdapterMissing)) {
		t.Fatal("错误文本应包含错误码")
	}
	if !strings.Contains(err.Error(), "pip install debugpy") {
		t.Fatal("错误文本应包含修复建议")
	}
	if err.Context["language"] != "python" {
		t.Fatal("上下文丢失")
	}
}

func TestCodeOfWrappedAndForeign(t *testing.T) {
	inner := New(CodeTargetExited, "目标已退出")
	wrapped := fmt.Errorf("上层包装: %w", inner)
	if CodeOf(wrapped) != CodeTargetExited {
		t.Fatalf("包装后应保留错误码，实际 %s", CodeOf(wrapped))
	}
	if CodeOf(errors.New("普通错误")) != CodeInternalProtocol {
		t.Fatal("普通错误应归入 INTERNAL_PROTOCOL")
	}
	if CodeOf(nil) != "" {
		t.Fatal("nil 错误应返回空码")
	}
}
