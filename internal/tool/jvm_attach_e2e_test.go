package tool

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/jvm"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// reservePort 申请一个当前空闲端口，用于手工以 JDWP 参数启动目标。
func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("释放端口失败: %v", err)
	}
	return port
}

// waitDebugPort 等待调试端口可连接。
func waitDebugPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("调试端口 %s 在 %s 内未就绪", addr, timeout)
}

// TestJVMAttachToRunningProcess 验证注入后的命令行可启动目标并被成功 attach。
//
// 这里手工拼出与工具注入一致的命令行（-agentlib:jdwp + 自动端口），再按端口
// 附着，覆盖 attach 路径：附着后应处于停止态，且能设置断点并命中。
func TestJVMAttachToRunningProcess(t *testing.T) {
	classDir := requireJVM(t)
	fixture := javaFixturePath(t)
	port := reservePort(t)

	args := jvm.AgentArgv("127.0.0.1:" + strconv.Itoa(port))
	cmd := exec.Command("java", append(args, "-cp", classDir, "App")...)
	cmd.Dir = filepath.Dir(fixture)
	proc, err := platform.StartProc(cmd)
	if err != nil {
		t.Fatalf("启动目标失败: %v", err)
	}
	defer proc.Close()
	waitDebugPort(t, port, 30*time.Second)

	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), backend.Java, jvm.Launcher{}, backend.LaunchSpec{
		Language: backend.Java,
		Mode:     "attach",
		Port:     port,
		Cwd:      filepath.Dir(fixture),
		Timeout:  40 * time.Second,
	})
	if err != nil {
		t.Fatalf("attach 失败: %v", err)
	}
	defer session.Close()

	if got := session.State(); got != model.StateStopped {
		t.Fatalf("suspend=y 的目标 attach 后应停在入口，实际状态 %s", got)
	}

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := javaBreakLine(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: line})
	if err != nil {
		t.Fatalf("attach 后设置断点失败: %v", err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q（reason=%s）", bp.ID, stop.BreakpointID, stop.Reason)
	}
	if stop.Snapshot == nil || len(stop.Snapshot.Frames) == 0 {
		t.Fatal("断点停止必须带回栈帧")
	}
	if top := stop.Snapshot.Frames[0]; top.Name != "compute" || top.Where.Line != line {
		t.Fatalf("attach 后停止位置不符: %s:%d", top.Name, top.Where.Line)
	}
}

// TestJVMAttachPreconditionReturnsRestartCommand 验证 attach 前提不满足时返回的重启
// 命令可以原样执行，执行后按返回的端口 attach 成功。
func TestJVMAttachPreconditionReturnsRestartCommand(t *testing.T) {
	classDir := requireJVM(t)
	fixture := javaFixturePath(t)
	cwd := filepath.Dir(fixture)
	port := reservePort(t)
	spec := backend.LaunchSpec{
		Language: backend.Java,
		Mode:     "attach",
		Port:     port,
		Command:  []string{"java", "-cp", classDir, "App"},
		Cwd:      cwd,
		Timeout:  20 * time.Second,
	}

	_, err := jvm.Open(context.Background(), backend.Java, spec)
	if err == nil {
		t.Fatal("目标未开启 JDWP 代理时 attach 必须失败")
	}
	coded := codedError(t, err)
	if model.CodeOf(err) != model.CodeEnvAdapterMissing {
		t.Fatalf("错误码应为 ENV_ADAPTER_MISSING，实际 %s", model.CodeOf(err))
	}
	restart := coded.Context["restartCommand"]
	if restart == "" {
		t.Fatalf("attach 失败必须给出可复制的重启命令，实际错误: %v", err)
	}
	usePort, convErr := strconv.Atoi(coded.Context["port"])
	if convErr != nil {
		t.Fatalf("重启命令必须带建议端口，实际 %q", coded.Context["port"])
	}

	// 原样执行返回的命令文本，再按建议端口 attach。
	shell, shellFlag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, shellFlag = "cmd", "/c"
	}
	cmd := exec.Command(shell, shellFlag, restart)
	cmd.Dir = cwd
	proc, runErr := platform.StartProc(cmd)
	if runErr != nil {
		t.Fatalf("执行重启命令失败: %v", runErr)
	}
	defer proc.Close()
	waitDebugPort(t, usePort, 30*time.Second)

	manager := NewManager(time.Minute)
	session, openErr := manager.Open(context.Background(), backend.Java, jvm.Launcher{}, backend.LaunchSpec{
		Language: backend.Java,
		Mode:     "attach",
		Port:     usePort,
		Cwd:      cwd,
		Timeout:  40 * time.Second,
	})
	if openErr != nil {
		t.Fatalf("按返回端口 attach 失败: %v", openErr)
	}
	defer session.Close()
	if got := session.State(); got != model.StateStopped {
		t.Fatalf("attach 后应停在入口，实际状态 %s", got)
	}
}

// codedError 断言错误是统一错误并返回它。
func codedError(t *testing.T, err error) *model.Error {
	t.Helper()
	var coded *model.Error
	if !errors.As(err, &coded) {
		t.Fatalf("应返回统一错误结构，实际 %v", err)
	}
	return coded
}
