package nodejs

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
)

// Prober 探测 Node.js 后端可用性。
type Prober struct{}

// Language 实现 backend.Prober。
func (Prober) Language() backend.Language { return backend.NodeJS }

// Backend 实现 backend.Prober。
func (Prober) Backend() string { return "cdp" }

// domains 描述某版本 Node 上可用的 CDP 域。
type domains struct {
	Debugger bool
	Runtime  bool
	Profiler bool
}

// domainsFor 按 Node 主版本号推导可用域。
//
// 版本阈值来自 V8 Inspector 的域引入时间：8 起具备 Debugger/Runtime，
// 10 起具备 Profiler（CPU profile）。低于 8 不支持 CDP 调试。
func domainsFor(major int) domains {
	return domains{
		Debugger: major >= 8,
		Runtime:  major >= 8,
		Profiler: major >= 10,
	}
}

// Probe 实现 backend.Prober。
func (p Prober) Probe() backend.Status {
	st := backend.Status{Language: backend.NodeJS, Backend: "cdp"}

	path, err := exec.LookPath("node")
	if err != nil {
		st.Missing = []string{"node"}
		st.InstallHint = "安装 Node.js 并确保 node 在 PATH 中（https://nodejs.org）"
		return st
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		st.Missing = []string{"node --version 执行失败"}
		st.InstallHint = "确认 node 可正常执行: " + path
		return st
	}

	version := strings.TrimSpace(string(out))
	major := parseMajor(version)
	st.Version = version
	st.Available = domainsFor(major).Debugger
	if !st.Available {
		st.Missing = []string{"Node.js 版本过低"}
		st.InstallHint = "升级到 Node.js 8 及以上（推荐 18+）"
		return st
	}
	st.Notes = []string{
		"直连 V8 Inspector，无需额外安装调试适配器",
		"可执行文件: " + path,
	}
	if d := domainsFor(major); !d.Profiler {
		st.Notes = append(st.Notes, "该版本不支持 CPU 剖析（需 Node.js 10+）")
	}
	return st
}

// parseMajor 从 vX.Y.Z 中取出主版本号。
func parseMajor(version string) int {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	major := 0
	started := false
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			break
		}
		started = true
		major = major*10 + int(r-'0')
	}
	if !started {
		return 0
	}
	return major
}
