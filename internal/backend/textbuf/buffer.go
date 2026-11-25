// Package textbuf 提供按行截尾的文本缓冲。
//
// 非 DAP 后端（JDWP、gdb MI）直接读目标进程或调试器的管道，需要自己攒行并
// 只保留尾部，避免长输出吃满内存。DAP 后端有 output 事件，不需要本包。
package textbuf

import (
	"strings"
	"sync"
)

// Buffer 是只保留尾部若干行的行缓冲。
type Buffer struct {
	mu      sync.Mutex
	lines   []string
	partial string
	max     int
	total   int
}

// New 创建一个最多保留 max 行的缓冲。
func New(max int) *Buffer {
	if max <= 0 {
		max = 500
	}
	return &Buffer{max: max}
}

// AppendChunk 追加一段可能不含行尾的文本。
func (b *Buffer) AppendChunk(chunk string) {
	if chunk == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	text := b.partial + chunk
	parts := strings.Split(text, "\n")
	for i, line := range parts {
		if i == len(parts)-1 {
			b.partial = line
			break
		}
		b.pushLocked(strings.TrimRight(line, "\r"))
	}
}

// AppendLine 追加一整行。
func (b *Buffer) AppendLine(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pushLocked(line)
}

// pushLocked 追加一行并裁剪超出的历史。
func (b *Buffer) pushLocked(line string) {
	b.total++
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

// Tail 返回最后 n 行；n<=0 表示全部保留内容。
func (b *Buffer) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := b.lines
	if b.partial != "" {
		lines = append(append([]string{}, lines...), b.partial)
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Total 返回累计写入的行数。
func (b *Buffer) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}
