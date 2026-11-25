package tool

import (
	"context"
	"encoding/json"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

type mutateArgs struct {
	Session  string       `json:"session"`
	Snapshot string       `json:"snapshot"`
	Frame    int          `json:"frame"`
	Ops      []mutateItem `json:"ops"`
}

type mutateItem struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

func registerMutate(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_mutate",
		Description: "在停止态批量修改变量值，用于验证根因假设（不是修复手段）。逐条执行并逐条返回结果，" +
			"成功项会回传修改前的原值。类型必须匹配，不做隐式转换；final/const、只读内存、已被优化掉的变量" +
			"会分别报告不可写原因。修改成功后旧快照立即作废，请使用返回的新快照标识。",
		InputSchema: mcp.Object(map[string]any{
			"session":  mcp.Str("会话标识"),
			"snapshot": mcp.Str("快照标识，省略表示当前快照"),
			"frame":    mcp.Int("栈帧序号，默认 0"),
			"ops": mcp.Arr("改值操作列表", mcp.Object(map[string]any{
				"path":  mcp.Str("变量路径"),
				"value": mcp.Str("目标字面量，字符串按字面量语义直接写入"),
			}, "path", "value")),
		}, "ops"),
		Handler: handleMutate,
	})
}

func handleMutate(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args mutateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	if len(args.Ops) == 0 {
		return failResult(model.New(model.CodeInputInvalid, "ops 不能为空")), nil
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

	ops := make([]backend.MutateOp, 0, len(args.Ops))
	for _, item := range args.Ops {
		ops = append(ops, backend.MutateOp{Frame: args.Frame, Path: item.Path, Value: item.Value})
	}
	results := dbg.Mutate(ops)

	anyOK := false
	for _, res := range results {
		if res.OK {
			anyOK = true
			break
		}
	}

	out := map[string]any{"results": results}
	if anyOK {
		// 改值后旧快照必须作废：值变了，旧引用可能指向别处。
		refreshed, refreshErr := session.RefreshAfterMutation("mutation")
		if refreshErr != nil {
			out["snapshot"] = snap.ID
			out["warn"] = "改值已生效，但刷新快照失败: " + refreshErr.Error()
		} else {
			out["snapshot"] = refreshed.ID
			out["note"] = "已作废旧快照，请使用新快照标识继续取值；目标内存已被修改，后续行为可能偏离原始逻辑"
		}
	} else {
		out["snapshot"] = snap.ID
	}
	return mcp.Text(out), nil
}
