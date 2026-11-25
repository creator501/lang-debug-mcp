package model

import (
	"strings"
	"testing"
)

type expandCounters struct {
	args  int
	items int
	this  int
}

// fixtureSnapshot 构造一棵带惰性节点的假值树，用于验证取值语义。
func fixtureSnapshot() (*Snapshot, *expandCounters) {
	c := &expandCounters{}

	itemsReal := &Value{Type: "list", Kind: KindList, Total: 3, Items: []*Value{
		{Type: "int", Kind: KindScalar, Display: "10"},
		{Type: "int", Kind: KindScalar, Display: "20"},
		{Type: "int", Kind: KindScalar, Display: "30"},
	}}
	itemsLazy := &Value{Type: "list", Kind: KindList, Total: 3}
	itemsLazy.Expand = func() (*Value, error) {
		c.items++
		return itemsReal, nil
	}

	argsReal := &Value{Type: "dict", Kind: KindObject, Fields: map[string]*Value{
		"name":    {Type: "str", Kind: KindScalar, Display: "bob"},
		"retries": {Type: "int", Kind: KindScalar, Display: "3"},
		"items":   itemsLazy,
		"ghost":   {Type: "int", Optimized: true},
		"big":     {Type: "str", Kind: KindScalar, Display: strings.Repeat("数", 300)},
	}}
	argsLazy := &Value{Type: "dict", Kind: KindObject}
	argsLazy.Expand = func() (*Value, error) {
		c.args++
		return argsReal, nil
	}

	thisReal := &Value{Type: "Handler", Kind: KindObject, Fields: map[string]*Value{
		"name": {Type: "str", Kind: KindScalar, Display: "handler"},
	}}
	thisLazy := &Value{Type: "Handler", Kind: KindObject}
	thisLazy.Expand = func() (*Value, error) {
		c.this++
		return thisReal, nil
	}

	frames := []Frame{{
		Index: 0,
		Name:  "handle",
		Where: Position{File: "svc/handler.py", Line: 42},
		Scopes: map[string]*Value{
			ScopeArgs: argsLazy,
			ScopeThis: thisLazy,
		},
	}}
	return NewSnapshot("s1", 7, "breakpoint", "main#1", frames), c
}

func TestResolveBatchSharesTraversal(t *testing.T) {
	snap, c := fixtureSnapshot()
	res, err := snap.Resolve([]string{"args.name", "args.retries", "args.items[0]", "args.items[1]"}, DefaultBudget())
	if err != nil {
		t.Fatalf("批量取值失败: %v", err)
	}
	checks := map[string]string{
		"args.name":     "bob",
		"args.retries":  "3",
		"args.items[0]": "10",
		"args.items[1]": "20",
	}
	for path, want := range checks {
		entry, ok := res.Values[path]
		if !ok {
			t.Fatalf("缺少路径 %s 的结果", path)
		}
		if entry.Value != want {
			t.Fatalf("路径 %s = %q, 期望 %q", path, entry.Value, want)
		}
	}
	if c.args != 1 {
		t.Fatalf("共享父节点应只展开一次，实际展开 %d 次", c.args)
	}
	if c.items != 1 {
		t.Fatalf("共享集合应只展开一次，实际展开 %d 次", c.items)
	}
	if res.Expands != 2 {
		t.Fatalf("后端展开次数应为 2，实际 %d", res.Expands)
	}
}

func TestResolveMissingPathDoesNotFailBatch(t *testing.T) {
	snap, _ := fixtureSnapshot()
	res, err := snap.Resolve([]string{"args.name", "args.nope"}, DefaultBudget())
	if err != nil {
		t.Fatalf("部分路径失败不应导致整体失败: %v", err)
	}
	if res.Values["args.name"].Value != "bob" {
		t.Fatal("存在的路径应正常返回")
	}
	missing, ok := res.Values["args.nope"]
	if !ok {
		t.Fatal("不存在的路径也应返回条目")
	}
	if missing.Error == "" {
		t.Fatal("不存在的路径应带有错误说明")
	}
	if missing.Optimized {
		t.Fatal("路径不存在不应被标记为已优化掉")
	}
}

func TestResolveOptimizedOutDistinguished(t *testing.T) {
	snap, _ := fixtureSnapshot()
	res, err := snap.Resolve([]string{"args.ghost", "args.nope"}, DefaultBudget())
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	ghost := res.Values["args.ghost"]
	if !ghost.Optimized {
		t.Fatal("已优化掉的变量应被标记")
	}
	if !strings.Contains(ghost.Error, "优化") {
		t.Fatalf("说明应提示优化级别: %s", ghost.Error)
	}
	if res.Values["args.nope"].Optimized {
		t.Fatal("路径不存在不能被标记为已优化掉")
	}
}

func TestResolveDefaultDepthReturnsRef(t *testing.T) {
	snap, _ := fixtureSnapshot()
	res, err := snap.Resolve([]string{"args.items"}, DefaultBudget())
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	entry := res.Values["args.items"]
	if entry.Ref == "" {
		t.Fatal("默认预算下复合值应返回可展开引用")
	}
	if entry.Total != 3 {
		t.Fatalf("应报告总量，实际 %d", entry.Total)
	}
	node, err := snap.Ref(entry.Ref)
	if err != nil {
		t.Fatalf("引用不可用: %v", err)
	}
	if node.Len() != 3 {
		t.Fatalf("引用节点长度不符: %d", node.Len())
	}
}

func TestResolveTruncationIsExplicit(t *testing.T) {
	snap, _ := fixtureSnapshot()
	budget := Budget{Depth: 2, MaxItems: 2, MaxStr: 200}
	res, err := snap.Resolve([]string{"args.*"}, budget)
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	// args 有 5 个成员，预算只允许 2 个。
	if len(res.Values) != 2 {
		t.Fatalf("应只返回 2 个成员，实际 %d: %v", len(res.Values), res.Values)
	}
	if len(res.Clips) == 0 {
		t.Fatal("发生裁剪时必须显式标注")
	}
	if !strings.Contains(strings.Join(res.Clips, "|"), "已返回 2/5 项") {
		t.Fatalf("裁剪说明应包含已返回与总量: %v", res.Clips)
	}
}

func TestResolveStringClip(t *testing.T) {
	snap, _ := fixtureSnapshot()
	res, err := snap.Resolve([]string{"args.big"}, Budget{Depth: 1, MaxItems: 20, MaxStr: 10})
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	entry := res.Values["args.big"]
	if len([]rune(entry.Value)) != 10 {
		t.Fatalf("字符串应按预算截断，实际长度 %d", len([]rune(entry.Value)))
	}
	if len(res.Clips) == 0 || !strings.Contains(res.Clips[0], "截断") {
		t.Fatalf("字符串截断应显式标注: %v", res.Clips)
	}
}

func TestResolveRangeClamped(t *testing.T) {
	snap, _ := fixtureSnapshot()
	res, err := snap.Resolve([]string{"args.items[1..9]"}, Budget{Depth: 2, MaxItems: 20, MaxStr: 200})
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if res.Values["args.items[1]"].Value != "20" || res.Values["args.items[2]"].Value != "30" {
		t.Fatalf("区间取值不符: %v", res.Values)
	}
	if _, exists := res.Values["args.items[3]"]; exists {
		t.Fatal("超出总长的索引不应产出条目")
	}
	if len(res.Clips) == 0 {
		t.Fatal("区间越界应显式标注")
	}
}

func TestResolveMetadataSurvivesClipping(t *testing.T) {
	snap, _ := fixtureSnapshot()
	// 极小预算下，变量值可以被裁掉，但总量与裁剪说明必须保留。
	res, err := snap.Resolve([]string{"args.*"}, Budget{Depth: 2, MaxItems: 1, MaxStr: 1})
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if len(res.Clips) == 0 {
		t.Fatal("裁剪说明不应被预算裁掉")
	}
	if !strings.Contains(strings.Join(res.Clips, "|"), "/5 项") {
		t.Fatalf("裁剪说明必须包含总量信息: %v", res.Clips)
	}
	if res.Nodes == 0 {
		t.Fatal("条目计数属于元信息，不应为 0")
	}
}

func TestResolveStaleSnapshotRejected(t *testing.T) {
	snap, _ := fixtureSnapshot()
	snap.Invalidate(StateRunning)
	_, err := snap.Resolve([]string{"args.name"}, DefaultBudget())
	if err == nil {
		t.Fatal("失效快照取值应报错")
	}
	if CodeOf(err) != CodeStateStaleSnapshot {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
}

func TestResolveEmptyPathsRejected(t *testing.T) {
	snap, _ := fixtureSnapshot()
	if _, err := snap.Resolve(nil, DefaultBudget()); err == nil {
		t.Fatal("空路径列表应报错")
	} else if CodeOf(err) != CodeInputInvalid {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
}

func TestResolveAllInvalidPathsRejected(t *testing.T) {
	snap, _ := fixtureSnapshot()
	_, err := snap.Resolve([]string{"a[1", "b[2..1]"}, DefaultBudget())
	if err == nil {
		t.Fatal("全部路径非法时应整体失败")
	}
	if CodeOf(err) != CodeInputInvalid {
		t.Fatalf("错误码不符: %s", CodeOf(err))
	}
}
