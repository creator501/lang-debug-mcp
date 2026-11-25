package model

import "errors"

// Kind 描述值节点的类别。
type Kind string

// 值节点类别。
const (
	// KindScalar 表示可整体呈现的标量。
	KindScalar Kind = "scalar"
	// KindObject 表示具有命名成员的复合值。
	KindObject Kind = "object"
	// KindList 表示可按序索引的复合值。
	KindList Kind = "list"
)

// 取值过程中的语义化错误。二者必须区分：前者说明路径写错，后者说明代码
// 在当前优化级别下不允许观察到该变量。
var (
	// ErrNotFound 表示路径在目标中不存在。
	ErrNotFound = errors.New("路径不存在")
	// ErrOptimizedOut 表示变量已因优化而不存在于调试信息中。
	ErrOptimizedOut = errors.New("变量已被优化掉")
)

// Value 是后端返回的语言无关值节点。
//
// 复合值可以惰性构造：Expand 非空时表示成员尚未取回，取值器需要时再触发。
// 这让“读一个深层字段”不必先展开整棵对象图。
type Value struct {
	// Type 是短类型名，如 int、str、list、Map$Entry。
	Type string
	// Display 是标量的可读值；复合值通常为空。
	Display string
	// Kind 是节点类别。
	Kind Kind
	// Fields 是对象成员。
	Fields map[string]*Value
	// Order 是成员的后端原始顺序（如 Python 字典的插入顺序）；为空时按名排序。
	Order []string
	// Items 是序列元素，可能已被后端截断。
	Items []*Value
	// Total 是序列的逻辑总长度；0 表示非序列或长度未知。
	Total int
	// Optimized 表示该变量在当前优化级别下已不存在于调试信息中。
	// 必须与“路径不存在”区分：前者要提示降低优化级别，后者要提示路径写错。
	Optimized bool
	// Handle 是后端私有的节点句柄（如 CDP 的 objectId、lldb 的 SBValue 标识），
	// 供改值与惰性展开使用，不参与对外序列化。
	Handle string
	// Expand 在成员尚未取回时提供惰性构造。
	Expand func() (*Value, error)
}

// IsScalar 报告是否标量。
func (v *Value) IsScalar() bool {
	return v == nil || v.Kind == KindScalar
}

// Len 返回序列的逻辑长度。
func (v *Value) Len() int {
	if v == nil {
		return 0
	}
	if v.Total > 0 {
		return v.Total
	}
	return len(v.Items)
}

// Field 按名取成员。
func (v *Value) Field(name string) (*Value, error) {
	if v == nil {
		return nil, ErrNotFound
	}
	if child, ok := v.Fields[name]; ok {
		return child, nil
	}
	if v.Kind == KindList {
		if idx, ok := parseUint(name); ok {
			return v.Item(idx)
		}
	}
	return nil, ErrNotFound
}

// Item 按序号取元素。
func (v *Value) Item(idx int) (*Value, error) {
	if v == nil {
		return nil, ErrNotFound
	}
	if idx < 0 || idx >= len(v.Items) {
		if idx >= 0 && idx < v.Len() {
			return nil, ErrNotFound
		}
		return nil, ErrNotFound
	}
	return v.Items[idx], nil
}

// Members 返回对象成员名。
//
// 后端给出原始顺序时按该顺序返回（保证同一份数据每次渲染一致，也保留映射类型
// 的语义顺序）；否则按名排序，避免 map 迭代顺序随机化输出。
func (v *Value) Members() []string {
	if v == nil || len(v.Fields) == 0 {
		return nil
	}
	if len(v.Order) > 0 {
		out := make([]string, 0, len(v.Fields))
		seen := make(map[string]bool, len(v.Order))
		for _, name := range v.Order {
			if _, ok := v.Fields[name]; !ok || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
		rest := make([]string, 0, len(v.Fields)-len(out))
		for name := range v.Fields {
			if !seen[name] {
				rest = append(rest, name)
			}
		}
		if len(rest) > 0 {
			sortStrings(rest)
			out = append(out, rest...)
		}
		return out
	}
	names := make([]string, 0, len(v.Fields))
	for name := range v.Fields {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func parseUint(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
