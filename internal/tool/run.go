package tool

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

type runArgs struct {
	Action    string     `json:"action"`
	Session   string     `json:"session"`
	Step      string     `json:"step"`
	Until     *untilSpec `json:"until"`
	TimeoutMs int        `json:"timeoutMs"`
	OnHit     *onHitSpec `json:"onHit"`
}

type untilSpec struct {
	Breakpoint string `json:"breakpoint"`
	Hits       int    `json:"hits"`
}

type onHitSpec struct {
	Inspect  []string `json:"inspect"`
	Frame    int      `json:"frame"`
	Depth    int      `json:"depth"`
	MaxItems int      `json:"maxItems"`
}

func registerRun(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_run",
		Description: "控制目标执行。continue 恢复运行并等待下一次停止，可在同一次调用中用 until 指定" +
			"要等待的断点与命中序号，并用 onHit 指定命中后立刻要读取的路径——这样命中与取数只需一次往返。" +
			"超时未命中会返回 running 状态。step 单步（into/over/out），pause 中断运行中的目标，" +
			"restart 重启并重放断点，terminate 结束会话。单步是最后手段，优先用条件断点与 logpoint。",
		InputSchema: mcp.Object(map[string]any{
			"action":  mcp.Enum("要执行的动作", "continue", "step", "pause", "restart", "terminate"),
			"session": mcp.Str("会话标识"),
			"step":    mcp.Enum("单步方式", "into", "over", "out"),
			"until": mcp.Object(map[string]any{
				"breakpoint": mcp.Str("要等待的断点标识；省略表示任意停止都算"),
				"hits":       mcp.Int("需要累计命中多少次才返回"),
			}),
			"timeoutMs": mcp.Int("等待停止的超时，默认 10000"),
			"onHit": mcp.Object(map[string]any{
				"inspect":  mcp.Arr("命中后立刻读取的路径", mcp.Str("路径")),
				"frame":    mcp.Int("取值使用的栈帧，默认 0"),
				"depth":    mcp.Int("展开深度，默认 1"),
				"maxItems": mcp.Int("单集合返回的元素数，默认 20"),
			}),
		}, "action"),
		Handler: handleRun,
	})
}

func handleRun(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args runArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	session, err := Sessions.Get(args.Session)
	if err != nil {
		return failResult(err), nil
	}
	dbg, err := session.Debugger(ctx)
	if err != nil {
		return failResult(err), nil
	}

	switch args.Action {
	case "continue", "step":
		// 采集循环独占驱动：此时再让调用方 continue 会与之抢同一个目标。
		if session.Tracing() {
			return failResult(model.New(model.CodeStateNotRunning,
				"采集进行中：目标由 debug_tracepoint 自动恢复，不能用 continue/step 驱动").
				WithFix("先调用 debug_tracepoint 的 stop 动作停止采集")), nil
		}
		if args.Action == "continue" {
			return runContinue(ctx, session, dbg, args)
		}
		return runStep(ctx, session, dbg, args)
	case "pause":
		session.stopTracing()
		session.MarkRunning()
		stop, pauseErr := dbg.Pause(ctx)
		if pauseErr != nil {
			return failResult(pauseErr), nil
		}
		return mcp.Text(stopPayload(session, stop, 0)), nil
	case "restart":
		session.stopTracing()
		return runRestart(ctx, session)
	case "terminate":
		id := session.ID
		if err := Sessions.Close(id); err != nil {
			return failResult(err), nil
		}
		return mcp.Text(map[string]any{"terminated": id}), nil
	}
	return failResult(model.New(model.CodeInputInvalid, "不支持的动作: %s", args.Action).
		WithFix("可用动作: continue / step / pause / restart / terminate")), nil
}

func runContinue(ctx context.Context, session *Session, dbg backend.Debugger, args runArgs) (*mcp.Result, error) {
	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	wantBP := ""
	wantHits := 0
	if args.Until != nil {
		wantBP = args.Until.Breakpoint
		wantHits = args.Until.Hits
	}

	deadline := time.Now().Add(timeout)
	session.MarkRunning()
	observed := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return stillRunning(timeout), nil
		}
		stop, err := dbg.Continue(ctx, remaining)
		if errors.Is(err, backend.ErrStillRunning) {
			session.MarkRunning()
			return stillRunning(timeout), nil
		}
		if err != nil {
			session.MarkDead()
			return failResult(err), nil
		}
		if wantBP != "" && stop.BreakpointID != wantBP {
			continue
		}
		observed++
		if wantHits > 0 && observed < wantHits {
			continue
		}
		payload := stopPayload(session, stop, observed)
		attachOnHit(session, stop, args.OnHit, payload)
		return mcp.Text(payload), nil
	}
}

func runStep(ctx context.Context, session *Session, dbg backend.Debugger, args runArgs) (*mcp.Result, error) {
	kind := backend.StepOver
	switch args.Step {
	case "into":
		kind = backend.StepInto
	case "out":
		kind = backend.StepOut
	}
	timeout := time.Duration(args.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	session.MarkRunning()
	stop, err := dbg.Step(ctx, kind, timeout)
	if errors.Is(err, backend.ErrStillRunning) {
		return stillRunning(timeout), nil
	}
	if err != nil {
		session.MarkDead()
		return failResult(err), nil
	}
	payload := stopPayload(session, stop, 0)
	attachOnHit(session, stop, args.OnHit, payload)
	return mcp.Text(payload), nil
}

func runRestart(ctx context.Context, session *Session) (*mcp.Result, error) {
	specs := session.BreakpointSpecs()
	snap, err := session.Restart(ctx)
	if err != nil {
		return failResult(err), nil
	}
	dbg, dbgErr := session.Debugger(ctx)
	if dbgErr != nil {
		return failResult(dbgErr), nil
	}
	replayed := make([]backend.Breakpoint, 0, len(specs))
	for _, spec := range specs {
		bp, addErr := dbg.AddBreakpoint(spec)
		if addErr != nil {
			return failResult(addErr), nil
		}
		replayed = append(replayed, bp)
	}
	out := stopPayload(session, &backend.Stop{Snapshot: snap, Reason: "entry"}, 0)
	out["breakpoints"] = replayed
	out["note"] = "已重启并重放断点；断点标识可能变化，请以本次返回为准"
	return mcp.Text(out), nil
}

func stopPayload(session *Session, stop *backend.Stop, observed int) map[string]any {
	if stop == nil || stop.Snapshot == nil {
		return map[string]any{"snapshot": "", "hint": "目标处于运行状态"}
	}
	snap := session.ApplyStop(stop)
	out := map[string]any{
		"snapshot": snap.ID,
		"stop":     stopInfo(snap),
	}
	if stop.BreakpointID != "" {
		out["breakpoint"] = stop.BreakpointID
	}
	if observed > 0 {
		out["hits"] = observed
	}
	return out
}

func attachOnHit(session *Session, stop *backend.Stop, onHit *onHitSpec, payload map[string]any) {
	if onHit == nil || len(onHit.Inspect) == 0 {
		return
	}
	snap := session.ApplyStop(stop)
	budget := model.DefaultBudget()
	if onHit.Depth > 0 {
		budget.Depth = onHit.Depth
	}
	if onHit.MaxItems > 0 {
		budget.MaxItems = onHit.MaxItems
	}
	res, err := snap.Resolve(onHit.Inspect, budget)
	if err != nil {
		payload["values"] = nil
		payload["valuesError"] = err.Error()
		return
	}
	payload["values"] = res.Values
	if len(res.Clips) > 0 {
		payload["clipped"] = res.Clips
	}
}

func stillRunning(timeout time.Duration) *mcp.Result {
	return mcp.Text(map[string]any{
		"running":   true,
		"timeoutMs": timeout.Milliseconds(),
		"hint":      "目标仍在运行，未在超时内停止。可再次调用 continue 继续等待，或设置断点后重试",
	})
}
