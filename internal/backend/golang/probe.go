package golang

import (
	"os/exec"
	"regexp"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
)

// versionPattern 从 `dlv version` 输出里取版本号。
var versionPattern = regexp.MustCompile(`Version:\s+(\d+\.\d+\.\d+)`)

// Prober 探测 Go 后端（dlv dap）的可用性。
type Prober struct{}

// Language 实现 backend.Prober。
func (Prober) Language() backend.Language { return backend.Go }

// Backend 实现 backend.Prober。
func (Prober) Backend() string { return backendName }

// Probe 实现 backend.Prober。
//
// 缺少 dlv 时明确给出安装命令；Go 工具链缺失同样会暴露出来（dlv 无法编译
// debug 模式的包），这比让会话在建连接时超时更容易定位。
func (Prober) Probe() backend.Status {
	st := backend.Status{Language: backend.Go, Backend: backendName}
	if _, err := resolveDLV(); err != nil {
		st.Missing = []string{"dlv"}
		st.InstallHint = "go install github.com/go-delve/delve/cmd/dlv@latest；" +
			"Windows 上确保 %GOPATH%\\bin 在 PATH 中，或用 LANGDEBUG_DLV 指定 dlv 路径"
		return st
	}
	st.Available = true
	st.Version = dlvVersion()
	st.Notes = append(st.Notes, describe()...)
	if _, err := exec.LookPath("go"); err != nil {
		st.Notes = append(st.Notes,
			"未找到 go 工具链：debug 模式（go run）无法编译，请改用 go build 后的二进制")
	}
	return st
}

// dlvVersion 读取 dlv 版本；读不到时留空，不编造。
func dlvVersion() string {
	path, err := resolveDLV()
	if err != nil {
		return ""
	}
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return ""
	}
	if match := versionPattern.FindStringSubmatch(string(out)); len(match) > 1 {
		return match[1]
	}
	return ""
}

// describe 返回 Go 调试的固有边界，供 debug_env 直接展示给模型。
func describe() []string {
	return []string{
		"后端：dlv dap（Delve），通过 DAP 协议对接",
		"只显示当前 goroutine 的栈；切换 goroutine 需要 dlv CLI",
		"优化构建下变量可能为 optimized out，调试用 -gcflags=\"all=-N -l\"",
		"interface 值与泛型实例需要展开，nil 接口显示为 nil",
		"采集点与栈采样可用（均由 DAP 断点驱动）",
	}
}

// 确保实现契约。
var _ backend.Prober = Prober{}
