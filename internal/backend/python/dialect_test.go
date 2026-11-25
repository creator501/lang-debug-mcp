package python

import (
	"path/filepath"
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
)

func TestResolveTargetScript(t *testing.T) {
	got, err := resolveTarget([]string{"python", "app.py", "--flag", "1"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Program != "app.py" {
		t.Fatalf("入口脚本不符: %q", got.Program)
	}
	if len(got.Args) != 2 || got.Args[1] != "1" {
		t.Fatalf("目标参数不符: %v", got.Args)
	}
	if got.Module != "" || got.Code != "" {
		t.Fatalf("脚本模式不应产生 module/code: %+v", got)
	}
}

func TestResolveTargetModuleWithInterpreterFlags(t *testing.T) {
	got, err := resolveTarget([]string{"python3", "-u", "-X", "dev", "-m", "pkg.mod", "arg"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Module != "pkg.mod" {
		t.Fatalf("模块名不符: %q", got.Module)
	}
	if len(got.Args) != 1 || got.Args[0] != "arg" {
		t.Fatalf("模块参数不符: %v", got.Args)
	}
	want := []string{"-u", "-X", "dev"}
	if len(got.PythonArgs) != len(want) {
		t.Fatalf("解释器参数不符: %v", got.PythonArgs)
	}
	for i := range want {
		if got.PythonArgs[i] != want[i] {
			t.Fatalf("解释器参数不符: %v", got.PythonArgs)
		}
	}
}

func TestResolveTargetCode(t *testing.T) {
	got, err := resolveTarget([]string{"py", "-c", "print(1)"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Code != "print(1)" {
		t.Fatalf("-c 形式应解析为 code，实际 %+v", got)
	}
}

func TestResolveTargetRejectsNonPython(t *testing.T) {
	if _, err := resolveTarget([]string{"node", "app.js"}); err == nil {
		t.Fatal("非 Python 启动命令必须被拒绝并给出改写建议")
	}
	if _, err := resolveTarget([]string{"python"}); err == nil {
		t.Fatal("缺少入口的命令必须被拒绝")
	}
	if _, err := resolveTarget(nil); err == nil {
		t.Fatal("空命令必须被拒绝")
	}
}

func TestIsPythonCommand(t *testing.T) {
	// 带路径的解释器要用 filepath.Join 构造，避免把 Windows 分隔符写死在用例里。
	venvPython := filepath.Join("venv", "Scripts", "python.exe")
	yes := []string{"python", "python3", "python3.12", venvPython, "py", "pypy3"}
	for _, in := range yes {
		if !isPythonCommand(in) {
			t.Fatalf("%q 应识别为 Python 解释器", in)
		}
	}
	no := []string{"node", "pytest", "pythonw.exe.bak", "java"}
	for _, in := range no {
		if isPythonCommand(in) {
			t.Fatalf("%q 不应识别为 Python 解释器", in)
		}
	}
}

func TestProberReportsConsistentStatus(t *testing.T) {
	st := (Prober{}).Probe()
	if st.Backend != backendName || st.Language != backend.Python {
		t.Fatalf("探测结果标识不符: %+v", st)
	}
	if st.Available {
		if st.Version == "" {
			t.Fatal("可用时必须给出 debugpy 版本")
		}
		return
	}
	if st.InstallHint == "" {
		t.Fatalf("不可用时必须给出安装提示: %+v", st)
	}
	if len(st.Missing) == 0 {
		t.Fatalf("不可用时必须给出缺失项: %+v", st)
	}
}

func TestAttachArguments(t *testing.T) {
	args, err := (Dialect{}).Attach(backend.LaunchSpec{Port: 5678})
	if err != nil {
		t.Fatalf("构造 attach 参数失败: %v", err)
	}
	connect, ok := args["connect"].(map[string]any)
	if !ok {
		t.Fatalf("attach 必须使用 connect 形式: %#v", args)
	}
	if connect["port"] != 5678 || connect["host"] != "127.0.0.1" {
		t.Fatalf("connect 参数不符: %#v", connect)
	}
	if _, err := (Dialect{}).Attach(backend.LaunchSpec{}); err == nil {
		t.Fatal("缺少端口时 attach 必须失败")
	}
}
