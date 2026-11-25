package native

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// miKind 是 MI 值的种类。
type miKind int

// MI 值的三种形态：常量、C 字符串、元组或列表。
const (
	miConst miKind = iota
	miString
	miTuple
	miList
)

// miValue 是 MI 结果里的一个值。
type miValue struct {
	kind  miKind
	text  string
	tuple map[string]miValue
	list  []miValue
}

// get 取元组成员。
func (v miValue) get(key string) miValue {
	if v.kind != miTuple {
		return miValue{}
	}
	return v.tuple[key]
}

// str 返回值文本：字符串与常量取其内容，其余返回空。
func (v miValue) str() string {
	switch v.kind {
	case miString, miConst:
		return v.text
	}
	return ""
}

// int 把值解析为整数，失败返回 0。
func (v miValue) int() int {
	n, err := strconv.Atoi(strings.TrimSpace(v.str()))
	if err != nil {
		return 0
	}
	return n
}

// miRecord 是一行 MI 记录。
type miRecord struct {
	token   int
	kind    byte // ^ 结果、* 异步执行、+ 异步通知、= 通知、~ 目标输出、@ 目标 stderr
	class   string
	payload map[string]miValue
	stream  string
}

// miParser 是一个最小的 MI 结果解析器（key=value 序列）。
type miParser struct {
	src string
	pos int
}

// parseMIResults 解析 "^done,a=b,c={...}" 中逗号之后的部分。
func parseMIResults(src string) (map[string]miValue, error) {
	p := &miParser{src: src}
	out := make(map[string]miValue, 4)
	for {
		p.skipSpaces()
		if p.pos >= len(p.src) {
			return out, nil
		}
		key := p.ident()
		if key == "" {
			return out, nil
		}
		if !p.consume('=') {
			return out, nil
		}
		val, err := p.value()
		if err != nil {
			return out, err
		}
		out[key] = val
		if !p.consume(',') {
			return out, nil
		}
	}
}

// ident 读取一个键名。
func (p *miParser) ident() string {
	start := p.pos
	for p.pos < len(p.src) && isMIIdentByte(p.src[p.pos]) {
		p.pos++
	}
	return p.src[start:p.pos]
}

// isMIIdentByte 报告字节是否可以是键名的一部分。
func isMIIdentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '-', '_', '.':
		return true
	}
	return false
}

// skipSpaces 跳过空白。
func (p *miParser) skipSpaces() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

// consume 在下一个字节匹配时前进并返回 true。
func (p *miParser) consume(b byte) bool {
	p.skipSpaces()
	if p.pos < len(p.src) && p.src[p.pos] == b {
		p.pos++
		return true
	}
	return false
}

// value 解析一个值。
func (p *miParser) value() (miValue, error) {
	p.skipSpaces()
	if p.pos >= len(p.src) {
		return miValue{}, model.New(model.CodeInternalProtocol, "MI 结果为空")
	}
	switch p.src[p.pos] {
	case '"':
		text, err := p.cString()
		if err != nil {
			return miValue{}, err
		}
		return miValue{kind: miString, text: text}, nil
	case '{':
		return p.group('{', '}', true)
	case '[':
		return p.group('[', ']', false)
	default:
		return miValue{kind: miConst, text: p.constant()}, nil
	}
}

// cString 解析 MI 的 C 字符串字面量。
func (p *miParser) cString() (string, error) {
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", model.New(model.CodeInternalProtocol, "MI 字符串缺少引号")
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		p.pos++
		switch ch {
		case '"':
			return b.String(), nil
		case '\\':
			if p.pos >= len(p.src) {
				return b.String(), nil
			}
			esc := p.src[p.pos]
			p.pos++
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				b.WriteByte(0)
			default:
				b.WriteByte(esc)
			}
		default:
			b.WriteByte(ch)
		}
	}
	return b.String(), nil
}

// group 解析元组或列表。
func (p *miParser) group(open, close byte, isTuple bool) (miValue, error) {
	if !p.consume(open) {
		return miValue{}, model.New(model.CodeInternalProtocol, "MI 分组缺少起始符号")
	}
	val := miValue{kind: miList}
	if isTuple {
		val = miValue{kind: miTuple, tuple: map[string]miValue{}}
	}
	for {
		p.skipSpaces()
		if p.pos >= len(p.src) {
			return val, nil
		}
		if p.src[p.pos] == close {
			p.pos++
			return val, nil
		}
		if isTuple {
			key := p.ident()
			if !p.consume('=') {
				return val, nil
			}
			item, err := p.value()
			if err != nil {
				return val, err
			}
			val.tuple[key] = item
		} else {
			// 列表项既可以是裸值，也可以是 key=value（如 frame={...}）；
			// 后者统一包成单键元组，使 item.get("frame") 可用。
			item, err := p.listItem()
			if err != nil {
				return val, err
			}
			val.list = append(val.list, item)
		}
		if !p.consume(',') {
			// 最后一个成员后面没有逗号，直接以闭合符结束。
			if p.pos < len(p.src) && p.src[p.pos] == close {
				p.pos++
			}
			return val, nil
		}
	}
}

// listItem 解析列表中的一项：key=value 或裸值。
func (p *miParser) listItem() (miValue, error) {
	save := p.pos
	key := p.ident()
	if key != "" && p.consume('=') {
		item, err := p.value()
		if err != nil {
			return miValue{}, err
		}
		return miValue{kind: miTuple, tuple: map[string]miValue{key: item}}, nil
	}
	p.pos = save
	return p.value()
}

// constant 读取一个裸常量。
func (p *miParser) constant() string {
	start := p.pos
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		if ch == ',' || ch == '}' || ch == ']' {
			break
		}
		p.pos++
	}
	return strings.TrimSpace(p.src[start:p.pos])
}

// parseMIRecord 解析一行 MI 记录。
func parseMIRecord(line string) (miRecord, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || line == "(gdb)" {
		return miRecord{}, false
	}
	rec := miRecord{}
	rest := line
	// 数字前缀是命令 token。
	idx := 0
	for idx < len(rest) && rest[idx] >= '0' && rest[idx] <= '9' {
		idx++
	}
	if idx > 0 {
		rec.token, _ = strconv.Atoi(rest[:idx])
		rest = rest[idx:]
	}
	if rest == "" {
		return miRecord{}, false
	}
	rec.kind = rest[0]
	rest = rest[1:]
	switch rec.kind {
	case '~', '@', '&':
		p := &miParser{src: rest}
		text, err := p.cString()
		if err != nil {
			return miRecord{}, false
		}
		rec.stream = text
		return rec, true
	case '^', '*', '+', '=':
		class, tail := rec.splitClass(rest)
		rec.class = class
		if payload, err := parseMIResults(tail); err == nil {
			rec.payload = payload
		}
		return rec, true
	}
	return miRecord{}, false
}

// splitClass 把 "done,a=b" 拆成类别与结果尾部。
func (rec miRecord) splitClass(rest string) (class, tail string) {
	if idx := strings.IndexByte(rest, ','); idx >= 0 {
		return rest[:idx], rest[idx+1:]
	}
	return rest, ""
}

// miClient 是 gdb MI 的进程内客户端。
type miClient struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *miOutput

	mu      sync.Mutex
	seq     int
	pending map[int]chan miRecord
	err     error

	events    chan miRecord
	done      chan struct{}
	closeOnce sync.Once
	exitCode  int
}

// miOutput 保存目标输出与 gdb 提示，只留尾部若干行。
type miOutput struct {
	mu    sync.Mutex
	lines []string
	limit int
}

// append 追加一行，超出上限时丢弃最旧的。
func (o *miOutput) append(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lines = append(o.lines, line)
	if o.limit > 0 && len(o.lines) > o.limit {
		o.lines = o.lines[len(o.lines)-o.limit:]
	}
}

// tail 返回末尾若干行。
func (o *miOutput) tail(max int) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if max <= 0 || max > len(o.lines) {
		max = len(o.lines)
	}
	out := make([]string, max)
	copy(out, o.lines[len(o.lines)-max:])
	return out
}

// miOutputLimit 是保留的输出行数上限。
const miOutputLimit = 400

// startMIClient 启动 gdb 并接管其 MI 输出。
func startMIClient(gdbPath string, argv []string, dir string, env []string) (*miClient, error) {
	args := append([]string{"--interpreter=mi2", "--quiet", "--nx", "--args"}, argv...)
	cmd := exec.Command(gdbPath, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, model.New(model.CodeEnvToolMissing, "无法连接 gdb 的标准输入: %v", err).Wrap(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, model.New(model.CodeEnvToolMissing, "无法连接 gdb 的标准输出: %v", err).Wrap(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, model.New(model.CodeEnvToolMissing, "启动 gdb 失败: %v", err).
			WithFix("确认 gdb 可执行且版本受支持（gdb --version）").Wrap(err)
	}
	client := &miClient{
		cmd:     cmd,
		stdin:   stdin,
		out:     &miOutput{limit: miOutputLimit},
		pending: make(map[int]chan miRecord, 4),
		events:  make(chan miRecord, 8),
		done:    make(chan struct{}),
	}
	go client.readLoop(bufio.NewReaderSize(stdout, 64<<10))
	return client, nil
}

// readLoop 持续读取 MI 记录并分发。
func (c *miClient) readLoop(reader *bufio.Reader) {
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			c.dispatch(line)
		}
		if err != nil {
			c.shutdown(err)
			return
		}
	}
}

// dispatch 把一条记录交给等待者、事件队列或输出缓冲。
func (c *miClient) dispatch(line string) {
	rec, ok := parseMIRecord(line)
	if !ok {
		if text := strings.TrimSpace(line); text != "" {
			c.out.append(text)
		}
		return
	}
	switch rec.kind {
	case '~', '@':
		c.out.append(rec.stream)
	case '^':
		c.mu.Lock()
		ch := c.pending[rec.token]
		delete(c.pending, rec.token)
		c.mu.Unlock()
		if ch != nil {
			ch <- rec
		}
	case '*', '+':
		select {
		case c.events <- rec:
		default:
			// 事件队列满时丢弃最旧的，保证最新停止仍可观察到。
			select {
			case <-c.events:
			default:
			}
			select {
			case c.events <- rec:
			default:
			}
		}
	}
}

// shutdown 记录退出原因并唤醒所有等待者。
func (c *miClient) shutdown(err error) {
	c.closeOnce.Do(func() {
		exitCode := -1
		if c.cmd.ProcessState != nil {
			exitCode = c.cmd.ProcessState.ExitCode()
		}
		c.mu.Lock()
		c.err = err
		c.exitCode = exitCode
		close(c.done)
		pending := c.pending
		c.pending = make(map[int]chan miRecord, 1)
		c.mu.Unlock()
		for _, ch := range pending {
			close(ch)
		}
	})
}

// exec 发送一条 MI 命令并等待其结果记录。
func (c *miClient) exec(ctx context.Context, command string) (miRecord, error) {
	c.mu.Lock()
	select {
	case <-c.done:
		err := c.err
		c.mu.Unlock()
		return miRecord{}, c.deadError(err)
	default:
	}
	c.seq++
	token := c.seq
	ch := make(chan miRecord, 1)
	c.pending[token] = ch
	_, werr := io.WriteString(c.stdin, fmt.Sprintf("%d%s\n", token, command))
	c.mu.Unlock()
	if werr != nil {
		return miRecord{}, model.New(model.CodeStateDeadSession, "向 gdb 写入命令失败: %v", werr).
			WithFix("重建会话").Wrap(werr)
	}

	select {
	case rec, ok := <-ch:
		if !ok {
			return miRecord{}, c.deadError(nil)
		}
		if rec.class == "error" {
			return rec, c.commandError(command, rec)
		}
		return rec, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, token)
		c.mu.Unlock()
		return miRecord{}, ctx.Err()
	case <-c.done:
		// 结果可能已随退出一起到达。
		select {
		case rec, ok := <-ch:
			if ok && rec.class != "error" {
				return rec, nil
			}
		default:
		}
		return miRecord{}, c.deadError(c.err)
	}
}

// commandError 把 MI 的 ^error 转换为统一错误。
func (c *miClient) commandError(command string, rec miRecord) error {
	msg := rec.payload["msg"].str()
	if msg == "" {
		msg = "gdb 未给出原因"
	}
	return model.New(model.CodeInternalProtocol, "gdb 执行 %s 失败: %s", command, msg).
		WithFix("确认目标可执行文件与调试信息；必要时先手工在 gdb 中验证该命令")
}

// deadError 描述客户端不可用的原因。
func (c *miClient) deadError(err error) error {
	coded := model.New(model.CodeStateDeadSession, "gdb 会话已结束").WithFix("重建会话")
	if err != nil {
		coded = model.New(model.CodeStateDeadSession, "gdb 会话已结束: %v", err).WithFix("重建会话")
	}
	return coded
}

// waitEvent 等待匹配 predicate 的异步事件。
func (c *miClient) waitEvent(ctx context.Context, timeout time.Duration, predicate func(miRecord) bool) (miRecord, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case rec := <-c.events:
			if predicate(rec) {
				return rec, nil
			}
		case <-timer.C:
			return miRecord{}, nil
		case <-ctx.Done():
			return miRecord{}, ctx.Err()
		case <-c.done:
			return miRecord{}, c.deadError(c.err)
		}
	}
}

// Close 请求 gdb 退出，超时后强制结束进程。
func (c *miClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = c.exec(ctx, "-gdb-exit")
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
	_ = c.stdin.Close()
	return nil
}

// Done 返回会话结束信号。
func (c *miClient) Done() <-chan struct{} { return c.done }
