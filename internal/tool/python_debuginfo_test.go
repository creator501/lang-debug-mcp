package tool

import (
	"strings"
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

func TestCheckPythonDebugInfoWarnsOnOptimize(t *testing.T) {
	warnings, err := checkPythonDebugInfo(sessionArgs{
		Command: []string{"python", "-O", pythonFixture},
	})
	if err != nil {
		t.Fatalf("检查不应失败: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "-O") {
		t.Fatalf("应就 -O 给出警告，实际 %v", warnings)
	}

	warnings, err = checkPythonDebugInfo(sessionArgs{
		Command: []string{"python", pythonFixture},
	})
	if err != nil {
		t.Fatalf("检查不应失败: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("普通启动不应有警告，实际 %v", warnings)
	}
}

func TestCheckPythonDebugInfoMissingEntry(t *testing.T) {
	_, err := checkPythonDebugInfo(sessionArgs{
		Command: []string{"python", "no-such-entry.py"},
	})
	if err == nil {
		t.Fatal("入口脚本不存在时必须失败")
	}
	if model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("错误码应为 INPUT_INVALID，实际 %s", model.CodeOf(err))
	}
}

func TestPythonEntryScriptSkipsInterpreterFlags(t *testing.T) {
	if got := pythonEntryScript([]string{"python", "-u", "-X", "dev", "main.py", "arg"}); got != "main.py" {
		t.Fatalf("应跳过解释器参数取入口脚本，实际 %q", got)
	}
	if got := pythonEntryScript([]string{"python", "-m", "pkg.mod"}); got != "" {
		t.Fatalf("-m 形式没有脚本路径，实际 %q", got)
	}
}
