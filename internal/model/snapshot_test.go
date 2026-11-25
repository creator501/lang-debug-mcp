package model

import (
	"errors"
	"testing"
)

func testSnapshot() *Snapshot {
	locals := &Value{Type: "dict", Kind: KindObject, Fields: map[string]*Value{
		"x": {Type: "int", Kind: KindScalar, Display: "1"},
	}}
	frames := []Frame{{
		Index: 0,
		Name:  "handle",
		Where: Position{File: "svc/handler.py", Line: 42, Func: "handle"},
		Scopes: map[string]*Value{
			ScopeLocals: locals,
		},
	}}
	return NewSnapshot("s1", 7, "breakpoint", "main#1", frames)
}

func TestSnapshotRefLifecycle(t *testing.T) {
	snap := testSnapshot()
	node := &Value{Type: "list", Kind: KindList, Total: 2}
	ref, err := snap.AddRef(node)
	if err != nil {
		t.Fatalf("登记引用失败: %v", err)
	}
	got, err := snap.Ref(ref)
	if err != nil {
		t.Fatalf("取回引用失败: %v", err)
	}
	if got != node {
		t.Fatal("引用指回的节点不一致")
	}

	snap.Invalidate(StateRunning)
	if snap.IsValid() {
		t.Fatal("作废后不应仍然有效")
	}
	_, err = snap.Ref(ref)
	if err == nil {
		t.Fatal("失效快照的引用应不可用")
	}
	var stale *StaleSnapshotError
	if !errors.As(err, &stale) {
		t.Fatalf("错误类型不符: %T", err)
	}
	if CodeOf(err) != CodeStateStaleSnapshot {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
	if stale.State != StateRunning {
		t.Fatalf("失效原因应记录当前状态，实际 %s", stale.State)
	}
}

func TestSnapshotUnknownRef(t *testing.T) {
	snap := testSnapshot()
	_, err := snap.Ref("r999")
	if err == nil {
		t.Fatal("未知引用应报错")
	}
	if CodeOf(err) != CodeInputNotFound {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
}

func TestSnapshotFrameAndScope(t *testing.T) {
	snap := testSnapshot()
	if _, err := snap.Frame(0); err != nil {
		t.Fatalf("存在的栈帧应可取: %v", err)
	}
	_, err := snap.Frame(3)
	if err == nil {
		t.Fatal("越界栈帧应报错")
	}
	if CodeOf(err) != CodeInputNotFound {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
	if _, err := snap.Scope(0, ScopeLocals); err != nil {
		t.Fatalf("存在的作用域应可取: %v", err)
	}
	if _, err := snap.Scope(0, ScopeArgs); err == nil {
		t.Fatal("不存在的作用域应报错")
	}
}
