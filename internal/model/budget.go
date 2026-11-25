package model

// Budget 是单次取值的返回体预算。
//
// 存在的目的不是省内存，而是省上下文：模型不需要整棵对象图，需要的是够用的
// 切片加上明确的截断标记。
type Budget struct {
	// Depth 是允许展开的层级数。
	Depth int `json:"depth"`
	// MaxItems 是单个集合允许返回的元素数。
	MaxItems int `json:"maxItems"`
	// MaxStr 是字符串允许返回的字符数。
	MaxStr int `json:"maxStr"`
}

// 预算默认值：保守优先，调用方可显式放宽。
const (
	DefaultDepth    = 1
	DefaultMaxItems = 20
	DefaultMaxStr   = 200
)

// DefaultBudget 返回默认预算。
func DefaultBudget() Budget {
	return Budget{Depth: DefaultDepth, MaxItems: DefaultMaxItems, MaxStr: DefaultMaxStr}
}

// Normalize 把非法取值回落到默认值，并限制上界避免单次返回失控。
func (b Budget) Normalize() Budget {
	out := b
	if out.Depth <= 0 {
		out.Depth = DefaultDepth
	}
	if out.Depth > 8 {
		out.Depth = 8
	}
	if out.MaxItems <= 0 {
		out.MaxItems = DefaultMaxItems
	}
	if out.MaxItems > 500 {
		out.MaxItems = 500
	}
	if out.MaxStr <= 0 {
		out.MaxStr = DefaultMaxStr
	}
	if out.MaxStr > 4096 {
		out.MaxStr = 4096
	}
	return out
}

// ClipString 按预算截断字符串，第二个返回值表示是否发生截断。
func (b Budget) ClipString(s string) (string, bool) {
	limit := b.MaxStr
	if limit <= 0 {
		limit = DefaultMaxStr
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s, false
	}
	return string(runes[:limit]), true
}
