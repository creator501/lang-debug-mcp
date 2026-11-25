package nodejs

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDomainsForVersion(t *testing.T) {
	cases := []struct {
		major    int
		debugger bool
		profiler bool
	}{
		{6, false, false},
		{8, true, false},
		{10, true, true},
		{18, true, true},
		{24, true, true},
	}
	for _, c := range cases {
		got := domainsFor(c.major)
		if got.Debugger != c.debugger || got.Profiler != c.profiler {
			t.Fatalf("Node %d 的可用域不符: %+v", c.major, got)
		}
	}
}

func TestParseMajor(t *testing.T) {
	cases := map[string]int{
		"v24.17.0": 24,
		"v8.0.0":   8,
		"24.1.0":   24,
		"unknown":  0,
		"":         0,
	}
	for in, want := range cases {
		if got := parseMajor(in); got != want {
			t.Fatalf("parseMajor(%q) = %d, 期望 %d", in, got, want)
		}
	}
}

func TestInjectInspectArgs(t *testing.T) {
	args, err := injectInspectArgs([]string{"node", "app.js", "--flag"}, 9229)
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	if len(args) != 4 || args[0] != "node" || args[1] != "--inspect-brk=127.0.0.1:9229" {
		t.Fatalf("注入结果不符: %v", args)
	}
	if args[2] != "app.js" || args[3] != "--flag" {
		t.Fatalf("原有参数被破坏: %v", args)
	}

	if _, err := injectInspectArgs([]string{"npm", "start"}, 9229); err == nil {
		t.Fatal("非 node 启动应给出明确错误而不是静默失败")
	} else if err.Error() == "" {
		t.Fatal("错误应可读")
	}
	if _, err := injectInspectArgs(nil, 9229); err == nil {
		t.Fatal("空命令应报错")
	}
}

func TestHitConditionMet(t *testing.T) {
	cases := []struct {
		expr string
		hits int
		want bool
	}{
		{"", 1, true},
		{">=3", 2, false},
		{">=3", 3, true},
		{"==2", 2, true},
		{"<2", 1, true},
		{"2", 1, false},
		{"2", 2, true},
		{"非法", 5, true},
	}
	for _, c := range cases {
		if got := hitConditionMet(c.expr, c.hits); got != c.want {
			t.Fatalf("hitConditionMet(%q, %d) = %v, 期望 %v", c.expr, c.hits, got, c.want)
		}
	}
}

func TestURLToPath(t *testing.T) {
	if got := urlToPath("file:///home/dev/app.js"); filepath.ToSlash(got) != "/home/dev/app.js" {
		t.Fatalf("Unix 形式的 URL 转路径不符: %q", got)
	}
	if got := urlToPath(""); got != "" {
		t.Fatalf("空 URL 应返回空: %q", got)
	}
	if got := urlToPath("file:///C:/work/app.js?v=1"); filepath.Base(got) != "app.js" {
		t.Fatalf("应去掉查询串: %q", got)
	}
}

func TestArrayLength(t *testing.T) {
	cases := map[string]int{
		"Array(3)":      3,
		"Uint8Array(4)": 4,
		"Object":        0,
		"":              0,
	}
	for in, want := range cases {
		if got := arrayLength(in); got != want {
			t.Fatalf("arrayLength(%q) = %d, 期望 %d", in, got, want)
		}
	}
}

// TestDomainsMatchRuntime 用真实 Node 校验版本推导的可用域与实际一致。
func TestDomainsMatchRuntime(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("未安装 node")
	}
	probe := Prober{}.Probe()
	if !probe.Available {
		t.Fatalf("Node 应可用: %+v", probe)
	}
	major := parseMajor(probe.Version)
	want := domainsFor(major)

	port, err := pickFreePort()
	if err != nil {
		t.Fatalf("申请端口失败: %v", err)
	}
	adapter, err := Launcher{}.Open(context.Background(), launchSpecForTest(port))
	if err != nil {
		t.Fatalf("打开后端失败: %v", err)
	}
	defer adapter.Close()

	var schema struct {
		Domains []struct {
			Name string `json:"name"`
		} `json:"domains"`
	}
	if err := adapter.(*Adapter).client.call(context.Background(), "Schema.getDomains", nil, &schema); err != nil {
		t.Fatalf("查询域失败: %v", err)
	}
	available := map[string]bool{}
	for _, d := range schema.Domains {
		available[d.Name] = true
	}
	if available["Debugger"] != want.Debugger {
		t.Fatalf("Debugger 域可用性不符: 实际 %v，推导 %v", available["Debugger"], want.Debugger)
	}
	if available["Runtime"] != want.Runtime {
		t.Fatalf("Runtime 域可用性不符")
	}
}
