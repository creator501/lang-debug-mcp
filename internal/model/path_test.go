package model

import "testing"

func TestParsePath(t *testing.T) {
	cases := []struct {
		raw   string
		frame int
		scope string
		parts []string
	}{
		{"this.items[0..19]", 0, ScopeThis, []string{"items[0..19]"}},
		{"frame[2].args.name", 2, ScopeArgs, []string{"name"}},
		{"locals.*", 0, ScopeLocals, []string{"*"}},
		{"retries", 0, ScopeLocals, []string{"retries"}},
		{"this.items[3]", 0, ScopeThis, []string{"items[3]"}},
		{"locals.items[*]", 0, ScopeLocals, []string{"items[*]"}},
		{"frame[1].locals.a.b", 1, ScopeLocals, []string{"a", "b"}},
	}
	for _, c := range cases {
		p, err := ParsePath(c.raw)
		if err != nil {
			t.Fatalf("%q 解析失败: %v", c.raw, err)
		}
		if p.Frame != c.frame || p.Scope != c.scope {
			t.Fatalf("%q 解析为 frame=%d scope=%s", c.raw, p.Frame, p.Scope)
		}
		if len(p.Parts) != len(c.parts) {
			t.Fatalf("%q 片段数 = %d, 期望 %d", c.raw, len(p.Parts), len(c.parts))
		}
		for i, want := range c.parts {
			if got := p.Parts[i].String(); got != want {
				t.Fatalf("%q 第 %d 段 = %q, 期望 %q", c.raw, i, got, want)
			}
		}
	}
}

func TestParsePathInvalid(t *testing.T) {
	bad := []string{"", "a[1", "a[3..1]", "a[x]", "a[..2]", "a..b"}
	for _, raw := range bad {
		if _, err := ParsePath(raw); err == nil {
			t.Fatalf("%q 应解析失败", raw)
		} else if CodeOf(err) != CodeInputInvalid {
			t.Fatalf("%q 的错误码不符: %s", raw, CodeOf(err))
		}
	}
}
