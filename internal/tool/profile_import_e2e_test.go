package tool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestProfileImportNodeCPUProfile 验证文档里给出的 Node 采集命令确实可用：
// `node --cpu-prof --cpu-prof-dir <目录> script.js` 产出的 .cpuprofile
// 能被 import(format=v8) 归一成热点报告。
func TestProfileImportNodeCPUProfile(t *testing.T) {
	requireNode(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "spin.js")
	source := strings.Join([]string{
		"function spin() {",
		"  const end = Date.now() + 300;",
		"  let acc = 0;",
		"  while (Date.now() < end) {",
		"    acc += Math.sqrt(acc + 1);",
		"  }",
		"  return acc;",
		"}",
		"console.log('acc=' + spin());",
	}, "\n")
	if err := os.WriteFile(script, []byte(source), 0o644); err != nil {
		t.Fatalf("写入夹具失败: %v", err)
	}

	cmd := exec.Command("node", "--cpu-prof", "--cpu-prof-dir", dir, script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --cpu-prof 执行失败: %v\n%s", err, out)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.cpuprofile"))
	if err != nil || len(files) == 0 {
		t.Fatalf("未生成 .cpuprofile（检查 --cpu-prof-dir 是否可写）: %v", err)
	}

	report, err := importProfile(profileArgs{Path: files[0], Format: "v8"})
	if err != nil {
		t.Fatalf("导入 .cpuprofile 失败: %v", err)
	}
	if report.Samples == 0 {
		t.Fatal("导入后应有有效样本")
	}
	if report.Source != "import" || report.Format != "v8" {
		t.Fatalf("应标注来源与格式: %+v", report)
	}
	if len(report.Cumulative) == 0 {
		t.Fatal("应有累计视图")
	}
	found := false
	for _, entry := range report.Cumulative {
		if entry.Func == "spin" {
			found = true
			if entry.Samples <= 0 || entry.Pct <= 0 {
				t.Fatalf("spin 的累计样本与占比应大于 0: %+v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("累计视图应包含夹具函数 spin: %+v", report.Cumulative)
	}
	if len(report.Filtered) == 0 {
		t.Fatalf("V8 剖析必然含 (program)/(root) 之类噪声，应计入 filtered: %+v", report.Filtered)
	}
	if !strings.Contains(strings.Join(report.Notes, "；"), "统计值") {
		t.Fatalf("必须标注统计性质: %v", report.Notes)
	}
}
