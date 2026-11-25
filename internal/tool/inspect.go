package tool

import (
	"context"
	"encoding/json"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

type inspectArgs struct {
	Session  string   `json:"session"`
	Snapshot string   `json:"snapshot"`
	Paths    []string `json:"paths"`
	Frame    int      `json:"frame"`
	Depth    int      `json:"depth"`
	MaxItems int      `json:"maxItems"`
	MaxStr   int      `json:"maxStr"`
}

type evalArgs struct {
	Session    string `json:"session"`
	Snapshot   string `json:"snapshot"`
	Expression string `json:"expression"`
	Frame      int    `json:"frame"`
}

func registerInspect(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_inspect",
		Description: "在停止态批量读取变量。一次调用可给出多条路径，服务端会合并遍历后返回扁平结果；" +
			"路径形如 this、args.*、statics.*（有类静态字段的后端）、this.items[0..19]，" +
			"默认展开深度 1、集合 20 项、字符串 200 字符。" +
			"凡裁剪都会显式标注；返回的 ref 可在同一快照内继续展开。目标恢复运行后快照立即失效。",
		InputSchema: mcp.Object(map[string]any{
			"session":  mcp.Str("会话标识"),
			"snapshot": mcp.Str("快照标识。省略表示使用当前快照；显式传入过期标识会被拒绝"),
			"paths":    mcp.Arr("要读取的路径列表", mcp.Str("路径")),
			"frame":    mcp.Int("栈帧序号，默认 0"),
			"depth":    mcp.Int("展开深度，默认 1"),
			"maxItems": mcp.Int("单集合返回元素数，默认 20"),
			"maxStr":   mcp.Int("字符串最大字符数，默认 200"),
		}, "paths"),
		Handler: handleInspect,
	})
}

func registerEval(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_eval",
		Description: "在指定栈帧内对表达式求值。与 debug_inspect 的区别：本动作会执行表达式，可能改变目标状态" +
			"（含函数调用）；只读取变量请优先使用 debug_inspect。求值时长与副作用由目标自身决定。",
		InputSchema: mcp.Object(map[string]any{
			"session":    mcp.Str("会话标识"),
			"snapshot":   mcp.Str("快照标识，省略表示当前快照"),
			"expression": mcp.Str("要执行的表达式"),
			"frame":      mcp.Int("栈帧序号，默认 0"),
		}, "expression"),
		Handler: handleEval,
	})
}

func handleInspect(_ context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args inspectArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	session, err := Sessions.Get(args.Session)
	if err != nil {
		return failResult(err), nil
	}
	snap, err := session.Snapshot()
	if err != nil {
		return failResult(err), nil
	}
	if args.Snapshot != "" && args.Snapshot != snap.ID {
		return failResult(model.New(model.CodeStateStaleSnapshot,
			"快照 %s 已失效，当前快照为 %s", args.Snapshot, snap.ID).
			WithFix("使用当前快照标识重新取值，避免把改前与改后的数据混用")), nil
	}

	budget := model.Budget{Depth: args.Depth, MaxItems: args.MaxItems, MaxStr: args.MaxStr}
	res, err := snap.Resolve(args.Paths, budget)
	if err != nil {
		return failResult(err), nil
	}
	out := map[string]any{
		"snapshot": snap.ID,
		"values":   res.Values,
		"nodes":    res.Nodes,
	}
	if len(res.Clips) > 0 {
		out["clipped"] = res.Clips
	}
	return mcp.Text(out), nil
}

func handleEval(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args evalArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	if args.Expression == "" {
		return failResult(model.New(model.CodeInputInvalid, "expression 不能为空")), nil
	}
	session, err := Sessions.Get(args.Session)
	if err != nil {
		return failResult(err), nil
	}
	snap, err := session.Snapshot()
	if err != nil {
		return failResult(err), nil
	}
	if args.Snapshot != "" && args.Snapshot != snap.ID {
		return failResult(model.New(model.CodeStateStaleSnapshot,
			"快照 %s 已失效，当前快照为 %s", args.Snapshot, snap.ID)), nil
	}
	dbg, err := session.Debugger(ctx)
	if err != nil {
		return failResult(err), nil
	}
	out, err := dbg.Eval(backend.EvalInput{
		Snapshot:   snap,
		Frame:      args.Frame,
		Expression: args.Expression,
	})
	if err != nil {
		return failResult(err), nil
	}
	result := map[string]any{
		"snapshot": snap.ID,
		"t":        out.Type,
		"v":        out.Value,
	}
	if out.SideEffect {
		result["sideEffect"] = true
		result["note"] = "该表达式可能已改变目标状态"
	}
	return mcp.Text(result), nil
}
