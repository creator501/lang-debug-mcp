package dap

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// ListenSpec 描述“先监听端口、再由客户端连入”的适配器启动方式。
//
// dlv dap 这类适配器不在标准输入输出上承载协议：它先监听一个端口，
// 把实际地址打印到输出里，客户端连入后才开始会话。
type ListenSpec struct {
	// Argv 是适配器命令；Env 为追加环境变量；Dir 是适配器工作目录。
	Argv []string
	Env  []string
	Dir  string
	// Address 用于从适配器输出里提取监听地址，第一个捕获组即 host:port。
	Address *regexp.Regexp
	// Timeout 是等待监听地址出现的时间。
	Timeout time.Duration
}

// StartListening 启动适配器进程，解析其监听地址，再以 TCP 建立 DAP 连接。
//
// 适配器进程句柄会随客户端一起关闭：否则断开连接后 dlv 会留在后台，
// 继续占用端口与被调试进程。
func StartListening(ctx context.Context, spec ListenSpec) (*Client, error) {
	if len(spec.Argv) == 0 {
		return nil, model.New(model.CodeInputInvalid, "适配器命令为空")
	}
	if spec.Address == nil {
		return nil, model.New(model.CodeInternalProtocol, "未配置适配器监听地址的正则")
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
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
	// stderr 必须持续读走，避免管道写满把适配器阻塞；内容单独留存用于诊断。
	errBuf := newTailBuffer(64 * 1024)
	go func() { _, _ = io.Copy(errBuf, stderr) }()

	address, err := waitListenAddress(ctx, stdout, spec.Address, timeout)
	if err != nil {
		_ = proc.Close()
		return nil, model.New(model.CodeEnvAdapterMissing, "等待适配器监听地址失败: %v", err).
			WithContext("stderr", errBuf.String()).
			Wrap(err)
	}
	client, err := dialProc(ctx, address, proc)
	if err != nil {
		_ = proc.Close()
		return nil, err
	}
	return client, nil
}

// waitListenAddress 从适配器输出里读出监听地址，读出后继续读走剩余输出。
func waitListenAddress(ctx context.Context, stdout io.Reader, pattern *regexp.Regexp, timeout time.Duration) (string, error) {
	reader := bufio.NewReader(stdout)
	type result struct {
		address string
		err     error
	}
	found := make(chan result, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if match := pattern.FindStringSubmatch(line); len(match) > 1 {
				found <- result{address: match[1]}
				// 地址已拿到，剩余输出继续读走避免阻塞适配器。
				_, _ = io.Copy(io.Discard, reader)
				return
			}
			if err != nil {
				found <- result{err: err}
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-found:
		if r.err != nil {
			return "", r.err
		}
		return r.address, nil
	case <-timer.C:
		return "", model.New(model.CodeEnvAdapterMissing, "适配器在 %s 内未报告监听地址", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// dialProc 连接监听地址，并把适配器进程句柄挂到客户端上以便一并回收。
func dialProc(ctx context.Context, address string, proc *platform.Proc) (*Client, error) {
	client, err := Dial(ctx, address)
	if err != nil {
		return nil, err
	}
	client.proc = proc
	return client, nil
}

// tailBuffer 是只保留末尾内容的有界缓冲，用于适配器错误输出诊断。
type tailBuffer struct {
	limit int
	data  []byte
}

// newTailBuffer 创建有界缓冲。
func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

// Write 实现 io.Writer：超出上限时丢弃最旧的一段。
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

// String 返回已保留的内容。
func (b *tailBuffer) String() string { return string(b.data) }
