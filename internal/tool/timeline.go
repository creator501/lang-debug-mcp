package tool

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/trace"
)

// maxTimelineBuckets 是时间线的桶数上限：跨度再大也放大桶宽，避免输出膨胀。
const maxTimelineBuckets = 40

// alignItem 是双源对齐结果中的一个函数条目。
type alignItem struct {
	Point   string `json:"point,omitempty"`
	Func    string `json:"func"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Events  int    `json:"events,omitempty"`
	Samples int    `json:"samples,omitempty"`
}

// timelineBucket 是同一时间窗内两源各自的数据量。
type timelineBucket struct {
	FromMs  int `json:"fromMs"`
	ToMs    int `json:"toMs"`
	Events  int `json:"events"`
	Samples int `json:"samples"`
	// Point 是该桶内事件最多的采集点。
	Point string `json:"point,omitempty"`
	// TopFunc 是该桶内作为叶子出现最多的采样函数。
	TopFunc string `json:"topFunc,omitempty"`
}

// timeline 把采集事件与最近一次栈采样对齐到同一时间基线。
//
// 两源精度不同：事件是精确事实，采样是统计值。因此本视图只做“互相定位”，
// 不把采样数当成调用次数；两源对不上的部分会被显式标注，而不是抹平。
func (t *tracer) timeline(args tracepointArgs) (map[string]any, error) {
	profile := t.session.lastProfile()
	if profile == nil {
		return nil, model.New(model.CodeStateNotStopped, "没有可对齐的栈采样数据").
			WithFix("先调用 debug_profile 的 sample 动作采集样本，再查询 timeline")
	}
	if profile.Samples == 0 || len(profile.instants) == 0 {
		return nil, model.New(model.CodeStateNotStopped, "最近一次采样没有拿到任何栈样本").
			WithFix("确认目标处于运行态（debug_run 的 continue）后重新采样")
	}
	events, total := t.buffer.Events(timelineQuery(args))
	start, end := timelineSpan(profile, events)
	width := timelineBucketWidth(start, end, profile.IntervalMs)
	buckets := buildBuckets(events, profile.instants, start, width)
	alignment, notes := alignSources(events, profile)
	return map[string]any{
		"view": "timeline",
		"baseline": fmt.Sprintf("会话内墙上时间；桶宽 %dms，两源共用同一时钟",
			int(width/time.Millisecond)),
		"bucketMs": int(width / time.Millisecond),
		"trace": map[string]any{
			"events": total,
			"kept":   len(events),
			"stats":  t.buffer.Stats(),
		},
		"profile": map[string]any{
			"tool":       profile.Tool,
			"samples":    profile.Samples,
			"intervalMs": profile.IntervalMs,
			"overhead":   profile.Overhead,
		},
		"buckets":   buckets,
		"alignment": alignment,
		"notes":     notes,
	}, nil
}

// timelineQuery 把时间线参数转换为事件查询条件；时间线需要看到全部保留事件。
func timelineQuery(args tracepointArgs) trace.Query {
	q := trace.Query{
		Point: args.Point,
		File:  args.File,
		Line:  args.Line,
		From:  args.From,
		To:    args.To,
		Limit: trace.DefaultCapacity,
	}
	if args.SinceMs > 0 {
		q.Since = time.Now().Add(-time.Duration(args.SinceMs) * time.Millisecond)
	}
	return q
}

// timelineSpan 计算两源合并后的时间范围。
func timelineSpan(profile *sampleReport, events []trace.Event) (time.Time, time.Time) {
	start := profile.started
	end := profile.started
	for _, inst := range profile.instants {
		if inst.At.After(end) {
			end = inst.At
		}
	}
	for _, e := range events {
		if e.Time.Before(start) {
			start = e.Time
		}
		if e.Time.After(end) {
			end = e.Time
		}
	}
	if !end.After(start) {
		end = start.Add(time.Millisecond)
	}
	return start, end
}

// timelineBucketWidth 以采样间隔为初始桶宽；跨度太大时按桶数上限放大。
func timelineBucketWidth(start, end time.Time, intervalMs int) time.Duration {
	width := time.Duration(intervalMs) * time.Millisecond
	if width <= 0 {
		width = defaultSampleInterval
	}
	span := end.Sub(start)
	if span/width+1 > maxTimelineBuckets {
		width = span / maxTimelineBuckets
	}
	if width < time.Millisecond {
		width = time.Millisecond
	}
	return width
}

// buildBuckets 按桶宽统计两源在各时间窗内的数据量与主导条目。
func buildBuckets(events []trace.Event, instants []sampleInstant, start time.Time, width time.Duration) []timelineBucket {
	span := 0
	for _, e := range events {
		if idx := int(e.Time.Sub(start) / width); idx > span {
			span = idx
		}
	}
	for _, inst := range instants {
		if idx := int(inst.At.Sub(start) / width); idx > span {
			span = idx
		}
	}
	if span+1 > maxTimelineBuckets {
		span = maxTimelineBuckets - 1
	}
	buckets := make([]timelineBucket, span+1)
	for i := range buckets {
		buckets[i] = timelineBucket{
			FromMs: int(time.Duration(i) * width / time.Millisecond),
			ToMs:   int(time.Duration(i+1) * width / time.Millisecond),
		}
	}
	points := make([]map[string]int, len(buckets))
	funcs := make([]map[string]int, len(buckets))
	for _, e := range events {
		idx := bucketIndex(e.Time, start, width, len(buckets))
		buckets[idx].Events++
		if points[idx] == nil {
			points[idx] = make(map[string]int)
		}
		points[idx][e.Point]++
	}
	for _, inst := range instants {
		idx := bucketIndex(inst.At, start, width, len(buckets))
		buckets[idx].Samples++
		if funcs[idx] == nil {
			funcs[idx] = make(map[string]int)
		}
		funcs[idx][inst.Leaf]++
	}
	for i := range buckets {
		buckets[i].Point = dominant(points[i])
		buckets[i].TopFunc = dominant(funcs[i])
	}
	return buckets
}

// bucketIndex 把时刻映射到桶下标并夹在范围内。
func bucketIndex(at, start time.Time, width time.Duration, n int) int {
	idx := int(at.Sub(start) / width)
	if idx < 0 {
		return 0
	}
	if idx >= n {
		return n - 1
	}
	return idx
}

// dominant 返回计数最多的键（并列时取字典序最小，保证输出稳定）。
func dominant(counts map[string]int) string {
	best := ""
	bestN := -1
	for key, n := range counts {
		if n > bestN || (n == bestN && key < best) {
			best = key
			bestN = n
		}
	}
	return best
}

// alignSources 比对两源覆盖的函数据，返回对齐结果与差异标注。
func alignSources(events []trace.Event, profile *sampleReport) (map[string]any, []string) {
	traceByFunc := make(map[string]*alignItem)
	order := make([]string, 0, 8)
	for _, e := range events {
		name := eventFunc(e)
		item, ok := traceByFunc[name]
		if !ok {
			item = &alignItem{Point: e.Point, Func: name, File: e.File, Line: e.Line}
			traceByFunc[name] = item
			order = append(order, name)
		}
		item.Events++
	}
	sampled := make(map[string]bool)
	leafByFunc := make(map[string]int)
	for _, inst := range profile.instants {
		for _, name := range inst.Frames {
			sampled[name] = true
		}
		leafByFunc[inst.Leaf]++
	}

	matched := make([]alignItem, 0, len(order))
	traceOnly := make([]alignItem, 0, len(order))
	for _, name := range order {
		item := *traceByFunc[name]
		if sampled[name] {
			item.Samples = leafByFunc[name]
			matched = append(matched, item)
			continue
		}
		traceOnly = append(traceOnly, item)
	}
	profileOnly := make([]alignItem, 0, defaultTopEntries)
	for _, spot := range profile.Hot {
		if len(profileOnly) >= defaultTopEntries {
			break
		}
		if _, ok := traceByFunc[spot.Func]; ok {
			continue
		}
		profileOnly = append(profileOnly, alignItem{
			Func: spot.Func, File: spot.File, Line: spot.Line, Samples: spot.Self,
		})
	}
	sort.Slice(profileOnly, func(i, j int) bool {
		if profileOnly[i].Samples != profileOnly[j].Samples {
			return profileOnly[i].Samples > profileOnly[j].Samples
		}
		return profileOnly[i].Func < profileOnly[j].Func
	})

	notes := []string{
		"事件是精确事实、采样是统计值：对齐只用于互相定位，采样数不代表调用次数",
		"两源共用会话内墙上时间；桶宽随跨度放大，跨桶的事件与样本不对应同一时刻",
	}
	if len(traceOnly) > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d 个采集点函数未出现在采样栈中（%s）：可能该路径不在采样窗口内，或采样密度不足",
			len(traceOnly), joinFuncs(traceOnly)))
	}
	if len(profileOnly) > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d 个采样热点没有对应的采集点事件（%s）：可能是未插桩路径，或短函数/运行时内部帧带来的采样噪声",
			len(profileOnly), joinFuncs(profileOnly)))
	}
	return map[string]any{
		"matched":     matched,
		"traceOnly":   traceOnly,
		"profileOnly": profileOnly,
	}, notes
}

// eventFunc 取事件所属函数名；后端未给出函数名时回退到 文件:行。
func eventFunc(e trace.Event) string {
	if e.Func != "" {
		return e.Func
	}
	if e.File == "" {
		return "<unknown>"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(e.File), e.Line)
}

// joinFuncs 拼接前若干函数名，用于差异说明。
func joinFuncs(items []alignItem) string {
	limit := len(items)
	if limit > 5 {
		limit = 5
	}
	out := ""
	for i := 0; i < limit; i++ {
		if i > 0 {
			out += ", "
		}
		out += items[i].Func
	}
	if len(items) > limit {
		out += " 等"
	}
	return out
}
