package native

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// fixtureSource 是 C 夹具的路径（native 包位于 internal/backend/native）。
const fixtureSource = "../../../testdata/fixture/app.c"

// buildVariant 把夹具复制到 dir 下并编译，返回可执行文件路径。
//
// 复制到与二进制同目录，才能同时覆盖“源码是否比二进制新”的判据。
func buildVariant(t *testing.T, dir string, gccArgs ...string) string {
	t.Helper()
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("未安装 gcc，跳过调试信息校验测试")
	}
	source := filepath.Join(dir, "app.c")
	data, err := os.ReadFile(fixtureSource)
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatalf("复制夹具失败: %v", err)
	}
	out := filepath.Join(dir, "app.exe")
	args := append(append([]string{}, gccArgs...), "-o", out, source)
	if combined, err := exec.Command("gcc", args...).CombinedOutput(); err != nil {
		t.Fatalf("编译失败（gcc %v）: %v\n%s", gccArgs, err, combined)
	}
	return out
}

func TestCheckDebugInfoAcceptsDebuggableBinary(t *testing.T) {
	exe := buildVariant(t, t.TempDir(), "-g", "-O0")

	warnings, err := CheckDebugInfo([]string{exe}, "")
	if err != nil {
		t.Fatalf("带调试信息的二进制不应被阻断: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("带 -g -O0 的二进制不应有警告: %v", warnings)
	}
}

func TestCheckDebugInfoBlocksMissingLineTable(t *testing.T) {
	exe := buildVariant(t, t.TempDir(), "-O0")

	_, err := CheckDebugInfo([]string{exe}, "")
	if model.CodeOf(err) != model.CodeEnvDebugInfo {
		t.Fatalf("无 -g 的二进制应返回 %s，实际 %v", model.CodeEnvDebugInfo, err)
	}
	if !strings.Contains(err.Error(), "源码行号") {
		t.Fatalf("错误应指出缺用户源码的行号信息: %v", err)
	}
}

func TestCheckDebugInfoBlocksStrippedBinary(t *testing.T) {
	exe := buildVariant(t, t.TempDir(), "-g", "-O0", "-s")

	_, err := CheckDebugInfo([]string{exe}, "")
	if model.CodeOf(err) != model.CodeEnvDebugInfo {
		t.Fatalf("strip 后的二进制应返回 %s，实际 %v", model.CodeEnvDebugInfo, err)
	}
}

func TestCheckDebugInfoWarnsOptimizedBuild(t *testing.T) {
	exe := buildVariant(t, t.TempDir(), "-g", "-O2")

	warnings, err := CheckDebugInfo([]string{exe}, "")
	if err != nil {
		t.Fatalf("优化构建不应阻断会话: %v", err)
	}
	if !containsAny(warnings, "-O2") {
		t.Fatalf("应警告 -O2 会优化掉局部变量，实际 %v", warnings)
	}
}

func TestCheckDebugInfoWarnsStaleSource(t *testing.T) {
	dir := t.TempDir()
	exe := buildVariant(t, dir, "-g", "-O0")

	source := filepath.Join(dir, "app.c")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(source, future, future); err != nil {
		t.Fatalf("调整源文件时间失败: %v", err)
	}

	warnings, err := CheckDebugInfo([]string{exe}, "")
	if err != nil {
		t.Fatalf("源码较新只应警告，不应阻断: %v", err)
	}
	if !containsAny(warnings, "源文件比二进制新") {
		t.Fatalf("应警告源码新于二进制，实际 %v", warnings)
	}
}

func TestCheckDebugInfoRejectsMissingTarget(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.exe")
	_, err := CheckDebugInfo([]string{missing}, "")
	if model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("不存在的目标应返回 %s，实际 %v", model.CodeInputInvalid, err)
	}
	if _, err := CheckDebugInfo(nil, ""); model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("空命令应返回 %s，实际 %v", model.CodeInputInvalid, err)
	}
}

// TestTargetExecutableResolvesAgainstCwd 覆盖相对路径按 cwd 补全。
func TestTargetExecutableResolvesAgainstCwd(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "app.exe")
	if err := os.WriteFile(want, nil, 0o755); err != nil {
		t.Fatalf("写占位可执行文件失败: %v", err)
	}
	got, err := targetExecutable([]string{"app.exe"}, dir)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != want {
		t.Fatalf("路径 = %s，期望 %s", got, want)
	}
}

// containsAny 报告警告列表里是否有任意一条包含关键片段。
func containsAny(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}
