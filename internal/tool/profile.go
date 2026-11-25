package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/mcp"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// profileArgs 是 debug_profile 的参数。
type profileArgs struct {
	Action     string `json:"action"`
	Session    string `json:"session"`
	IntervalMs int    `json:"intervalMs"`
	Samples    int    `json:"samples"`
	Top        int    `json:"top"`
	Offset     int    `json:"offset"`
	Focus      string `json:"focus"`
	// 以下参数供 external 与 import 使用。
	Pid        int    `json:"pid"`
	DurationMs int    `json:"durationMs"`
	TimeoutMs  int    `json:"timeoutMs"`
	Format     string `json:"format"`
	Path       string `json:"path"`
}

// 采样参数的默认值与上限。
const (
	defaultSampleInterval = 50 * time.Millisecond
	defaultSampleCount    = 40
	maxSampleCount        = 200
	defaultTopEntries     = 10
	// resumeWait 是恢复运行后等待目标真正跑起来的时间。
	resumeWait = 60 * time.Millisecond
)

// hotSpot 是自身样本最多的一帧。
type hotSpot struct {
	Func string  `json:"func"`
	File string  `json:"file,omitempty"`
	Line int     `json:"line,omitempty"`
	Self int     `json:"self"`
	Pct  float64 `json:"pct"`
}

// cumEntry 是累计样本条目：函数出现在栈上的样本数。
//
// 自身样本按位置精确计数，累计样本按函数名聚合：同一函数在多处被调用时，
// 按位置拆开会把它的累计耗时切碎，看不出真实占比。
type cumEntry struct {
	Func    string  `json:"func"`
	File    string  `json:"file,omitempty"`
	Line    int     `json:"line,omitempty"`
	Samples int     `json:"samples"`
	Pct     float64 `json:"pct"`
}

// callerEntry 是某个函数的调用来源分布条目。
type callerEntry struct {
	Caller string  `json:"caller"`
	Count  int     `json:"count"`
	Pct    float64 `json:"pct"`
}

// callEdge 是一条调用边及其样本数。
type callEdge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Count  int    `json:"count"`
}

// sampleInstant 是一次采样在时间线上的落点，用于与采集事件对齐。
type sampleInstant struct {
	At     time.Time `json:"at"`
	Leaf   string    `json:"leaf"`
	Frames []string  `json:"frames,omitempty"`
}

// sampleReport 是一次中断采样的结果。
//
// 只提供三种视图（规范要求）：自身样本最高者、累计样本最高者、指定函数的
// 调用来源分布。全量明细不返回，避免上下文膨胀；total* 字段让调用方知道
// 手里的是前几条，可以按 offset 继续翻页。
type sampleReport struct {
	Tool            string        `json:"tool"`
	IntervalMs      int           `json:"intervalMs"`
	Samples         int           `json:"samples"`
	Failed          int           `json:"failed"`
	DurationMs      int           `json:"durationMs"`
	Overhead        string        `json:"overhead"`
	Notes           []string      `json:"notes"`
	Focus           string        `json:"focus,omitempty"`
	Hot             []hotSpot     `json:"hot"`
	Cumulative      []cumEntry    `json:"cumulative"`
	Callers         []callerEntry `json:"callers,omitempty"`
	Edges           []callEdge    `json:"edges"`
	Offset          int           `json:"offset,omitempty"`
	TotalSelf       int           `json:"totalSelf"`
	TotalCumulative int           `json:"totalCumulative"`
	TotalCallers    int           `json:"totalCallers"`

	// started 与 instants 只服务于双源时间线对齐，不进入对外 JSON。
	started  time.Time
	instants []sampleInstant
}

// sampleOptions 是一次采样的参数。
type sampleOptions struct {
	interval time.Duration
	count    int
	// focus 非空时额外给出该函数的调用来源分布。
	focus string
}

func registerProfile(srv *mcp.Server) error {
	return srv.AddTool(&mcp.Tool{
		Name: "debug_profile",
		Description: "性能剖析，三种动作：sample 用调试后端中断采样（无需外部工具），" +
			"start/stop 调用外部剖析器（py-spy / async-profiler）并归一化其输出，" +
			"import 解析已有的折叠栈或 .cpuprofile 文件。" +
			"三种来源都归一成同一份报告：hot（自身样本最高）、cumulative（累计样本最高）、callers（指定 focus 函数的调用来源）；" +
			"top/offset 控制分页，total* 标注总量，filtered 报告被过滤的运行时噪声帧。" +
			"结果是统计值，不是精确调用次数；采样会短暂停顿目标，外部剖析还会受附着权限与超时影响。" +
			"目标处于停止态时不允许开始剖析；剖析器缺失时返回安装命令与该语言的替代方案。" +
			"sample 的结果可与 debug_tracepoint 的 timeline 动作按同一时间基线对齐。",
		InputSchema: mcp.Object(map[string]any{
			"action":     mcp.Enum("要执行的动作", "sample", "start", "stop", "status", "import"),
			"session":    mcp.Str("会话标识；import 不需要"),
			"intervalMs": mcp.Int("sample 的采样间隔毫秒，默认 50；越小越准但停顿越频繁"),
			"samples":    mcp.Int("sample 的样本数上限，默认 40，最大 200"),
			"top":        mcp.Int("每个视图返回条数，默认 10"),
			"offset":     mcp.Int("每个视图的起始偏移，用于翻页"),
			"focus":      mcp.Str("要统计调用来源的函数名；留空则不返回 callers 视图"),
			"pid":        mcp.Int("start 必需：被调试目标的操作系统进程号"),
			"durationMs": mcp.Int("start 的采集时长，默认 3000，最大 30000"),
			"timeoutMs":  mcp.Int("start 的整体超时，超时后终止剖析并标记为部分结果"),
			"format":     mcp.Enum("import 的文件格式", "folded", "v8"),
			"path":       mcp.Str("import 的剖析结果文件路径"),
		}, "action"),
		Handler: handleProfile,
	})
}

func handleProfile(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var args profileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return failResult(model.New(model.CodeInputInvalid, "参数非法: %v", err)), nil
	}
	switch args.Action {
	case "import":
		// 导入已有剖析文件不需要会话：它是纯解析，不驱动目标。
		report, err := importProfile(args)
		if err != nil {
			return failResult(err), nil
		}
		pageReport(report, args)
		return mcp.Text(report), nil
	case "sample", "start", "stop", "status":
	default:
		return failResult(model.New(model.CodeInputInvalid, "不支持的动作: %s", args.Action).
			WithFix("可用动作: sample / start / stop / status / import")), nil
	}
	session, err := Sessions.Get(args.Session)
	if err != nil {
		return failResult(err), nil
	}
	if args.Action != "sample" {
		return handleExternal(ctx, session, args)
	}
	if session.Tracing() {
		return failResult(model.New(model.CodeStateNotRunning,
			"采集进行中，无法同时做中断采样").WithFix("先停止 debug_tracepoint 的采集")), nil
	}
	dbg, err := session.Debugger(ctx)
	if err != nil {
		return failResult(err), nil
	}
	interval := time.Duration(args.IntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = defaultSampleInterval
	}
	count := args.Samples
	if count <= 0 {
		count = defaultSampleCount
	}
	if count > maxSampleCount {
		count = maxSampleCount
	}
	report, err := sampleStacks(ctx, session, dbg, sampleOptions{
		interval: interval,
		count:    count,
		focus:    args.Focus,
	})
	if err != nil {
		return failResult(err), nil
	}
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	top := args.Top
	if top <= 0 {
		top = defaultTopEntries
	}
	report.Offset = offset
	report.Hot = pageSlice(report.Hot, offset, top)
	report.Cumulative = pageSlice(report.Cumulative, offset, top)
	report.Callers = pageSlice(report.Callers, offset, top)
	return mcp.Text(report), nil
}

// pageSlice 按 offset/top 截取列表；总量由调用方从 total* 字段读取。
func pageSlice[T any](list []T, offset, top int) []T {
	if offset >= len(list) {
		return nil
	}
	if offset > 0 {
		list = list[offset:]
	}
	if len(list) > top {
		list = list[:top]
	}
	return list
}

// ensureRunning 让目标进入运行态，返回是否由本函数恢复。
func ensureRunning(ctx context.Context, dbg backend.Debugger) error {
	if dbg.CurrentStop() == nil {
		return nil
	}
	if _, err := dbg.Continue(ctx, resumeWait); err != nil && !errors.Is(err, backend.ErrStillRunning) {
		return model.New(model.CodeOf(err), "目标未能恢复运行，无法采样: %v", err).
			WithFix("确认没有命中普通断点；需要交互时先处理该断点")
	}
	return nil
}

// sampleStacks 按间隔中断取栈并统计三种视图。
func sampleStacks(ctx context.Context, session *Session, dbg backend.Debugger, opts sampleOptions) (*sampleReport, error) {
	if err := ensureRunning(ctx, dbg); err != nil {
		return nil, err
	}
	session.MarkRunning()
	report := &sampleReport{
		Tool:       "interrupt-sample",
		IntervalMs: int(opts.interval / time.Millisecond),
		Overhead:   overheadLabel(opts.interval),
		Notes: []string{
			"统计值非精确计数：样本按定时中断采集，代表采样瞬间仍在帧上的概率",
			"每次采样都会短暂停止目标，间隔越小对时序与短跑程序影响越大",
			"视图：hot 按自身样本、cumulative 按累计样本（函数出现在栈上）、callers 需指定 focus",
		},
	}
	acc := newProfileAcc()
	instants := make([]sampleInstant, 0, opts.count)
	start := time.Now()
	for i := 0; i < opts.count; i++ {
		if err := sleepCtx(ctx, opts.interval); err != nil {
			break
		}
		stop, err := dbg.Pause(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			report.Failed++
			break
		}
		if stop != nil && stop.Snapshot != nil && len(stop.Snapshot.Frames) > 0 {
			report.Samples++
			instants = append(instants, acc.add(stop.Snapshot.Frames, time.Now()))
		}
		if err := resumeAfterSample(ctx, session, dbg); err != nil {
			report.Notes = append(report.Notes, err.Error())
			break
		}
	}
	report.DurationMs = int(time.Since(start) / time.Millisecond)
	if report.Samples == 0 {
		return nil, model.New(model.CodeStateNotStopped, "未采集到任何样本：目标可能已退出或始终处于停止态").
			WithFix("确认目标在运行（debug_run 的 continue），或先用采集点定位执行流")
	}
	report.started = start
	report.instants = instants
	report.Focus = opts.focus
	report.Hot = acc.rankSelf(report.Samples)
	report.Cumulative = acc.rankCumulative(report.Samples)
	report.Callers = acc.rankCallers(opts.focus, report.Samples)
	report.Edges = acc.rankEdges()
	report.TotalSelf = len(report.Hot)
	report.TotalCumulative = len(report.Cumulative)
	report.TotalCallers = len(report.Callers)
	if opts.focus != "" && len(report.Callers) == 0 {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"采样栈中未出现函数 %s：它可能不在采样窗口内，或名称与调试符号不一致", opts.focus))
	}
	session.setProfile(report)
	return report, nil
}

// sleepCtx 等待指定时长，可被取消。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// profileAcc 累积一次采样过程中的计数。
type profileAcc struct {
	self    map[string]int            // 帧键 → 自身样本数
	cum     map[string]int            // 函数名 → 出现在栈上的样本数
	edges   map[string]int            // "caller -> callee" → 次数
	callers map[string]map[string]int // callee → caller → 次数
	meta    map[string]hotSpot        // 帧键 → 位置信息
	cumMeta map[string]hotSpot        // 函数名 → 首次出现的位置
}

// newProfileAcc 创建累加器。
func newProfileAcc() *profileAcc {
	return &profileAcc{
		self:    make(map[string]int),
		cum:     make(map[string]int),
		edges:   make(map[string]int),
		callers: make(map[string]map[string]int),
		meta:    make(map[string]hotSpot),
		cumMeta: make(map[string]hotSpot),
	}
}

// add 记一次样本，返回该样本在时间线上的落点。
func (a *profileAcc) add(frames []model.Frame, at time.Time) sampleInstant {
	leaf := frames[0]
	key := frameKey(leaf)
	a.self[key]++
	if _, ok := a.meta[key]; !ok {
		a.meta[key] = hotSpot{Func: displayFunc(leaf), File: leaf.Where.File, Line: leaf.Where.Line}
	}
	names := make([]string, 0, len(frames))
	seen := make(map[string]bool, len(frames))
	for _, frame := range frames {
		name := displayFunc(frame)
		names = append(names, name)
		if seen[name] {
			continue
		}
		seen[name] = true
		a.cum[name]++
		if _, ok := a.cumMeta[name]; !ok {
			a.cumMeta[name] = hotSpot{Func: name, File: frame.Where.File, Line: frame.Where.Line}
		}
	}
	for i := 0; i+1 < len(frames); i++ {
		caller := displayFunc(frames[i+1])
		callee := displayFunc(frames[i])
		a.edges[caller+" -> "+callee]++
		if a.callers[callee] == nil {
			a.callers[callee] = make(map[string]int)
		}
		a.callers[callee][caller]++
	}
	return sampleInstant{At: at, Leaf: displayFunc(leaf), Frames: names}
}

// frameKey 用函数名与位置标识一帧。
func frameKey(frame model.Frame) string {
	return fmt.Sprintf("%s|%s:%d", displayFunc(frame), frame.Where.File, frame.Where.Line)
}

// displayFunc 取可读的函数名，空名时用占位。
func displayFunc(frame model.Frame) string {
	if frame.Name != "" {
		return frame.Name
	}
	if frame.Where.Func != "" {
		return frame.Where.Func
	}
	return "<unknown>"
}

// resumeAfterSample 恢复运行；若停在普通断点上则结束采样。
func resumeAfterSample(ctx context.Context, session *Session, dbg backend.Debugger) error {
	stop, err := dbg.Continue(ctx, resumeWait)
	if errors.Is(err, backend.ErrStillRunning) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("恢复运行失败，采样提前结束: %v", err)
	}
	if stop != nil {
		session.ApplyStop(stop)
		return fmt.Errorf("目标在断点 %s 处停下，采样提前结束", stop.BreakpointID)
	}
	return nil
}

// overheadLabel 按间隔给出开销等级说明。
func overheadLabel(interval time.Duration) string {
	switch {
	case interval < 10*time.Millisecond:
		return "高：间隔小于 10ms，目标会被频繁中断，明显改变时序"
	case interval <= 50*time.Millisecond:
		return "中：间隔 10–50ms，短跑程序的总耗时可能被拉长"
	}
	return "低：间隔大于 50ms，停顿影响小但采样密度低"
}

// rankSelf 按自身样本数排序热点。
func (a *profileAcc) rankSelf(samples int) []hotSpot {
	out := make([]hotSpot, 0, len(a.self))
	for key, n := range a.self {
		spot := a.meta[key]
		spot.Self = n
		spot.Pct = ratio(n, samples)
		out = append(out, spot)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self > out[j].Self
		}
		return out[i].Func < out[j].Func
	})
	return out
}

// rankCumulative 按累计样本数排序：函数出现在栈上的样本数。
func (a *profileAcc) rankCumulative(samples int) []cumEntry {
	out := make([]cumEntry, 0, len(a.cum))
	for name, n := range a.cum {
		entry := cumEntry{Func: name, Samples: n, Pct: ratio(n, samples)}
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

// rankCallers 返回指定函数的调用来源分布；占比以该函数出现的样本数为分母。
func (a *profileAcc) rankCallers(focus string, _ int) []callerEntry {
	callers, ok := a.callers[focus]
	if !ok {
		return nil
	}
	denominator := a.cum[focus]
	out := make([]callerEntry, 0, len(callers))
	for caller, n := range callers {
		out = append(out, callerEntry{Caller: caller, Count: n, Pct: ratio(n, denominator)})
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
func (a *profileAcc) rankEdges() []callEdge {
	out := make([]callEdge, 0, len(a.edges))
	for key, n := range a.edges {
		caller, callee, ok := splitEdge(key)
		if !ok {
			continue
		}
		out = append(out, callEdge{Caller: caller, Callee: callee, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Callee < out[j].Callee
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
