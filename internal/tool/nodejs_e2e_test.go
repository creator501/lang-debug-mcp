package tool

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/nodejs"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

const fixtureScript = "../../testdata/fixture/app.js"

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("未安装 node，跳过 Node.js 后端端到端测试")
	}
}

func breakLine(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(fixtureScript)
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "BREAK_HERE") {
			return i + 1
		}
	}
	t.Fatal("夹具中未找到断点标记")
	return 0
}

func openNodeSession(t *testing.T) *Session {
	t.Helper()
	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), backend.NodeJS, nodejs.Launcher{}, backend.LaunchSpec{
		Language: backend.NodeJS,
		Mode:     "launch",
		Command:  []string{"node", fixtureScript},
		Timeout:  25 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立会话失败: %v", err)
	}
	return session
}

func TestNodeSessionEntryStop(t *testing.T) {
	requireNode(t)
	session := openNodeSession(t)
	defer session.Close()

	snap, err := session.Snapshot()
	if err != nil {
		t.Fatalf("入口应处于停止态并可取值: %v", err)
	}
	if len(snap.Frames) == 0 {
		t.Fatal("停止态应至少有一个栈帧")
	}
	if session.State() != model.StateStopped {
		t.Fatalf("会话状态应为 stopped，实际 %s", session.State())
	}
}

func TestNodeBreakpointAndInspect(t *testing.T) {
	requireNode(t)
	session := openNodeSession(t)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := breakLine(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixtureScript, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	if bp.ID == "" {
		t.Fatal("断点应有标识")
	}

	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 20*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q", bp.ID, stop.BreakpointID)
	}

	snap := session.ApplyStop(stop)
	if got := snap.Frames[0].Where.File; !strings.HasSuffix(got, "app.js") {
		t.Fatalf("停止位置文件名不符: %q", got)
	}
	if snap.Frames[0].Where.Line != line {
		t.Fatalf("停止行号 = %d, 期望 %d", snap.Frames[0].Where.Line, line)
	}

	res, err := snap.Resolve([]string{"locals.total", "locals.i", "locals.items"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if res.Values["locals.total"].Value != "0" {
		t.Fatalf("total 初值应为 0，实际 %q", res.Values["locals.total"].Value)
	}
	if res.Values["locals.items"].Total != 2 {
		t.Fatalf("items 长度应为 2，实际 %d", res.Values["locals.items"].Total)
	}
}

func TestNodeMutationInvalidatesSnapshot(t *testing.T) {
	requireNode(t)
	session := openNodeSession(t)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := breakLine(t)
	if _, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixtureScript, Line: line}); err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 20*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	snap := session.ApplyStop(stop)

	results := dbg.Mutate([]backend.MutateOp{{Path: "locals.total", Value: "100"}})
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("改值应成功: %+v", results)
	}
	if results[0].OldValue != "0" {
		t.Fatalf("应回传原值 0，实际 %q", results[0].OldValue)
	}

	refreshed, err := session.RefreshAfterMutation("test")
	if err != nil {
		t.Fatalf("刷新快照失败: %v", err)
	}
	if refreshed.ID == snap.ID {
		t.Fatal("改值后必须产生新快照")
	}
	if _, err := snap.Resolve([]string{"locals.total"}, model.DefaultBudget()); err == nil {
		t.Fatal("旧快照在改值后必须失效")
	} else if model.CodeOf(err) != model.CodeStateStaleSnapshot {
		t.Fatalf("旧快照应返回 STATE_STALE_SNAPSHOT，实际 %s", model.CodeOf(err))
	}

	res, err := refreshed.Resolve([]string{"locals.total"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("新快照取值失败: %v", err)
	}
	if res.Values["locals.total"].Value != "100" {
		t.Fatalf("改值未生效，实际 %q", res.Values["locals.total"].Value)
	}
}

func TestNodeEvalAndTypeStrictness(t *testing.T) {
	requireNode(t)
	session := openNodeSession(t)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := breakLine(t)
	if _, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixtureScript, Line: line}); err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 20*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	snap := session.ApplyStop(stop)

	out, err := dbg.Eval(backend.EvalInput{Snapshot: snap, Frame: 0, Expression: "items.length + total"})
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if out.Value != "2" {
		t.Fatalf("求值结果不符: %q", out.Value)
	}

	// 类型不匹配必须失败，不做隐式转换。
	bad := dbg.Mutate([]backend.MutateOp{{Path: "locals.total", Value: "不是数字"}})
	if len(bad) != 1 || bad[0].OK {
		t.Fatalf("类型不匹配应失败: %+v", bad)
	}
	if bad[0].Code != model.CodeInputInvalid {
		t.Fatalf("错误码应为 INPUT_INVALID，实际 %s", bad[0].Code)
	}
}

func TestNodeHitConditionSkipsMisses(t *testing.T) {
	requireNode(t)
	session := openNodeSession(t)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := breakLine(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{
		File:         fixtureScript,
		Line:         line,
		HitCondition: ">=2",
	})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}

	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 20*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s", bp.ID)
	}
	list := dbg.ListBreakpoints()
	if len(list) != 1 || list[0].Hits < 2 {
		t.Fatalf("命中次数应至少为 2: %+v", list)
	}
}
