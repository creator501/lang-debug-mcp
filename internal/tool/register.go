// Package tool 实现暴露给模型的全部 MCP 工具。
//
// 工具面约束见 design.md 决定 D17：按动词分组、动作判别、总数不超过 12。
package tool

import (
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/cangjie"
	"github.com/langdebug/lang-debug-mcp/internal/backend/golang"
	"github.com/langdebug/lang-debug-mcp/internal/backend/jvm"
	"github.com/langdebug/lang-debug-mcp/internal/backend/native"
	"github.com/langdebug/lang-debug-mcp/internal/backend/nodejs"
	"github.com/langdebug/lang-debug-mcp/internal/backend/python"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
)

// Registry 是本进程内的后端探测器注册表。
var Registry = backend.NewRegistry()

// 各后端承载的语言族成员：注册探测器与启动器时按族展开。
var (
	jvmLanguages    = []backend.Language{backend.Java, backend.Kotlin, backend.Scala, backend.Groovy}
	nativeLanguages = []backend.Language{backend.C, backend.Cpp, backend.Rust}
)

// ServerVersion 由 main 注入，用于在工具结果中标注版本。
var ServerVersion = "0.0.0"

var reaperOnce sync.Once

// RegisterAll 注册后端探测器、启动器与全部工具。
func RegisterAll(srv *mcp.Server) error {
	if err := Registry.Register(nodejs.Prober{}); err != nil {
		return err
	}
	if err := Registry.Register(python.Prober{}); err != nil {
		return err
	}
	if err := Registry.Register(golang.Prober{}); err != nil {
		return err
	}
	if err := Registry.Register(cangjie.Prober{}); err != nil {
		return err
	}
	for _, lang := range jvmLanguages {
		if err := Registry.Register(jvm.Prober{Lang: lang}); err != nil {
			return err
		}
	}
	for _, lang := range nativeLanguages {
		if err := Registry.Register(native.Prober{Lang: lang}); err != nil {
			return err
		}
	}
	// 兜底：今后新增但尚无后端的语言，明确报告不可用，而不是建会话时超时。
	for _, lang := range backend.Supported {
		if lang == backend.NodeJS || lang == backend.Python ||
			lang == backend.Go || lang == backend.Cangjie {
			continue
		}
		if fam := lang.Family(); fam == "jvm" || fam == "native" {
			continue
		}
		if err := Registry.Register(notImplementedProber{lang: lang}); err != nil {
			return err
		}
	}

	Launchers[backend.NodeJS] = nodejs.Launcher{}
	Launchers[backend.Python] = python.Launcher{}
	Launchers[backend.Go] = golang.Launcher{}
	Launchers[backend.Cangjie] = cangjie.Launcher{}
	for _, lang := range jvmLanguages {
		Launchers[lang] = jvm.Launcher{}
	}
	for _, lang := range nativeLanguages {
		Launchers[lang] = native.Launcher{}
	}
	startReaper()

	registrars := []func(*mcp.Server) error{
		registerEnv,
		registerSession,
		registerBreakpoint,
		registerRun,
		registerInspect,
		registerEval,
		registerMutate,
		registerOutput,
		registerTracepoint,
		registerProfile,
	}
	for _, register := range registrars {
		if err := register(srv); err != nil {
			return err
		}
	}
	return nil
}

// languageEnum 构造「语言」参数的枚举值。
//
// 取值直接来自 backend.Supported：新增后端语言时 schema 不会漏掉（Go 与仓颉曾因
// 手写列表而缺失，导致严格校验的客户端拒绝传值）。
func languageEnum(desc string) map[string]any {
	values := make([]string, 0, len(backend.Supported))
	for _, lang := range backend.Supported {
		values = append(values, string(lang))
	}
	return mcp.Enum(desc, values...)
}

// notImplementedProber 对尚未实现后端的语言给出诚实的不可用状态。
//
// 宁可明确报告“未实现”，也不要让调用方对着连接超时猜原因。
type notImplementedProber struct {
	lang backend.Language
}

func (p notImplementedProber) Language() backend.Language { return p.lang }

func (p notImplementedProber) Backend() string { return "" }

func (p notImplementedProber) Probe() backend.Status {
	return backend.Status{
		Language:    p.lang,
		Available:   false,
		Missing:     []string{"backend"},
		InstallHint: "该语言的调试后端尚未实现；当前版本已支持 Node.js、Python、Go、JVM、仓颉与 C/C++/Rust",
	}
}

// startReaper 启动空闲会话回收。
func startReaper() {
	reaperOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for now := range ticker.C {
				Sessions.Reap(now)
			}
		}()
	})
}
