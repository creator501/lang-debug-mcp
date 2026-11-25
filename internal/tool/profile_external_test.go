package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/profile"
)

// TestProfileImportFoldedFile 验证 import 动作把折叠栈归一成三视图报告。
func TestProfileImportFoldedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.folded")
	content := strings.Join([]string{
		"main (app.py:10);run (app.py:4);parse (app.py:8) 30",
		"main (app.py:10);run (app.py:4);parse (app.py:8) 10",
		"main (app.py:10);io (app.py:20) 5",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入夹具失败: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"action": "import",
		"format": "folded",
		"path":   path,
		"focus":  "parse",
	})
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}
	res, err := handleProfile(context.Background(), raw)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("import 应成功，实际: %s", res.Content[0].Text)
	}
	var report profile.Report
	if err := json.Unmarshal([]byte(res.Content[0].Text), &report); err != nil {
		t.Fatalf("解析返回报告失败: %v\n%s", err, res.Content[0].Text)
	}
	if report.Source != "import" || report.Samples != 45 {
		t.Fatalf("来源与样本数不符: %+v", report)
	}
	if len(report.Hot) == 0 || report.Hot[0].Func != "parse" {
		t.Fatalf("自身视图应以 parse 为首: %+v", report.Hot)
	}
	if len(report.Cumulative) == 0 || report.Cumulative[0].Func != "main" {
		t.Fatalf("累计视图应以 main 为首: %+v", report.Cumulative)
	}
	if len(report.Callers) == 0 || report.Callers[0].Caller != "run" {
		t.Fatalf("callers 视图应给出 parse 的来源: %+v", report.Callers)
	}
	if !strings.Contains(strings.Join(report.Notes, "；"), "统计值") {
		t.Fatalf("必须标注统计性质: %v", report.Notes)
	}
}

// TestProfileImportV8File 验证 import 能解析 .cpuprofile 节点图。
func TestProfileImportV8File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.cpuprofile")
	data := `{"nodes":[
	  {"id":1,"callFrame":{"functionName":"(root)","url":"","lineNumber":-1},"children":[2]},
	  {"id":2,"callFrame":{"functionName":"work","url":"file:///app.js","lineNumber":5},"children":[]}
	],"samples":[2,2,2]}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("写入夹具失败: %v", err)
	}
	report, err := importProfile(profileArgs{Path: path, Format: "v8"})
	if err != nil {
		t.Fatalf("导入 V8 剖析失败: %v", err)
	}
	if report.Samples != 3 || report.Format != profile.FormatV8 {
		t.Fatalf("V8 导入结果不符: %+v", report)
	}
	if len(report.Hot) == 0 || report.Hot[0].Func != "work" || report.Hot[0].Self != 3 {
		t.Fatalf("自身视图应为 work(3): %+v", report.Hot)
	}
}

// TestProfileImportRejectsBadInput 验证导入失败时给出可执行的修复建议。
func TestProfileImportRejectsBadInput(t *testing.T) {
	if _, err := importProfile(profileArgs{}); err == nil {
		t.Fatal("缺少 path 应报错")
	} else if model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("错误码应为 INPUT_INVALID，实际 %s", model.CodeOf(err))
	}
	if _, err := importProfile(profileArgs{Path: filepath.Join(t.TempDir(), "缺失.folded")}); err == nil {
		t.Fatal("文件不存在应报错")
	}
	bad := filepath.Join(t.TempDir(), "bad.folded")
	if err := os.WriteFile(bad, []byte("没有样本数的一行\n"), 0o644); err != nil {
		t.Fatalf("写入夹具失败: %v", err)
	}
	if _, err := importProfile(profileArgs{Path: bad}); err == nil {
		t.Fatal("格式不合法应报错，而不是返回空报告")
	}
}

// TestProfileExternalGuards 验证外部剖析开始前的状态与参数校验。
func TestProfileExternalGuards(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
	session := openNodeScript(t, script)
	defer session.Close()

	_, err := startExternal(context.Background(), session, profileArgs{})
	if err == nil || model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("缺少 pid 时应报 INPUT_INVALID，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Fatalf("错误应说明需要 pid: %v", err)
	}

	// 入口停止态：规范要求不能在被调试目标停止时开始剖析。
	_, err = startExternal(context.Background(), session, profileArgs{Pid: 1})
	if err == nil || model.CodeOf(err) != model.CodeStateNotStopped {
		t.Fatalf("目标停止时应报 STATE_NOT_STOPPED，实际 %v", err)
	}

	if out := statusExternal(session); out["profiling"] != false {
		t.Fatalf("没有剖析进行时 status 应报告 false: %v", out)
	}
	if _, err := stopExternal(context.Background(), session, profileArgs{}); err == nil {
		t.Fatal("没有进行中的剖析时 stop 应报错")
	}
}
