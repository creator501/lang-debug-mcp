package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"lldb-dap version 19.1.0":           "19.1.0",
		"lldb version 18.1.8 (https://...)": "18.1.8",
		"no version here":                   "",
	}
	for text, want := range cases {
		if got := parseVersion(text); got != want {
			t.Errorf("parseVersion(%q) = %q, 期望 %q", text, got, want)
		}
	}
}

func TestInitializeKeepsPathFormat(t *testing.T) {
	init := Dialect{}.Initialize()
	if got := init["adapterID"]; got != backendName {
		t.Fatalf("adapterID = %v, 期望 %s", got, backendName)
	}
	if got := init["pathFormat"]; got != "path" {
		t.Fatalf("pathFormat = %v, 期望 path", got)
	}
	if got := init["supportsRunInTerminalRequest"]; got != false {
		t.Fatalf("supportsRunInTerminalRequest = %v, 期望 false（输出走 output 事件）", got)
	}
}

func TestLaunchBuildsProgramAndArgs(t *testing.T) {
	program := tempExecutable(t, "app.exe")
	dir := t.TempDir()
	spec := backend.LaunchSpec{
		Language: backend.Cpp,
		Command:  []string{program, "--flag", "value"},
		Cwd:      dir,
		Env:      []string{"A=1", "BROKEN"},
	}
	out, err := Dialect{}.Launch(spec)
	if err != nil {
		t.Fatalf("Launch 出错: %v", err)
	}
	if out["program"] != program {
		t.Fatalf("program = %v, 期望 %s", out["program"], program)
	}
	args, ok := out["args"].([]string)
	if !ok || len(args) != 2 || args[0] != "--flag" {
		t.Fatalf("args = %#v, 期望 [--flag value]", out["args"])
	}
	if out["cwd"] != dir {
		t.Fatalf("cwd = %v, 期望 %s", out["cwd"], dir)
	}
	env, ok := out["env"].(map[string]string)
	if !ok || len(env) != 1 || env["A"] != "1" {
		t.Fatalf("env = %#v, 期望只保留 A=1", out["env"])
	}
	if out["console"] != "internalConsole" {
		t.Fatalf("console = %v, 期望 internalConsole", out["console"])
	}
}

func TestLaunchResolvesRelativeProgramAgainstCwd(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "bin", "app.exe")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(want, nil, 0o755); err != nil {
		t.Fatalf("写占位可执行文件失败: %v", err)
	}
	out, err := Dialect{}.Launch(backend.LaunchSpec{
		Language: backend.C,
		Command:  []string{filepath.Join("bin", "app.exe")},
		Cwd:      dir,
	})
	if err != nil {
		t.Fatalf("Launch 出错: %v", err)
	}
	if out["program"] != want {
		t.Fatalf("program = %v, 期望 %s", out["program"], want)
	}
}

func TestLaunchRejectsEmptyAndMissingProgram(t *testing.T) {
	if _, err := (Dialect{}).Launch(backend.LaunchSpec{Language: backend.C}); model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("空命令应返回 %s，实际 %v", model.CodeInputInvalid, err)
	}
	missing := filepath.Join(t.TempDir(), "nope.exe")
	_, err := Dialect{}.Launch(backend.LaunchSpec{Language: backend.Rust, Command: []string{missing}})
	if model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("不存在的可执行文件应返回 %s，实际 %v", model.CodeInputInvalid, err)
	}
}

func TestAttachReportsUnsupported(t *testing.T) {
	_, err := Dialect{}.Attach(backend.LaunchSpec{Language: backend.C, Mode: "attach", Port: 4711})
	if model.CodeOf(err) != model.CodeCapUnsupported {
		t.Fatalf("attach 应返回 %s，实际 %v", model.CodeCapUnsupported, err)
	}
}

func TestLauncherRejectsAttachBeforeConnecting(t *testing.T) {
	_, err := (Launcher{}).Open(context.Background(), backend.LaunchSpec{
		Language: backend.Cpp,
		Mode:     "attach",
		Port:     4711,
	})
	if model.CodeOf(err) != model.CodeCapUnsupported {
		t.Fatalf("attach 应在连端口前被拒绝并返回 %s，实际 %v", model.CodeCapUnsupported, err)
	}
}

// TestMapWriteErrorClassifiesReasons 覆盖改值失败的三类边界与能力缺失。
func TestMapWriteErrorClassifiesReasons(t *testing.T) {
	cases := []struct {
		message string
		want    model.Code
	}{
		{"cannot assign to variable 'k' with const-qualified type 'const int'", model.CodeTargetImmutable},
		{"error: cannot write to read-only memory", model.CodeTargetReadOnly},
		{"variable has no location, value may have been optimized out", model.CodeTargetOptimized},
		{"unsupported request: setExpression", model.CodeCapUnsupported},
		{"something else entirely", ""},
	}
	for _, c := range cases {
		err := (Dialect{}).MapWriteError("int", errors.New(c.message))
		if c.want == "" {
			if err != nil {
				t.Fatalf("%q 不应被归类，实际 %v", c.message, err)
			}
			continue
		}
		if got := model.CodeOf(err); got != c.want {
			t.Fatalf("%q 应归类为 %s，实际 %s", c.message, c.want, got)
		}
	}
	if err := (Dialect{}).MapWriteError("int", nil); err != nil {
		t.Fatalf("nil 错误应返回 nil，实际 %v", err)
	}
}

func TestProbeReportsLanguageAndBackend(t *testing.T) {
	for _, lang := range []backend.Language{backend.C, backend.Cpp, backend.Rust} {
		st := (Prober{Lang: lang}).Probe()
		if st.Language != lang {
			t.Fatalf("Language = %s, 期望 %s", st.Language, lang)
		}
		if st.Backend != backendName {
			t.Fatalf("Backend = %s, 期望 %s", st.Backend, backendName)
		}
		if !st.Available && st.InstallHint == "" {
			t.Fatalf("%s 不可用时应给出安装指引", lang)
		}
		if st.Available && len(st.Notes) == 0 {
			t.Fatalf("%s 可用时应说明读值边界", lang)
		}
	}
}

// TestDiscoverSkipsBrokenAdapter 确认残缺安装不会被当成可用适配器：
// 与其让会话在 initialize 阶段超时，不如直接报告“没装”。
func TestDiscoverSkipsBrokenAdapter(t *testing.T) {
	broken := tempExecutable(t, "lldb-dap.exe")
	t.Setenv("LANGDEBUG_LLDB_DAP", broken)

	adapter, err := Discover()
	if err == nil {
		t.Fatalf("不可执行的适配器不应被采用，实际返回 %+v", adapter)
	}
	if model.CodeOf(err) != model.CodeEnvAdapterMissing {
		t.Fatalf("应返回 %s，实际 %v", model.CodeEnvAdapterMissing, err)
	}
}

func TestProberDefaultsToC(t *testing.T) {
	if got := (Prober{}).Language(); got != backend.C {
		t.Fatalf("Language = %s, 期望 %s", got, backend.C)
	}
}

// tempExecutable 在临时目录里造一个占位可执行文件，用于路径校验。
func tempExecutable(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte{}, 0o755); err != nil {
		t.Fatalf("写占位可执行文件失败: %v", err)
	}
	return path
}
