package trace

import (
	"time"
)

// Query 是事件查询条件；零值表示不限制。
type Query struct {
	// Point 按采集点标识过滤。
	Point string
	// File / Line 按位置过滤（Line 为 0 时只按文件）。
	File string
	Line int
	// From / To 按事件序号区间过滤（闭区间，0 表示不限）。
	From int
	To   int
	// Since / Until 按时间区间过滤。
	Since time.Time
	Until time.Time
	// Limit 是明细返回条数上限；0 表示使用默认值。
	Limit int
}

// DefaultEventLimit 是明细视图的默认条数。
const DefaultEventLimit = 20

// match 报告事件是否命中查询条件。
func (q Query) match(e Event) bool {
	if q.Point != "" && e.Point != q.Point {
		return false
	}
	if q.File != "" && !samePath(e.File, q.File) {
		return false
	}
	if q.Line > 0 && e.Line != q.Line {
		return false
	}
	if q.From > 0 && e.Seq < q.From {
		return false
	}
	if q.To > 0 && e.Seq > q.To {
		return false
	}
	if !q.Since.IsZero() && e.Time.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && e.Time.After(q.Until) {
		return false
	}
	return true
}

// samePath 比较路径是否相同（忽略分隔符方向与大小写差异）。
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	return normalizePath(a) == normalizePath(b)
}

// normalizePath 统一分隔符与大小写，仅用于比较。
func normalizePath(path string) string {
	out := make([]byte, 0, len(path))
	for i := 0; i < len(path); i++ {
		ch := path[i]
		if ch == '\\' {
			ch = '/'
		}
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		out = append(out, ch)
	}
	return string(out)
}

// Events 按条件返回明细事件（最多 Limit 条）与命中总数。
//
// 返回总数是为了让调用方知道“看到的不是全部”，避免把截断当成全量。
func (b *Buffer) Events(q Query) ([]Event, int) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, 0, limit)
	total := 0
	for _, e := range b.events {
		if !q.match(e) {
			continue
		}
		total++
		if len(out) < limit {
			out = append(out, e)
		}
	}
	return out, total
}

// DurationStat 是耗时的最小 / 平均 / 最大值（纳秒）。
type DurationStat struct {
	Min int64 `json:"minNs"`
	Avg int64 `json:"avgNs"`
	Max int64 `json:"maxNs"`
	// Samples 是参与统计的事件数；0 表示没有可用的耗时样本。
	Samples int `json:"samples"`
}

// Aggregate 是按采集点聚合的统计。
type Aggregate struct {
	Point    string       `json:"point"`
	File     string       `json:"file,omitempty"`
	Line     int          `json:"line,omitempty"`
	Count    int          `json:"count"`
	FirstSeq int          `json:"firstSeq"`
	LastSeq  int          `json:"lastSeq"`
	First    time.Time    `json:"first"`
	Last     time.Time    `json:"last"`
	Duration DurationStat `json:"duration,omitempty"`
}

// Aggregates 按采集点聚合事件次数与耗时。
func (b *Buffer) Aggregates(q Query) []Aggregate {
	b.mu.Lock()
	defer b.mu.Unlock()
	index := make(map[string]*Aggregate)
	order := make([]string, 0, 8)
	durations := make(map[string][]int64)
	for _, e := range b.events {
		if !q.match(e) {
			continue
		}
		agg, ok := index[e.Point]
		if !ok {
			agg = &Aggregate{Point: e.Point, File: e.File, Line: e.Line, FirstSeq: e.Seq, First: e.Time}
			index[e.Point] = agg
			order = append(order, e.Point)
		}
		agg.Count++
		agg.LastSeq = e.Seq
		agg.Last = e.Time
		if e.Duration > 0 {
			durations[e.Point] = append(durations[e.Point], int64(e.Duration))
		}
	}
	out := make([]Aggregate, 0, len(order))
	for _, key := range order {
		agg := index[key]
		agg.Duration = summarize(durations[key])
		out = append(out, *agg)
	}
	return out
}

// summarize 计算耗时统计。
func summarize(values []int64) DurationStat {
	if len(values) == 0 {
		return DurationStat{}
	}
	stat := DurationStat{Min: values[0], Max: values[0], Samples: len(values)}
	var sum int64
	for _, v := range values {
		if v < stat.Min {
			stat.Min = v
		}
		if v > stat.Max {
			stat.Max = v
		}
		sum += v
	}
	stat.Avg = sum / int64(len(values))
	return stat
}
