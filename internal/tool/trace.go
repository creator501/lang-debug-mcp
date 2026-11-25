package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
	"github.com/langdebug/lang-debug-mcp/internal/trace"
)

// tracepointArgs 是 debug_tracepoint 的参数。
type tracepointArgs struct {
	Action     string   `json:"action"`
	Session    string   `json:"session"`
	Point      string   `json:"point"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Condition  string   `json:"condition"`
	Template   []string `json:"template"`
	Rate       int      `json:"rate"`
	PointLimit int      `json:"pointLimit"`
	Kind       string   `json:"kind"`
	Aggregate  *bool    `json:"aggregate"`
	EventLimit int      `json:"eventLimit"`
	From       int      `json:"from"`
	To         int      `json:"to"`
	SinceMs    int      `json:"sinceMs"`
	RateLimit  int      `json:"rateLimit"`
}

func registerTracepoint(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_tracepoint",
		Description: "非交互式采集执行流：命中采集点后立即恢复目标，不停下来等你操作。" +
			"add 定义采集点（template 指定要记录的路径表达式，rate 降频，pointLimit 限制单点条数），" +
			"events 返回聚合或明细视图并如实报告丢弃数量，stop 移除采集点并结束采集，" +
			"timeline 把采集事件与最近一次 debug_profile 采样对齐到同一时间基线并标注两源差异。" +
			"采集由后台循环驱动目标；普通断点命中会暂停采集并把控制权交回，此后用 debug_run 交互。" +
			"高频位置必须先降频，否则事件会被速率上限丢弃。",
		InputSchema: mcp.Object(map[string]any{
			"action":     mcp.Enum("要执行的动作", "add", "remove", "list", "events", "timeline", "stop"),
			"session":    mcp.Str("会话标识"),
			"point":      mcp.Str("采集点标识；remove 与 events 可按它过滤"),
			"file":       mcp.Str("采集点所在文件"),
			"line":       mcp.Int("采集点行号"),
			"condition":  mcp.Str("命中条件（后端表达式，只有为真才记录）"),
			"template":   mcp.Arr("命中时要记录的路径表达式", mcp.Str("路径")),
			"rate":       mcp.Int("采样率：每 N 次命中记录 1 条，默认 1（全量）"),
			"pointLimit": mcp.Int("单点记录条数上限，默认 500"),
			"kind":       mcp.Enum("采集点类型；enter/exit 成对时计算耗时", "instant", "enter", "exit"),
			"aggregate":  mcp.Bool("events 是否返回聚合视图，默认 true"),
			"eventLimit": mcp.Int("明细视图的事件条数上限，默认 20"),
			"from":       mcp.Int("事件序号下界（含）"),
			"to":         mcp.Int("事件序号上界（含）"),
			"sinceMs":    mcp.Int("只返回最近 N 毫秒内的事件"),
			"rateLimit":  mcp.Int("每秒记录上限，默认 200；超出部分丢弃并计数"),
		}, "action"),
		Handler: handleTracepoint,
	})
}

func handleTracepoint(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args tracepointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	session, err := Sessions.Get(args.Session)
	if err != nil {
		return failResult(err), nil
	}
	tracer := session.tracerOf()
	switch args.Action {
	case "add":
		out, err := tracer.add(ctx, args)
		if err != nil {
			return failResult(err), nil
		}
		return mcp.Text(out), nil
	case "remove":
		if err := tracer.remove(ctx, args.Point); err != nil {
			return failResult(err), nil
		}
		return mcp.Text(map[string]any{"removed": args.Point, "points": tracer.list()}), nil
	case "list":
		return mcp.Text(map[string]any{"points": tracer.list(), "tracing": tracer.Running()}), nil
	case "events":
		return mcp.Text(tracer.query(args)), nil
	case "timeline":
		out, err := tracer.timeline(args)
		if err != nil {
			return failResult(err), nil
		}
		return mcp.Text(out), nil
	case "stop":
		return mcp.Text(tracer.stopAll(ctx)), nil
	}
	return failResult(model.New(model.CodeInputInvalid, "不支持的动作: %s", args.Action).
		WithFix("可用动作: add / remove / list / events / timeline / stop")), nil
}

// tracer 驱动一次采集会话。
//
// 采集点用后端断点实现，命中后立即恢复运行：规范要求采集不得让目标停在
// 那里等交互，因此本结构不把停止状态暴露给调用方（除非命中普通断点）。
type tracer struct {
	session *Session
	buffer  *trace.Buffer
	limiter *trace.Limiter

	mu        sync.Mutex
	points    map[string]*trace.Point
	byBackend map[string]*trace.Point
	recorded  map[string]int
	pending   map[string]time.Time
	seq       int
	cancel    context.CancelFunc
	done      chan struct{}
	running   bool
	lastErr   error
}

// newTracer 创建采集驱动。
func newTracer(session *Session) *tracer {
	return &tracer{
		session:   session,
		buffer:    trace.NewBuffer(trace.DefaultCapacity),
		limiter:   trace.NewLimiter(trace.DefaultRate),
		points:    make(map[string]*trace.Point),
		byBackend: make(map[string]*trace.Point),
		recorded:  make(map[string]int),
		pending:   make(map[string]time.Time),
	}
}

// Running 报告采集循环是否在驱动目标。
func (t *tracer) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// pointFor 把后端断点标识映射回采集点。
func (t *tracer) pointFor(backendID string) *trace.Point {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byBackend[backendID]
}

// add 注册采集点、设后端断点并启动采集循环。
func (t *tracer) add(ctx context.Context, args tracepointArgs) (map[string]any, error) {
	point := &trace.Point{
		File:      args.File,
		Line:      args.Line,
		Condition: args.Condition,
		Template:  args.Template,
		Rate:      args.Rate,
		Limit:     args.PointLimit,
		Kind:      trace.Kind(args.Kind),
	}
	if err := point.Normalize(); err != nil {
		return nil, err
	}
	dbg, err := t.session.Debugger(ctx)
	if err != nil {
		return nil, err
	}
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{
		File:      point.File,
		Line:      point.Line,
		Condition: point.Condition,
	})
	if err != nil {
		return nil, err
	}
	point.BackendID = bp.ID
	t.mu.Lock()
	t.seq++
	point.ID = fmt.Sprintf("tp%d", t.seq)
	t.points[point.ID] = point
	t.byBackend[bp.ID] = point
	t.mu.Unlock()

	if args.RateLimit > 0 {
		t.limiter = trace.NewLimiter(args.RateLimit)
	}
	t.start()
	return map[string]any{
		"point":             point.ID,
		"file":              point.File,
		"line":              point.Line,
		"kind":              point.Kind,
		"rate":              point.Rate,
		"pointLimit":        point.Limit,
		"backendBreakpoint": bp.ID,
		"verified":          bp.Verified,
		"tracing":           true,
	}, nil
}

// start 启动采集循环（已在运行时不重复启动）。
func (t *tracer) start() {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.done = make(chan struct{})
	t.running = true
	t.lastErr = nil
	t.mu.Unlock()
	go t.run(ctx)
}

// run 是采集主循环：继续执行，命中的是采集点就记录并继续。
func (t *tracer) run(ctx context.Context) {
	defer close(t.done)
	for {
		if ctx.Err() != nil {
			t.finish(nil, nil)
			return
		}
		dbg, err := t.session.Debugger(ctx)
		if err != nil {
			t.finish(err, nil)
			return
		}
		stop, err := dbg.Continue(ctx, 2*time.Second)
		if errors.Is(err, backend.ErrStillRunning) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				t.finish(nil, nil)
				return
			}
			t.finish(err, nil)
			return
		}
		point := t.pointFor(stop.BreakpointID)
		if point == nil {
			// 普通断点命中：停止采集，把控制权交回调用方。
			t.finish(nil, stop)
			return
		}
		t.record(point, stop)
	}
}

// finish 记录退出原因；stop 非空表示停在普通断点上。
func (t *tracer) finish(err error, stop *backend.Stop) {
	t.mu.Lock()
	t.running = false
	t.lastErr = err
	t.mu.Unlock()
	if stop != nil {
		t.session.ApplyStop(stop)
	}
	switch {
	case err == nil:
	case model.CodeOf(err) == model.CodeTargetExited:
		t.session.MarkDead()
	default:
		t.session.MarkRunning()
	}
}

// record 应用采样率、单点上限与速率上限后写入事件。
func (t *tracer) record(point *trace.Point, stop *backend.Stop) {
	now := time.Now()
	hits := t.buffer.Hit(point.ID)
	if point.Rate > 1 && hits%point.Rate != 0 {
		t.buffer.Skip(point.ID)
		return
	}
	if t.limitReached(point) {
		t.buffer.Drop(point.ID)
		return
	}
	if !t.limiter.Allow(now) {
		t.buffer.Drop(point.ID)
		return
	}

	event := trace.Event{Point: point.ID, File: point.File, Line: point.Line, Time: now, Thread: stop.Thread}
	if stop.Snapshot != nil && len(stop.Snapshot.Frames) > 0 {
		frame := stop.Snapshot.Frames[0]
		event.File = frame.Where.File
		event.Line = frame.Where.Line
		event.Func = frame.Where.Func
	}
	if len(point.Template) > 0 && stop.Snapshot != nil {
		res, err := stop.Snapshot.Resolve(point.Template, model.DefaultBudget())
		if err != nil {
			event.Error = err.Error()
		} else {
			event.Values = res.Values
			event.Truncated = len(res.Clips) > 0
		}
	}
	t.pairDuration(point, now, &event)
	t.buffer.Push(event)

	t.mu.Lock()
	t.recorded[point.ID]++
	t.mu.Unlock()
}

// limitReached 报告该点是否已达记录上限。
func (t *tracer) limitReached(point *trace.Point) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.recorded[point.ID] >= point.Limit
}

// pairDuration 为 enter/exit 采集点配对并计算耗时。
func (t *tracer) pairDuration(point *trace.Point, now time.Time, event *trace.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch point.Kind {
	case trace.KindEnter:
		t.pending[point.ID] = now
	case trace.KindExit:
		if start, ok := t.pending[point.ID]; ok {
			event.Duration = now.Sub(start)
			delete(t.pending, point.ID)
		}
	}
}

// remove 移除采集点与其后端断点；最后一个点被移除时停止采集。
func (t *tracer) remove(ctx context.Context, id string) error {
	t.mu.Lock()
	point, ok := t.points[id]
	if ok {
		delete(t.points, id)
		if point.BackendID != "" {
			delete(t.byBackend, point.BackendID)
		}
		delete(t.recorded, id)
		delete(t.pending, id)
	}
	remaining := len(t.points)
	t.mu.Unlock()
	if !ok {
		return model.New(model.CodeInputNotFound, "采集点 %s 不存在", id).
			WithFix("调用 debug_tracepoint 的 list 动作查看可用标识")
	}
	if point.BackendID != "" {
		if dbg, err := t.session.Debugger(ctx); err == nil {
			_ = dbg.RemoveBreakpoint(point.BackendID)
		}
	}
	if remaining == 0 {
		t.stop()
	}
	return nil
}

// stop 取消采集循环并等待其退出。
func (t *tracer) stop() {
	t.mu.Lock()
	cancel := t.cancel
	done := t.done
	running := t.running
	t.cancel = nil
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if running && done != nil {
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
	}
}

// stopAll 停止采集、移除全部采集点并返回统计。
func (t *tracer) stopAll(ctx context.Context) map[string]any {
	points := t.list()
	t.mu.Lock()
	ids := make([]string, 0, len(t.points))
	for id := range t.points {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	for _, id := range ids {
		_ = t.remove(ctx, id)
	}
	t.stop()
	return map[string]any{
		"stopped":  true,
		"points":   points,
		"stats":    t.buffer.Stats(),
		"counters": t.buffer.Counters(),
	}
}

// pointView 是采集点的对外描述。
type pointView struct {
	ID        string     `json:"point"`
	File      string     `json:"file"`
	Line      int        `json:"line"`
	Kind      trace.Kind `json:"kind"`
	Rate      int        `json:"rate"`
	Limit     int        `json:"pointLimit"`
	Condition string     `json:"condition,omitempty"`
	Template  []string   `json:"template,omitempty"`
	Recorded  int        `json:"recorded"`
	hits      int
}

// list 返回采集点列表（按标识排序）。
func (t *tracer) list() []pointView {
	t.mu.Lock()
	out := make([]pointView, 0, len(t.points))
	for id, point := range t.points {
		out = append(out, pointView{
			ID: id, File: point.File, Line: point.Line, Kind: point.Kind,
			Rate: point.Rate, Limit: point.Limit, Condition: point.Condition,
			Template: point.Template, Recorded: t.recorded[id], hits: 0,
		})
	}
	t.mu.Unlock()
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].ID > out[j].ID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// query 按条件返回聚合或明细视图。
func (t *tracer) query(args tracepointArgs) map[string]any {
	q := trace.Query{
		Point: args.Point,
		File:  args.File,
		Line:  args.Line,
		From:  args.From,
		To:    args.To,
		Limit: args.EventLimit,
	}
	if args.SinceMs > 0 {
		q.Since = time.Now().Add(-time.Duration(args.SinceMs) * time.Millisecond)
	}
	out := map[string]any{
		"stats":    t.buffer.Stats(),
		"counters": t.buffer.Counters(),
		"tracing":  t.Running(),
		"points":   t.list(),
	}
	aggregate := true
	if args.Aggregate != nil {
		aggregate = *args.Aggregate
	}
	if aggregate {
		aggs := t.buffer.Aggregates(q)
		total := 0
		for _, agg := range aggs {
			total += agg.Count
		}
		out["aggregate"] = aggs
		out["total"] = total
		out["view"] = "aggregate"
		return out
	}
	events, total := t.buffer.Events(q)
	out["events"] = events
	out["total"] = total
	out["view"] = "detail"
	return out
}
