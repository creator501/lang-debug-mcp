package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/nodejs"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

func TestSessionIdleReapAndRevive(t *testing.T) {
	requireNode(t)
	manager := NewManager(10 * time.Minute)
	session, err := manager.Open(context.Background(), backend.NodeJS, nodejs.Launcher{}, backend.LaunchSpec{
		Language: backend.NodeJS,
		Mode:     "launch",
		Command:  []string{"node", fixtureScript},
		Timeout:  25 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立会话失败: %v", err)
	}
	defer session.Close()

	if session.PeekDebugger() == nil {
		t.Fatal("会话应有活动后端")
	}
	if released := manager.Reap(time.Now().Add(2 * time.Hour)); released != 1 {
		t.Fatalf("应回收 1 个空闲会话，实际 %d", released)
	}
	if session.PeekDebugger() != nil {
		t.Fatal("回收后不应保留后端进程")
	}
	if session.State() != model.StateDetached {
		t.Fatalf("回收后状态应为 detached，实际 %s", session.State())
	}

	if _, err := session.Debugger(context.Background()); err != nil {
		t.Fatalf("按需重建后端失败: %v", err)
	}
	snap, err := session.Snapshot()
	if err != nil {
		t.Fatalf("重建后应回到入口停止态: %v", err)
	}
	if snap.ID == "" {
		t.Fatal("重建后应生成新快照")
	}
}

func TestLogpointRecordsWithoutStopping(t *testing.T) {
	requireNode(t)
	session := openNodeSession(t)
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := breakLine(t)
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{
		File:       fixtureScript,
		Line:       line,
		LogMessage: "i={i} total={total}",
	})
	if err != nil {
		t.Fatalf("设置 logpoint 失败: %v", err)
	}

	session.MarkRunning()
	// logpoint 不中断执行，目标会一路跑到结束；此处返回 TARGET_EXITED 属预期。
	_, _ = dbg.Continue(ctx, 15*time.Second)

	out := waitForOutput(t, dbg, "[trace] "+bp.ID)
	if !strings.Contains(out, "i=0") {
		t.Fatalf("logpoint 应插值出局部变量: %s", out)
	}
	if !strings.Contains(out, "total=") {
		t.Fatalf("目标自身的输出也应被采集: %s", out)
	}
}

func waitForOutput(t *testing.T, dbg backend.Debugger, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = dbg.Output(200)
		if strings.Contains(out, want) && strings.Contains(out, "total=") {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待输出 %q 超时，当前输出:\n%s", want, out)
	return out
}
