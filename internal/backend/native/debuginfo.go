package native

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

const (
	// infoTimeout 是单次调试信息分析命令的超时。
	infoTimeout = 10 * time.Second
	// producerScanLimit 是扫描优化选项时读取的最大字节数：producer 字符串在
	// .debug_info 里，超大二进制不必整份读完。
	producerScanLimit = 64 << 20
	// scanChunk 是流式扫描的块大小。
	scanChunk = 1 << 20
)

// CheckDebugInfo 校验 native 目标产物的调试信息。
//
// 行号表缺失会让按行断点直接失效、符号表缺失会让栈帧与变量全部不可用，两者都
// 阻断会话；高优化级别与源码新于二进制只降低结论可信度，作为警告返回。
// 找不到分析工具时跳过校验并说明原因，而不是假装通过。
func CheckDebugInfo(command []string, cwd string) ([]string, error) {
	exe, err := targetExecutable(command, cwd)
	if err != nil {
		return nil, err
	}
	sections, ok := debugSections(exe)
	if !ok {
		return []string{"未找到 objdump / readelf，跳过调试信息校验"}, nil
	}
	if !sections.line {
		return nil, model.New(model.CodeEnvDebugInfo, "目标不含行号表（.debug_line），断点无法按行命中: %s", exe).
			WithFix("用 -g 重新编译后再调试（调试期建议 -g -O0）")
	}
	// MinGW 的 crt 对象自带调试信息，段存在不代表用户的编译单元带 -g；
	// 只有行号表里出现用户源码（.c/.cpp/.rs）才算真的可调试。
	if covered, probed := hasUserLineInfo(exe); probed && !covered {
		return nil, model.New(model.CodeEnvDebugInfo,
			"目标只有运行库的调试信息，不含你的源码行号（未用 -g 编译），断点无法按行命中: %s", exe).
			WithFix("用 -g 重新编译后再调试（调试期建议 -g -O0）")
	}
	if present, probed := hasSymbols(exe); probed && !present {
		return nil, model.New(model.CodeEnvDebugInfo, "目标没有符号表（已 strip），栈帧与变量信息不可用: %s", exe).
			WithFix("去掉 strip 步骤或用 -g 重新编译，并另行保留一份带符号的构建用于调试")
	}

	var warnings []string
	if !sections.info {
		warnings = append(warnings, "目标缺少 .debug_info：能按行断点，但变量与类型信息不可用；建议用 -g 重新编译")
	}
	switch flag := optimizeFlag(exe); flag {
	case "", "-O0", "-Og":
	default:
		warnings = append(warnings, "目标以 "+flag+" 编译：局部变量可能被优化掉（显示 <optimized out>），"+
			"排除取值问题时用 -g -O0 重新编译复现")
	}
	if stale := staleSources(exe); len(stale) > 0 {
		warnings = append(warnings, "源文件比二进制新，断点行号可能与实际执行不一致: "+
			strings.Join(stale, "、")+"；重新构建后再调试")
	}
	return warnings, nil
}

// targetExecutable 解析 launch 命令里的目标可执行文件。
//
// native 的 command[0] 就是被调试程序本身（不经过解释器），相对路径按 cwd 补全。
func targetExecutable(command []string, cwd string) (string, error) {
	if len(command) == 0 {
		return "", model.New(model.CodeInputInvalid, "缺少启动命令").
			WithFix(`给出可执行文件，如 ["build/app", "--flag"]`)
	}
	exe := command[0]
	if !filepath.IsAbs(exe) {
		base := cwd
		if base == "" {
			base, _ = os.Getwd()
		}
		exe = filepath.Join(base, exe)
	}
	info, err := os.Stat(exe)
	if err != nil || info.IsDir() {
		return "", model.New(model.CodeInputInvalid, "目标可执行文件不存在: %s", exe).
			WithFix("先完成构建，或把 command 的第一个参数改成可执行文件的路径")
	}
	return exe, nil
}

// sections 是调试相关段的探测结果。
type sections struct {
	// info 表示存在 .debug_info（变量与类型信息）。
	info bool
	// line 表示存在 .debug_line（行号表）。
	line bool
}

// debugSections 列出目标包含的调试段；ok 为 false 表示没有可用的分析工具。
func debugSections(exe string) (sections, bool) {
	cases := []struct {
		tool string
		args []string
	}{
		{"objdump", []string{"-h", exe}},
		{"readelf", []string{"-S", "-W", exe}},
	}
	for _, c := range cases {
		path, err := exec.LookPath(c.tool)
		if err != nil {
			continue
		}
		out, err := runInfoTool(path, c.args)
		if err != nil {
			continue
		}
		return sections{
			info: strings.Contains(out, ".debug_info"),
			line: strings.Contains(out, ".debug_line"),
		}, true
	}
	return sections{}, false
}

// nameLinePattern 从 objdump --dwarf=info 的输出里提取 DW_AT_name 的值。
var nameLinePattern = regexp.MustCompile(`DW_AT_name\s*:\s*(\([^()]*\):\s*)?(\S+)\s*$`)

// userSourceExt 是用户源码的典型后缀：行号表里出现这些文件，说明业务代码带 -g。
var userSourceExt = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true,
	".c++": true, ".rs": true, ".m": true, ".mm": true,
}

// systemSourceMarkers 是运行库源码路径的特征片段：这些 CU 自带调试信息，
// 不能证明用户代码带 -g。
var systemSourceMarkers = []string{
	"libgcc", "crtstuff", "libstdc++", "gcc/config", "cygwin",
	"rustlib", "toolchains", ".rustup", "windows kits", "vc\\tools",
}

// hasUserLineInfo 报告调试信息是否覆盖用户源码；probed 为 false 表示无法判断。
//
// MinGW 链接进来的 crt/libgcc 对象（cygwin.S、libgcc2.c）自带调试信息，仅看段
// 存在会把“没带 -g”误判为可调试，因此要求出现不属于系统库的 .c/.cpp/.rs。
func hasUserLineInfo(exe string) (present, probed bool) {
	path, err := exec.LookPath("objdump")
	if err != nil {
		return false, false
	}
	present, err = streamMatch(path, []string{"--dwarf=info", exe}, func(line string) bool {
		return isUserCompileUnit(line)
	})
	if err != nil {
		return false, false
	}
	return present, true
}

// isUserCompileUnit 判断一行 DW_AT_name 是否指向用户源码文件。
func isUserCompileUnit(line string) bool {
	match := nameLinePattern.FindStringSubmatch(line)
	if match == nil {
		return false
	}
	name := match[2]
	if !userSourceExt[strings.ToLower(filepath.Ext(name))] {
		return false
	}
	lower := strings.ToLower(name)
	for _, marker := range systemSourceMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

// streamMatch 逐行扫描命令输出，命中后立即结束，避免解析整张行号表。
func streamMatch(path string, args []string, predicate func(string) bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	found := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		if predicate(scanner.Text()) {
			found = true
			break
		}
	}
	scanErr := scanner.Err()
	if found {
		// 提前命中：结束子进程，不必读完整张表。
		cancel()
	}
	waitErr := cmd.Wait()
	if found {
		return true, nil
	}
	if scanErr != nil {
		return false, scanErr
	}
	return false, waitErr
}

// hasSymbols 报告目标是否还带符号表；probed 为 false 表示 nm 不可用。
func hasSymbols(exe string) (present, probed bool) {
	path, err := exec.LookPath("nm")
	if err != nil {
		return false, false
	}
	out, err := runInfoTool(path, []string{exe})
	if err != nil {
		// nm 对 stripped 文件以 "no symbols" 失败，这本身就是结论。
		return false, true
	}
	return strings.TrimSpace(out) != "", true
}

// producerPattern 匹配 DWARF producer 字符串（编译命令摘要）。
var producerPattern = regexp.MustCompile(`GNU C\S* [^\x00]{0,200}|clang version [^\x00]{0,200}|rustc [^\x00]{0,200}`)

// optimizePattern 匹配 producer 里的优化选项。
var optimizePattern = regexp.MustCompile(`-O[0-3sg]?\b`)

// optimizeFlag 从 producer 字符串里取优化选项；取不到返回空。
//
// producer 是编译命令的明文摘要（如 "GNU C17 13.2.0 ... -g -O2"），不必解析
// 完整的 DWARF 结构就能拿到优化级别。
func optimizeFlag(exe string) string {
	file, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer file.Close()

	reader := bufio.NewReaderSize(io.LimitReader(file, producerScanLimit), scanChunk)
	tail := make([]byte, 0, 256)
	for {
		chunk := make([]byte, scanChunk)
		n, readErr := reader.Read(chunk)
		if n > 0 {
			buf := append(tail, chunk[:n]...)
			if match := producerPattern.Find(buf); match != nil {
				return firstOptimizeFlag(string(match))
			}
			if len(buf) > 256 {
				tail = append(tail[:0], buf[len(buf)-256:]...)
			} else {
				tail = append(tail[:0], buf...)
			}
		}
		if readErr != nil {
			return ""
		}
	}
}

// firstOptimizeFlag 返回 producer 中的第一个优化选项。
func firstOptimizeFlag(producer string) string {
	if flag := optimizePattern.FindString(producer); flag != "" {
		return flag
	}
	return ""
}

// sourceExtensions 是参与“源码是否比二进制新”判断的源文件后缀。
var sourceExtensions = []string{".c", ".cc", ".cpp", ".cxx", ".rs"}

// staleSources 找出比二进制更新的源文件。
//
// 用同目录同名源文件（app → app.c）与 Rust 的 main.rs 作启发式判据：读 DWARF
// 里的 CU 文件名更准，但代价远超收益，这里宁可漏报也不误报。
func staleSources(exe string) []string {
	info, err := os.Stat(exe)
	if err != nil {
		return nil
	}
	dir := filepath.Dir(exe)
	stem := strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))
	names := make([]string, 0, len(sourceExtensions)+1)
	for _, ext := range sourceExtensions {
		names = append(names, stem+ext)
	}
	names = append(names, "main.rs")

	var out []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		stale, err := os.Stat(path)
		if err != nil || stale.IsDir() {
			continue
		}
		if stale.ModTime().After(info.ModTime()) {
			out = append(out, path)
		}
	}
	return out
}

// runInfoTool 执行一个分析命令并返回标准输出。
func runInfoTool(path string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
