// Package python 实现 Python 调试后端。
//
// 协议层复用 internal/backend/dap；本包只负责 Python 侧的差异：找解释器、
// 探测 debugpy、把用户给的启动命令改写成 debugpy 的 launch/attach 参数。
package python

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// backendName 是 Python 后端的适配器名。
const backendName = "debugpy"

// pythonEnv 是显式指定解释器的环境变量，优先于 PATH 探测。
const pythonEnv = "LANGDEBUG_PYTHON"

// probeTimeout 是单次探测子进程的超时。
const probeTimeout = 15 * time.Second

// Interpreter 描述一个 Python 解释器环境。
type Interpreter struct {
	// Path 是解释器可执行文件路径。
	Path string
	// Version 是 Python 版本，如 3.12.4。
	Version string
	// Debugpy 是 debugpy 版本；为空表示该环境未安装 debugpy。
	Debugpy string
}

// Prober 探测 Python 后端可用性。
type Prober struct{}

// Language 实现 backend.Prober。
func (Prober) Language() backend.Language { return backend.Python }

// Backend 实现 backend.Prober。
func (Prober) Backend() string { return backendName }

// Probe 实现 backend.Prober。
func (Prober) Probe() backend.Status {
	st := backend.Status{Language: backend.Python, Backend: backendName}
	interp, err := Discover()
	if err != nil {
		st.Missing = []string{missingItem(err)}
		st.InstallHint = fixOf(err)
		if interp.Path != "" {
			st.Notes = []string{"已找到解释器: " + interp.Path}
		}
		return st
	}
	st.Available = true
	st.Version = interp.Debugpy
	st.Notes = []string{
		"解释器: " + interp.Path,
		"Python " + interp.Version,
		"适配器: " + interp.Path + " -m debugpy.adapter（DAP over stdio）",
	}
	return st
}

// Discover 找出第一个已安装 debugpy 的解释器。
//
// 返回值即使 error 非空也可能带有已找到的解释器，便于安装提示指名道姓。
func Discover() (Interpreter, error) {
	candidates := interpreterCandidates()
	var found Interpreter
	for _, path := range candidates {
		interp, err := probeInterpreter(path)
		if err != nil {
			continue
		}
		if interp.Debugpy != "" {
			return interp, nil
		}
		if found.Path == "" {
			found = interp
		}
	}
	if found.Path == "" {
		return Interpreter{}, model.New(model.CodeEnvAdapterMissing, "未找到 Python 解释器").
			WithFix("安装 Python 3 并确保 python 或 python3 在 PATH 中")
	}
	return found, model.New(model.CodeEnvAdapterMissing, "解释器 %s 未安装 debugpy", found.Path).
		WithFix(fmt.Sprintf("%s -m pip install debugpy", found.Path))
}

// DiscoverVenv 在指定目录的虚拟环境（.venv / venv）中查找已装 debugpy 的解释器。
//
// 与 Discover 分开提供：服务进程的工作目录不一定是被调试项目，显式指定目录
// 才能可靠地找到项目自带的虚拟环境。
func DiscoverVenv(dir string) (Interpreter, bool) {
	for _, name := range []string{".venv", "venv"} {
		python := venvPython(filepath.Join(dir, name))
		if python == "" {
			continue
		}
		interp, err := probeInterpreter(python)
		if err == nil && interp.Debugpy != "" {
			return interp, true
		}
	}
	return Interpreter{}, false
}

// interpreterCandidates 返回候选解释器：环境变量优先，其次 PATH 上的常见名，
// 最后是当前目录下的虚拟环境（项目依赖通常只装在虚拟环境里）。
func interpreterCandidates() []string {
	if explicit := strings.TrimSpace(os.Getenv(pythonEnv)); explicit != "" {
		return []string{explicit}
	}
	out := make([]string, 0, 6)
	seen := make(map[string]bool, 6)
	for _, name := range []string{"python", "python3", "py"} {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if cwd, err := os.Getwd(); err == nil {
		for _, name := range []string{".venv", "venv"} {
			if python := venvPython(filepath.Join(cwd, name)); python != "" && !seen[python] {
				seen[python] = true
				out = append(out, python)
			}
		}
	}
	return out
}

// venvPython 返回虚拟环境中的解释器路径。
func venvPython(dir string) string {
	candidates := []string{
		filepath.Join(dir, "Scripts", "python.exe"),
		filepath.Join(dir, "bin", "python3"),
		filepath.Join(dir, "bin", "python"),
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

// probeInterpreter 探测解释器版本与 debugpy 版本。
func probeInterpreter(path string) (Interpreter, error) {
	version, err := runPython(path, "import sys; print(sys.version.split()[0])")
	if err != nil {
		return Interpreter{}, err
	}
	interp := Interpreter{Path: path, Version: version}
	if debugpy, derr := runPython(path, "import debugpy; print(debugpy.__version__)"); derr == nil {
		interp.Debugpy = debugpy
	}
	return interp, nil
}

// runPython 执行一段 Python 代码并返回去掉首尾空白的输出。
func runPython(python, code string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, python, "-c", code).Output()
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", errors.New("解释器输出为空")
	}
	return strings.Split(text, "\n")[0], nil
}

// isPythonCommand 报告某个命令行首项是否为 Python 解释器。
func isPythonCommand(arg string) bool {
	base := strings.ToLower(filepath.Base(arg))
	base = strings.TrimSuffix(base, ".exe")
	switch {
	case base == "py", base == "python", base == "python3":
		return true
	case strings.HasPrefix(base, "python3."), strings.HasPrefix(base, "python2."):
		return true
	case strings.HasPrefix(base, "pypy"):
		return true
	}
	return false
}

// missingItem 从错误中提炼缺失项名称，供 debug_env 展示。
func missingItem(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "未安装 debugpy") {
		return "debugpy"
	}
	if strings.Contains(msg, "解释器") {
		return "python"
	}
	return backendName
}

// fixOf 取出统一错误里的修复建议。
func fixOf(err error) string {
	var coded *model.Error
	if errors.As(err, &coded) {
		return coded.Fix
	}
	return ""
}
