package trace

import (
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

func TestPointNormalizeDefaultsAndValidation(t *testing.T) {
	point := Point{File: "app.c", Line: 12}
	if err := point.Normalize(); err != nil {
		t.Fatalf("合法采集点不应报错: %v", err)
	}
	if point.Kind != KindInstant || point.Rate != 1 || point.Limit != DefaultPointLimit {
		t.Fatalf("默认值不符: %+v", point)
	}
	if err := (&Point{File: "app.c"}).Normalize(); model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("缺行号应返回 %s，实际 %v", model.CodeInputInvalid, err)
	}
	if err := (&Point{File: "a", Line: 1, Kind: "bogus"}).Normalize(); model.CodeOf(err) != model.CodeInputInvalid {
		t.Fatalf("未知类型应返回 %s，实际 %v", model.CodeInputInvalid, err)
	}
}

func TestBufferDropsOldestWhenFull(t *testing.T) {
	buf := NewBuffer(2)
	buf.Push(Event{Point: "tp1", Line: 1})
	buf.Push(Event{Point: "tp1", Line: 2})
	buf.Push(Event{Point: "tp1", Line: 3})

	events, total := buf.Events(Query{})
	if total != 2 || len(events) != 2 {
		t.Fatalf("容量 2 时应保留 2 条，实际 total=%d len=%d", total, len(events))
	}
	if events[0].Line != 2 || events[1].Line != 3 {
		t.Fatalf("应丢弃最旧事件，实际 %+v", events)
	}
	stats := buf.Stats()
	if stats.Dropped != 1 || stats.Total != 3 {
		t.Fatalf("丢弃计数不符: %+v", stats)
	}
}

func TestQueryFiltersAndLimit(t *testing.T) {
	buf := NewBuffer(10)
	now := time.Now()
	buf.Push(Event{Point: "tp1", File: `C:\repo\app.c`, Line: 7, Time: now.Add(-2 * time.Second)})
	buf.Push(Event{Point: "tp1", File: `c:/repo/app.c`, Line: 7, Time: now})
	buf.Push(Event{Point: "tp2", File: `C:\repo\app.c`, Line: 9, Time: now})

	events, total := buf.Events(Query{Point: "tp1", File: "c:/repo/app.c", Line: 7, Limit: 1})
	if total != 2 {
		t.Fatalf("两条同位置事件都应符合条件，实际 %d", total)
	}
	if len(events) != 1 {
		t.Fatalf("Limit 应生效，实际 %d 条", len(events))
	}

	_, total = buf.Events(Query{Since: now.Add(-time.Second)})
	if total != 2 {
		t.Fatalf("时间过滤应排除更早的事件，实际 %d", total)
	}

	_, total = buf.Events(Query{From: 2, To: 2})
	if total != 1 {
		t.Fatalf("序号区间过滤应只留 1 条，实际 %d", total)
	}
}

func TestAggregatesCountsHitsAndDurations(t *testing.T) {
	buf := NewBuffer(10)
	buf.Hit("tp1")
	buf.Hit("tp1")
	buf.Skip("tp1")
	buf.Drop("tp1")
	buf.Push(Event{Point: "tp1", File: "app.c", Line: 3, Duration: 10 * time.Millisecond})
	buf.Push(Event{Point: "tp1", File: "app.c", Line: 3, Duration: 30 * time.Millisecond})
	buf.Push(Event{Point: "tp2", File: "app.c", Line: 9})

	aggs := buf.Aggregates(Query{})
	if len(aggs) != 2 {
		t.Fatalf("应按点聚合出 2 条，实际 %d", len(aggs))
	}
	first := aggs[0]
	if first.Point != "tp1" || first.Count != 2 {
		t.Fatalf("tp1 聚合不符: %+v", first)
	}
	if first.Duration.Samples != 2 || first.Duration.Min != int64(10*time.Millisecond) ||
		first.Duration.Max != int64(30*time.Millisecond) ||
		first.Duration.Avg != int64(20*time.Millisecond) {
		t.Fatalf("耗时统计不符: %+v", first.Duration)
	}

	counters := buf.Counters()
	if len(counters) != 2 {
		t.Fatalf("计数应覆盖 2 个点，实际 %d", len(counters))
	}
	for _, c := range counters {
		if c.Point != "tp1" {
			continue
		}
		if c.Hits != 2 || c.Skipped != 1 || c.Dropped != 1 || c.Recorded != 2 {
			t.Fatalf("tp1 计数不符: %+v", c)
		}
	}
}

func TestLimiterPerSecondWindow(t *testing.T) {
	limiter := NewLimiter(2)
	start := time.Now()
	if !limiter.Allow(start) || !limiter.Allow(start) {
		t.Fatal("窗口内前两条应允许")
	}
	if limiter.Allow(start) {
		t.Fatal("超过每秒上限应拒绝")
	}
	if !limiter.Allow(start.Add(time.Second)) {
		t.Fatal("进入新窗口后应重新允许")
	}
	unlimited := NewLimiter(0)
	for i := 0; i < 10; i++ {
		if !unlimited.Allow(start) {
			t.Fatal("0 表示不限制")
		}
	}
}
