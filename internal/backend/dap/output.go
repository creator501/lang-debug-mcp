package dap

import (
	"strings"
	"sync"
)

// outputBuffer 保存目标输出（含 logpoint 记录）的尾部若干行。
//
// 只保留尾部：模型需要的是“最近发生了什么”，而不是完整日志。DAP 的 output
// 事件按块到达，可能把一行拆成多条，因此这里先拼行再入库。
type outputBuffer struct {
	mu      sync.Mutex
	lines   []string
	pending string
	max     int
	total   int
}

// maxPending 是未遇到换行时允许缓存的最大字符数，超过即落为一行。
const maxPending = 4096

func newOutputBuffer(maxLines int) *outputBuffer {
	if maxLines <= 0 {
		maxLines = 500
	}
	return &outputBuffer{max: maxLines}
}

// appendChunk 追加一段可能不含换行的输出。
func (b *outputBuffer) appendChunk(chunk string) {
	if chunk == "" {
		return
	}
	chunk = strings.ReplaceAll(chunk, "\r\n", "\n")
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pending += chunk
	for {
		idx := strings.IndexByte(b.pending, '\n')
		if idx < 0 {
			break
		}
		line := b.pending[:idx]
		b.pending = b.pending[idx+1:]
		b.push(line)
	}
	if len(b.pending) > maxPending {
		b.push(b.pending)
		b.pending = ""
	}
}

// appendLine 追加一条完整记录（如中断位置、错误说明）。
func (b *outputBuffer) appendLine(line string) {
	if line == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.push(strings.TrimRight(line, "\r\n"))
}

// push 在持锁状态下写入一行并裁剪。
func (b *outputBuffer) push(line string) {
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
	return strings.Join(lines, "\n")
}
