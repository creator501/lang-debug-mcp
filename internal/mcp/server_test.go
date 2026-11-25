package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestServerListsAndCallsTool(t *testing.T) {
	srv := NewServer("test-server", "0.0.1")
	called := false
	if err := srv.AddTool(&Tool{
		Name:        "demo",
		Description: "演示工具",
		InputSchema: Object(map[string]any{"value": Str("任意值")}),
		Handler: func(_ context.Context, args json.RawMessage) (*Result, error) {
			called = true
			var p struct {
				Value string `json:"value"`
			}
			_ = json.Unmarshal(args, &p)
			return Text(map[string]any{"echo": p.Value}), nil
		},
	}); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}

	serverIn, clientWrite := io.Pipe()
	clientRead, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), serverIn, serverOut) }()

	enc := json.NewEncoder(clientWrite)
	dec := json.NewDecoder(clientRead)

	send := func(v any) {
		t.Helper()
		if err := enc.Encode(v); err != nil {
			t.Fatalf("发送请求失败: %v", err)
		}
	}
	recv := func() map[string]any {
		t.Helper()
		ch := make(chan map[string]any, 1)
		go func() {
			var m map[string]any
			_ = dec.Decode(&m)
			ch <- m
		}()
		select {
		case m := <-ch:
			if m == nil {
				t.Fatal("未收到响应")
			}
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("等待响应超时")
			return nil
		}
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18"}})
	initResp := recv()
	initResult, ok := initResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize 未返回 result: %v", initResp)
	}
	if initResult["protocolVersion"] != "2025-06-18" {
		t.Fatalf("协议版本协商错误: %v", initResult["protocolVersion"])
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	listResp := recv()
	listResult, ok := listResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list 未返回 result: %v", listResp)
	}
	tools, ok := listResult["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("工具列表不符合预期: %v", listResult["tools"])
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "demo" {
		t.Fatalf("工具名不符: %v", first["name"])
	}
	if _, has := first["inputSchema"]; !has {
		t.Fatal("工具缺少 inputSchema")
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "demo", "arguments": map[string]any{"value": "你好"}}})
	callResp := recv()
	callResult, ok := callResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call 未返回 result: %v", callResp)
	}
	if isErr, _ := callResult["isError"].(bool); isErr {
		t.Fatalf("工具调用返回失败: %v", callResult)
	}
	if !called {
		t.Fatal("工具处理器未被调用")
	}
	content, _ := callResult["content"].([]any)
	if len(content) == 0 {
		t.Fatal("工具结果缺少 content")
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	if !strings.Contains(text, "你好") {
		t.Fatalf("工具结果未回显入参: %s", text)
	}

	_ = clientWrite.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("服务端退出异常: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("服务端未在输入关闭后退出")
	}
}

func TestServerRejectsUnknownTool(t *testing.T) {
	srv := NewServer("test-server", "0.0.1")
	serverIn, clientWrite := io.Pipe()
	clientRead, serverOut := io.Pipe()
	go func() { _ = srv.Serve(context.Background(), serverIn, serverOut) }()

	enc := json.NewEncoder(clientWrite)
	dec := json.NewDecoder(clientRead)
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "不存在"}}); err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp["error"] == nil {
		t.Fatalf("未知工具应返回错误: %v", resp)
	}
	_ = clientWrite.Close()
}

func TestServerToleratesBOM(t *testing.T) {
	srv := NewServer("test-server", "0.0.1")
	serverIn, clientWrite := io.Pipe()
	clientRead, serverOut := io.Pipe()
	go func() { _ = srv.Serve(context.Background(), serverIn, serverOut) }()

	// 部分终端在首条消息前写入 UTF-8 BOM，不应导致请求被判定为解析失败。
	payload := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`+"\n")...)
	go func() {
		_, _ = clientWrite.Write(payload)
	}()

	dec := json.NewDecoder(clientRead)
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp["error"] != nil {
		t.Fatalf("带 BOM 的请求不应失败: %v", resp)
	}
	if _, ok := resp["result"]; !ok {
		t.Fatalf("应返回 result: %v", resp)
	}
	_ = clientWrite.Close()
}

func TestServerDuplicates(t *testing.T) {
	srv := NewServer("test-server", "0.0.1")
	tool := &Tool{Name: "dup", Handler: func(context.Context, json.RawMessage) (*Result, error) {
		return Text(nil), nil
	}}
	if err := srv.AddTool(tool); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	if err := srv.AddTool(tool); err == nil {
		t.Fatal("重复注册应失败")
	}
	if names := srv.ToolNames(); len(names) != 1 {
		t.Fatalf("工具名数量不符: %v", names)
	}
}
