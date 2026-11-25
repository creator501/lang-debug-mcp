package dap

import (
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// scalarConverter 是只保留标量信息的转换器，用于隔离容器组装逻辑。
func scalarConverter(v variable) *model.Value {
	return &model.Value{Type: v.Type, Kind: model.KindScalar, Display: v.Value}
}

func TestBuildContainerListFiltersPseudoEntries(t *testing.T) {
	vars := []variable{
		{Name: "special variables", VariablesReference: 9},
		{Name: "function variables", VariablesReference: 10},
		{Name: "000", Value: "1", Type: "int"},
		{Name: "001", Value: "2", Type: "int"},
		{Name: "more", VariablesReference: 11},
		{Name: "len()", Value: "120", Type: "int"},
	}
	got := buildContainer("list", vars, scalarConverter)
	if got.Kind != model.KindList {
		t.Fatalf("列表应映射为 KindList，实际 %s", got.Kind)
	}
	if len(got.Items) != 2 {
		t.Fatalf("伪变量与续页标记必须被过滤，实际 %d 项", len(got.Items))
	}
	if got.Total != 120 {
		t.Fatalf("逻辑长度应取自 len()，实际 %d", got.Total)
	}
	if got.Items[0].Display != "1" || got.Items[1].Display != "2" {
		t.Fatalf("元素顺序不符: %+v", got.Items)
	}
}

func TestBuildContainerDictKeepsInsertionOrder(t *testing.T) {
	vars := []variable{
		{Name: "special variables", VariablesReference: 3},
		{Name: "'z'", Value: "1", Type: "int"},
		{Name: "'a'", Value: "'x'", Type: "str"},
		{Name: "3", Value: "None", Type: "NoneType"},
		{Name: "len()", Value: "3", Type: "int"},
	}
	got := buildContainer("dict", vars, scalarConverter)
	if got.Kind != model.KindObject {
		t.Fatalf("字典应映射为 KindObject，实际 %s", got.Kind)
	}
	if _, ok := got.Fields["z"]; !ok {
		t.Fatalf("字符串键必须去掉引号，实际字段: %v", got.Members())
	}
	if _, ok := got.Fields["3"]; !ok {
		t.Fatalf("数字键应保留为成员名，实际字段: %v", got.Members())
	}
	members := got.Members()
	want := []string{"z", "a", "3"}
	if len(members) != len(want) {
		t.Fatalf("成员数不符: %v", members)
	}
	for i := range want {
		if members[i] != want[i] {
			t.Fatalf("字典成员应保持插入顺序 %v，实际 %v", want, members)
		}
	}
}

func TestBuildContainerSetAndGenerator(t *testing.T) {
	set := buildContainer("set", []variable{
		{Name: "140718829274232", Value: "1", Type: "int"},
		{Name: "140718829274264", Value: "2", Type: "int"},
	}, scalarConverter)
	if set.Kind != model.KindList || len(set.Items) != 2 {
		t.Fatalf("集合应按序列展示: kind=%s items=%d", set.Kind, len(set.Items))
	}

	gen := buildContainer("generator", []variable{
		{Name: "gi_running", Value: "False", Type: "bool"},
	}, scalarConverter)
	if gen.Kind != model.KindObject {
		t.Fatalf("生成器应按对象展示（没有可枚举的剩余元素），实际 %s", gen.Kind)
	}
	if _, ok := gen.Fields["gi_running"]; !ok {
		t.Fatalf("生成器内部属性应作为成员: %v", gen.Members())
	}
}

func TestBuildContainerInstanceSkipsSpecial(t *testing.T) {
	got := buildContainer("Point", []variable{
		{Name: "special variables", VariablesReference: 4},
		{Name: "x", Value: "7", Type: "int"},
		{Name: "y", Value: "8", Type: "int"},
	}, scalarConverter)
	if got.Kind != model.KindObject {
		t.Fatalf("实例应映射为对象，实际 %s", got.Kind)
	}
	if got.Total != 2 {
		t.Fatalf("成员数应为 2，实际 %d", got.Total)
	}
	if members := got.Members(); len(members) != 2 || members[0] != "x" || members[1] != "y" {
		t.Fatalf("实例成员不符: %v", members)
	}
}

func TestFieldNameStripsOnlyPairedQuotes(t *testing.T) {
	cases := map[string]string{
		"'a'":  "a",
		`"a"`:  "a",
		"3":    "3",
		"'":    "'",
		"'a\"": "'a\"",
	}
	for in, want := range cases {
		if got := fieldName(in, true); got != want {
			t.Fatalf("fieldName(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := fieldName("'a'", false); got != "'a'" {
		t.Fatalf("非映射类型不应去引号，实际 %q", got)
	}
}

func TestLiteralToExpr(t *testing.T) {
	cases := []struct {
		typ     string
		literal string
		want    string
	}{
		{"int", "42", "42"},
		{"float", "3.14", "3.14"},
		{"bool", "true", "True"},
		{"bool", "False", "False"},
		{"str", "abc", `"abc"`},
		{"NoneType", "None", "None"},
	}
	for _, c := range cases {
		got, err := literalToExpr(c.typ, c.literal)
		if err != nil {
			t.Fatalf("literalToExpr(%s, %q) 出错: %v", c.typ, c.literal, err)
		}
		if got != c.want {
			t.Fatalf("literalToExpr(%s, %q) = %q，期望 %q", c.typ, c.literal, got, c.want)
		}
	}
	if _, err := literalToExpr("int", "abc"); err == nil {
		t.Fatal("数字类型写入非数字必须失败")
	}
	if _, err := literalToExpr("list", "[]"); err == nil {
		t.Fatal("容器类型应返回 CAP_UNSUPPORTED")
	}
}
