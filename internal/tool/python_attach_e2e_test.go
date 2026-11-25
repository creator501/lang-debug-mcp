package tool

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/python"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// TestPythonAttachToListeningTarget 验证 attach 路径：目标自己监听端口并承载
// DAP，本进程直接连过去，不需要本地托管目标进程。
func TestPythonAttachToListeningTarget(t *testing.T) {
	interp := requireDebugpy(t)
	port := freeTCPPort(t)
	fixture := pythonFixturePath(t)

	target := exec.Command(interp.Path, "-m", "debugpy",
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--wait-for-client", fixture)
	target.Dir = filepath.Dir(fixture)
	if err := target.Start(); err != nil {
		t.Fatalf("启动被调试目标失败: %v", err)
	}
	defer func() {
		_ = target.Process.Kill()
		_, _ = target.Process.Wait()
	}()

	session := openAttachWithRetry(t, port)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := pythonBreakLine(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}

	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 25*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q", bp.ID, stop.BreakpointID)
	}
	if stop.Snapshot == nil || len(stop.Snapshot.Frames) == 0 {
		t.Fatal("attach 断点停止必须带回栈帧")
	}
	if got := stop.Snapshot.Frames[0].Where.Line; got != line {
		t.Fatalf("停止行号 = %d, 期望 %d", got, line)
	}
	if got := stop.Snapshot.Frames[0].Name; got != "compute" {
		t.Fatalf("栈顶函数应为 compute，实际 %q", got)
	}
}

// openAttachWithRetry 反复尝试 attach，直到目标开始监听。
//
// 这里不能在测试里先自行连一次端口做探测：debugpy 的监听端对多余的连接敏感，
// 用 attach 本身探活既准确又不会干扰目标。
func openAttachWithRetry(t *testing.T, port int) *Session {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		manager := NewManager(time.Minute)
		session, err := manager.Open(context.Background(), backend.Python, python.Launcher{}, backend.LaunchSpec{
			Language: backend.Python,
			Mode:     "attach",
			Port:     port,
			Timeout:  10 * time.Second,
		})
		if err == nil {
			return session
		}
		lastErr = err
		if model.CodeOf(err) != model.CodeEnvAdapterMissing {
			t.Fatalf("attach 会话失败: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("目标未在超时内开始监听端口 %d: %v", port, lastErr)
	return nil
}

// freeTCPPort 申请一个空闲端口。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
