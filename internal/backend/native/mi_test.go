package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

func TestParseMIRecordResult(t *testing.T) {
	rec, ok := parseMIRecord(`12^done,bkpt={number="1",line="14",addr="0x401000"}`)
	if !ok {
		t.Fatal("结果记录应解析成功")
	}
	if rec.token != 12 || rec.kind != '^' || rec.class != "done" {
		t.Fatalf("记录头不符: %+v", rec)
	}
	bkpt := rec.payload["bkpt"]
	if got := bkpt.get("number").str(); got != "1" {
		t.Fatalf("bkpt.number = %q", got)
	}
	if got := bkpt.get("addr").str(); got != "0x401000" {
		t.Fatalf("bkpt.addr = %q", got)
	}
}

func TestParseMIRecordStopEvent(t *testing.T) {
	rec, ok := parseMIRecord(`*stopped,reason="breakpoint-hit",bkptno="2",thread-id="1",` +
		`frame={addr="0x401000",func="main",file="app.c",fullname="E:\\repo\\app.c",line="14"}`)
	if !ok {
		t.Fatal("停止事件应解析成功")
	}
	if rec.kind != '*' || rec.class != "stopped" {
		t.Fatalf("记录头不符: %+v", rec)
	}
	if got := rec.payload["reason"].str(); got != "breakpoint-hit" {
		t.Fatalf("reason = %q", got)
	}
	frame := rec.payload["frame"]
	if got := frame.get("fullname").str(); got != `E:\repo\app.c` {
		t.Fatalf("fullname 转义应还原，实际 %q", got)
	}
	if got := frame.get("line").int(); got != 14 {
		t.Fatalf("line = %d", got)
	}
}

func TestParseMIRecordStreamAndList(t *testing.T) {
	stream, ok := parseMIRecord(`~"total=7\n"`)
	if !ok {
		t.Fatal("流记录应解析成功")
	}
	if stream.stream != "total=7\n" {
		t.Fatalf("流内容 = %q", stream.stream)
	}

	rec, ok := parseMIRecord(`^done,stack=[frame={level="0",func="main"},frame={level="1",func="start"}]`)
	if !ok {
		t.Fatal("列表记录应解析成功")
	}
	stack := rec.payload["stack"]
	if len(stack.list) != 2 {
		t.Fatalf("栈帧数 = %d, 期望 2", len(stack.list))
	}
	if got := stack.list[1].get("frame").get("func").str(); got != "start" {
		t.Fatalf("第二帧 func = %q", got)
	}
}

func TestParseHitCondition(t *testing.T) {
	if ignore, ok := parseHitCondition(">=3"); !ok || ignore != 2 {
		t.Fatalf(">=3 应转成 ignore 2，实际 %d/%v", ignore, ok)
	}
	for _, cond := range []string{"", "=3", ">=1", "abc"} {
		if _, ok := parseHitCondition(cond); ok {
			t.Fatalf("%q 不应被接受", cond)
		}
	}
}

// TestMIFallbackBackend 覆盖 lldb-dap 缺失时降级到 gdb MI 的完整路径。
func TestMIFallbackBackend(t *testing.T) {
	if _, err := exec.LookPath("gdb"); err != nil {
		t.Skip("未安装 gdb，跳过 MI 降级测试")
	}
	dir := t.TempDir()
	program := buildVariant(t, dir, "-g", "-O0")

	// 断掉 lldb-dap，强制走降级路径。
	t.Setenv("LANGDEBUG_LLDB_DAP", filepath.Join(t.TempDir(), "absent-lldb-dap.exe"))
	if _, err := Discover(); err == nil {
		t.Skip("强制覆盖后仍找到 lldb-dap，跳过降级测试")
	}

	dbg, err := (Launcher{}).Open(context.Background(), backend.LaunchSpec{
		Language: backend.C,
		Mode:     "launch",
		Command:  []string{program},
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立 MI 会话失败: %v", err)
	}
	defer dbg.Close()

	if got := dbg.Capability().Backend; got != gdbBackendName {
		t.Fatalf("后端名 = %s，期望 %s", got, gdbBackendName)
	}
	if dbg.Capability().Write {
		t.Fatal("降级后端不应声明写能力")
	}
	if dbg.CurrentStop() == nil {
		t.Fatalf("入口应停在 main；输出:\n%s", dbg.Output(30))
	}

	source := filepath.Join(dir, "app.c")
	line := breakLineOf(t, source)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: source, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	stop, err := dbg.Continue(context.Background(), 25*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v\n输出:\n%s", err, dbg.Output(40))
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q", bp.ID, stop.BreakpointID)
	}

	res, err := stop.Snapshot.Resolve([]string{"locals.total", "locals.items", "locals.items[0]"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "0" {
		t.Fatalf("total 初值应为 0，实际 %q（错误: %s）", got, res.Values["locals.total"].Error)
	}
	if got := res.Values["locals.items[0]"].Value; got != "3" {
		t.Fatalf("items[0] 应为 3，实际 %q（错误: %s）", got, res.Values["locals.items[0]"].Error)
	}

	results := dbg.Mutate([]backend.MutateOp{{Path: "locals.total", Value: "100"}})
	if len(results) != 1 || results[0].Code != model.CodeCapUnsupported {
		t.Fatalf("降级后端的改值应返回 %s，实际 %+v", model.CodeCapUnsupported, results)
	}
}

// breakLineOf 返回源文件中 BREAK_HERE 标记所在行。
func breakLineOf(t *testing.T, source string) int {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("读取源文件失败: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "BREAK_HERE") {
			return i + 1
		}
	}
	t.Fatalf("%s 中未找到断点标记", source)
	return 0
}
