package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/dap"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// Launcher 创建 native 调试后端。
type Launcher struct{}

// Open 实现 backend.Launcher。
//
// native 目标不改写命令行：可执行文件与参数原样交给适配器，由它启动并托管。
// 首选 lldb-dap（能力完整）；缺失时降级到 gdb MI，只承诺断点、栈与变量读取。
func (Launcher) Open(ctx context.Context, spec backend.LaunchSpec) (backend.Debugger, error) {
	lang := spec.Language
	if lang.Family() != "native" {
		lang = backend.C
	}
	// 提前拒绝 attach：等到连端口再失败会把“未实现”说成“连不上”。
	if spec.Mode == "attach" {
		return nil, attachUnsupported(lang)
	}
	spec.Language = lang
	if _, err := Discover(); err == nil {
		return dap.Open(ctx, Dialect{Lang: lang}, spec)
	}
	if _, gerr := discoverGDB(); gerr == nil {
		return openMISession(ctx, lang, spec)
	}
	return nil, model.New(model.CodeEnvAdapterMissing,
		"未找到 lldb-dap，也没有可用的 gdb，native 调试后端不可用").
		WithFix("安装 lldb-dap（能力完整）或 gdb（降级为仅断点/栈/变量），"+
			"MSYS2 装在非默认目录时用 MSYS2_ROOT 指向其根目录，"+
			"或用 LANGDEBUG_LLDB_DAP 指定 lldb-dap 路径").
		WithContext("backend", backendName)
}

// Dialect 提供 lldb-dap 的语言侧差异。
type Dialect struct {
	// Lang 是本次会话的语言（C/C++/Rust），用于能力说明与错误提示。
	Lang backend.Language
}

// Language 实现 dap.Dialect。
func (d Dialect) Language() backend.Language {
	if d.Lang == "" {
		return backend.C
	}
	return d.Lang
}

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
			"优化编译（-O2 及以上）会让变量显示 <optimized out>，这类值不可恢复",
			"容器依赖调试器自带的美化打印：C 定长数组、std::vector、std::string 按序列展示；" +
				"Rust 的 Vec/String 需要 lldb 的 Rust 格式化脚本",
			"指针只给地址与目标对象，void* 需要先确认实际类型",
			"改值受只读内存段、编译期内联与寄存器分配限制，失败会给出原因",
			"仅支持 launch 模式由 lldb-dap 托管目标；attach（按端口或进程标识附着）尚未实现",
			"Rust 的 Vec/String/Option 没有调试器格式化脚本时按内部字段展示，元素读取用 debug_eval",
			"采集点追踪与栈采样尚未在该后端可用",
		},
	}
}

// Initialize 实现 dap.Dialect。
func (Dialect) Initialize() map[string]any {
	return map[string]any{
		"clientID":                     "lang-debug-mcp",
		"clientName":                   "lang-debug-mcp",
		"adapterID":                    backendName,
		"linesStartAt1":                true,
		"columnsStartAt1":              true,
		"pathFormat":                   "path",
		"supportsVariableType":         true,
		"supportsVariablePaging":       false,
		"supportsRunInTerminalRequest": false,
		"supportsMemoryReferences":     false,
	}
}

// AdapterCommand 实现 dap.Dialect。
func (Dialect) AdapterCommand(spec backend.LaunchSpec) ([]string, []string, string, error) {
	adapter, err := Discover()
	if err != nil {
		return nil, nil, "", err
	}
	return []string{adapter.Path}, spec.Env, spec.Cwd, nil
}

// Launch 实现 dap.Dialect。
//
// 目标命令行原样透传：第一个元素是可执行文件，其余是它的参数，不注入任何调试参数。
func (Dialect) Launch(spec backend.LaunchSpec) (map[string]any, error) {
	program, args, err := splitCommand(spec)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"program": program,
		"args":    args,
		// internalConsole 让目标输出以 output 事件回传，不会触发 runInTerminal 反向请求。
		"console":     "internalConsole",
		"stopOnEntry": true,
	}
	if spec.Cwd != "" {
		out["cwd"] = spec.Cwd
	}
	if len(spec.Env) > 0 {
		out["env"] = envPairs(spec.Env)
	}
	return out, nil
}

// Attach 实现 dap.Dialect。
//
// lldb-dap 的附着以进程标识为入口，而会话参数只有端口；这里明确报告能力缺失，
// 避免把“连上了但没附上”包装成成功。
func (Dialect) Attach(spec backend.LaunchSpec) (map[string]any, error) {
	return nil, attachUnsupported(spec.Language)
}

// attachUnsupported 是 native 侧附着未实现时统一返回的错误。
func attachUnsupported(lang backend.Language) error {
	return model.New(model.CodeCapUnsupported, "native 后端暂不支持 attach 模式").
		WithFix("改用 launch 模式由工具托管目标进程；按进程标识附着尚未实现").
		WithContext("language", string(lang)).
		WithContext("backend", backendName)
}

// splitCommand 把启动命令拆成可执行文件与参数，并把相对路径补成绝对路径。
func splitCommand(spec backend.LaunchSpec) (string, []string, error) {
	if len(spec.Command) == 0 {
		return "", nil, model.New(model.CodeInputInvalid, "启动命令为空").
			WithFix(`示例: ["C:\proj\app.exe", "--flag"] 或 ["./app"]`)
	}
	program := spec.Command[0]
	if !filepath.IsAbs(program) {
		base := spec.Cwd
		if base == "" {
			base = "."
		}
		program = filepath.Join(base, program)
	}
	abs, err := filepath.Abs(program)
	if err != nil {
		return "", nil, model.New(model.CodeInputInvalid, "无法解析可执行文件路径: %v", err).
			WithFix("给出可执行文件的绝对路径")
	}
	if st, serr := os.Stat(abs); serr != nil || st.IsDir() {
		return "", nil, model.New(model.CodeInputInvalid, "可执行文件不存在: %s", abs).
			WithFix("先编译目标，再把编译产物的绝对路径作为启动命令的第一个参数").
			WithContext("hint", compilerHint(string(spec.Language)))
	}
	return abs, append([]string{}, spec.Command[1:]...), nil
}

// envPairs 把 KEY=VALUE 形式的环境变量转成 DAP 需要的映射。
func envPairs(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = val
	}
	return out
}

// 确保实现契约。
var (
	_ backend.Launcher = Launcher{}
	_ dap.Dialect      = Dialect{}
)
