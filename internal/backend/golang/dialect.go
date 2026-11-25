// Package golang 是 Go 语言的调试后端：以 dlv dap 为适配器，走通用 DAP 协议。
//
// 与 debugpy 的区别在传输形态：dlv dap 自己监听端口并把地址打印到输出，
// 客户端要先等地址出现再连入，因此实现 dap.ListenDialect。
package golang

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/dap"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// backendName 是本后端的展示名。
const backendName = "dlv-dap"

// listenPattern 匹配 dlv dap 报告的监听地址。
var listenPattern = regexp.MustCompile(`DAP server listening at:\s+(\S+)`)

// dlvEnvOverride 允许指定 dlv 可执行文件路径。
const dlvEnvOverride = "LANGDEBUG_DLV"

// buildFlagWithValue 是 go run 中需要取值的编译参数。
var buildFlagWithValue = map[string]bool{
	"-tags": true, "-ldflags": true, "-gcflags": true, "-asmflags": true,
	"-mod": true, "-modfile": true, "-overlay": true, "-p": true, "-C": true,
}

// Launcher 创建 Go 调试后端。
type Launcher struct{}

// Open 实现 backend.Launcher。
func (Launcher) Open(ctx context.Context, spec backend.LaunchSpec) (backend.Debugger, error) {
	if spec.Language == "" {
		spec.Language = backend.Go
	}
	return dap.Open(ctx, Dialect{}, spec)
}

// Dialect 提供 dlv dap 的语言侧差异。
type Dialect struct{}

// Language 实现 dap.Dialect。
func (Dialect) Language() backend.Language { return backend.Go }

// Backend 实现 dap.Dialect。
func (Dialect) Backend() string { return backendName }

// Capability 实现 dap.Dialect。
func (Dialect) Capability() model.Capability {
	return model.Capability{
		Backend: backendName,
		Read:    true,
		Write:   true,
		Eval:    true,
		Limits: []string{
			"只显示当前 goroutine 的栈：切换 goroutine 需要 dlv CLI，本工具不做",
			"编译优化后变量可能显示为 optimized out，调试请用 -gcflags=\"all=-N -l\" 重编",
			"内联函数的帧可能被折叠，栈上看到的是调用方",
			"map/slice/struct 需要展开读取，超大容器建议用区间取值",
			"改值只接受基础字面量（数字、布尔、字符串、nil）",
			"dlv dap 以独立进程运行，会话关闭时一并终止",
		},
	}
}

// Initialize 实现 dap.Dialect。
func (Dialect) Initialize() map[string]any {
	return map[string]any{
		"clientID":                     "lang-debug-mcp",
		"clientName":                   "lang-debug-mcp",
		"adapterID":                    "go",
		"linesStartAt1":                true,
		"columnsStartAt1":              true,
		"pathFormat":                   "path",
		"supportsVariableType":         true,
		"supportsVariablePaging":       true,
		"supportsRunInTerminalRequest": false,
		"supportsProgressReporting":    false,
	}
}

// AdapterCommand 实现 dap.Dialect；dlv 走端口而非标准输入输出，这里返回等价命令。
func (Dialect) AdapterCommand(spec backend.LaunchSpec) ([]string, []string, string, error) {
	dlv, err := resolveDLV()
	if err != nil {
		return nil, nil, "", err
	}
	return []string{dlv, "dap", "--listen=127.0.0.1:0"}, spec.Env, spec.Cwd, nil
}

// ListenCommand 实现 dap.ListenDialect。
func (Dialect) ListenCommand(spec backend.LaunchSpec) (dap.ListenSpec, error) {
	dlv, err := resolveDLV()
	if err != nil {
		return dap.ListenSpec{}, err
	}
	return dap.ListenSpec{
		// 监听 :0 让系统分配空闲端口，避免与其它会话抢固定端口。
		Argv:    []string{dlv, "dap", "--listen=127.0.0.1:0"},
		Env:     spec.Env,
		Dir:     spec.Cwd,
		Address: listenPattern,
		Timeout: 20 * time.Second,
	}, nil
}

// EntryFunctions 实现 dap.EntryStopper：入口停在该函数上。
//
// dlv 的 stopOnEntry 只给出一个占位线程（目标还没初始化 Go 运行时），此时
// stackTrace 会以 unknown goroutine 失败，停止快照里没有任何栈帧与变量。
// 声明 main.main 后，会话会在放行目标前下内部断点，收到占位停止后继续到
// main.main，入口停止就有 main.main → runtime.main 的真实栈。
func (Dialect) EntryFunctions() []string { return []string{"main.main"} }

// Launch 实现 dap.Dialect：把 go run / 可执行文件两种启动方式翻译成 dlv 的 launch 参数。
func (Dialect) Launch(spec backend.LaunchSpec) (map[string]any, error) {
	target, err := parseTarget(spec)
	if err != nil {
		return nil, err
	}
	args := map[string]any{
		"mode":        target.mode,
		"program":     target.program,
		"stopOnEntry": true,
		"apiVersion":  2,
	}
	if spec.Cwd != "" {
		args["cwd"] = spec.Cwd
	}
	if len(target.args) > 0 {
		args["args"] = target.args
	}
	if len(target.buildFlags) > 0 {
		args["buildFlags"] = target.buildFlags
		args["buildFlagsAll"] = true
	}
	return args, nil
}

// Attach 实现 dap.Dialect：连接已在监听 DAP 端口的目标（dlv dap --listen）。
func (Dialect) Attach(spec backend.LaunchSpec) (map[string]any, error) {
	if spec.Port <= 0 {
		return nil, model.New(model.CodeInputInvalid, "attach 模式必须提供调试端口").
			WithFix("先运行 dlv dap --listen=127.0.0.1:<端口>，再把该端口传给 debug_session")
	}
	return map[string]any{"mode": "local"}, nil
}

// resolved 是启动命令解析后的结果。
type resolved struct {
	mode       string
	program    string
	args       []string
	buildFlags []string
}

// parseTarget 把启动命令翻译成 dlv 的 mode/program/args。
//
// 支持两种形态：`go run <包或文件> [参数...]`（dlv 负责编译）与
// 直接给可执行文件（已编译好，dlv 以 exec 方式加载）。
func parseTarget(spec backend.LaunchSpec) (resolved, error) {
	command := spec.Command
	if len(command) == 0 {
		return resolved{}, model.New(model.CodeInputInvalid, "启动命令为空").
			WithFix(`示例: ["go", "run", "./cmd/app"] 或 ["./app"]`)
	}
	if isGoCommand(command[0]) {
		return parseGoRun(spec, command)
	}
	program := command[0]
	if !filepath.IsAbs(program) {
		base := spec.Cwd
		if base == "" {
			base, _ = os.Getwd()
		}
		program = filepath.Join(base, program)
	}
	if st, err := os.Stat(program); err != nil || st.IsDir() {
		return resolved{}, model.New(model.CodeEnvDebugInfo, "找不到可调试的可执行文件 %s", command[0]).
			WithFix(`用 ["go", "run", "<包路径>"] 让 dlv 负责编译，或先 go build 再传二进制路径`)
	}
	return resolved{mode: "exec", program: program, args: command[1:]}, nil
}

// parseGoRun 解析 `go run` 形态，编译参数原样透传给 dlv。
func parseGoRun(spec backend.LaunchSpec, command []string) (resolved, error) {
	if len(command) < 3 || command[1] != "run" {
		return resolved{}, model.New(model.CodeInputInvalid, "只支持 go run 形态").
			WithFix(`示例: ["go", "run", "./cmd/app"]；其它 go 子命令请先 go build 再调试二进制`)
	}
	out := resolved{mode: "debug"}
	rest := command[2:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if !strings.HasPrefix(arg, "-") {
			out.program = absolutePackage(spec.Cwd, arg)
			out.args = append([]string{}, rest[i+1:]...)
			return out, nil
		}
		if buildFlagWithValue[arg] {
			if i+1 >= len(rest) {
				return resolved{}, model.New(model.CodeInputInvalid, "%s 之后缺少取值", arg)
			}
			out.buildFlags = append(out.buildFlags, arg, rest[i+1])
			i++
			continue
		}
		out.buildFlags = append(out.buildFlags, arg)
	}
	return resolved{}, model.New(model.CodeInputInvalid, "go run 之后缺少包路径或源文件").
		WithFix(`示例: ["go", "run", "./cmd/app"]`)
}

// absolutePackage 把相对包路径补成绝对路径：dlv 的 debug 模式对相对路径的
// 解析依赖它自己的工作目录，补全后会话建在哪个目录都一致。
func absolutePackage(cwd, program string) string {
	if program == "." || strings.HasPrefix(program, "./") || strings.HasPrefix(program, ".\\") {
		base := cwd
		if base == "" {
			base, _ = os.Getwd()
		}
		return filepath.Clean(filepath.Join(base, program))
	}
	if filepath.IsAbs(program) {
		return program
	}
	return program
}

// resolveDLV 定位 dlv 可执行文件，缺失时给出安装命令。
func resolveDLV() (string, error) {
	if override := os.Getenv(dlvEnvOverride); override != "" {
		return override, nil
	}
	path, err := exec.LookPath("dlv")
	if err != nil {
		return "", model.New(model.CodeEnvAdapterMissing, "未找到 Go 调试器 dlv").
			WithFix("go install github.com/go-delve/delve/cmd/dlv@latest，并确保 $(go env GOPATH)/bin 在 PATH 中").
			WithContext("language", string(backend.Go))
	}
	return path, nil
}

// isGoCommand 判断命令是否是 go 工具链。
func isGoCommand(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return base == "go" || base == "go.exe"
}

// 确保实现契约。
var (
	_ backend.Launcher  = Launcher{}
	_ dap.Dialect       = Dialect{}
	_ dap.ListenDialect = Dialect{}
)
