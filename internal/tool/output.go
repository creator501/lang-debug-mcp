package tool

import (
	"context"
	"encoding/json"

	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

type outputArgs struct {
	Session  string `json:"session"`
	MaxLines int    `json:"maxLines"`
}

func registerOutput(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name:        "debug_output",
		Description: "读取目标程序标准输出与标准错误的尾部若干行（含 logpoint 记录）。用于查看目标自身打印的信息；只返回尾部并标注总行数。attach 模式无法采集输出。",
		InputSchema: mcp.Object(map[string]any{
			"session":  mcp.Str("会话标识"),
			"maxLines": mcp.Int("返回的尾部行数，默认 50"),
		}),
		Handler: handleOutput,
	})
}

func handleOutput(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args outputArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	session, err := Sessions.Get(args.Session)
	if err != nil {
		return failResult(err), nil
	}
	// 读取输出不应为了取一句日志就重启目标，因此不触发会话重建。
	dbg := session.PeekDebugger()
	if dbg == nil {
		return failResult(model.New(model.CodeStateDeadSession, "会话后端已释放，无法读取输出").
			WithFix("重新建立会话后再读取")), nil
	}
	maxLines := args.MaxLines
	if maxLines <= 0 {
		maxLines = 50
	}
	text := dbg.Output(maxLines)
	out := map[string]any{"tail": text}
	if text == "" {
		out["hint"] = "暂无输出。attach 模式无法采集目标输出，请让目标自行写日志"
	}
	return mcp.Text(out), nil
}
