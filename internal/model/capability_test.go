package model

import (
	"strings"
	"testing"
)

func TestCapabilityEnsure(t *testing.T) {
	cap := Capability{
		Backend: "lldb-dap",
		Read:    true,
		Write:   false,
		Limits:  []string{"不支持修改只读内存段"},
	}
	if err := cap.Ensure(ActionRead); err != nil {
		t.Fatalf("支持的动作不应报错: %v", err)
	}
	err := cap.Ensure(ActionWrite)
	if err == nil {
		t.Fatal("不支持的动作应报错")
	}
	if CodeOf(err) != CodeCapUnsupported {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
	msg := err.Error()
	if !strings.Contains(msg, ActionWrite.Label()) {
		t.Fatalf("错误文本应指明动作: %s", msg)
	}
	if !strings.Contains(msg, "不支持修改只读内存段") {
		t.Fatalf("修复建议应包含能力限制: %s", msg)
	}
	if !strings.Contains(msg, "lldb-dap") {
		t.Fatalf("错误应带后端上下文: %s", msg)
	}
}

func TestCapabilityActionLists(t *testing.T) {
	cap := Capability{Read: true, Eval: true, Profile: true}
	supported := cap.SupportedActions()
	if len(supported) != 3 {
		t.Fatalf("支持动作数量不符: %v", supported)
	}
	unsupported := cap.UnsupportedActions()
	if len(unsupported) != 3 {
		t.Fatalf("不支持动作数量不符: %v", unsupported)
	}
	for _, a := range supported {
		if !cap.Supports(a) {
			t.Fatalf("动作 %s 判定不一致", a)
		}
	}
	// 六个动作必须被完整划分，不允许遗漏。
	if len(supported)+len(unsupported) != 6 {
		t.Fatalf("动作划分不完整: %v + %v", supported, unsupported)
	}
}
