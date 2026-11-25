package profile

import (
	"strings"
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// TestParseFoldedAndBuild 覆盖折叠栈解析与三种视图的聚合。
func TestParseFoldedAndBuild(t *testing.T) {
	text := strings.Join([]string{
		"main (app.py:10);run (app.py:4);parse (app.py:8) 30",
		"main (app.py:10);run (app.py:4);parse (app.py:8) 10",
		"main (app.py:10);io (app.py:20) 5",
		"(program) 7",
	}, "\n")
	samples, err := ParseFolded(strings.NewReader(text))
	if err != nil {
		t.Fatalf("解析折叠栈失败: %v", err)
	}
	// 噪声行在解析阶段保留，聚合阶段才过滤并计入 filtered。
	if len(samples) != 4 {
		t.Fatalf("应解析出 4 条栈（含噪声样本），实际 %d", len(samples))
	}
	if got := samples[0].Frames[0].Func; got != "parse" {
		t.Fatalf("叶帧应为 parse，实际 %q", got)
	}

	report := Build(Source{Tool: "py-spy", Format: FormatFolded, Kind: "import"},
		samples, Options{Focus: "parse"})
	if report.Samples != 45 {
		t.Fatalf("有效样本应为 45（噪声样本不进统计），实际 %d", report.Samples)
	}
	if report.Source != "import" || report.StackKind != "sync" {
		t.Fatalf("应如实标注来源与栈类型: %+v", report)
	}
	if len(report.Hot) == 0 || report.Hot[0].Func != "parse" || report.Hot[0].Self != 40 {
		t.Fatalf("自身样本最高者应为 parse(40): %+v", report.Hot)
	}
	if report.Hot[0].File != "app.py" || report.Hot[0].Line != 8 {
		t.Fatalf("自身条目应带位置: %+v", report.Hot[0])
	}
	if len(report.Cumulative) == 0 || report.Cumulative[0].Func != "main" ||
		report.Cumulative[0].Samples != 45 {
		t.Fatalf("累计样本最高者应为 main(45): %+v", report.Cumulative)
	}
	if len(report.Edges) == 0 {
		t.Fatal("应有调用边计数")
	}
	if len(report.Callers) == 0 || report.Callers[0].Caller != "run" || report.Callers[0].Count != 40 {
		t.Fatalf("parse 的调用来源应为 run(40): %+v", report.Callers)
	}
	if len(report.Filtered) == 0 {
		t.Fatal("噪声帧必须计入 filtered，而不是静默丢弃")
	}
	if !strings.Contains(strings.Join(report.Notes, "；"), "统计值") {
		t.Fatalf("必须声明统计性质: %v", report.Notes)
	}
	if !strings.Contains(strings.Join(report.Notes, "；"), "异步") {
		t.Fatalf("必须声明异步栈缺失: %v", report.Notes)
	}
	if report.TotalSelf != len(report.Hot) || report.TotalCumulative != len(report.Cumulative) {
		t.Fatalf("总量字段应等于视图条数: %+v", report)
	}
}

// TestParseV8RebuildsStacks 覆盖 V8 节点图的父子关系还原。
func TestParseV8RebuildsStacks(t *testing.T) {
	data := []byte(`{
	  "nodes": [
	    {"id": 1, "callFrame": {"functionName": "(root)", "url": "", "lineNumber": -1}, "children": [2]},
	    {"id": 2, "callFrame": {"functionName": "main", "url": "file:///app.js", "lineNumber": 9}, "children": [3]},
	    {"id": 3, "callFrame": {"functionName": "work", "url": "file:///app.js", "lineNumber": 13}, "children": []},
	    {"id": 4, "callFrame": {"functionName": "(program)", "url": "", "lineNumber": -1}, "children": []}
	  ],
	  "samples": [3, 3, 4]
	}`)
	samples, err := ParseV8(data)
	if err != nil {
		t.Fatalf("解析 V8 结果失败: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("应按 samples 生成 3 个样本，实际 %d", len(samples))
	}
	if len(samples[0].Frames) != 2 {
		t.Fatalf("work 的栈应为 work→main，实际 %+v", samples[0].Frames)
	}
	if samples[0].Frames[0].Func != "work" || samples[0].Frames[1].Func != "main" {
		t.Fatalf("栈顺序应为叶到根: %+v", samples[0].Frames)
	}
	if samples[0].Frames[0].Line != 14 {
		t.Fatalf("V8 行号应从 0 基还原成 1 基，实际 %d", samples[0].Frames[0].Line)
	}

	report := Build(Source{Tool: "V8 CPU profile", Format: FormatV8, Kind: "import"},
		samples, Options{})
	if report.Samples != 2 {
		t.Fatalf("(program) 样本应被过滤，有效样本为 2，实际 %d", report.Samples)
	}
	if report.Hot[0].Func != "work" || report.Hot[0].Self != 2 {
		t.Fatalf("自身样本最高者应为 work(2): %+v", report.Hot)
	}
}

// TestParseV8UsesHitCountWhenSamplesMissing 覆盖只有 hitCount 的来源。
func TestParseV8UsesHitCountWhenSamplesMissing(t *testing.T) {
	data := []byte(`{
	  "nodes": [
	    {"id": 1, "callFrame": {"functionName": "(root)", "url": "", "lineNumber": -1}, "children": [2]},
	    {"id": 2, "callFrame": {"functionName": "loop", "url": "file:///b.js", "lineNumber": 2}, "hitCount": 5, "children": []}
	  ]
	}`)
	samples, err := ParseV8(data)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(samples) != 1 || samples[0].Weight != 5 {
		t.Fatalf("应按 hitCount 生成权重 5 的样本: %+v", samples)
	}
}

// TestParseRejectsEmptyAndUnknown 覆盖空数据与未知格式。
func TestParseRejectsEmptyAndUnknown(t *testing.T) {
	if _, err := Parse(FormatFolded, []byte("\n\n")); err == nil {
		t.Fatal("空折叠栈应报错")
	}
	if _, err := Parse("speedscope", []byte("{}")); err == nil {
		t.Fatal("未支持的格式应报错")
	}
}

// TestPlansPerLanguage 覆盖各语言的外部剖析器计划与降级说明。
func TestPlansPerLanguage(t *testing.T) {
	python := Plans("python", 4321, 3, "out.folded")
	if len(python) != 1 || python[0].Command != "py-spy" || python[0].Format != FormatFolded {
		t.Fatalf("python 应给出 py-spy 折叠栈计划: %+v", python)
	}
	if !strings.Contains(strings.Join(python[0].Args, " "), "4321") {
		t.Fatalf("计划里应带上目标 pid: %v", python[0].Args)
	}
	jvm := Plans("java", 99, 3, "out.folded")
	if len(jvm) != 1 || jvm[0].Command != "asprof" {
		t.Fatalf("jvm 应给出 async-profiler 计划: %+v", jvm)
	}
	if got := Plans("nodejs", 1, 3, "out"); len(got) != 0 {
		t.Fatalf("node 没有可直接调用的外部剖析器，实际 %+v", got)
	}
	if guide := Guidance("nodejs"); !strings.Contains(guide, "--cpu-prof") ||
		!strings.Contains(guide, "import") {
		t.Fatalf("node 的替代路径应说明 --cpu-prof 与 import: %s", guide)
	}
	if guide := Guidance("c"); !strings.Contains(guide, "perf") || !strings.Contains(guide, "sample") {
		t.Fatalf("native 的替代路径应说明 perf 与中断采样: %s", guide)
	}
}

// TestProbeMissingProfiler 覆盖剖析器缺失时的错误码与安装指引。
func TestProbeMissingProfiler(t *testing.T) {
	_, err := Probe(Tool{Name: "不存在的剖析器", Command: "lang-debug-no-such-profiler", Install: "装一下"})
	if err == nil {
		t.Fatal("缺失的剖析器应报错")
	}
	if got := model.CodeOf(err); got != model.CodeEnvAdapterMissing {
		t.Fatalf("错误码应为 ENV_ADAPTER_MISSING，实际 %s", got)
	}
	if !strings.Contains(err.Error(), "装一下") {
		t.Fatalf("错误应带安装指引: %v", err)
	}
}
