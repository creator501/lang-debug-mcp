package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/profile"
)

// 外部剖析的默认时长与上限。
const (
	defaultExternalMs = 3000
	maxExternalMs     = 30000
	// stopGrace 是 stop 等待剖析器退出的额外宽限，避免杀进程后无限等待。
	stopGrace = 5 * time.Second
)

// profileRun 是一次正在进行的外部剖析。
type profileRun struct {
	tool    profile.Tool
	out     string
	cmd     *exec.Cmd
	stderr  bytes.Buffer
	done    chan struct{}
	started time.Time
	timeout time.Duration

	mu      sync.Mutex
	partial bool
}

// markPartial 标记本次采集被超时截断。
func (r *profileRun) markPartial() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = true
}

// isPartial 报告是否被截断。
func (r *profileRun) isPartial() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.partial
}

// elapsed 返回已采集时长。
func (r *profileRun) elapsed() time.Duration { return time.Since(r.started) }

// kill 终止剖析器进程。
func (r *profileRun) kill() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
}

// setProfilerRun 记录进行中的外部剖析。
func (s *Session) setProfilerRun(run *profileRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runningProfile = run
}

// takeProfilerRun 取出并清空进行中的外部剖析。
func (s *Session) takeProfilerRun() *profileRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runningProfile
	s.runningProfile = nil
	return run
}

// peekProfilerRun 查看是否有进行中的外部剖析。
func (s *Session) peekProfilerRun() *profileRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningProfile
}

// startExternal 启动外部剖析器；目标处于停止态或剖析器缺失时直接报错。
func startExternal(_ context.Context, session *Session, args profileArgs) (map[string]any, error) {
	if session.peekProfilerRun() != nil {
		return nil, model.New(model.CodeInputInvalid, "已有剖析在进行中").
			WithFix("先调用 debug_profile 的 stop 动作结束当前剖析")
	}
	if args.Pid <= 0 {
		return nil, model.New(model.CodeInputInvalid, "外部剖析需要目标进程号 pid").
			WithFix("用系统工具查出被调试进程号后重试；调试端口号不是 pid")
	}
	if session.State() == model.StateStopped {
		return nil, model.New(model.CodeStateNotStopped, "目标处于停止态，无法开始剖析").
			WithFix("先用 debug_run 的 continue 让目标运行起来")
	}
	lang := string(session.Language)
	millis := args.DurationMs
	if millis <= 0 {
		millis = defaultExternalMs
	}
	if millis > maxExternalMs {
		millis = maxExternalMs
	}
	seconds := millis / 1000
	if seconds < 1 {
		seconds = 1
	}
	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Duration(millis)*time.Millisecond + stopGrace
	}
	out := filepath.Join(os.TempDir(), fmt.Sprintf("lang-debug-prof-%d-%d.out", os.Getpid(), time.Now().UnixNano()))
	plans := profile.Plans(lang, args.Pid, seconds, out)
	if len(plans) == 0 {
		return nil, model.New(model.CodeCapUnsupported, "%s 没有可直接调用的外部剖析器", lang).
			WithFix(profile.Guidance(lang))
	}
	tool := plans[0]
	if _, err := profile.Probe(tool); err != nil {
		return nil, model.New(model.CodeOf(err), "未找到剖析器 %s", tool.Name).
			WithFix(tool.Install+"；替代方案："+profile.Guidance(lang)).
			WithContext("profiler", tool.Name)
	}
	run := &profileRun{
		tool:    tool,
		out:     out,
		cmd:     exec.Command(tool.Command, tool.Args...),
		done:    make(chan struct{}),
		started: time.Now(),
		timeout: timeout,
	}
	run.cmd.Stderr = &run.stderr
	if err := run.cmd.Start(); err != nil {
		return nil, model.New(model.CodeEnvAdapterMissing, "启动剖析器 %s 失败: %v", tool.Name, err).
			WithFix(tool.Install)
	}
	go func() {
		_ = run.cmd.Wait()
		close(run.done)
	}()
	// 看门狗：到点仍未退出就杀掉并标记部分结果，避免调用方永远等不到 stop。
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-run.done:
		case <-timer.C:
			run.markPartial()
			run.kill()
		}
	}()
	session.setProfilerRun(run)
	return map[string]any{
		"profiling":   true,
		"tool":        tool.Name,
		"format":      tool.Format,
		"pid":         args.Pid,
		"output":      out,
		"durationMs":  millis,
		"timeoutMs":   int(timeout / time.Millisecond),
		"asyncStacks": tool.AsyncStacks,
		"hint":        "采集进行中；调用 stop 结束并取回统一热点报告",
	}, nil
}

// stopExternal 结束外部剖析并返回归一化报告。
func stopExternal(ctx context.Context, session *Session, args profileArgs) (*profile.Report, error) {
	run := session.takeProfilerRun()
	if run == nil {
		return nil, model.New(model.CodeInputInvalid, "没有进行中的外部剖析").
			WithFix("先调用 debug_profile 的 start 动作，再 stop")
	}
	select {
	case <-run.done:
	case <-ctx.Done():
		run.kill()
		<-run.done
		run.markPartial()
	case <-time.After(run.timeout + stopGrace):
		run.markPartial()
		run.kill()
		<-run.done
	}
	data, err := os.ReadFile(run.out)
	if err != nil {
		return nil, model.New(model.CodeInternalProtocol, "剖析器 %s 没有产出结果文件: %v", run.tool.Name, err).
			WithContext("stderr", run.stderr.String()).
			WithFix("确认剖析器有权限附着目标进程；必要时改用 debug_profile 的 sample 做中断采样")
	}
	samples, err := profile.Parse(run.tool.Format, data)
	if err != nil {
		return nil, model.New(model.CodeInternalProtocol, "解析 %s 输出失败: %v", run.tool.Name, err).
			WithContext("stderr", run.stderr.String())
	}
	return profile.Build(profile.Source{
		Tool:       run.tool.Name,
		Format:     run.tool.Format,
		Kind:       "external",
		DurationMs: int(run.elapsed() / time.Millisecond),
		Partial:    run.isPartial(),
	}, samples, profile.Options{Focus: args.Focus}), nil
}

// statusExternal 报告进行中的外部剖析状态。
func statusExternal(session *Session) map[string]any {
	run := session.peekProfilerRun()
	if run == nil {
		return map[string]any{"profiling": false}
	}
	done := false
	select {
	case <-run.done:
		done = true
	default:
	}
	return map[string]any{
		"profiling": !done,
		"tool":      run.tool.Name,
		"format":    run.tool.Format,
		"output":    run.out,
		"elapsedMs": int(run.elapsed() / time.Millisecond),
		"timeoutMs": int(run.timeout / time.Millisecond),
		"partial":   run.isPartial(),
		"finished":  done,
	}
}

// importProfile 解析已有的剖析输出文件；不需要会话。
func importProfile(args profileArgs) (*profile.Report, error) {
	if args.Path == "" {
		return nil, model.New(model.CodeInputInvalid, "import 需要 path").
			WithFix(`示例: {"action":"import","format":"folded","path":"profile.folded"}`)
	}
	format := args.Format
	if format == "" {
		format = profile.FormatFolded
	}
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return nil, model.New(model.CodeInputInvalid, "读取 %s 失败: %v", args.Path, err).
			WithFix("确认路径存在且可读；折叠栈用 format=folded，.cpuprofile 用 format=v8")
	}
	samples, err := profile.Parse(format, data)
	if err != nil {
		return nil, model.New(model.CodeInputInvalid, "解析 %s 失败: %v", args.Path, err)
	}
	return profile.Build(profile.Source{
		Tool:   importToolName(format),
		Format: format,
		Kind:   "import",
	}, samples, profile.Options{Focus: args.Focus}), nil
}

// importToolName 给离线导入的来源一个可读名字。
func importToolName(format string) string {
	switch format {
	case profile.FormatV8:
		return "V8 CPU profile"
	case profile.FormatFolded:
		return "folded stacks"
	}
	return format
}

// handleExternal 处理 start / stop / status 三个动作。
func handleExternal(ctx context.Context, session *Session, args profileArgs) (*mcp.Result, error) {
	switch args.Action {
	case "start":
		out, err := startExternal(ctx, session, args)
		if err != nil {
			return failResult(err), nil
		}
		return mcp.Text(out), nil
	case "stop":
		report, err := stopExternal(ctx, session, args)
		if err != nil {
			return failResult(err), nil
		}
		pageReport(report, args)
		return mcp.Text(report), nil
	case "status":
		return mcp.Text(statusExternal(session)), nil
	}
	return failResult(model.New(model.CodeInputInvalid, "不支持的动作: %s", args.Action)), nil
}

// pageReport 按 offset/top 截断三种视图；总量字段保持不变，便于继续翻页。
func pageReport(report *profile.Report, args profileArgs) {
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	top := args.Top
	if top <= 0 {
		top = defaultTopEntries
	}
	report.Offset = offset
	report.Hot = pageSlice(report.Hot, offset, top)
	report.Cumulative = pageSlice(report.Cumulative, offset, top)
	report.Callers = pageSlice(report.Callers, offset, top)
}
