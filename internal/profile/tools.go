package profile

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// Tool 是一个外部剖析器的调用计划。
type Tool struct {
	// Name 是展示名，如 py-spy。
	Name string
	// Command 是可执行文件名（走 PATH 查找）。
	Command string
	// Args 是已展开的参数，输出写到 Args 里给出的文件。
	Args []string
	// Format 是输出文件的格式，用于选择解析器。
	Format string
	// Install 是缺失时的安装指引。
	Install string
	// AsyncStacks 声明来源是否支持异步栈；不支持时报告里必须写明缺失。
	AsyncStacks bool
}

// Plans 返回该语言可直接产出可解析结果的剖析器候选，按优先级排列。
//
// 只列“一次调用就能得到可解析文件”的工具：像 perf 那样需要再做一次
// 折叠转换的来源不放进候选，而是通过 Guidance 给出完整命令链，避免
// 工具层假装自己已经能从 perf.data 里读出热点。
func Plans(lang string, pid, seconds int, out string) []Tool {
	sec := strconv.Itoa(seconds)
	out2 := out
	switch lang {
	case "python":
		return []Tool{{
			Name:        "py-spy",
			Command:     "py-spy",
			Args:        []string{"record", "--format", "raw", "--output", out2, "--pid", strconv.Itoa(pid), "--duration", sec},
			Format:      FormatFolded,
			Install:     "python -m pip install py-spy（需要目标进程权限；容器内需 --cap-add SYS_PTRACE）",
			AsyncStacks: false,
		}}
	case "java", "kotlin", "scala", "groovy":
		return []Tool{{
			Name:        "async-profiler",
			Command:     "asprof",
			Args:        []string{"-e", "cpu", "-d", sec, "-o", "collapsed", "-f", out2, strconv.Itoa(pid)},
			Format:      FormatFolded,
			Install:     "下载 async-profiler 解压后把 bin/asprof 加入 PATH；也可用 jcmd 采 JFR 后转换再 import",
			AsyncStacks: false,
		}}
	}
	return nil
}

// Guidance 在没有可直接调用的剖析器时给出替代路径。
//
// 返回空字符串表示该语言有可用候选。这里的说明必须可执行，
// 因为调用方接下来要么照着做，要么改用 debug_profile 的 sample 降级。
func Guidance(lang string) string {
	switch lang {
	case "nodejs":
		return "Node 没有可附着的独立采样器：用 `node --cpu-prof --cpu-prof-dir <目录> app.js` 启动，" +
			"跑完后用 debug_profile 的 import 动作导入生成的 .cpuprofile（format=v8）"
	case "c", "cpp", "rust":
		return "native 目标：Windows 上 perf 不可用，直接用 debug_profile 的 sample 做中断采样；" +
			"Linux 上用 `perf record -F 99 -g -p <pid> -- sleep <秒>` 采集，" +
			"再 `perf script | stackcollapse-perf.pl > out.folded` 折叠后用 import 导入（format=folded）"
	}
	return "当前语言没有可用的外部剖析器，改用 debug_profile 的 sample 做中断采样"
}

// Probe 检查剖析器是否就位；缺失时返回带安装指引的错误。
func Probe(tool Tool) (string, error) {
	path, err := exec.LookPath(tool.Command)
	if err != nil {
		return "", model.New(model.CodeEnvAdapterMissing, "未找到剖析器 %s", tool.Name).
			WithFix(tool.Install).
			WithContext("profiler", tool.Name)
	}
	return path, nil
}

// Result 是一次外部剖析的原始结果。
type Result struct {
	// DurationMs 是实际采集时长。
	DurationMs int
	// Partial 表示被超时截断，结果是部分数据。
	Partial bool
	// Stderr 是剖析器的错误输出，失败时用于诊断。
	Stderr string
}

// Run 启动外部剖析器并等待其退出；超时会终止进程并标记 partial。
//
// 不把超时当失败：剖析器已经写出的部分数据仍然可用，丢掉它才是浪费。
func Run(ctx context.Context, tool Tool, timeout time.Duration) (Result, error) {
	cmd := exec.Command(tool.Command, tool.Args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, model.New(model.CodeEnvAdapterMissing, "启动剖析器 %s 失败: %v", tool.Name, err).
			WithFix(tool.Install)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return Result{
			DurationMs: int(time.Since(start) / time.Millisecond),
			Stderr:     stderr.String(),
		}, nil
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		return Result{
			DurationMs: int(time.Since(start) / time.Millisecond),
			Partial:    true,
			Stderr:     stderr.String(),
		}, nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return Result{}, ctx.Err()
	}
}
