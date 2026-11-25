package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/python"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

const pythonFixture = "../../testdata/fixture/app.py"

// requireDebugpy 在缺少 debugpy 时跳过 Python 端到端测试。
//
// 除了 PATH 与环境变量，还会检查仓库根目录的虚拟环境：go test 的工作目录是包
// 目录，Discover 看不到仓库根的 .venv。
func requireDebugpy(t *testing.T) python.Interpreter {
	t.Helper()
	interp, err := python.Discover()
	if err == nil {
		t.Logf("使用解释器 %s（Python %s，debugpy %s）", interp.Path, interp.Version, interp.Debugpy)
		return interp
	}
	root, absErr := filepath.Abs(filepath.Join("..", ".."))
	if absErr == nil {
		if venv, ok := python.DiscoverVenv(root); ok {
			return venv
		}
	}
	t.Skipf("未安装 debugpy，跳过 Python 端到端测试: %v", err)
	return python.Interpreter{}
}

func pythonFixturePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(pythonFixture)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	return abs
}

func pythonBreakLine(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(pythonFixture)
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

func openPythonSession(t *testing.T, interp python.Interpreter) *Session {
	t.Helper()
	fixture := pythonFixturePath(t)
	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), backend.Python, python.Launcher{}, backend.LaunchSpec{
		Language: backend.Python,
		Mode:     "launch",
		Command:  []string{interp.Path, fixture},
		Cwd:      filepath.Dir(fixture),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立会话失败: %v", err)
	}
	return session
}

func TestPythonSessionEntryStop(t *testing.T) {
	interp := requireDebugpy(t)
	session := openPythonSession(t, interp)
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
	if snap.Reason != "entry" {
		t.Fatalf("入口停止原因应为 entry，实际 %q", snap.Reason)
	}
}

func TestPythonBreakpointAndInspect(t *testing.T) {
	interp := requireDebugpy(t)
	session := openPythonSession(t, interp)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	fixture := pythonFixturePath(t)
	line := pythonBreakLine(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	if !bp.Verified {
		t.Fatalf("源码已加载的断点应验证通过: %+v", bp)
	}

	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 25*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q", bp.ID, stop.BreakpointID)
	}
	if stop.Snapshot == nil || len(stop.Snapshot.Frames) == 0 {
		t.Fatal("断点停止必须带回栈帧")
	}

	snap := session.ApplyStop(stop)
	top := snap.Frames[0]
	if top.Where.Line != line {
		t.Fatalf("停止行号 = %d, 期望 %d", top.Where.Line, line)
	}
	if !strings.HasSuffix(top.Where.File, "app.py") {
		t.Fatalf("停止文件名不符: %q", top.Where.File)
	}
	if top.Name != "compute" {
		t.Fatalf("栈顶函数应为 compute，实际 %q", top.Name)
	}

	res, err := snap.Resolve([]string{"locals.total", "locals.items", "locals.item.price"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "0" {
		t.Fatalf("total 初值应为 0，实际 %q", got)
	}
	if got := res.Values["locals.items"].Total; got != 2 {
		t.Fatalf("items 长度应为 2，实际 %d", got)
	}
	if got := res.Values["locals.item.price"].Value; got != "10" {
		t.Fatalf("item.price 应为 10，实际 %q", got)
	}
}

// TestPythonConditionalBreakpoint 验证条件表达式被原样交给调试器求值。
func TestPythonConditionalBreakpoint(t *testing.T) {
	interp := requireDebugpy(t)
	session := openPythonSession(t, interp)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	fixture := pythonFixturePath(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{
		File:      fixture,
		Line:      pythonBreakLine(t),
		Condition: "total == 20",
	})
	if err != nil {
		t.Fatalf("设置条件断点失败: %v", err)
	}

	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 25*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中条件断点 %s，实际 %q", bp.ID, stop.BreakpointID)
	}
	snap := session.ApplyStop(stop)
	res, err := snap.Resolve([]string{"locals.total"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "20" {
		t.Fatalf("条件断点应停在第二轮的 total=20，实际 %q", got)
	}
}

func TestPythonMutationAndEval(t *testing.T) {
	interp := requireDebugpy(t)
	session := openPythonSession(t, interp)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	fixture := pythonFixturePath(t)
	if _, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: pythonBreakLine(t)}); err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 25*time.Second)
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
	if got := res.Values["locals.total"].Value; got != "100" {
		t.Fatalf("改值未生效，实际 %q", got)
	}

	out, err := dbg.Eval(backend.EvalInput{Snapshot: refreshed, Frame: 0, Expression: "total + 1"})
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if out.Value != "101" {
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

func TestPythonLogpointDoesNotStop(t *testing.T) {
	interp := requireDebugpy(t)
	session := openPythonSession(t, interp)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	fixture := pythonFixturePath(t)
	if _, err := dbg.AddBreakpoint(backend.BreakpointSpec{
		File:       fixture,
		Line:       pythonBreakLine(t),
		LogMessage: "logpoint total={total}",
	}); err != nil {
		t.Fatalf("设置 logpoint 失败: %v", err)
	}

	session.MarkRunning()
	if _, err := dbg.Continue(ctx, 25*time.Second); err == nil {
		t.Fatal("目标运行结束后应返回错误，而不是把 logpoint 当作停止")
	} else if model.CodeOf(err) != model.CodeTargetExited {
		t.Fatalf("应返回 TARGET_EXITED，实际 %s (%v)", model.CodeOf(err), err)
	}

	out := dbg.Output(100)
	if !strings.Contains(out, "logpoint total=") {
		t.Fatalf("logpoint 应产生输出，实际:\n%s", out)
	}
	if !strings.Contains(out, "total=50") {
		t.Fatalf("目标自身输出应被采集，实际:\n%s", out)
	}
}
