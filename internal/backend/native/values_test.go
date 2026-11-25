package native

import (
	"testing"
)

func TestSequenceDetectsIndexedChildren(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  bool
	}{
		{"固定数组", []string{"[0]", "[1]"}, true},
		{"容器带内部字段", []string{"[0]", "[1]", "len", "capacity"}, true},
		{"结构体", []string{"x", "y"}, false},
		{"Rust 枚举", []string{"variant0", "variant1", "tag"}, false},
	}
	for _, c := range cases {
		if got := (Dialect{}).Sequence("int[2]", c.names); got != c.want {
			t.Fatalf("%s: Sequence = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestElementExcludesContainerBookkeeping(t *testing.T) {
	d := Dialect{}
	for _, name := range []string{"[0]", "[12]"} {
		if !d.Element(name) {
			t.Fatalf("%q 应算元素", name)
		}
	}
	for _, name := range []string{"len", "capacity", "ptr", "size", "buf"} {
		if d.Element(name) {
			t.Fatalf("%q 是容器内部字段，不应算元素", name)
		}
	}
}

func TestMemberNameKeepsNativeNames(t *testing.T) {
	if got := (Dialect{}).MemberName("point_t", "x"); got != "x" {
		t.Fatalf("成员名应原样保留，实际 %q", got)
	}
}

func TestScalarMarksOptimizedValues(t *testing.T) {
	d := Dialect{}
	for _, display := range []string{"<optimized out>", "value optimized out", "<unavailable>"} {
		val := d.Scalar("int", display)
		if val == nil || !val.Optimized {
			t.Fatalf("%q 应被标记为已优化掉", display)
		}
	}
	for _, display := range []string{"0", "<invalid>", ""} {
		if val := d.Scalar("int", display); val != nil {
			t.Fatalf("%q 不应被标记，实际 %+v", display, val)
		}
	}
}
