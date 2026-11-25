// Package profile 把不同来源的性能剖析输出归一成一种热点记录。
//
// 来源之间差异很大：折叠栈文本只有“栈 + 样本数”，V8 节点图有父子关系与命中数，
// 外部工具还会混入运行时内部帧。本包只做两件事：解析成统一的 Sample（叶到根的
// 栈 + 权重），再聚合成三种视图。精度损失与噪声过滤一律显式上报，不静默丢数据。
package profile

import (
	"fmt"
	"sort"
)

// Frame 是栈上的一帧。
type Frame struct {
	Func string
	File string
	Line int
}

// Sample 是一次采样；Frames 从叶（当前执行位置）到根排列。
type Sample struct {
	Frames []Frame
	Weight int
}

// Source 描述数据来源，用于在报告里如实标注精度。
type Source struct {
	// Tool 是来源工具名，如 py-spy、V8、async-profiler。
	Tool string
	// Format 是原始格式，如 folded、v8。
	Format string
	// Kind 是数据来路：external（本次调用外部工具）或 import（导入已有文件）。
	Kind string
	// DurationMs 是采集时长；离线导入时为 0。
	DurationMs int
	// Partial 表示采集被超时截断，结果是部分数据。
	Partial bool
}

// SelfEntry 是按位置精确统计的自身样本条目。
type SelfEntry struct {
	Func string  `json:"func"`
	File string  `json:"file,omitempty"`
	Line int     `json:"line,omitempty"`
	Self int     `json:"self"`
	Pct  float64 `json:"pct"`
}

// CumEntry 是按函数名聚合的累计样本条目。
type CumEntry struct {
	Func    string  `json:"func"`
	File    string  `json:"file,omitempty"`
	Line    int     `json:"line,omitempty"`
	Samples int     `json:"samples"`
	Pct     float64 `json:"pct"`
}

// CallerEntry 是某函数的一条调用来源。
type CallerEntry struct {
	Caller string  `json:"caller"`
	Count  int     `json:"count"`
	Pct    float64 `json:"pct"`
}

// Edge 是一条调用边。
type Edge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Count  int    `json:"count"`
}

// Noise 是被过滤掉的运行时噪声帧统计。
type Noise struct {
	Category string `json:"category"`
	Frames   int    `json:"frames"`
}

// Report 是归一化后的剖析报告，只提供三种视图。
type Report struct {
	Tool       string        `json:"tool"`
	Format     string        `json:"format"`
	Source     string        `json:"source"`
	Samples    int           `json:"samples"`
	DurationMs int           `json:"durationMs,omitempty"`
	Partial    bool          `json:"partial,omitempty"`
	StackKind  string        `json:"stackKind"`
	Notes      []string      `json:"notes"`
	Hot        []SelfEntry   `json:"hot"`
	Cumulative []CumEntry    `json:"cumulative"`
	Callers    []CallerEntry `json:"callers,omitempty"`
	Edges      []Edge        `json:"edges"`
	Filtered   []Noise       `json:"filtered,omitempty"`
	Focus      string        `json:"focus,omitempty"`
	Offset     int           `json:"offset,omitempty"`
	// Total* 是视图总条数，调用方据此翻页。
	TotalSelf       int `json:"totalSelf"`
	TotalCumulative int `json:"totalCumulative"`
	TotalCallers    int `json:"totalCallers"`
}

// Options 控制报告构建。
type Options struct {
	// Focus 非空时额外给出该函数的调用来源分布。
	Focus string
}

// noiseCategories 是需要过滤的运行时内部帧及其归类。
//
// 过滤不是"清理干净"，而是把噪声挪到 filtered 里计数：让调用方知道
// 有多少样本没有进入热点统计，而不是以为样本总量就是有效总量。
var noiseCategories = map[string]string{
	"(root)":              "V8 根节点",
	"(program)":           "V8 程序空转",
	"(idle)":              "空闲",
	"(garbage collector)": "GC",
	"(native)":            "原生帧",
	"[unknown]":           "未解析帧",
	"[kernel]":            "内核帧",
	"[libc]":              "libc 内部",
}

// Build 把样本聚合成报告。
func Build(src Source, samples []Sample, opts Options) *Report {
	acc := newAccumulator()
	for _, sample := range samples {
		frames := make([]Frame, 0, len(sample.Frames))
		for _, frame := range sample.Frames {
			if category, noise := noiseOf(frame); noise {
				acc.filtered[category]++
				continue
			}
			frames = append(frames, frame)
		}
		if len(frames) == 0 {
			acc.filtered["整条栈均为运行时帧"]++
			continue
		}
		weight := sample.Weight
		if weight <= 0 {
			weight = 1
		}
		acc.add(frames, weight)
	}
	report := &Report{
		Tool:      src.Tool,
		Format:    src.Format,
		Source:    src.Kind,
		Samples:   acc.weight,
		StackKind: "sync",
		Focus:     opts.Focus,
	}
	if report.Source == "" {
		report.Source = "external"
	}
	report.DurationMs = src.DurationMs
	report.Partial = src.Partial
	report.Notes = notes(src, acc)
	report.Hot = acc.rankSelf()
	report.Cumulative = acc.rankCumulative()
	report.Callers = acc.rankCallers(opts.Focus)
	report.Edges = acc.rankEdges()
	report.Filtered = acc.rankFiltered()
	report.TotalSelf = len(report.Hot)
	report.TotalCumulative = len(report.Cumulative)
	report.TotalCallers = len(report.Callers)
	if opts.Focus != "" && len(report.Callers) == 0 {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"采样栈中未出现函数 %s：可能不在采样窗口内，或名称与调试符号不一致", opts.Focus))
	}
	return report
}

// noiseOf 判断一帧是否为运行时噪声，返回其归类。
func noiseOf(frame Frame) (string, bool) {
	if frame.Func == "" {
		return noiseCategories["[unknown]"], true
	}
	if category, ok := noiseCategories[frame.Func]; ok {
		return category, true
	}
	return "", false
}

// notes 生成精度与降级说明；缺失的维度必须写出来，不能用同步栈冒充。
func notes(src Source, acc *accumulator) []string {
	out := []string{
		fmt.Sprintf("来源 %s（格式 %s）：统计值非精确调用次数，样本代表采样瞬间仍在栈上的概率", src.Tool, src.Format),
		"栈类型仅同步帧：异步/协程栈需要来源自身支持，本报告不做补齐或推测",
	}
	if src.DurationMs > 0 {
		out = append(out, fmt.Sprintf("采集时长 %dms，共 %d 个加权样本", src.DurationMs, acc.weight))
	}
	if src.Partial {
		out = append(out, "采集被超时截断：以下为已采集到的部分数据，结论需按部分样本解读")
	}
	if len(acc.filtered) > 0 {
		out = append(out, "已过滤运行时噪声帧（见 filtered）：被过滤的帧不计入自身与累计样本")
	}
	return out
}

// accumulator 累积样本统计。
type accumulator struct {
	weight   int
	self     map[string]int
	selfMeta map[string]Frame
	cum      map[string]int
	cumMeta  map[string]Frame
	edges    map[string]int
	callers  map[string]map[string]int
	filtered map[string]int
}

func newAccumulator() *accumulator {
	return &accumulator{
		self:     make(map[string]int),
		selfMeta: make(map[string]Frame),
		cum:      make(map[string]int),
		cumMeta:  make(map[string]Frame),
		edges:    make(map[string]int),
		callers:  make(map[string]map[string]int),
		filtered: make(map[string]int),
	}
}

// add 记一个样本（frames 叶到根）。
func (a *accumulator) add(frames []Frame, weight int) {
	a.weight += weight
	leaf := frames[0]
	key := frameKey(leaf)
	a.self[key] += weight
	if _, ok := a.selfMeta[key]; !ok {
		a.selfMeta[key] = leaf
	}
	seen := make(map[string]bool, len(frames))
	for _, frame := range frames {
		if seen[frame.Func] {
			continue
		}
		seen[frame.Func] = true
		a.cum[frame.Func] += weight
		if _, ok := a.cumMeta[frame.Func]; !ok {
			a.cumMeta[frame.Func] = frame
		}
	}
	for i := 0; i+1 < len(frames); i++ {
		callee := frames[i].Func
		caller := frames[i+1].Func
		a.edges[caller+" -> "+callee] += weight
		if a.callers[callee] == nil {
			a.callers[callee] = make(map[string]int)
		}
		a.callers[callee][caller] += weight
	}
}

// frameKey 用函数名与位置标识一帧。
func frameKey(frame Frame) string {
	return fmt.Sprintf("%s|%s:%d", frame.Func, frame.File, frame.Line)
}

// rankSelf 按自身样本排序。
func (a *accumulator) rankSelf() []SelfEntry {
	out := make([]SelfEntry, 0, len(a.self))
	for key, n := range a.self {
		frame := a.selfMeta[key]
		out = append(out, SelfEntry{
			Func: frame.Func, File: frame.File, Line: frame.Line,
			Self: n, Pct: ratio(n, a.weight),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self > out[j].Self
		}
		return out[i].Func < out[j].Func
	})
	return out
}

// rankCumulative 按累计样本排序。
func (a *accumulator) rankCumulative() []CumEntry {
	out := make([]CumEntry, 0, len(a.cum))
	for name, n := range a.cum {
		entry := CumEntry{Func: name, Samples: n, Pct: ratio(n, a.weight)}
		if meta, ok := a.cumMeta[name]; ok {
			entry.File = meta.File
			entry.Line = meta.Line
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].Func < out[j].Func
	})
	return out
}

// rankCallers 返回指定函数的调用来源分布。
func (a *accumulator) rankCallers(focus string) []CallerEntry {
	callers, ok := a.callers[focus]
	if !ok {
		return nil
	}
	denominator := a.cum[focus]
	out := make([]CallerEntry, 0, len(callers))
	for caller, n := range callers {
		out = append(out, CallerEntry{Caller: caller, Count: n, Pct: ratio(n, denominator)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Caller < out[j].Caller
	})
	return out
}

// rankEdges 按样本数排序调用边。
func (a *accumulator) rankEdges() []Edge {
	out := make([]Edge, 0, len(a.edges))
	for key, n := range a.edges {
		caller, callee, ok := splitEdge(key)
		if !ok {
			continue
		}
		out = append(out, Edge{Caller: caller, Callee: callee, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Callee < out[j].Callee
	})
	return out
}

// rankFiltered 按过滤帧数排序噪声统计。
func (a *accumulator) rankFiltered() []Noise {
	out := make([]Noise, 0, len(a.filtered))
	for category, n := range a.filtered {
		out = append(out, Noise{Category: category, Frames: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Frames != out[j].Frames {
			return out[i].Frames > out[j].Frames
		}
		return out[i].Category < out[j].Category
	})
	return out
}

// ratio 计算占比，分母为 0 时返回 0。
func ratio(n, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(n) / float64(total)
}

// splitEdge 拆分 "caller -> callee" 形式的键。
func splitEdge(key string) (string, string, bool) {
	for i := 0; i+3 < len(key); i++ {
		if key[i] == ' ' && key[i+1] == '-' && key[i+2] == '>' && key[i+3] == ' ' {
			return key[:i], key[i+4:], true
		}
	}
	return "", "", false
}
