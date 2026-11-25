// Package trace 实现执行流采集点与事件聚合。
//
// 采集点不提供交互：命中后由驱动方立即恢复目标运行，只把当时的事实
// 写进环形缓冲。所有上限（单点条数、每秒速率、总容量）都显式计数并上报，
// 因为“少了一条但不说”会让调用方把缺失当成事实。
package trace

import (
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// Kind 是采集点的语义：瞬时、进入或离开。
//
// enter/exit 成对出现时用于计算耗时：exit 与同一点位最近一次未闭合的
// enter 配对，耗时记在 exit 事件上。
type Kind string

// 采集点类型。
const (
	KindInstant Kind = "instant"
	KindEnter   Kind = "enter"
	KindExit    Kind = "exit"
)

// DefaultCapacity 是事件缓冲的默认容量。
const DefaultCapacity = 2000

// DefaultRate 是每秒记录事件的默认上限。
const DefaultRate = 200

// DefaultPointLimit 是单点记录条数的默认上限。
const DefaultPointLimit = 500

// Point 是一个采集点定义。
type Point struct {
	ID        string   `json:"id"`
	File      string   `json:"file"`
	Line      int      `json:"line"`
	Condition string   `json:"condition,omitempty"`
	Template  []string `json:"template,omitempty"`
	// Rate 是采样率：每 Rate 次命中记录 1 条；小于等于 1 表示全量。
	Rate int `json:"rate,omitempty"`
	// Limit 是该点的记录条数上限。
	Limit int  `json:"limit,omitempty"`
	Kind  Kind `json:"kind,omitempty"`
	// BackendID 是后端断点标识，用于把命中事件映射回采集点。
	BackendID string `json:"-"`
}

// Normalize 补齐默认值并校验必填项。
func (p *Point) Normalize() error {
	if p.File == "" || p.Line <= 0 {
		return model.New(model.CodeInputInvalid, "采集点需要文件与行号").
			WithFix(`示例: {"action":"add","file":"app.js","line":12,"template":["locals.total"]}`)
	}
	if p.Kind == "" {
		p.Kind = KindInstant
	}
	switch p.Kind {
	case KindInstant, KindEnter, KindExit:
	default:
		return model.New(model.CodeInputInvalid, "采集点类型只能是 instant / enter / exit")
	}
	if p.Rate < 1 {
		p.Rate = 1
	}
	if p.Limit < 1 {
		p.Limit = DefaultPointLimit
	}
	return nil
}

// Event 是一条采集记录。
type Event struct {
	Seq    int       `json:"seq"`
	Point  string    `json:"point"`
	File   string    `json:"file,omitempty"`
	Line   int       `json:"line,omitempty"`
	Func   string    `json:"func,omitempty"`
	Thread string    `json:"thread,omitempty"`
	Time   time.Time `json:"time"`
	// Duration 只在 exit 事件上有效（与最近的 enter 配对）。
	Duration time.Duration          `json:"durationNs,omitempty"`
	Values   map[string]model.Entry `json:"values,omitempty"`
	// Truncated 表示模板取值被预算裁剪过。
	Truncated bool `json:"truncated,omitempty"`
	// Error 记录取值失败的原因；事件本身仍然入队，避免静默丢事实。
	Error string `json:"error,omitempty"`
}

// Counter 是单个采集点的命中统计。
type Counter struct {
	Point    string `json:"point"`
	Hits     int    `json:"hits"`
	Recorded int    `json:"recorded"`
	// Skipped 是按采样率主动跳过的命中数。
	Skipped int `json:"skipped"`
	// Dropped 是因速率上限、单点上限或缓冲满被丢弃的命中数。
	Dropped int `json:"dropped"`
}

// Stats 是缓冲的整体统计。
type Stats struct {
	Capacity int `json:"capacity"`
	Kept     int `json:"kept"`
	Total    int `json:"total"`
	Dropped  int `json:"dropped"`
}

// Buffer 是采集事件的环形缓冲。
type Buffer struct {
	mu       sync.Mutex
	events   []Event
	capacity int
	seq      int
	total    int
	dropped  int
	counters map[string]*Counter
}

// NewBuffer 创建事件缓冲；capacity 小于等于 0 时用默认值。
func NewBuffer(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Buffer{
		events:   make([]Event, 0, min(capacity, 256)),
		capacity: capacity,
		counters: make(map[string]*Counter),
	}
}

// counter 取（或创建）某点的计数器。
func (b *Buffer) counter(point string) *Counter {
	c, ok := b.counters[point]
	if !ok {
		c = &Counter{Point: point}
		b.counters[point] = c
	}
	return c
}

// Hit 记一次命中并返回累计命中数（含本次）。
func (b *Buffer) Hit(point string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.counter(point)
	c.Hits++
	return c.Hits
}

// Skip 记一次因采样率被跳过的命中。
func (b *Buffer) Skip(point string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counter(point).Skipped++
}

// Drop 记一次被丢弃的命中（速率上限、单点上限或缓冲容量）。
func (b *Buffer) Drop(point string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counter(point).Dropped++
	b.dropped++
}

// Push 写入一条事件；缓冲已满时丢弃最旧的一条并计数。
func (b *Buffer) Push(e Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	b.total++
	if len(b.events) >= b.capacity {
		copy(b.events, b.events[1:])
		b.events = b.events[:len(b.events)-1]
		b.dropped++
		if c := b.counter(e.Point); c != nil {
			c.Dropped++
		}
	}
	b.events = append(b.events, e)
	b.counter(e.Point).Recorded++
	return e
}

// Stats 返回缓冲统计。
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{Capacity: b.capacity, Kept: len(b.events), Total: b.total, Dropped: b.dropped}
}

// Counters 返回各点的计数快照（按点标识排序）。
func (b *Buffer) Counters() []Counter {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Counter, 0, len(b.counters))
	for _, c := range b.counters {
		out = append(out, *c)
	}
	sortCounters(out)
	return out
}

// sortCounters 按点标识排序，保证输出稳定。
func sortCounters(list []Counter) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].Point > list[j].Point; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

// Reset 清空事件与计数，保留容量。
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = b.events[:0]
	b.seq = 0
	b.total = 0
	b.dropped = 0
	b.counters = make(map[string]*Counter)
}

// Limiter 是每秒记录数上限（固定 1 秒窗口）。
type Limiter struct {
	mu        sync.Mutex
	perSecond int
	window    time.Time
	count     int
}

// NewLimiter 创建速率限制器；perSecond 小于等于 0 表示不限制。
func NewLimiter(perSecond int) *Limiter {
	return &Limiter{perSecond: perSecond}
}

// Allow 报告当前时刻是否还允许记录一条事件。
func (l *Limiter) Allow(now time.Time) bool {
	if l.perSecond <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.window) >= time.Second {
		l.window = now
		l.count = 0
	}
	if l.count >= l.perSecond {
		return false
	}
	l.count++
	return true
}
