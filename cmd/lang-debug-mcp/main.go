// Command lang-debug-mcp 启动面向 AI 的调试 MCP 服务端。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/tool"
)

// version 是服务端版本，随发布更新。
const version = "0.1.0"

func main() {
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	tool.ServerVersion = version
	srv := mcp.NewServer("lang-debug-mcp", version)
	if err := tool.RegisterAll(srv); err != nil {
		fmt.Fprintf(os.Stderr, "lang-debug-mcp: 注册工具失败: %v\n", err)
		os.Exit(1)
	}
	defer tool.Sessions.CloseAll()
	if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lang-debug-mcp: 服务异常: %v\n", err)
		os.Exit(1)
	}
}
