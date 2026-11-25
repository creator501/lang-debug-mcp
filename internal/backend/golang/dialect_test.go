package golang

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// TestParseGoRunTarget 验证 go run 形态被翻译成 dlv 的 debug 模式。
func TestParseGoRunTarget(t *testing.T) {
	target, err := parseTarget(backend.LaunchSpec{
		Command: []string{"go", "run", "./cmd/app", "-port=8080"},
		Cwd:     "E:\\work",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if target.mode != "debug" {
		t.Fatalf("应翻译成 debug 模式: %+v", target)
	}
	if !filepath.IsAbs(target.program) || filepath.Base(target.program) != "app" {
		t.Fatalf("包路径应补全为绝对路径: %q", target.program)
	}
	if len(target.args) != 1 || target.args[0] != "-port=8080" {
		t.Fatalf("目标参数应原样保留: %+v", target.args)
	}
}

// TestParseGoRunBuildFlags 验证编译参数被透传而不是当成目标参数。
func TestParseGoRunBuildFlags(t *testing.T) {
	target, err := parseTarget(backend.LaunchSpec{
		Command: []string{"go", "run", "-tags", "dev", "-race", "./cmd/app"},
		Cwd:     "E:\\work",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if filepath.Base(target.program) != "app" {
		t.Fatalf("包路径解析错误: %+v", target)
	}
	if len(target.buildFlags) != 3 {
		t.Fatalf("应保留 -tags dev 与 -race: %+v", target.buildFlags)
	}
	if len(target.args) != 0 {
		t.Fatalf("编译参数不应进入目标参数: %+v", target.args)
	}
}

// TestParseExecTarget 验证直接给二进制时使用 exec 模式。
func TestParseExecTarget(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取当前可执行文件失败: %v", err)
	}
	target, err := parseTarget(backend.LaunchSpec{Command: []string{exe, "arg1"}})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if target.mode != "exec" || target.program == "" {
		t.Fatalf("应翻译成 exec 模式: %+v", target)
	}
	if len(target.args) != 1 || target.args[0] != "arg1" {
		t.Fatalf("目标参数应保留: %+v", target.args)
	}
}

// TestParseTargetErrors 覆盖不可调试的启动命令，必须给出可执行的修复建议。
func TestParseTargetErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"go", "build", "./..."},
		{"go", "run"},
		{filepath.Join("nope", "missing-binary")},
	}
	for _, command := range cases {
		if _, err := parseTarget(backend.LaunchSpec{Command: command}); err == nil {
			t.Fatalf("命令 %v 应报错", command)
		}
	}
	_, err := parseTarget(backend.LaunchSpec{Command: []string{"go", "build", "./..."}})
	if model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("go build 应报 INPUT_INVALID，实际 %s", model.CodeOf(err))
	}
}

// TestLaunchArgsCarryDebugFlags 验证 launch 参数把编译参数交给 dlv。
func TestLaunchArgsCarryDebugFlags(t *testing.T) {
	args, err := Dialect{}.Launch(backend.LaunchSpec{
		Command: []string{"go", "run", "-gcflags", "all=-N -l", "./cmd/app"},
		Cwd:     "E:\\work",
	})
	if err != nil {
		t.Fatalf("构造 launch 参数失败: %v", err)
	}
	if args["mode"] != "debug" {
		t.Fatalf("mode 不符: %+v", args)
	}
	if program, _ := args["program"].(string); filepath.Base(program) != "app" {
		t.Fatalf("program 应为补全后的包路径: %+v", args)
	}
	if args["stopOnEntry"] != true {
		t.Fatalf("应在入口停下以便先设断点: %+v", args)
	}
	if _, ok := args["buildFlags"]; !ok {
		t.Fatalf("编译参数应透传给 dlv: %+v", args)
	}
}

// TestAttachRequiresPort 验证 attach 缺少端口时给出修复建议。
func TestAttachRequiresPort(t *testing.T) {
	if _, err := (Dialect{}).Attach(backend.LaunchSpec{}); err == nil {
		t.Fatal("缺少端口应报错")
	}
	if _, err := (Dialect{}).Attach(backend.LaunchSpec{Port: 4040}); err != nil {
		t.Fatalf("带端口应通过: %v", err)
	}
}
