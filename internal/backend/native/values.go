package native

import (
	"regexp"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// indexNamePattern 匹配 lldb 给数组/切片元素的子项名（[0]、[12]）。
var indexNamePattern = regexp.MustCompile(`^\[\d+\]$`)

// optimizedOutMarkers 是调试器标记“值已不存在”的写法。
var optimizedOutMarkers = []string{
	"optimized out", "optimized away", "<unavailable>",
}

// Sequence 实现 dap 的值渲染方言：出现 [n] 子项即按序列组装。
//
// lldb 对固定数组、std::vector、Vec 的元素用 [n] 命名；容器同时会带上 size、
// capacity、ptr 这类内部字段，它们由 Element 排除，不计入元素。
func (Dialect) Sequence(_ string, names []string) bool {
	for _, name := range names {
		if indexNamePattern.MatchString(name) {
			return true
		}
	}
	return false
}

// Element 实现 dap 的值渲染方言：只有 [n] 形式才是元素。
func (Dialect) Element(name string) bool { return indexNamePattern.MatchString(name) }

// MemberName 实现 dap 的值渲染方言：C/C++/Rust 的结构体字段名原样使用。
func (Dialect) MemberName(_, name string) string { return name }

// Scalar 实现 dap 的值渲染方言：标出已被优化掉的变量。
//
// 返回 null 或返回别名的变量不要动：只有显示文本明确说“优化掉”时才标记，
// 否则会把可读的值误报为不可恢复。
func (Dialect) Scalar(typ, display string) *model.Value {
	if !isOptimizedOut(display) {
		return nil
	}
	return &model.Value{Type: typ, Kind: model.KindScalar, Display: display, Optimized: true}
}

// isOptimizedOut 报告显示文本是否表示值已不存在。
func isOptimizedOut(display string) bool {
	lower := strings.ToLower(display)
	for _, marker := range optimizedOutMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
