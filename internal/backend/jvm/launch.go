package jvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/textbuf"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// defaultOpenTimeout 是建立会话的默认超时。
const defaultOpenTimeout = 25 * time.Second

// entryWait 是等待入口事件的时间；超时则合成一个“停在入口”的停止点。
const entryWait = 2 * time.Second

// Launcher 创建 JVM 调试后端。
type Launcher struct{}

// Open 实现 backend.Launcher。
func (Launcher) Open(ctx context.Context, spec backend.LaunchSpec) (backend.Debugger, error) {
	return Open(ctx, spec.Language, spec)
}

// Open 建立 JVM 调试会话。
func Open(ctx context.Context, lang backend.Language, spec backend.LaunchSpec) (backend.Debugger, error) {
	mode := spec.Mode
	if mode == "" {
		mode = "launch"
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultOpenTimeout
	}
	openCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s := &Session{
		lang:    lang,
		mode:    mode,
		out:     textbuf.New(500),
		cache:   newCache(),
		threads: make(map[uint64]string),
		stopCh:  make(chan *backend.Stop, 8),
		doneCh:  make(chan struct{}),
	}
	if spec.Cwd != "" {
		s.sourceDirs = append(s.sourceDirs, spec.Cwd)
	}

	switch mode {
	case "launch":
		if err := s.startProcess(openCtx, spec, timeout); err != nil {
			_ = s.Close()
			return nil, err
		}
	case "attach":
		if spec.Port <= 0 {
			return nil, attachFailure(model.CodeInputInvalid, spec, 0, "attach 模式必须提供调试端口")
		}
		w, err := dial(openCtx, fmt.Sprintf("127.0.0.1:%d", spec.Port))
		if err != nil {
			return nil, attachFailure(model.CodeEnvAdapterMissing, spec, spec.Port,
				fmt.Sprintf("无法连接到调试端口 %d，目标可能未开启 JDWP 代理", spec.Port)).Wrap(err)
		}
		s.wire = w
		go s.eventLoop()
		s.probeAttachState(openCtx)
	default:
		return nil, model.New(model.CodeInputInvalid, "不支持的会话模式: %s", mode).
			WithFix("mode 只能是 launch 或 attach")
	}
	return s, nil
}

// attachFailure 报告 attach 前提不满足，并尽可能给出可直接复制的重启命令。
//
// 只报告“失败”会让调用方卡住：JVM 的 JDWP 代理必须在启动时注入，无法事后开启，
// 因此这里把注入代理后的完整命令行一并返回，照着执行再 attach 即可。
func attachFailure(code model.Code, spec backend.LaunchSpec, port int, message string) *model.Error {
	hint, ok := RestartHintFor(spec, port)
	if !ok {
		return model.New(code, "%s", message).
			WithFix("目标需以 -agentlib:jdwp=transport=dt_socket,server=y,suspend=y,address=<端口> 启动；" +
				"若能在本工具内启动，改用 mode=launch 由工具注入代理参数")
	}
	err := model.New(code, "%s", message).
		WithContext("restartCommand", hint.Display).
		WithContext("port", strconv.Itoa(hint.Port))
	return err.WithFix("在目标环境执行该命令重启，再按建议端口 attach: " + hint.Display +
		"（随后用 port=" + strconv.Itoa(hint.Port) + "）")
}

// startProcess 启动目标并连上它的调试端口。
func (s *Session) startProcess(ctx context.Context, spec backend.LaunchSpec, timeout time.Duration) error {
	cmdInfo, err := parseJavaCommand(spec.Command)
	if err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}

	argv := cmdInfo.argv(port)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Cwd
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return model.New(model.CodeInternalProtocol, "无法读取目标标准输出").Wrap(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return model.New(model.CodeInternalProtocol, "无法读取目标标准错误").Wrap(err)
	}
	proc, err := platform.StartProc(cmd)
	if err != nil {
		return model.New(model.CodeEnvDebugInfo, "目标启动失败").Wrap(err)
	}
	s.proc = proc
	go drainTo(s.out, stdout)
	go drainTo(s.out, stderr)
	go s.watchExit(proc)

	wire, dialErr := s.dialWithRetry(ctx, port)
	if dialErr != nil {
		return dialErr
	}
	s.wire = wire
	go s.eventLoop()
	return s.waitEntry(ctx)
}

// dialWithRetry 反复尝试连接调试端口，直到超时或目标退出。
func (s *Session) dialWithRetry(ctx context.Context, port int) (*wire, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var lastErr error
	for {
		w, err := dial(ctx, addr)
		if err == nil {
			return w, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, s.exitError(lastErr, addr)
		case <-s.doneCh:
			return nil, s.exitError(lastErr, addr)
		case <-time.After(120 * time.Millisecond):
		}
	}
}

// exitError 在目标提前退出时组装带输出的错误。
func (s *Session) exitError(cause error, addr string) error {
	tail := strings.TrimSpace(s.out.Tail(20))
	detail := fmt.Sprintf("目标在调试端口 %s 就绪前退出", addr)
	if tail != "" {
		detail += "；目标输出:\n" + tail
	}
	err := model.New(model.CodeEnvDebugInfo, "%s", detail)
	if cause != nil {
		err = err.Wrap(cause)
	}
	return err.WithFix("检查主类名、类路径与启动参数；目标自身报错见上方输出")
}

// watchExit 等待目标进程结束并记录退出码。
func (s *Session) watchExit(proc *platform.Proc) {
	err := proc.Cmd().Wait()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	s.mu.Lock()
	if s.mode == "launch" && code != 0 {
		s.exitCode = code
	}
	s.mu.Unlock()
	s.onTerminated(code)
}

// waitEntry 等待入口事件，拿不到时合成一个入口停止点。
//
// 目标以 suspend=y 启动，因此在第一次 resume 之前它不会执行用户代码；即使
// 适配器没有发 VMStart 事件，也可以安全地把当前状态当作“停在入口”。
func (s *Session) waitEntry(ctx context.Context) error {
	// 个别虚拟机不支持 VMStart 请求：注册失败不视为失败，退回合成入口停止。
	_, _ = s.wire.setEvent(ctx, evVMStart, suspendAll, nil)
	if stop := s.waitAttached(ctx, entryWait); stop != nil {
		return nil
	}
	thread := s.pickPauseThread(ctx)
	stop := s.buildStop(ctx, thread, "entry", 0)
	s.publishStop(stop)
	return nil
}

// probeAttachState 判断 attach 进来时目标是否已经处于挂起状态。
func (s *Session) probeAttachState(ctx context.Context) {
	threads, err := s.wire.allThreads(ctx)
	if err != nil {
		s.mu.Lock()
		s.running = true
		s.mu.Unlock()
		return
	}
	mainThread := uint64(0)
	for _, t := range threads {
		if s.threadNameOf(ctx, t) == "main" {
			mainThread = t
			break
		}
	}
	if mainThread != 0 {
		if suspended, serr := s.wire.threadSuspended(ctx, mainThread); serr == nil && suspended {
			stop := s.buildStop(ctx, mainThread, "pause", 0)
			s.publishStop(stop)
			return
		}
	}
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
}

// drainTo 把管道内容按行写入缓冲。
func drainTo(buf *textbuf.Buffer, pipe interface{ Read([]byte) (int, error) }) {
	b := make([]byte, 4096)
	for {
		n, err := pipe.Read(b)
		if n > 0 {
			buf.AppendChunk(string(b[:n]))
		}
		if err != nil {
			return
		}
	}
}
