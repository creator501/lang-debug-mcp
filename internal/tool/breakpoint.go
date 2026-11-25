package tool

import (
	"context"
	"encoding/json"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

type breakpointArgs struct {
	Action       string   `json:"action"`
	Session      string   `json:"session"`
	Breakpoint   string   `json:"breakpoint"`
	File         string   `json:"file"`
	Line         int      `json:"line"`
	Condition    string   `json:"condition"`
	HitCondition string   `json:"hitCondition"`
	LogMessage   string   `json:"logMessage"`
	Breakpoints  []bpItem `json:"breakpoints"`
}

type bpItem struct {
	File         string `json:"file"`
	Line         int    `json:"line"`
	Condition    string `json:"condition"`
	HitCondition string `json:"hitCondition"`
	LogMessage   string `json:"logMessage"`
}

func registerBreakpoint(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_breakpoint",
		Description: "管理断点。set 可一次设置多个断点（优先使用 condition 或 hitCondition 收敛命中）；" +
			"logMessage 非空时该断点不中断执行，只记录一条带 {表达式} 插值的消息，适合追踪高频路径；" +
			"update 修改已有断点，remove 删除，list 查看当前断点与命中次数。",
		InputSchema: mcp.Object(map[string]any{
			"action":       mcp.Enum("要执行的动作", "set", "update", "remove", "list"),
			"session":      mcp.Str("会话标识"),
			"breakpoint":   mcp.Str("断点标识，update 与 remove 时必填"),
			"file":         mcp.Str("源文件路径"),
			"line":         mcp.Int("行号，从 1 开始"),
			"condition":    mcp.Str("中断条件表达式，为真时才停止"),
			"hitCondition": mcp.Str("命中次数条件，如 >=3、==2"),
			"logMessage":   mcp.Str("logpoint 消息模板，支持 {表达式} 插值；设置后该断点不中断执行"),
			"breakpoints": mcp.Arr("批量设置时使用", mcp.Object(map[string]any{
				"file":         mcp.Str("源文件路径"),
				"line":         mcp.Int("行号"),
				"condition":    mcp.Str("中断条件"),
				"hitCondition": mcp.Str("命中次数条件"),
				"logMessage":   mcp.Str("logpoint 消息模板"),
			})),
		}, "action"),
		Handler: handleBreakpoint,
	})
}

func handleBreakpoint(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args breakpointArgs
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
	case "set":
		specs := breakpointSpecs(args)
		if len(specs) == 0 {
			return failResult(model.New(model.CodeInputInvalid, "未提供断点定义").
				WithFix("提供 file 与 line，或用 breakpoints 数组批量设置")), nil
		}
		created := make([]backend.Breakpoint, 0, len(specs))
		for _, spec := range specs {
			bp, addErr := dbg.AddBreakpoint(spec)
			if addErr != nil {
				return failResult(addErr), nil
			}
			created = append(created, bp)
		}
		return mcp.Text(map[string]any{"breakpoints": created}), nil

	case "update":
		if args.Breakpoint == "" {
			return failResult(model.New(model.CodeInputInvalid, "update 需要 breakpoint 标识")), nil
		}
		bp, updateErr := dbg.UpdateBreakpoint(args.Breakpoint, backend.BreakpointSpec{
			File:         args.File,
			Line:         args.Line,
			Condition:    args.Condition,
			HitCondition: args.HitCondition,
			LogMessage:   args.LogMessage,
		})
		if updateErr != nil {
			return failResult(updateErr), nil
		}
		return mcp.Text(map[string]any{"breakpoint": bp}), nil

	case "remove":
		if args.Breakpoint == "" {
			return failResult(model.New(model.CodeInputInvalid, "remove 需要 breakpoint 标识")), nil
		}
		if err := dbg.RemoveBreakpoint(args.Breakpoint); err != nil {
			return failResult(err), nil
		}
		return mcp.Text(map[string]any{"removed": args.Breakpoint}), nil

	case "list":
		return mcp.Text(map[string]any{"breakpoints": dbg.ListBreakpoints()}), nil
	}
	return failResult(model.New(model.CodeInputInvalid, "不支持的动作: %s", args.Action).
		WithFix("可用动作: set / update / remove / list")), nil
}

func breakpointSpecs(args breakpointArgs) []backend.BreakpointSpec {
	specs := make([]backend.BreakpointSpec, 0, len(args.Breakpoints)+1)
	if args.File != "" || args.Line > 0 {
		specs = append(specs, backend.BreakpointSpec{
			File:         args.File,
			Line:         args.Line,
			Condition:    args.Condition,
			HitCondition: args.HitCondition,
			LogMessage:   args.LogMessage,
		})
	}
	for _, item := range args.Breakpoints {
		specs = append(specs, backend.BreakpointSpec{
			File:         item.File,
			Line:         item.Line,
			Condition:    item.Condition,
			HitCondition: item.HitCondition,
			LogMessage:   item.LogMessage,
		})
	}
	return specs
}
