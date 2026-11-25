// Package native 是 C/C++/Rust 的调试后端：以 lldb-dap 承载 DAP 协议。
package native

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// backendName 是后端名称。
const backendName = "lldb-dap"

// versionPattern 从 `--version` 输出里取语义化版本号。
var versionPattern = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

// Adapter 是 lldb-dap 可执行文件的信息。
type Adapter struct {
	Path    string
	Version string
}

// Discover 按优先级查找 lldb-dap。
//
// 顺序：环境变量覆盖 → PATH → 常见安装位置。LLVM 官方安装与 MSYS2 UCRT64 工具链
// （C/C++ 夹具的既定环境）都会把 lldb-dap 放在各自 bin 目录下。
//
// 找到文件后还要确认它真能跑：残缺安装（如缺 liblldb.dll）会让适配器一启动就
// 退出，那种情况下报“没装”比让调用方对着 initialize 超时猜原因更有用。
func Discover() (Adapter, error) {
	for _, path := range candidates() {
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			continue
		}
		probe := probeAdapter(path)
		if !probe.ok {
			continue
		}
		return Adapter{Path: path, Version: probe.version}, nil
	}
	return Adapter{}, model.New(model.CodeEnvAdapterMissing, "未找到 lldb 调试适配器 lldb-dap").
		WithFix("安装 LLVM（含 lldb-dap）或 MSYS2 的 mingw-w64-ucrt-x86_64-lldb；"+
			"MSYS2 装在非默认目录时用环境变量 MSYS2_ROOT 指向其根目录，"+
			"或用 LANGDEBUG_LLDB_DAP 指定 lldb-dap 的完整路径").
		WithContext("language", "c/cpp/rust").
		WithContext("backend", backendName)
}

// candidates 返回按优先级排列的候选路径。
func candidates() []string {
	if override := os.Getenv("LANGDEBUG_LLDB_DAP"); override != "" {
		return []string{override}
	}
	out := make([]string, 0, 8)
	for _, name := range []string{"lldb-dap", "lldb-vscode"} {
		if path, err := exec.LookPath(name); err == nil {
			out = append(out, path)
		}
	}
	// 官方 LLVM 安装（含 lldb-dap）默认落在 <ProgramFiles>\LLVM\bin。
	dirs := append(toolchainDirs(), msysBinDirs()...)
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)"} {
		if base := os.Getenv(env); base != "" {
			dirs = append(dirs, filepath.Join(base, "LLVM", "bin"))
		}
	}
	for _, dir := range dirs {
		for _, name := range adapterNames() {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// adapterNames 返回适配器可执行文件的文件名（保留 lldb-vscode 旧名兼容）。
func adapterNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"lldb-dap.exe", "lldb-vscode.exe"}
	}
	return []string{"lldb-dap", "lldb-vscode"}
}

// toolchainDirs 从已安装的编译器位置推导适配器目录：MSYS2 把 lldb 与 gcc 放在同一个 bin 下。
func toolchainDirs() []string {
	out := make([]string, 0, 4)
	for _, name := range []string{"gcc", "g++", "clang", "clang++"} {
		if path, err := exec.LookPath(name); err == nil {
			out = append(out, filepath.Dir(path))
		}
	}
	return out
}

// msysBinDirs 返回 MSYS2 的 bin 目录：装好 lldb 后适配器就在工具链目录下。
//
// 优先用 MSYS2 shell 导出的变量（MINGW_PREFIX 指向当前环境），其次是最常见的
// 默认安装位置；其他安装位置可让 MSYS2 的 bin 进入 PATH，由 LookPath 命中。
func msysBinDirs() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	dirs := make([]string, 0, 8)
	roots := []string{`C:\msys64`}
	if root := os.Getenv("MSYS2_ROOT"); root != "" {
		roots = append([]string{root}, roots...)
	}
	for _, root := range roots {
		for _, sub := range []string{"ucrt64", "mingw64", "clang64"} {
			dirs = append(dirs, filepath.Join(root, sub, "bin"))
		}
	}
	if prefix := os.Getenv("MINGW_PREFIX"); prefix != "" {
		dirs = append(dirs, filepath.Join(prefix, "bin"))
	}
	return dirs
}

// adapterProbe 是对某个候选适配器可执行性的探测结果。
type adapterProbe struct {
	version string
	// ok 表示适配器能正常启动并返回退出码 0。
	ok bool
}

// 探测要起子进程，缓存起来：C/C++/Rust 三个探测器与 launch 都会问同一个适配器。
var (
	probeMu    sync.Mutex
	probeCache = map[string]adapterProbe{}
)

// probeAdapter 取（并缓存）适配器探测结果。
func probeAdapter(path string) adapterProbe {
	probeMu.Lock()
	cached, ok := probeCache[path]
	probeMu.Unlock()
	if ok {
		return cached
	}
	result := runProbe(path)
	probeMu.Lock()
	probeCache[path] = result
	probeMu.Unlock()
	return result
}

// runProbe 执行一次 --version，确认适配器能跑并解析版本号。
func runProbe(path string) adapterProbe {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return adapterProbe{}
	}
	return adapterProbe{version: parseVersion(string(out)), ok: true}
}

// parseVersion 从版本输出中提取版本号。
func parseVersion(text string) string {
	if m := versionPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// compilerHint 返回语言对应的编译命令提示，用于缺符号时的修复建议。
func compilerHint(lang string) string {
	switch lang {
	case "c":
		return "gcc -g -O0 -o <输出> <源码>"
	case "cpp":
		return "g++ -g -O0 -o <输出> <源码>"
	case "rust":
		return "rustc -g -O0 -o <输出> <源码>"
	case "cangjie":
		return "cjc -g -o <输出> <源码>"
	}
	return "以 -g -O0 重新编译目标"
}

// describe 生成探测结果的备注。
func describe(lang string) []string {
	return []string{
		fmt.Sprintf("launch 模式由 lldb-dap 托管目标，编译时需带 %s", compilerHint(lang)),
		"attach 尚未实现：只能用 launch 模式由 lldb-dap 托管目标进程",
	}
}
