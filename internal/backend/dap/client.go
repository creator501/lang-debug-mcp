// Package dap 实现通用 Debug Adapter Protocol 客户端与调试后端。
//
// Python（debugpy）、JVM（java-debug）与 native（lldb-dap）共用本包，语言差异
// 由 Dialect 插槽提供。传输层支持两种形态：
//
//   - stdio：由本进程启动适配器，用它的标准输入输出承载 DAP 报文；
//   - TCP：目标进程自身内嵌适配器（如 debugpy --listen），直接连其监听端口。
//
// 两种形态共用同一套帧读写、请求关联与事件分发逻辑，后端实现不必区分。
package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// requestTimeout 是单次 DAP 请求的超时。
const requestTimeout = 20 * time.Second

// Message 是 DAP 的一条报文。
type Message struct {
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	Command    string          `json:"command,omitempty"`
	Event      string          `json:"event,omitempty"`
	RequestSeq int             `json:"request_seq,omitempty"`
	Success    bool            `json:"success,omitempty"`
	Message    string          `json:"message,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
}

// Client 是 DAP 客户端。
type Client struct {
	// proc 仅在 stdio 形态下非空，用于终止整棵适配器进程树。
	proc  *platform.Proc
	stdin io.WriteCloser

	mu      sync.Mutex
	seq     int
	waiters map[int]chan Message

	writeMu sync.Mutex
	events  chan Message
	done    chan struct{}
	once    sync.Once

	dropped int64
}

// Start 启动适配器进程并以其标准输入输出建立 DAP 连接。
func Start(argv []string, env []string, dir string) (*Client, error) {
	if len(argv) == 0 {
		return nil, model.New(model.CodeInputInvalid, "适配器命令为空")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	// 管道必须在 StartProc 之前取：进程启动后再取会报“already started”。
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, model.New(model.CodeInternalProtocol, "获取适配器输入管道失败: %v", err).Wrap(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, model.New(model.CodeInternalProtocol, "获取适配器输出管道失败: %v", err).Wrap(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, model.New(model.CodeInternalProtocol, "获取适配器错误管道失败: %v", err).Wrap(err)
	}
	proc, err := platform.StartProc(cmd)
	if err != nil {
		return nil, model.New(model.CodeEnvAdapterMissing, "启动调试适配器失败: %v", err).Wrap(err)
	}

	c := newClient(stdin, stdout, proc)
	// 适配器 stderr 必须持续读走，否则管道写满会让适配器阻塞。
	go drain(stderr)
	return c, nil
}

// Dial 连接已内嵌适配器的目标进程。
//
// 部分后端（debugpy --listen）把适配器放在目标进程里，此时没有适配器子进程，
// 连接关闭即代表会话结束。
func Dial(ctx context.Context, address string) (*Client, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, model.New(model.CodeEnvAdapterMissing, "无法连接调试端口 %s: %v", address, err).
			WithFix("确认目标已用调试器监听该端口（如 python -m debugpy --listen " + address + " --wait-for-client 脚本.py）").
			Wrap(err)
	}
	return newClient(conn, conn, nil), nil
}

func newClient(stdin io.WriteCloser, stdout io.Reader, proc *platform.Proc) *Client {
	c := &Client{
		proc:    proc,
		stdin:   stdin,
		waiters: make(map[int]chan Message),
		events:  make(chan Message, 1024),
		done:    make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c
}

// drain 丢弃适配器的 stderr，避免管道写满阻塞。
func drain(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
	}
}

func (c *Client) readLoop(stdout io.Reader) {
	defer c.finish()
	reader := bufio.NewReader(stdout)
	for {
		body, err := readFrame(reader)
		if err != nil {
			return
		}
		var msg Message
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "response":
			c.mu.Lock()
			ch, ok := c.waiters[msg.RequestSeq]
			delete(c.waiters, msg.RequestSeq)
			c.mu.Unlock()
			if ok {
				ch <- msg
			}
		case "event":
			// 事件缓冲满时丢弃而不是阻塞：丢一条事件远好于让整个会话卡死。
			select {
			case c.events <- msg:
			default:
				atomic.AddInt64(&c.dropped, 1)
			}
		case "request":
			// 适配器的反向请求（如 runInTerminal）不受支持，明确拒绝而不是挂起。
			c.respondUnsupported(msg)
		}
	}
}

// finish 在连接结束后收尾：先关闭完成信号，再回收适配器进程。
func (c *Client) finish() {
	c.shutdown()
	if c.proc != nil {
		// 管道读到 EOF 说明适配器已退出，此时 Wait 不会与读操作竞争。
		_ = c.proc.Cmd().Wait()
	}
}

// readFrame 读取一个 Content-Length 帧。
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
			value := strings.TrimSpace(trimmed[len("content-length:"):])
			n, convErr := strconv.Atoi(value)
			if convErr != nil {
				return nil, fmt.Errorf("非法的 Content-Length: %s", value)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("缺少 Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *Client) write(msg Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.stdin.Write(body)
	return err
}

func (c *Client) respondUnsupported(req Message) {
	_ = c.write(Message{
		Seq:        0,
		Type:       "response",
		RequestSeq: req.Seq,
		Command:    req.Command,
		Success:    false,
		Message:    "本调试客户端不支持该反向请求: " + req.Command,
	})
}

// Pending 是一个已发出但尚未等待响应的请求。
//
// DAP 允许适配器推迟响应（debugpy 的 launch/attach 会在 configurationDone 之后
// 才回复），因此发送与等待必须能分开：先发请求、处理 initialized 事件、配置断点，
// 最后再回收 launch 的结果。
type Pending struct {
	client  *Client
	id      int
	command string
	ch      chan Message
}

// Send 发送请求但不等待响应。
func (c *Client) Send(command string, args any) (*Pending, error) {
	c.mu.Lock()
	c.seq++
	id := c.seq
	ch := make(chan Message, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	msg := Message{Seq: id, Type: "request", Command: command}
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			c.dropWaiter(id)
			return nil, model.New(model.CodeInternalProtocol, "序列化 %s 参数失败: %v", command, err).Wrap(err)
		}
		msg.Arguments = raw
	}
	if err := c.write(msg); err != nil {
		c.dropWaiter(id)
		return nil, model.New(model.CodeStateDeadSession, "写入适配器失败: %v", err).Wrap(err)
	}
	return &Pending{client: c, id: id, command: command, ch: ch}, nil
}

// Wait 等待请求的响应，并把响应体解析到 out。
func (p *Pending) Wait(ctx context.Context, out any) error {
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	select {
	case resp := <-p.ch:
		if !resp.Success {
			return mapAdapterError(p.command, adapterErrorText(resp))
		}
		if out != nil && len(resp.Body) > 0 {
			if err := json.Unmarshal(resp.Body, out); err != nil {
				return model.New(model.CodeInternalProtocol, "解析 %s 响应失败: %v", p.command, err).Wrap(err)
			}
		}
		return nil
	case <-callCtx.Done():
		p.client.dropWaiter(p.id)
		return model.New(model.CodeInternalProtocol, "%s 请求超时", p.command)
	case <-p.client.done:
		p.client.dropWaiter(p.id)
		return model.New(model.CodeTargetExited, "调试适配器已退出（%s 未完成）", p.command).
			WithFix("查看 debug_output 与目标退出码，然后重建会话")
	}
}

// Request 发送一次 DAP 请求并同步等待响应。
func (c *Client) Request(ctx context.Context, command string, args any, out any) error {
	p, err := c.Send(command, args)
	if err != nil {
		return err
	}
	return p.Wait(ctx, out)
}

func (c *Client) dropWaiter(id int) {
	c.mu.Lock()
	delete(c.waiters, id)
	c.mu.Unlock()
}

// Events 返回事件流。
func (c *Client) Events() <-chan Message { return c.events }

// Done 返回连接结束信号。
func (c *Client) Done() <-chan struct{} { return c.done }

// Closed 报告连接是否已结束。
func (c *Client) Closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// DroppedEvents 返回因缓冲满被丢弃的事件数，用于诊断。
func (c *Client) DroppedEvents() int64 { return atomic.LoadInt64(&c.dropped) }

func (c *Client) shutdown() {
	c.once.Do(func() { close(c.done) })
}

// Close 关闭传输并终止适配器进程树。
//
// 这里不发送 disconnect：是否终止目标由会话决定（attach 模式必须保留目标），
// 由上层在关闭前显式发送该请求。
func (c *Client) Close() error {
	c.shutdown()
	err := c.stdin.Close()
	if c.proc != nil {
		// 适配器被终止后，其子进程（被调试目标）随进程树一并回收。
		if kerr := c.proc.Close(); err == nil {
			err = kerr
		}
	}
	return err
}

// adapterErrorText 组合响应里的错误信息。
//
// 有的适配器（dlv）把细节放在 body.error.format，message 只有 "Failed to launch"
// 这种没有信息量的一句话；只读 message 会让调用方拿不到任何可行动线索。
func adapterErrorText(resp Message) string {
	text := resp.Message
	if len(resp.Body) == 0 {
		return text
	}
	var body struct {
		Error struct {
			Format string `json:"format"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return text
	}
	detail := strings.TrimSpace(body.Error.Format)
	switch {
	case detail == "":
		return text
	case text == "":
		return detail
	case strings.Contains(text, detail):
		return text
	}
	return text + ": " + detail
}

// mapAdapterError 把适配器的错误文本映射为稳定的错误码。
func mapAdapterError(command, message string) error {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(message, "Error:"):
		// 表达式求值失败（NameError、SyntaxError 等）属于入参问题，不是协议故障。
		return model.New(model.CodeInputInvalid, "%s 求值失败: %s", command, message).
			WithFix("检查表达式拼写、作用域与目标语言语法")
	case strings.Contains(lower, "not available") || strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported"):
		return model.New(model.CodeCapUnsupported, "%s 不受支持: %s", command, message)
	case strings.Contains(lower, "read only") || strings.Contains(lower, "readonly") || strings.Contains(lower, "frozen"):
		return model.New(model.CodeTargetReadOnly, "%s 失败：目标不可写: %s", command, message)
	case strings.Contains(lower, "not stopped") || strings.Contains(lower, "not paused"):
		return model.New(model.CodeStateNotStopped, "%s 失败：目标未停止: %s", command, message)
	case strings.Contains(lower, "no such") || strings.Contains(lower, "not found"):
		return model.New(model.CodeInputNotFound, "%s 失败：%s", command, message)
	}
	return model.New(model.CodeInternalProtocol, "%s 失败: %s", command, message)
}
