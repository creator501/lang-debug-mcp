// Package mcp 实现本项目所需的最小 MCP 服务端：stdio 传输 + JSON-RPC 2.0。
//
// 有意不引入第三方 SDK：工具面固定为十余个、协议面只需 initialize /
// tools/list / tools/call / ping，自行实现可保证依赖轻且行为可控。
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion 是服务端默认协商的 MCP 协议版本。
const ProtocolVersion = "2025-06-18"

// maxMessageBytes 限制单条 JSON-RPC 消息体积，避免异常输入耗尽内存。
const maxMessageBytes = 16 << 20

// utf8BOM 是 UTF-8 字节序标记，部分终端会在首条消息前写入。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// supportedVersions 是服务端可协商的协议版本集合。
var supportedVersions = []string{"2024-11-05", "2025-03-26", ProtocolVersion}

// Content 是 MCP 工具返回的内容块。
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Result 是工具调用的返回体。
type Result struct {
	Content           []Content `json:"content"`
	IsError           bool      `json:"isError,omitempty"`
	StructuredContent any       `json:"structuredContent,omitempty"`
}

// Handler 处理一次工具调用。
type Handler func(ctx context.Context, args json.RawMessage) (*Result, error)

// Tool 描述一个可被模型调用的工具。
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     Handler
}

// Server 是 MCP 服务端。
type Server struct {
	Name    string
	Version string

	mu    sync.RWMutex
	tools map[string]*Tool
	order []string

	outMu sync.Mutex
	out   io.Writer
}

// NewServer 创建一个 MCP 服务端。
func NewServer(name, version string) *Server {
	return &Server{
		Name:    name,
		Version: version,
		tools:   make(map[string]*Tool),
	}
}

// AddTool 注册一个工具。
func (s *Server) AddTool(t *Tool) error {
	if t == nil || t.Name == "" {
		return fmt.Errorf("工具定义不完整")
	}
	if t.Handler == nil {
		return fmt.Errorf("工具 %s 缺少处理器", t.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.tools[t.Name]; dup {
		return fmt.Errorf("工具 %s 重复注册", t.Name)
	}
	s.tools[t.Name] = t
	s.order = append(s.order, t.Name)
	return nil
}

// ToolNames 按注册顺序返回工具名。
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.order))
	copy(out, s.order)
	return out
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve 在给定的输入输出上运行服务端，直到输入结束或上下文取消。
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.outMu.Lock()
	s.out = out
	s.outMu.Unlock()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)

	var wg sync.WaitGroup
	for sc.Scan() {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		default:
		}
		line := bytes.TrimSpace(sc.Bytes())
		// 容忍部分终端/中间层写入的 UTF-8 BOM（可能重复），避免首条请求被判定为解析失败。
		for bytes.HasPrefix(line, utf8BOM) {
			line = line[len(utf8BOM):]
		}
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(nil, -32700, "无法解析 JSON-RPC 消息: "+err.Error())
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		wg.Add(1)
		go func(r rpcRequest) {
			defer wg.Done()
			s.dispatch(ctx, r)
		}(req)
	}
	wg.Wait()
	return sc.Err()
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) {
	switch req.Method {
	case "initialize":
		s.writeResult(req.ID, s.initializeResult(req.Params))
	case "ping":
		s.writeResult(req.ID, map[string]any{})
	case "tools/list":
		s.writeResult(req.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		s.handleToolCall(ctx, req)
	default:
		s.writeError(req.ID, -32601, "不支持的方法: "+req.Method)
	}
}

func (s *Server) initializeResult(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := ProtocolVersion
	for _, v := range supportedVersions {
		if v == p.ProtocolVersion {
			version = v
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{"name": s.Name, "version": s.Version},
	}
}

func (s *Server) toolList() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := t.InputSchema
		if schema == nil {
			schema = Object(nil)
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return out
}

func (s *Server) handleToolCall(ctx context.Context, req rpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, -32602, "工具调用参数非法: "+err.Error())
		return
	}
	s.mu.RLock()
	tool, ok := s.tools[p.Name]
	s.mu.RUnlock()
	if !ok {
		s.writeError(req.ID, -32602, "未知工具: "+p.Name)
		return
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := tool.Handler(ctx, args)
	if err != nil {
		s.writeResult(req.ID, Fail(err.Error()))
		return
	}
	if res == nil {
		s.writeResult(req.ID, Text(map[string]any{}))
		return
	}
	s.writeResult(req.ID, res)
}

func (s *Server) writeResult(id json.RawMessage, result any) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeError(id json.RawMessage, code int, message string) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (s *Server) write(resp rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		data, _ = json.Marshal(rpcResponse{
			JSONRPC: "2.0",
			ID:      resp.ID,
			Error:   &rpcError{Code: -32603, Message: "响应序列化失败: " + err.Error()},
		})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.out == nil {
		return
	}
	_, _ = s.out.Write(append(data, '\n'))
}

// Text 把任意值序列化为 JSON 文本内容返回。
func Text(v any) *Result {
	data, err := json.Marshal(v)
	if err != nil {
		return Fail("结果序列化失败: " + err.Error())
	}
	return &Result{
		Content:           []Content{{Type: "text", Text: string(data)}},
		StructuredContent: v,
	}
}

// Fail 返回一个标记为失败的调用结果。
func Fail(message string) *Result {
	return &Result{
		Content: []Content{{Type: "text", Text: message}},
		IsError: true,
	}
}
