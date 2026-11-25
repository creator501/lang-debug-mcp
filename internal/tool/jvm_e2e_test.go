package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/jvm"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

const javaFixture = "../../testdata/fixture/App.java"

var (
	javaCompileOnce sync.Once
	javaClassDir    string
	javaCompileErr  error
)

// requireJVM 编译 Java 夹具，返回 class 目录；缺少 JDK 时跳过。
func requireJVM(t *testing.T) string {
	t.Helper()
	javaCompileOnce.Do(func() {
		javac, err := exec.LookPath("javac")
		if err != nil {
			javaCompileErr = err
			return
		}
		src, absErr := filepath.Abs(javaFixture)
		if absErr != nil {
			javaCompileErr = absErr
			return
		}
		dir, tmpErr := os.MkdirTemp("", "ldmcp-java")
		if tmpErr != nil {
			javaCompileErr = tmpErr
			return
		}
		out, compileErr := exec.Command(javac, "-g", "-encoding", "UTF-8", "-d", dir, src).CombinedOutput()
		if compileErr != nil {
			javaCompileErr = fmt.Errorf("javac 编译失败: %v\n%s", compileErr, out)
			return
		}
		javaClassDir = dir
	})
	if javaCompileErr != nil {
		t.Skipf("跳过 JVM 端到端测试: %v", javaCompileErr)
	}
	t.Logf("Java class 目录: %s", javaClassDir)
	return javaClassDir
}

// javaBreakLine 返回夹具中标记行的行号。
func javaBreakLine(t *testing.T) int {
	t.Helper()
	return sourceMarkerLine(t, javaFixture, "BREAK_HERE")
}

// javaFixturePath 返回夹具绝对路径。
func javaFixturePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(javaFixture)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	return abs
}

// openJVMSession 建立 JVM 调试会话。
func openJVMSession(t *testing.T, classDir string) *Session {
	t.Helper()
	fixture := javaFixturePath(t)
	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), backend.Java, jvm.Launcher{}, backend.LaunchSpec{
		Language: backend.Java,
		Mode:     "launch",
		Command:  []string{"java", "-cp", classDir, "App"},
		Cwd:      filepath.Dir(fixture),
		Timeout:  40 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立 JVM 会话失败: %v", err)
	}
	return session
}

func TestJVMSessionEntryStop(t *testing.T) {
	classDir := requireJVM(t)
	session := openJVMSession(t, classDir)
	defer session.Close()

	if session.State() != model.StateStopped {
		t.Fatalf("入口应处于停止态，实际 %s", session.State())
	}
	if got := session.Info().Breakpoints; got != 0 {
		t.Fatalf("新会话不应有断点，实际 %d", got)
	}
}

func TestJVMBreakpointAndValues(t *testing.T) {
	classDir := requireJVM(t)
	session := openJVMSession(t, classDir)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	fixture := javaFixturePath(t)
	line := javaBreakLine(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	t.Logf("断点 %s verified=%v note=%q", bp.ID, bp.Verified, bp.Note)

	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q（reason=%s）", bp.ID, stop.BreakpointID, stop.Reason)
	}
	if stop.Snapshot == nil || len(stop.Snapshot.Frames) == 0 {
		t.Fatal("断点停止必须带回栈帧")
	}
	top := stop.Snapshot.Frames[0]
	if top.Name != "compute" {
		t.Fatalf("栈顶方法应为 compute，实际 %q", top.Name)
	}
	if top.Where.Line != line {
		t.Fatalf("停止行号 = %d，期望 %d", top.Where.Line, line)
	}
	if !strings.HasSuffix(top.Where.File, "App.java") {
		t.Fatalf("栈帧文件应能还原为 App.java，实际 %q", top.Where.File)
	}

	snap := session.ApplyStop(stop)
	budget := model.Budget{Depth: 6, MaxItems: 50}
	res, err := snap.Resolve([]string{
		"locals.total", "locals.base", "args.base",
		"locals.items", "locals.items[0].price", "locals.item.price",
		"statics.calls", "statics.LIMIT",
	}, budget)
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "0" {
		t.Fatalf("total 初值应为 0，实际 %q", got)
	}
	if got := res.Values["args.base"].Value; got != "2" {
		t.Fatalf("base 应为 2，实际 %q", got)
	}
	if got := res.Values["locals.items"].Total; got != 2 {
		t.Fatalf("items 长度应为 2，实际 %d", got)
	}
	if got := res.Values["locals.items[0].price"].Value; got != "10" {
		t.Fatalf("items[0].price 应为 10，实际 %q", got)
	}
	if got := res.Values["locals.item.price"].Value; got != "10" {
		t.Fatalf("增强 for 循环变量 item.price 应为 10，实际 %q", got)
	}
	if got := res.Values["statics.calls"].Value; got != "1" {
		t.Fatalf("statics.calls 应为 1，实际 %q", got)
	}
	if got := res.Values["statics.LIMIT"].Value; got != "3" {
		t.Fatalf("statics.LIMIT 应为 3，实际 %q", got)
	}
	if limits := strings.Join(dbg.Capability().Limits, "\n"); !strings.Contains(limits, "toString") {
		t.Fatalf("能力限制应说明不调用 toString，实际: %s", limits)
	}
}

func TestJVMMutationAndEvalLimit(t *testing.T) {
	classDir := requireJVM(t)
	session := openJVMSession(t, classDir)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	fixture := javaFixturePath(t)
	if _, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: javaBreakLine(t)}); err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	session.ApplyStop(stop)

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
	res, err := refreshed.Resolve([]string{"locals.total"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("新快照取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "100" {
		t.Fatalf("改值未生效，实际 %q", got)
	}

	// 静态字段走 ReferenceType.SetValues，同样要能写回并读到新值。
	static := dbg.Mutate([]backend.MutateOp{{Path: "statics.calls", Value: "7"}})
	if len(static) != 1 || !static[0].OK {
		t.Fatalf("静态字段改值应成功: %+v", static)
	}
	staticSnap, err := session.RefreshAfterMutation("test-static")
	if err != nil {
		t.Fatalf("静态改值后刷新快照失败: %v", err)
	}
	staticRes, err := staticSnap.Resolve([]string{"statics.calls"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("静态字段取值失败: %v", err)
	}
	if got := staticRes.Values["statics.calls"].Value; got != "7" {
		t.Fatalf("静态字段改值未生效，实际 %q", got)
	}

	// 类型不匹配必须失败且不改动目标。
	bad := dbg.Mutate([]backend.MutateOp{{Path: "locals.total", Value: "not-a-number"}})
	if len(bad) != 1 || bad[0].OK {
		t.Fatalf("类型不匹配应失败: %+v", bad)
	}
	if bad[0].Code != model.CodeInputInvalid {
		t.Fatalf("错误码应为 INPUT_INVALID，实际 %s", bad[0].Code)
	}

	// JDWP 没有表达式求值能力，必须明确报告能力缺失。
	if _, err := dbg.Eval(backend.EvalInput{Snapshot: refreshed, Expression: "total + 1"}); err == nil {
		t.Fatal("求值应返回能力缺失")
	} else if model.CodeOf(err) != model.CodeCapUnsupported {
		t.Fatalf("求值错误码应为 CAP_UNSUPPORTED，实际 %s", model.CodeOf(err))
	}

	// 条件断点与 logpoint 同样不支持，应给出稳定错误码与替代方案。
	if _, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: javaBreakLine(t), Condition: "total > 0"}); err == nil {
		t.Fatal("条件断点应返回能力缺失")
	} else if model.CodeOf(err) != model.CodeCapUnsupported {
		t.Fatalf("条件断点错误码应为 CAP_UNSUPPORTED，实际 %s", model.CodeOf(err))
	}
}
