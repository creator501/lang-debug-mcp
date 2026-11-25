// Package nodejs 实现 Node.js 调试后端：直连 V8 Inspector（CDP）。
//
// 有意不使用 js-debug 适配器：CDP 本身即完整调试协议，直连可避免再引入一个
// 需要安装的 JavaScript 调试适配器，符合“依赖轻”。
package nodejs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// callTimeout 是单次协议调用的超时。
const callTimeout = 20 * time.Second

// cdpMessage 是 CDP 的一条报文。
type cdpMessage struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

// cdpClient 是 V8 Inspector 的 JSON-RPC 客户端。
type cdpClient struct {
	conn *websocket.Conn

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int
	waiters map[int]chan cdpMessage

	events chan cdpMessage
	done   chan struct{}
	once   sync.Once
	err    error
}

func dialCDP(ctx context.Context, wsURL string) (*cdpClient, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, model.New(model.CodeEnvAdapterMissing, "连接 V8 Inspector 失败: %v", err).
			WithFix("确认目标进程以 --inspect 或 --inspect-brk 启动，且端口未被占用")
	}
	c := &cdpClient{
		conn:    conn,
		waiters: make(map[int]chan cdpMessage),
		events:  make(chan cdpMessage, 1024),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *cdpClient) readLoop() {
	defer c.shutdown(nil)
	for {
		var msg cdpMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			c.shutdown(err)
			return
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch, ok := c.waiters[msg.ID]
			delete(c.waiters, msg.ID)
			c.mu.Unlock()
			if ok {
				ch <- msg
			}
			continue
		}
		select {
		case c.events <- msg:
		case <-c.done:
			return
		}
	}
}

func (c *cdpClient) shutdown(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
		_ = c.conn.Close()
	})
}

// Done 返回连接结束信号。
func (c *cdpClient) Done() <-chan struct{} { return c.done }

// Events 返回事件流。
func (c *cdpClient) Events() <-chan cdpMessage { return c.events }

func (c *cdpClient) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan cdpMessage, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return model.New(model.CodeInternalProtocol, "序列化 %s 请求失败: %v", method, err).Wrap(err)
	}

	c.writeMu.Lock()
	writeErr := c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	if writeErr != nil {
		c.dropWaiter(id)
		return model.New(model.CodeStateDeadSession, "调试连接已断开: %v", writeErr).Wrap(writeErr)
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return model.New(model.CodeInternalProtocol, "%s 失败: %s", method, msg.Error.Message).
				WithContext("cdpCode", fmt.Sprint(msg.Error.Code))
		}
		if out != nil && len(msg.Result) > 0 {
			if err := json.Unmarshal(msg.Result, out); err != nil {
				return model.New(model.CodeInternalProtocol, "解析 %s 响应失败: %v", method, err).Wrap(err)
			}
		}
		return nil
	case <-callCtx.Done():
		c.dropWaiter(id)
		return model.New(model.CodeInternalProtocol, "%s 调用超时", method)
	case <-c.done:
		c.dropWaiter(id)
		return model.New(model.CodeStateDeadSession, "调试连接已断开")
	}
}

func (c *cdpClient) dropWaiter(id int) {
	c.mu.Lock()
	delete(c.waiters, id)
	c.mu.Unlock()
}

func (c *cdpClient) close() {
	c.shutdown(nil)
}

// targetInfo 是 /json/list 返回的目标描述。
type targetInfo struct {
	Description       string `json:"description"`
	WebSocketDebugURL string `json:"webSocketDebuggerUrl"`
}

// discoverTarget 通过 HTTP 端点发现调试目标。
func discoverTarget(ctx context.Context, port int) (targetInfo, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return targetInfo{}, model.New(model.CodeInternalProtocol, "构造探测请求失败: %v", err).Wrap(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return targetInfo{}, model.New(model.CodeEnvAdapterMissing, "调试端口 %d 不可达: %v", port, err).
			WithFix("确认目标以 --inspect-brk=127.0.0.1:" + fmt.Sprint(port) + " 启动")
	}
	defer resp.Body.Close()
	var targets []targetInfo
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return targetInfo{}, model.New(model.CodeInternalProtocol, "解析探测响应失败: %v", err).Wrap(err)
	}
	for _, t := range targets {
		if t.WebSocketDebugURL != "" {
			return t, nil
		}
	}
	return targetInfo{}, model.New(model.CodeEnvAdapterMissing, "调试端口 %d 上没有可用的调试目标", port).
		WithFix("确认进程未在启动后立即退出")
}
