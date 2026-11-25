package tool

import (
	"context"
	"encoding/json"
	"runtime"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
)

// registerEnv 注册 debug_env 工具。
//
// 该工具是模型的第一个调用：在任何会话建立之前，先确认目标语言的后端是否
// 就位；缺失时给出可直接执行的安装命令，而不是让模型面对连接超时。
func registerEnv(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_env",
		Description: "探测调试能力。返回受支持语言清单与每个语言的后端可用性（后端名称、版本、" +
			"缺失项及安装命令）。任何调试会话之前都应先调用本工具；若目标语言不可用，" +
			"直接把缺失项与安装命令告知用户，不要尝试建立会话。",
		InputSchema: mcp.Object(map[string]any{
			"language": languageEnum("可选。只查询某一种语言的后端可用性"),
		}),
		Handler: handleEnv,
	})
}

func handleEnv(_ context.Context, args json.RawMessage) (*mcp.Result, error) {
	var p struct {
		Language string `json:"language"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return mcp.Fail("参数非法: " + err.Error()), nil
		}
	}

	statuses := Registry.Detect()
	if p.Language != "" {
		lang := backend.Language(p.Language)
		if !lang.Valid() {
			return mcp.Fail("不支持的语言: " + p.Language), nil
		}
		st, _ := Registry.ProbeLanguage(lang)
		statuses = []backend.Status{st}
	}

	result := map[string]any{
		"server": map[string]any{
			"name":    "lang-debug-mcp",
			"version": ServerVersion,
		},
		"runtime": map[string]any{
			"os":   runtime.GOOS,
			"arch": runtime.GOARCH,
		},
		"languages": backend.Supported,
		"backends":  statuses,
	}
	if len(statuses) == 0 {
		result["hint"] = "尚无已注册的后端探测器，当前版本仅提供协议骨架"
	}
	return mcp.Text(result), nil
}
