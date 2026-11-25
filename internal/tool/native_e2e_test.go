package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/native"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// nativeFixture 描述一种 native 语言的夹具与编译方式。
type nativeFixture struct {
	lang    backend.Language
	source  string
	compile []string // 编译器与其参数；末尾追加 -o <输出> <源文件>
	// structured 表示该夹具的 items 能被调试器识别为定长数组。
	structured bool
}

func nativeFixtures() []nativeFixture {
	return []nativeFixture{
		{lang: backend.C, source: "../../testdata/fixture/app.c", compile: []string{"gcc", "-g", "-O0"}, structured: true},
		{lang: backend.Cpp, source: "../../testdata/fixture/app.cpp", compile: []string{"g++", "-g", "-O0"}, structured: true},
		{lang: backend.Rust, source: "../../testdata/fixture/app.rs", compile: []string{"rustc", "-g"}, structured: true},
	}
}

// requireAdapter 在缺少 lldb-dap 时跳过，避免把环境缺失当成后端缺陷。
func requireAdapter(t *testing.T) {
	t.Helper()
	if _, err := native.Discover(); err != nil {
		t.Skipf("未找到 lldb-dap，跳过 native 后端端到端测试: %v", err)
	}
}

// build 编译夹具，返回可执行文件的绝对路径；编译器缺失时跳过。
func (f nativeFixture) build(t *testing.T) string {
	t.Helper()
	compiler := f.compile[0]
	if _, err := exec.LookPath(compiler); err != nil {
		t.Skipf("未安装 %s，跳过 %s 夹具", compiler, f.lang)
	}
	out := filepath.Join(t.TempDir(), "app.exe")
	source, err := filepath.Abs(f.source)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	args := append(append([]string{}, f.compile[1:]...), "-o", out, source)
	if combined, err := exec.Command(compiler, args...).CombinedOutput(); err != nil {
		t.Fatalf("编译 %s 夹具失败: %v\n%s", f.lang, err, combined)
	}
	return out
}

// breakLine 返回夹具中行尾标记所在的行号。
func (f nativeFixture) breakLine(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.source)
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "BREAK_HERE") {
			return i + 1
		}
	}
	t.Fatalf("%s 夹具中未找到断点标记", f.lang)
	return 0
}

// sourcePath 返回夹具的绝对路径（断点按绝对路径匹配）。
func (f nativeFixture) sourcePath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(f.source)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	return path
}

// open 建立 native 会话（入口停止）。
func (f nativeFixture) open(t *testing.T) *Session {
	t.Helper()
	requireAdapter(t)
	program := f.build(t)
	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), f.lang, native.Launcher{}, backend.LaunchSpec{
		Language: f.lang,
		Mode:     "launch",
		Command:  []string{program},
		Cwd:      filepath.Dir(program),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立 %s 会话失败: %v", f.lang, err)
	}
	return session
}

// stopAtBreakpoint 设置断点并跑到命中，返回停止快照。
func stopAtBreakpoint(t *testing.T, session *Session, f nativeFixture) (*model.Snapshot, backend.Breakpoint) {
	t.Helper()
	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: f.sourcePath(t), Line: f.breakLine(t)})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 25*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v\n输出:\n%s", err, dbg.Output(50))
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q", bp.ID, stop.BreakpointID)
	}
	return session.ApplyStop(stop), bp
}

func TestNativeSessionEntryStop(t *testing.T) {
	session := nativeFixtures()[0].open(t)
	defer session.Close()

	if session.State() != model.StateStopped {
		t.Fatalf("入口应处于停止态，实际 %s", session.State())
	}
	snap, err := session.Snapshot()
	if err != nil {
		t.Fatalf("入口应可取值: %v", err)
	}
	if len(snap.Frames) == 0 {
		t.Fatal("停止态应至少有一个栈帧")
	}
}

func TestNativeBreakpointStopsOnFixtureLine(t *testing.T) {
	for _, f := range nativeFixtures() {
		t.Run(string(f.lang), func(t *testing.T) {
			session := f.open(t)
			defer session.Close()

			snap, _ := stopAtBreakpoint(t, session, f)
			if snap.Frames[0].Where.Line != f.breakLine(t) {
				t.Fatalf("停止行号 = %d, 期望 %d", snap.Frames[0].Where.Line, f.breakLine(t))
			}
			res, err := snap.Resolve([]string{"locals.total"}, model.DefaultBudget())
			if err != nil {
				t.Fatalf("取值失败: %v", err)
			}
			if got := res.Values["locals.total"].Value; got != "0" {
				t.Fatalf("total 初值应为 0，实际 %q", got)
			}
		})
	}
}

func TestNativeReadsFixedArray(t *testing.T) {
	for _, f := range nativeFixtures() {
		if !f.structured {
			continue
		}
		t.Run(string(f.lang), func(t *testing.T) {
			session := f.open(t)
			defer session.Close()

			snap, _ := stopAtBreakpoint(t, session, f)
			res, err := snap.Resolve([]string{"locals.items", "locals.items[0]", "locals.items[1]"}, model.DefaultBudget())
			if err != nil {
				t.Fatalf("取值失败: %v", err)
			}
			if got := res.Values["locals.items"].Total; got != 2 {
				t.Fatalf("items 长度应为 2，实际 %d", got)
			}
			if got := res.Values["locals.items[0]"].Value; got != "3" {
				t.Fatalf("items[0] 应为 3，实际 %q（错误: %s）", got, res.Values["locals.items[0]"].Error)
			}
			if got := res.Values["locals.items[1]"].Value; got != "4" {
				t.Fatalf("items[1] 应为 4，实际 %q（错误: %s）", got, res.Values["locals.items[1]"].Error)
			}
		})
	}
}

func TestNativeMutationReportsOldValue(t *testing.T) {
	f := nativeFixtures()[0]
	session := f.open(t)
	defer session.Close()

	dbg, err := session.Debugger(context.Background())
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	stopAtBreakpoint(t, session, f)

	results := dbg.Mutate([]backend.MutateOp{{Path: "locals.total", Value: "100"}})
	if len(results) != 1 {
		t.Fatalf("应有 1 条改值结果: %+v", results)
	}
	if !results[0].OK {
		t.Skipf("该 lldb-dap 版本无法写入局部变量: %s", results[0].Reason)
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
}
