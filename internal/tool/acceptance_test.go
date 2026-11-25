package tool

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/nodejs"
	"github.com/langdebug/lang-debug-mcp/internal/backend/python"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// TestAcceptanceParallelSessions 验证两个语言的会话可以同时调试，
// 且关掉其中一个不会影响另一个：这是「多会话并行」的验收点。
func TestAcceptanceParallelSessions(t *testing.T) {
	requireNode(t)
	interp := requireDebugpy(t)
	ctx := context.Background()

	// 同一个管理器下同时开两个会话，才叫并行调试；各自建管理器会掩盖会话编号冲突。
	manager := NewManager(time.Minute)
	nodeScript := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
	nodeSession, err := manager.Open(ctx, backend.NodeJS, nodejs.Launcher{}, backend.LaunchSpec{
		Language: backend.NodeJS,
		Mode:     "launch",
		Command:  []string{"node", nodeScript},
		Timeout:  25 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立 Node 会话失败: %v", err)
	}
	nodeClosed := false
	defer func() {
		if !nodeClosed {
			_ = nodeSession.Close()
		}
	}()
	pyFixture := pythonFixturePath(t)
	pySession, err := manager.Open(ctx, backend.Python, python.Launcher{}, backend.LaunchSpec{
		Language: backend.Python,
		Mode:     "launch",
		Command:  []string{interp.Path, pyFixture},
		Cwd:      filepath.Dir(pyFixture),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立 Python 会话失败: %v", err)
	}
	defer pySession.Close()

	if nodeSession.ID == pySession.ID {
		t.Fatalf("两个会话的标识不应相同: %s", nodeSession.ID)
	}
	if nodeSession.PeekDebugger() == pySession.PeekDebugger() {
		t.Fatal("两个会话应持有各自的后端实例")
	}

	nodeDbg, err := nodeSession.Debugger(ctx)
	if err != nil {
		t.Fatalf("取 Node 后端失败: %v", err)
	}
	pyDbg, err := pySession.Debugger(ctx)
	if err != nil {
		t.Fatalf("取 Python 后端失败: %v", err)
	}

	// 两边各自下断点并命中，确认互不干扰。
	nodeLine := lineOfMarker(t, "../../testdata/fixture/trace.js", "TRACE_HERE")
	nodeBP, err := nodeDbg.AddBreakpoint(backend.BreakpointSpec{File: nodeScript, Line: nodeLine})
	if err != nil {
		t.Fatalf("Node 断点设置失败: %v", err)
	}
	nodeSession.MarkRunning()
	nodeStop, err := nodeDbg.Continue(ctx, 20*time.Second)
	if err != nil {
		t.Fatalf("Node 继续执行失败: %v", err)
	}
	if nodeStop.BreakpointID != nodeBP.ID {
		t.Fatalf("Node 应命中自己的断点 %s，实际 %q", nodeBP.ID, nodeStop.BreakpointID)
	}

	pyBP, err := pyDbg.AddBreakpoint(backend.BreakpointSpec{File: pyFixture, Line: pythonBreakLine(t)})
	if err != nil {
		t.Fatalf("Python 断点设置失败: %v", err)
	}
	pySession.MarkRunning()
	pyStop, err := pyDbg.Continue(ctx, 25*time.Second)
	if err != nil {
		t.Fatalf("Python 继续执行失败: %v", err)
	}
	if pyStop.BreakpointID != pyBP.ID {
		t.Fatalf("Python 应命中自己的断点 %s，实际 %q", pyBP.ID, pyStop.BreakpointID)
	}
	pySession.ApplyStop(pyStop)

	// 关掉 Node 会话（模拟一个目标消失），Python 必须还能继续调试。
	nodeClosed = true
	if err := nodeSession.Close(); err != nil {
		t.Fatalf("关闭 Node 会话失败: %v", err)
	}
	pySession.MarkRunning()
	stop, err := pyDbg.Continue(ctx, 25*time.Second)
	if err != nil {
		t.Fatalf("关闭其它会话后 Python 应仍可调试: %v", err)
	}
	snap := pySession.ApplyStop(stop)
	res, err := snap.Resolve([]string{"locals.total"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("第二次命中取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "20" {
		t.Fatalf("第二次命中时 total 应为 20，实际 %q", got)
	}
}

// TestAcceptanceDegradationPaths 覆盖三类降级：缺适配器、目标语言无外部剖析器、
// 以及导入文件不合法。三者都必须给出可执行的下一步，而不是笼统失败。
func TestAcceptanceDegradationPaths(t *testing.T) {
	// 1. 缺适配器：没有 launcher 时直接报 ENV_ADAPTER_MISSING。
	_, err := Sessions.Open(context.Background(), backend.Language("cobol"), nil, backend.LaunchSpec{
		Language: backend.Language("cobol"),
		Mode:     "launch",
		Command:  []string{"no-such-binary"},
	})
	if err == nil {
		t.Fatal("未知语言应报错")
	}
	if model.CodeOf(err) != model.CodeEnvAdapterMissing {
		t.Fatalf("错误码应为 ENV_ADAPTER_MISSING，实际 %s", model.CodeOf(err))
	}

	// 2. 无外部剖析器：Node 运行态请求 start，必须提示 --cpu-prof + import。
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "busy.js"))
	session := openNodeScript(t, script)
	defer session.Close()
	dbg, err := session.Debugger(context.Background())
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	// busy.js 会持续运行，continue 超时属正常。
	if _, err := dbg.Continue(context.Background(), resumeWait); err != nil && !errors.Is(err, backend.ErrStillRunning) {
		t.Fatalf("恢复运行失败: %v", err)
	}
	session.MarkRunning()
	_, err = startExternal(context.Background(), session, profileArgs{Pid: 1})
	if err == nil {
		t.Fatal("Node 没有可调用的外部剖析器，start 应报错")
	}
	if model.CodeOf(err) != model.CodeCapUnsupported {
		t.Fatalf("错误码应为 CAP_UNSUPPORTED，实际 %s", model.CodeOf(err))
	}
	if !containsAll(err.Error(), "--cpu-prof", "import") {
		t.Fatalf("修复建议应给出 --cpu-prof 与 import 的替代路径: %v", err)
	}

	// 3. 导入不合法文件：报错要说明期望的格式，而不是返回空报告。
	if _, err := importProfile(profileArgs{Path: filepath.Join(t.TempDir(), "missing.folded")}); err == nil {
		t.Fatal("文件不存在时应报错")
	} else if model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("错误码应为 INPUT_INVALID，实际 %s", model.CodeOf(err))
	}
}

// containsAll 判断文本是否同时包含全部关键词。
func containsAll(text string, keywords ...string) bool {
	for _, kw := range keywords {
		if !strings.Contains(text, kw) {
			return false
		}
	}
	return true
}
