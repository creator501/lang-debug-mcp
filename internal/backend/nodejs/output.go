package nodejs

import (
	"bufio"
	"io"
	"sync"

	"github.com/langdebug/lang-debug-mcp/internal/platform"
)

// outputBuffer 保存目标进程输出的尾部若干行。
//
// 只保留尾部：模型需要的是“最近发生了什么”，而不是完整日志；保留全量会让
// 上下文与内存同时失控。
type outputBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
	total int
}

func newOutputBuffer(maxLines int) *outputBuffer {
	if maxLines <= 0 {
		maxLines = 200
	}
	return &outputBuffer{max: maxLines}
}

// capture 持续读取进程输出并规范化编码。
func (b *outputBuffer) capture(r io.Reader) {
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			b.append(platform.DecodeOutput(sc.Bytes()))
		}
	}()
}

func (b *outputBuffer) append(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total++
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		overflow := len(b.lines) - b.max
		b.lines = append([]string(nil), b.lines[overflow:]...)
	}
}

// Tail 返回尾部若干行及总行数。
func (b *outputBuffer) Tail(maxLines int) ([]string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if maxLines <= 0 || maxLines > len(b.lines) {
		maxLines = len(b.lines)
	}
	out := make([]string, maxLines)
	copy(out, b.lines[len(b.lines)-maxLines:])
	return out, b.total
}

// Join 返回尾部文本。
func (b *outputBuffer) Join(maxLines int) string {
	lines, _ := b.Tail(maxLines)
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}
