package model

import "testing"

func TestBudgetNormalize(t *testing.T) {
	def := DefaultBudget()
	if def.Depth != DefaultDepth || def.MaxItems != DefaultMaxItems || def.MaxStr != DefaultMaxStr {
		t.Fatalf("默认预算不符: %+v", def)
	}
	zero := Budget{}.Normalize()
	if zero != def {
		t.Fatalf("零值预算应回落到默认: %+v", zero)
	}
	huge := Budget{Depth: 100, MaxItems: 10000, MaxStr: 100000}.Normalize()
	if huge.Depth != 8 || huge.MaxItems != 500 || huge.MaxStr != 4096 {
		t.Fatalf("超限预算应被夹紧: %+v", huge)
	}
}

func TestBudgetClipString(t *testing.T) {
	b := Budget{MaxStr: 5}.Normalize()
	got, clipped := b.ClipString("一二三四五六七")
	if !clipped || []rune(got)[4] != '五' || len([]rune(got)) != 5 {
		t.Fatalf("截断结果不符: %q clipped=%v", got, clipped)
	}
	short, clipped := b.ClipString("短")
	if clipped || short != "短" {
		t.Fatal("未超长不应截断")
	}
}
