package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/nodejs"
	"github.com/langdebug/lang-debug-mcp/internal/trace"
)

// openNodeScript 用指定脚本建立 Node 会话（入口停止）。
func openNodeScript(t *testing.T, script string) *Session {
	t.Helper()
	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), backend.NodeJS, nodejs.Launcher{}, backend.LaunchSpec{
		Language: backend.NodeJS,
		Mode:     "launch",
		Command:  []string{"node", script},
		Timeout:  25 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立会话失败: %v", err)
	}
	return session
}

// lineOfMarker 返回文件中标记所在行号。
func lineOfMarker(t *testing.T, path, marker string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	t.Fatalf("%s 中未找到标记 %s", path, marker)
	return 0
}

// waitUntil 轮询等待条件成立。
func waitUntil(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestTracepointCollectsAndContinues(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
	line := lineOfMarker(t, "../../testdata/fixture/trace.js", "TRACE_HERE")
	session := openNodeScript(t, script)
	defer session.Close()

	tracer := session.tracerOf()
	out, err := tracer.add(context.Background(), tracepointArgs{
		File:     script,
		Line:     line,
		Template: []string{"locals.total"},
	})
	if err != nil {
		t.Fatalf("注册采集点失败: %v", err)
	}
	if out["point"] != "tp1" {
		t.Fatalf("采集点标识不符: %v", out["point"])
	}

	if !waitUntil(func() bool { return !tracer.Running() }, 30*time.Second) {
		t.Fatal("采集循环未在超时内结束")
	}

	events, total := tracer.buffer.Events(trace.Query{})
	if total != 20 {
		t.Fatalf("循环 20 次应记录 20 条，实际 %d", total)
	}
	if len(events) != trace.DefaultEventLimit {
		t.Fatalf("明细默认应只返回 %d 条，实际 %d", trace.DefaultEventLimit, len(events))
	}
	if got := events[0].Values["locals.total"].Value; got != "0" {
		t.Fatalf("首条 total 应为 0，实际 %q（错误: %s）", got, events[0].Error)
	}
	if got := events[2].Values["locals.total"].Value; got != "1" {
		t.Fatalf("第三次命中时 total 应为 1，实际 %q", got)
	}
	if events[0].File == "" || events[0].Line != line {
		t.Fatalf("事件应带命中位置: %+v", events[0])
	}
	if tracer.buffer.Stats().Dropped != 0 {
		t.Fatalf("低速采集不应丢弃事件: %+v", tracer.buffer.Stats())
	}
}

func TestTracepointSamplingRateSkipsHits(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
	line := lineOfMarker(t, "../../testdata/fixture/trace.js", "TRACE_HERE")
	session := openNodeScript(t, script)
	defer session.Close()

	tracer := session.tracerOf()
	if _, err := tracer.add(context.Background(), tracepointArgs{
		File: script, Line: line, Rate: 4,
	}); err != nil {
		t.Fatalf("注册采集点失败: %v", err)
	}
	if !waitUntil(func() bool { return !tracer.Running() }, 30*time.Second) {
		t.Fatal("采集循环未在超时内结束")
	}

	counters := tracer.buffer.Counters()
	if len(counters) != 1 {
		t.Fatalf("应只有一个采集点计数，实际 %d", len(counters))
	}
	c := counters[0]
	if c.Hits != 20 || c.Recorded != 5 || c.Skipped != 15 {
		t.Fatalf("采样率 4 应记录 5 条、跳过 15 条，实际 %+v", c)
	}
}

func TestTracepointRateLimitDrops(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
	line := lineOfMarker(t, "../../testdata/fixture/trace.js", "TRACE_HERE")
	session := openNodeScript(t, script)
	defer session.Close()

	tracer := session.tracerOf()
	// 每秒只允许 1 条：20 次连续命中大部分会被丢弃。
	if _, err := tracer.add(context.Background(), tracepointArgs{
		File: script, Line: line, RateLimit: 1,
	}); err != nil {
		t.Fatalf("注册采集点失败: %v", err)
	}
	if !waitUntil(func() bool { return !tracer.Running() }, 30*time.Second) {
		t.Fatal("采集循环未在超时内结束")
	}

	stats := tracer.buffer.Stats()
	if stats.Dropped == 0 {
		t.Fatalf("速率上限应产生丢弃计数，实际 %+v", stats)
	}
	if stats.Kept > 1 {
		t.Fatalf("每秒上限 1 条时不应保留 %d 条", stats.Kept)
	}
}

func TestTracepointQueryViewsAndStop(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
	line := lineOfMarker(t, "../../testdata/fixture/trace.js", "TRACE_HERE")
	session := openNodeScript(t, script)
	defer session.Close()

	tracer := session.tracerOf()
	if _, err := tracer.add(context.Background(), tracepointArgs{File: script, Line: line}); err != nil {
		t.Fatalf("注册采集点失败: %v", err)
	}
	if !waitUntil(func() bool { return !tracer.Running() }, 30*time.Second) {
		t.Fatal("采集循环未在超时内结束")
	}

	aggregate := tracer.query(tracepointArgs{})
	if aggregate["view"] != "aggregate" {
		t.Fatalf("默认应返回聚合视图: %v", aggregate["view"])
	}
	aggs, ok := aggregate["aggregate"].([]trace.Aggregate)
	if !ok || len(aggs) != 1 || aggs[0].Count != 20 {
		t.Fatalf("聚合结果不符: %#v", aggregate["aggregate"])
	}

	detail := tracer.query(tracepointArgs{Aggregate: boolPtr(false), EventLimit: 3})
	if detail["view"] != "detail" || detail["total"] != 20 {
		t.Fatalf("明细视图应标注总量 20: %v / %v", detail["view"], detail["total"])
	}
	events, ok := detail["events"].([]trace.Event)
	if !ok || len(events) != 3 {
		t.Fatalf("明细应受 eventLimit 限制: %#v", detail["events"])
	}

	stopped := tracer.stopAll(context.Background())
	if stopped["stopped"] != true {
		t.Fatalf("stop 应返回 stopped=true: %v", stopped)
	}
	if len(tracer.list()) != 0 {
		t.Fatal("stop 后不应残留采集点")
	}
}

// boolPtr 返回布尔指针，便于构造可选参数。
func boolPtr(v bool) *bool { return &v }

func TestProfileInterruptSampling(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "busy.js"))
	session := openNodeScript(t, script)
	defer session.Close()

	dbg, err := session.Debugger(context.Background())
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	// 让目标先跑起来，采样才有栈可取。
	if _, err := dbg.Continue(context.Background(), resumeWait); err != nil && !errors.Is(err, backend.ErrStillRunning) {
		t.Fatalf("恢复运行失败: %v", err)
	}

	report, err := sampleStacks(context.Background(), session, dbg, sampleOptions{
		interval: 30 * time.Millisecond,
		count:    8,
	})
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}
	if report.Samples == 0 {
		t.Fatal("应至少采集到一个样本")
	}
	if len(report.Hot) == 0 {
		t.Fatal("应有热点条目")
	}
	if report.Hot[0].Func == "" {
		t.Fatalf("热点条目应带函数名: %+v", report.Hot[0])
	}
	if len(report.Edges) == 0 {
		t.Fatal("应有调用边计数")
	}
	if report.Overhead == "" || report.IntervalMs != 30 {
		t.Fatalf("应如实报告间隔与开销: %+v", report)
	}
	if !strings.Contains(strings.Join(report.Notes, "；"), "统计值") {
		t.Fatalf("必须标注统计性质: %v", report.Notes)
	}
}

func TestProfileCumulativeAndCallerViews(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "busy.js"))
	session := openNodeScript(t, script)
	defer session.Close()

	dbg, err := session.Debugger(context.Background())
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	if _, err := dbg.Continue(context.Background(), resumeWait); err != nil && !errors.Is(err, backend.ErrStillRunning) {
		t.Fatalf("恢复运行失败: %v", err)
	}

	report, err := sampleStacks(context.Background(), session, dbg, sampleOptions{
		interval: 20 * time.Millisecond,
		count:    10,
		focus:    "spin",
	})
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}
	if len(report.Cumulative) == 0 {
		t.Fatal("应有累计样本视图")
	}
	if report.TotalCumulative < len(report.Cumulative) {
		t.Fatalf("累计视图总量应不少于返回条数: %d / %d",
			report.TotalCumulative, len(report.Cumulative))
	}
	if report.Cumulative[0].Samples < report.Hot[0].Self {
		t.Fatalf("累计样本不应少于自身样本: %+v / %+v", report.Cumulative[0], report.Hot[0])
	}
	if report.Focus != "spin" {
		t.Fatalf("应回填 focus: %q", report.Focus)
	}
	if len(report.Callers) == 0 {
		t.Fatalf("应给出 spin 的调用来源分布: %v", report.Notes)
	}
	for _, caller := range report.Callers {
		if caller.Caller == "" || caller.Pct <= 0 {
			t.Fatalf("调用来源条目应有名称与占比: %+v", caller)
		}
	}
	if report.Callers[0].Count < report.Callers[len(report.Callers)-1].Count {
		t.Fatalf("调用来源应按次数降序: %+v", report.Callers)
	}
}

func TestTracepointTimelineAlignsProfile(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "busy.js"))
	line := lineOfMarker(t, "../../testdata/fixture/busy.js", "SPIN_HERE")
	session := openNodeScript(t, script)
	defer session.Close()

	tracer := session.tracerOf()
	// 降频 + 速率上限：热路径上只留少量事件，验证采集与采样能共存于同一会话。
	if _, err := tracer.add(context.Background(), tracepointArgs{
		File:      script,
		Line:      line,
		Template:  []string{"locals.acc"},
		Rate:      50,
		RateLimit: 20,
	}); err != nil {
		t.Fatalf("注册采集点失败: %v", err)
	}
	if !waitUntil(func() bool { return tracer.buffer.Stats().Kept >= 1 }, 15*time.Second) {
		t.Fatal("采集点未在超时内命中")
	}
	stopped := tracer.stopAll(context.Background())
	if stopped["stopped"] != true {
		t.Fatalf("stop 应返回 stopped=true: %v", stopped)
	}

	dbg, err := session.Debugger(context.Background())
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	if _, err := dbg.Continue(context.Background(), resumeWait); err != nil && !errors.Is(err, backend.ErrStillRunning) {
		t.Fatalf("恢复运行失败: %v", err)
	}
	report, err := sampleStacks(context.Background(), session, dbg, sampleOptions{
		interval: 20 * time.Millisecond,
		count:    8,
	})
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}

	out, err := tracer.timeline(tracepointArgs{})
	if err != nil {
		t.Fatalf("时间线对齐失败: %v", err)
	}
	if out["view"] != "timeline" {
		t.Fatalf("应返回时间线视图: %v", out["view"])
	}
	buckets, ok := out["buckets"].([]timelineBucket)
	if !ok || len(buckets) == 0 {
		t.Fatalf("应返回时间桶: %#v", out["buckets"])
	}
	samples := 0
	for _, bucket := range buckets {
		if bucket.ToMs < bucket.FromMs {
			t.Fatalf("时间桶区间非法: %+v", bucket)
		}
		samples += bucket.Samples
	}
	if samples != report.Samples {
		t.Fatalf("桶内样本合计应为 %d，实际 %d", report.Samples, samples)
	}

	alignment, ok := out["alignment"].(map[string]any)
	if !ok {
		t.Fatalf("应返回对齐结果: %#v", out["alignment"])
	}
	matched, ok := alignment["matched"].([]alignItem)
	if !ok || len(matched) == 0 {
		t.Fatalf("采集点函数应与采样栈匹配: %#v", alignment)
	}
	if matched[0].Events == 0 {
		t.Fatalf("匹配条目应保留事件数: %+v", matched[0])
	}
	notes, ok := out["notes"].([]string)
	if !ok || len(notes) == 0 {
		t.Fatalf("必须说明对齐口径与差异: %#v", out["notes"])
	}
	if !strings.Contains(strings.Join(notes, "；"), "统计值") {
		t.Fatalf("必须标注采样是统计值: %v", notes)
	}
}

func TestTracepointTimelineWithoutProfile(t *testing.T) {
	requireNode(t)
	script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
	session := openNodeScript(t, script)
	defer session.Close()

	if _, err := session.tracerOf().timeline(tracepointArgs{}); err == nil {
		t.Fatal("没有采样数据时 timeline 应报错，而不是返回空视图")
	}
}
