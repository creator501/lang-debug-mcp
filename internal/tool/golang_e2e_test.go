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
	"github.com/langdebug/lang-debug-mcp/internal/backend/cangjie"
	"github.com/langdebug/lang-debug-mcp/internal/backend/golang"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// requireGoToolchain 在缺少 go 或 dlv 时跳过：两者都是 Go 调试的前提，
// dlv 的 debug 模式还要靠 go 现场编译。
func requireGoToolchain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("未安装 go 工具链，跳过 Go 端到端测试")
	}
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skipf("未安装 dlv，跳过 Go 端到端测试: %v", err)
	}
}

// goFixtureModule 把夹具复制到临时模块目录：仓库根有自己的 go.mod，
// 直接在 testdata 里 go run 会把夹具当成模块内包，测不出独立工程的形态。
func goFixtureModule(t *testing.T) (dir, file string, line int) {
	t.Helper()
	source := filepath.Join("..", "..", "testdata", "fixture", "main.go")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("读取 Go 夹具失败: %v", err)
	}
	dir = t.TempDir()
	file = filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatalf("写入 Go 夹具失败: %v", err)
	}
	gomod := "module fixture\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("写入 go.mod 失败: %v", err)
	}
	return dir, file, sourceMarkerLine(t, source, "BREAK_HERE")
}

// openGoSession 建立 Go 调试会话（dlv dap，入口停止）。
func openGoSession(t *testing.T, dir string) *Session {
	t.Helper()
	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), backend.Go, golang.Launcher{}, backend.LaunchSpec{
		Language: backend.Go,
		Mode:     "launch",
		Command:  []string{"go", "run", "."},
		Cwd:      dir,
		Timeout:  90 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立 Go 会话失败: %v", err)
	}
	return session
}

// TestGoSessionEntryStop 验证入口停止与栈帧可用。
func TestGoSessionEntryStop(t *testing.T) {
	requireGoToolchain(t)
	dir, _, _ := goFixtureModule(t)
	session := openGoSession(t, dir)
	defer session.Close()

	snap, err := session.Snapshot()
	if err != nil {
		t.Fatalf("入口应处于停止态并可取值: %v", err)
	}
	if len(snap.Frames) == 0 {
		t.Fatal("停止态应至少有一个栈帧")
	}
	// dlv 自己的 entry 停止落在运行时初始化之前（占位线程，无栈无变量），
	// 因此入口停止被推进到 main.main：栈顶必须是用户代码而不是空栈。
	if name := snap.Frames[0].Name; name != "main.main" {
		t.Fatalf("入口栈顶应为 main.main，实际 %q", name)
	}
	if snap.Reason != "entry" {
		t.Fatalf("入口停止原因应为 entry，实际 %q", snap.Reason)
	}
	if session.State() != model.StateStopped {
		t.Fatalf("会话状态应为 stopped，实际 %s", session.State())
	}
}

// TestGoBreakpointAndValues 验证断点命中、行号与批量取值。
func TestGoBreakpointAndValues(t *testing.T) {
	requireGoToolchain(t)
	dir, file, line := goFixtureModule(t)
	session := openGoSession(t, dir)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: file, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 90*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v\n输出:\n%s", err, dbg.Output(30))
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q", bp.ID, stop.BreakpointID)
	}
	snap := session.ApplyStop(stop)
	top := snap.Frames[0]
	if top.Where.Line != line {
		t.Fatalf("停止行号 = %d, 期望 %d", top.Where.Line, line)
	}
	if filepath.Base(top.Where.File) != "main.go" {
		t.Fatalf("停止文件名不符: %q", top.Where.File)
	}

	res, err := snap.Resolve([]string{"locals.total", "locals.items"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "0" {
		t.Fatalf("首次命中时 total 应为 0，实际 %q", got)
	}
	items := res.Values["locals.items"]
	if items.Value == "" && items.Total == 0 {
		t.Fatalf("items 应能读到（值或元素数）: %+v", items)
	}
}

// TestGoProberReportsDLV 验证探测结果如实报告后端与版本。
func TestGoProberReportsDLV(t *testing.T) {
	status := golang.Prober{}.Probe()
	if !status.Available {
		t.Skipf("dlv 不可用，跳过探测断言: %s", status.InstallHint)
	}
	if status.Backend != "dlv-dap" {
		t.Fatalf("后端名应为 dlv-dap，实际 %q", status.Backend)
	}
	if status.Version == "" {
		t.Fatal("应报告 dlv 版本，而不是留空")
	}
	if status.Language != backend.Go {
		t.Fatalf("语言应为 go，实际 %q", status.Language)
	}
}

// TestCangjieProberRequiresCJC 验证缺少编译器时如实报告不可用与缺失项。
func TestCangjieProberRequiresCJC(t *testing.T) {
	status := cangjie.Prober{}.Probe()
	if status.Language != backend.Cangjie {
		t.Fatalf("语言应为 cangjie，实际 %q", status.Language)
	}
	if _, err := exec.LookPath("cjc"); err == nil {
		if !status.Available && len(status.Missing) == 2 {
			t.Fatalf("cjc 已安装但探测仍报全部缺失: %+v", status)
		}
		return
	}
	if status.Available {
		t.Fatal("缺少 cjc 时不应报告可用")
	}
	found := false
	for _, missing := range status.Missing {
		if missing == "cjc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Missing 应包含 cjc: %+v", status.Missing)
	}
	if status.InstallHint == "" {
		t.Fatal("应给出安装指引")
	}
}

// TestCangjieCapabilityAddsLimits 验证仓颉能力清单追加了语言侧边界说明。
func TestCangjieCapabilityAddsLimits(t *testing.T) {
	limits := cangjie.Limits()
	if len(limits) == 0 {
		t.Fatal("仓颉应至少给出一条语言边界说明")
	}
	joined := strings.Join(limits, "；")
	for _, want := range []string{"cjc -g", "lldb-dap", "Option"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("边界说明应提到 %q: %v", want, limits)
		}
	}
}
