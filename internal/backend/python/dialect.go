package python

import (
	"context"
	"fmt"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/dap"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// Launcher 创建 Python 调试后端。
type Launcher struct{}

// Open 实现 backend.Launcher。
func (Launcher) Open(ctx context.Context, spec backend.LaunchSpec) (backend.Debugger, error) {
	if spec.Language == "" {
		spec.Language = backend.Python
	}
	return dap.Open(ctx, Dialect{}, spec)
}

// Dialect 提供 debugpy 的语言侧差异。
type Dialect struct{}

// Language 实现 dap.Dialect。
func (Dialect) Language() backend.Language { return backend.Python }

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
			"采集点追踪与栈采样尚未在该后端可用",
			"生成器与协程只有在挂起时可查看，需展开 gi_frame / cr_frame",
			"C 扩展帧没有源码与局部变量，只能查看函数名",
			"单次展开最多跟随 5 页（约 500 项），超大容器建议用区间取值",
			"只预取前 10 层栈帧的作用域，更深的帧可以求值但读不到局部变量",
			"改值只接受数字、布尔、字符串与 None 字面量",
		},
	}
}

// Initialize 实现 dap.Dialect。
func (Dialect) Initialize() map[string]any {
	return map[string]any{
		"clientID":                     "lang-debug-mcp",
		"clientName":                   "lang-debug-mcp",
		"adapterID":                    "python",
		"linesStartAt1":                true,
		"columnsStartAt1":              true,
		"pathFormat":                   "path",
		"supportsVariableType":         true,
		"supportsVariablePaging":       true,
		"supportsRunInTerminalRequest": false,
		"supportsProgressReporting":    false,
	}
}

// AdapterCommand 实现 dap.Dialect。
//
// debugpy 不带 --port 时通过标准输入输出承载 DAP，因此由本进程托管适配器，
// 目标进程由适配器在 launch 时自行启动。
func (Dialect) AdapterCommand(spec backend.LaunchSpec) ([]string, []string, string, error) {
	interp, err := resolveInterpreter(spec)
	if err != nil {
		return nil, nil, "", err
	}
	return []string{interp.Path, "-m", "debugpy.adapter"}, spec.Env, spec.Cwd, nil
}

// Launch 实现 dap.Dialect。
func (Dialect) Launch(spec backend.LaunchSpec) (map[string]any, error) {
	interp, target, err := prepareLaunch(spec)
	if err != nil {
		return nil, err
	}
	args := map[string]any{
		// internalConsole 让适配器把目标输出作为 output 事件回传，而不是尝试
		// 发起 runInTerminal 反向请求（本客户端不支持终端接管）。
		"console":     "internalConsole",
		"stopOnEntry": true,
		"justMyCode":  true,
		"python":      []string{interp.Path},
	}
	if len(target.PythonArgs) > 0 {
		args["pythonArgs"] = target.PythonArgs
	}
	if len(target.Args) > 0 {
		args["args"] = target.Args
	}
	switch {
	case target.Module != "":
		args["module"] = target.Module
	case target.Code != "":
		args["code"] = []string{target.Code}
	default:
		args["program"] = target.Program
	}
	if spec.Cwd != "" {
		args["cwd"] = spec.Cwd
	}
	return args, nil
}

// Attach 实现 dap.Dialect。
//
// attach 走 TCP：目标自己用 debugpy 监听端口并承载 DAP，本进程直接连过去，
// 因此不需要本地安装 debugpy，也不会把目标当作子进程托管。
func (Dialect) Attach(spec backend.LaunchSpec) (map[string]any, error) {
	if spec.Port <= 0 {
		return nil, model.New(model.CodeInputInvalid, "attach 模式必须提供调试端口")
	}
	return map[string]any{
		"connect": map[string]any{
			"host": "127.0.0.1",
			"port": spec.Port,
		},
		"justMyCode": true,
	}, nil
}

// target 是从启动命令中拆出的调用方式。
type target struct {
	PythonArgs []string
	Program    string
	Module     string
	Code       string
	Args       []string
}

// prepareLaunch 解析解释器与启动方式。
func prepareLaunch(spec backend.LaunchSpec) (Interpreter, target, error) {
	t, err := resolveTarget(spec.Command)
	if err != nil {
		return Interpreter{}, target{}, err
	}
	interp, err := resolveInterpreter(spec)
	if err != nil {
		return Interpreter{}, target{}, err
	}
	return interp, t, nil
}

// resolveInterpreter 选择承载 debugpy 的解释器。
//
// 启动命令里的解释器优先：目标依赖通常只装在它自己的虚拟环境里。但它必须
// 装有 debugpy，否则退回自动发现会让目标换到另一个环境运行，风险更大。
func resolveInterpreter(spec backend.LaunchSpec) (Interpreter, error) {
	if len(spec.Command) > 0 && isPythonCommand(spec.Command[0]) {
		interp, err := probeInterpreter(spec.Command[0])
		if err != nil {
			return Discover()
		}
		if interp.Debugpy == "" {
			return interp, model.New(model.CodeEnvAdapterMissing, "解释器 %s 未安装 debugpy", interp.Path).
				WithFix(fmt.Sprintf("%s -m pip install debugpy", interp.Path)).
				WithContext("language", string(backend.Python)).
				WithContext("backend", backendName)
		}
		return interp, nil
	}
	return Discover()
}

// resolveTarget 把 "python 入口 参数" 拆成 debugpy 的 program/module/code 三种形式。
func resolveTarget(command []string) (target, error) {
	if len(command) == 0 {
		return target{}, model.New(model.CodeInputInvalid, "启动命令为空").
			WithFix(`示例: ["python", "main.py"]`)
	}
	if !isPythonCommand(command[0]) {
		return target{}, model.New(model.CodeEnvDebugInfo, "无法为 %s 注入调试参数", command[0]).
			WithFix(`请用 Python 解释器启动入口，如 ["python", "main.py"] 或 ["python", "-m", "pkg.module"]；` +
				`若目标已在运行，让它带 debugpy --listen 启动后改用 attach 模式`)
	}

	t := target{}
	for i := 1; i < len(command); i++ {
		arg := command[i]
		switch {
		case arg == "-m" || arg == "-c":
			if i+1 >= len(command) {
				return target{}, model.New(model.CodeInputInvalid, "%s 之后缺少参数", arg)
			}
			if arg == "-m" {
				t.Module = command[i+1]
			} else {
				t.Code = command[i+1]
			}
			t.Args = append([]string{}, command[i+2:]...)
			return t, nil
		case strings.HasPrefix(arg, "-"):
			t.PythonArgs = append(t.PythonArgs, arg)
			// -X / -W 的取值可以是独立参数，需要一并透传给解释器。
			if arg == "-X" || arg == "-W" {
				if i+1 >= len(command) {
					return target{}, model.New(model.CodeInputInvalid, "%s 之后缺少参数", arg)
				}
				t.PythonArgs = append(t.PythonArgs, command[i+1])
				i++
			}
		default:
			t.Program = arg
			t.Args = append([]string{}, command[i+1:]...)
			return t, nil
		}
	}
	return target{}, model.New(model.CodeInputInvalid, "启动命令中没有可调试的入口").
		WithFix(`示例: ["python", "main.py"] 或 ["python", "-m", "pkg.module"]`)
}

// 确保实现契约。
var (
	_ backend.Launcher = Launcher{}
	_ dap.Dialect      = Dialect{}
)
