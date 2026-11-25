package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/jvm"
	"github.com/langdebug/lang-debug-mcp/internal/backend/native"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// Launchers 保存各语言的可用后端启动器。
//
// 未注册的语言在建立会话时会得到明确的“尚未实现”错误，而不是超时。
var Launchers = map[backend.Language]backend.Launcher{}

// sessionArgs 是 debug_session 的参数。
type sessionArgs struct {
	Action         string            `json:"action"`
	Session        string            `json:"session"`
	Language       string            `json:"language"`
	Mode           string            `json:"mode"`
	Command        []string          `json:"command"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	Port           int               `json:"port"`
	TimeoutMs      int               `json:"timeoutMs"`
	DebugInfoCheck string            `json:"debugInfoCheck"`
}

func registerSession(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_session",
		Description: "管理调试会话。open 建立会话（launch 由工具注入调试参数，你只需给出程序原本的启动命令；" +
			"attach 附着到已开启调试端口的进程）。list 列出会话，status 查看会话与当前停止位置，" +
			"close 关闭会话并清理进程树。建立会话前应先调用 debug_env 确认后端可用。",
		InputSchema: mcp.Object(map[string]any{
			"action":  mcp.Enum("要执行的动作", "open", "list", "status", "close"),
			"session": mcp.Str("会话标识。除 open/list 外必填；仅有一个会话时可省略"),
			"language": languageEnum("目标语言，open 时必填"),
			"mode":    mcp.Enum("launch 由工具启动目标；attach 附着已运行进程", "launch", "attach"),
			"command": mcp.Arr("目标程序原本的启动命令行，不要自行添加任何调试参数", mcp.Str("参数")),
			"cwd":     mcp.Str("工作目录"),
			"env": map[string]any{
				"type":                 "object",
				"description":          "追加的环境变量",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"port":           mcp.Int("attach 模式必填：目标已开启的调试端口"),
			"timeoutMs":      mcp.Int("建立会话与等待首次停止的超时，默认 20000"),
			"debugInfoCheck": mcp.Enum("调试信息校验模式", "block", "warn", "off"),
		}, "action"),
		Handler: handleSession,
	})
}

func handleSession(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args sessionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	switch args.Action {
	case "open":
		return openSession(ctx, args)
	case "list":
		return mcp.Text(map[string]any{"sessions": Sessions.List()}), nil
	case "status":
		session, err := Sessions.Get(args.Session)
		if err != nil {
			return failResult(err), nil
		}
		return mcp.Text(sessionStatus(ctx, session)), nil
	case "close":
		session, err := Sessions.Get(args.Session)
		if err != nil {
			return failResult(err), nil
		}
		id := session.ID
		if err := Sessions.Close(id); err != nil {
			return failResult(err), nil
		}
		return mcp.Text(map[string]any{"closed": id}), nil
	}
	return failResult(model.New(model.CodeInputInvalid, "不支持的动作: %s", args.Action).
		WithFix("可用动作: open / list / status / close")), nil
}

func openSession(ctx context.Context, args sessionArgs) (*mcp.Result, error) {
	lang := backend.Language(args.Language)
	if !lang.Valid() {
		return failResult(model.New(model.CodeInputInvalid, "不支持的语言: %s", args.Language).
			WithFix("先用 debug_env 查看可用语言")), nil
	}
	mode := args.Mode
	if mode == "" {
		mode = "launch"
	}
	if mode != "launch" && mode != "attach" {
		return failResult(model.New(model.CodeInputInvalid, "mode 只能是 launch 或 attach")), nil
	}
	if mode == "attach" && args.Port == 0 {
		return failResult(model.New(model.CodeInputInvalid, "attach 模式必须提供 port").
			WithFix("给出目标已开启的调试端口")), nil
	}
	if mode == "launch" && len(args.Command) == 0 {
		return failResult(model.New(model.CodeInputInvalid, "launch 模式必须提供 command").
			WithFix(`示例: ["node", "app.js"]`)), nil
	}
	args.Mode = mode

	checker := debugInfoCheckerFor(lang)
	warnings, err := runDebugInfoCheck(checker, args)
	if err != nil {
		return failResult(err), nil
	}
	if warnings == nil {
		warnings = []string{}
	}

	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	spec := backend.LaunchSpec{
		Language: lang,
		Mode:     mode,
		Command:  args.Command,
		Cwd:      args.Cwd,
		Port:     args.Port,
		Timeout:  timeout,
	}
	for key, value := range args.Env {
		spec.Env = append(spec.Env, key+"="+value)
	}

	session, err := Sessions.Open(ctx, lang, Launchers[lang], spec)
	if err != nil {
		return failResult(err), nil
	}

	out := sessionStatus(ctx, session)
	out["warnings"] = warnings
	return mcp.Text(out), nil
}

func sessionStatus(ctx context.Context, session *Session) map[string]any {
	info := session.Info()
	out := map[string]any{
		"session":     info.ID,
		"language":    info.Language,
		"mode":        info.Mode,
		"state":       info.State,
		"breakpoints": info.Breakpoints,
	}
	if cap := session.Capability(); cap.Backend != "" {
		out["capability"] = map[string]any{
			"backend":     cap.Backend,
			"supported":   cap.SupportedActions(),
			"unsupported": cap.UnsupportedActions(),
			"limits":      cap.Limits,
		}
	}

	snap, err := session.Snapshot()
	if err != nil {
		out["hint"] = "目标正在运行；调用 debug_run 的 pause 或设置断点后 run"
		return out
	}
	out["snapshot"] = snap.ID
	out["stop"] = stopInfo(snap)
	return out
}

// stopInfo 渲染停止位置等元信息。
//
// 这些信息不受返回体预算裁剪：失去它们调用方就失去了判断依据。
func stopInfo(snap *model.Snapshot) map[string]any {
	if snap == nil || len(snap.Frames) == 0 {
		return map[string]any{}
	}
	frame := snap.Frames[0]
	return map[string]any{
		"reason": snap.Reason,
		"thread": snap.Thread,
		"at": map[string]any{
			"file": platform.NormalizePath(frame.Where.File),
			"line": frame.Where.Line,
			"func": frame.Where.Func,
		},
		"frames": len(snap.Frames),
		"scopes": scopeNames(frame),
	}
}

func scopeNames(frame model.Frame) []string {
	names := make([]string, 0, len(frame.Scopes))
	for name := range frame.Scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// debugInfoChecker 校验目标产物的调试信息。
type debugInfoChecker func(args sessionArgs) ([]string, error)

func debugInfoCheckerFor(lang backend.Language) debugInfoChecker {
	switch lang {
	case backend.NodeJS:
		return checkNodeDebugInfo
	case backend.Python:
		return checkPythonDebugInfo
	case backend.Java, backend.Kotlin, backend.Scala, backend.Groovy:
		return checkJVMInfo
	case backend.C, backend.Cpp, backend.Rust:
		return checkNativeInfo
	}
	return nil
}

// checkNativeInfo 校验 native 目标的二进制调试信息。
func checkNativeInfo(args sessionArgs) ([]string, error) {
	if args.Cwd != "" {
		if info, err := os.Stat(args.Cwd); err != nil || !info.IsDir() {
			return nil, model.New(model.CodeInputInvalid, "工作目录不存在: %s", args.Cwd).
				WithFix("确认 cwd 参数")
		}
	}
	if args.Mode == "attach" || len(args.Command) == 0 {
		return nil, nil
	}
	return native.CheckDebugInfo(args.Command, args.Cwd)
}

// checkJVMInfo 校验 JVM 目标的字节码调试信息。
//
// 行号表缺失会阻止断点按行命中，因此作为 blocker；局部变量表缺失只影响读值，
// 作为警告返回。使用 -jar 或找不到 class 文件时跳过校验并说明原因。
func checkJVMInfo(args sessionArgs) ([]string, error) {
	if args.Mode == "attach" || len(args.Command) == 0 {
		return nil, nil
	}
	if args.Cwd != "" {
		if info, err := os.Stat(args.Cwd); err != nil || !info.IsDir() {
			return nil, model.New(model.CodeInputInvalid, "工作目录不存在: %s", args.Cwd).
				WithFix("确认 cwd 参数")
		}
	}
	return jvm.CheckDebugInfo(backend.Language(args.Language), args.Command, args.Cwd)
}

// checkPythonDebugInfo 校验 Python 目标的可调试性。
//
// 两类问题会直接影响结论可信度：-O/-OO 让 assert 与 __debug__ 分支消失，
// 实际执行的代码与源码不一致；入口脚本不存在则连断点都无法设置。
func checkPythonDebugInfo(args sessionArgs) ([]string, error) {
	var warnings []string
	if args.Cwd != "" {
		if info, err := os.Stat(args.Cwd); err != nil || !info.IsDir() {
			return nil, model.New(model.CodeInputInvalid, "工作目录不存在: %s", args.Cwd).
				WithFix("确认 cwd 参数")
		}
	}
	if args.Mode == "attach" || len(args.Command) == 0 {
		// attach 的目标不是本工具启动的，没有入口脚本可校验。
		return nil, nil
	}
	for _, arg := range args.Command[1:] {
		if arg == "-O" || arg == "-OO" {
			warnings = append(warnings, "以 "+arg+" 运行时 assert 与 __debug__ 分支被移除，"+
				"实际执行的代码与你阅读的源码不一致；建议去掉优化参数后重启")
			break
		}
	}

	entry := pythonEntryScript(args.Command)
	if entry == "" {
		return warnings, nil
	}
	base := args.Cwd
	if base == "" {
		base, _ = os.Getwd()
	}
	full := entry
	if !filepath.IsAbs(entry) {
		full = filepath.Join(base, entry)
	}
	if _, err := os.Stat(full); err != nil {
		return nil, model.New(model.CodeInputInvalid, "入口脚本不存在: %s", full).
			WithFix("确认 command 中的脚本路径，或先完成构建/安装")
	}
	return warnings, nil
}

// pythonEntryScript 取命令中的入口脚本路径；-m / -c 形式没有脚本路径。
func pythonEntryScript(command []string) string {
	for i := 1; i < len(command); i++ {
		arg := command[i]
		if arg == "-m" || arg == "-c" {
			return ""
		}
		if strings.HasPrefix(arg, "-") {
			if arg == "-X" || arg == "-W" {
				i++
			}
			continue
		}
		return arg
	}
	return ""
}

func runDebugInfoCheck(checker debugInfoChecker, args sessionArgs) ([]string, error) {
	mode := args.DebugInfoCheck
	if mode == "" {
		mode = "warn"
	}
	if mode == "off" || checker == nil {
		return nil, nil
	}
	warnings, err := checker(args)
	if err != nil {
		if mode == "block" {
			return nil, err
		}
		return nil, err
	}
	return warnings, nil
}

// checkNodeDebugInfo 校验 Node.js 目标的可调试性。
func checkNodeDebugInfo(args sessionArgs) ([]string, error) {
	var warnings []string
	if args.Cwd != "" {
		if info, err := os.Stat(args.Cwd); err != nil || !info.IsDir() {
			return nil, model.New(model.CodeInputInvalid, "工作目录不存在: %s", args.Cwd).
				WithFix("确认 cwd 参数")
		}
	}
	if len(args.Command) >= 2 {
		entry := args.Command[len(args.Command)-1]
		if strings.HasSuffix(entry, ".js") || strings.HasSuffix(entry, ".mjs") || strings.HasSuffix(entry, ".cjs") {
			base := args.Cwd
			if base == "" {
				base, _ = os.Getwd()
			}
			full := entry
			if !filepath.IsAbs(entry) {
				full = filepath.Join(base, entry)
			}
			if _, err := os.Stat(full); err != nil {
				return nil, model.New(model.CodeInputInvalid, "入口脚本不存在: %s", full).
					WithFix("确认 command 中的脚本路径，或先完成构建")
			}
			if strings.Contains(filepath.ToSlash(full), "/dist/") || strings.Contains(filepath.ToSlash(full), "/build/") {
				warnings = append(warnings, "入口位于构建产物目录，断点会落在编译后的行号；建议改用源码入口或开启 sourcemap")
			}
		}
	}
	return warnings, nil
}

// failResult 把错误渲染为工具失败结果，保留错误码与修复建议。
func failResult(err error) *mcp.Result {
	var mErr *model.Error
	payload := map[string]any{"message": err.Error()}
	if asModelError(err, &mErr) {
		payload["code"] = mErr.Code
		payload["message"] = mErr.Message
		if mErr.Fix != "" {
			payload["fix"] = mErr.Fix
		}
		if len(mErr.Context) > 0 {
			payload["context"] = mErr.Context
		}
	} else if code := model.CodeOf(err); code != "" {
		payload["code"] = code
	}
	data, _ := json.Marshal(payload)
	return &mcp.Result{Content: []mcp.Content{{Type: "text", Text: string(data)}}, IsError: true}
}
