package model

import (
	"errors"
	"fmt"
	"strconv"
)

// Entry 是单条路径的取值结果。
//
// 键使用扁平路径而非嵌套结构：模型无需解析层级即可定位值，也避免了重复的
// 键名占用上下文。
type Entry struct {
	Type      string `json:"t,omitempty"`
	Value     string `json:"v,omitempty"`
	Total     int    `json:"n,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Optimized bool   `json:"optimized,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ResolveResult 是一次批量取值的结果。
type ResolveResult struct {
	Values map[string]Entry `json:"values"`
	// Clips 是发生裁剪的位置说明。凡裁剪必须显式标注，否则调用方会把截断
	// 数据误认为全量。
	Clips []string `json:"clipped,omitempty"`
	// Nodes 是实际产出的条目数。
	Nodes int `json:"nodes"`
	// Expands 是实际触发的后端展开次数，用于验证共享父节点只遍历一次。
	Expands int `json:"expands"`
}

// Resolve 按批量路径取值。
//
// 单条路径失败不影响其余路径；仅当全部路径都无法解析时才整体失败。
func (s *Snapshot) Resolve(requests []string, budget Budget) (*ResolveResult, error) {
	if len(requests) == 0 {
		return nil, New(CodeInputInvalid, "至少需要一条取值路径").
			WithFix(`示例: ["this", "args.*", "this.items[0..4]"]`)
	}
	if !s.IsValid() {
		return nil, s.staleError()
	}

	r := &resolver{
		snap:     s,
		budget:   budget.Normalize(),
		expanded: make(map[*Value]*Value),
		values:   make(map[string]Entry),
	}

	var parseErrs []error
	for _, raw := range requests {
		err := r.resolveOne(raw)
		if err == nil {
			continue
		}
		var coded *Error
		if errors.As(err, &coded) && coded.Code == CodeInputInvalid {
			parseErrs = append(parseErrs, err)
		}
		if _, exists := r.values[raw]; !exists {
			r.values[raw] = entryForError(err)
		}
	}
	if len(parseErrs) > 0 && len(parseErrs) == len(requests) {
		return nil, parseErrs[0]
	}
	return &ResolveResult{Values: r.values, Clips: r.clips, Nodes: r.nodes, Expands: r.expands}, nil
}

func entryForError(err error) Entry {
	if errors.Is(err, ErrOptimizedOut) {
		return Entry{Optimized: true, Error: err.Error()}
	}
	return Entry{Error: err.Error()}
}

type resolver struct {
	snap     *Snapshot
	budget   Budget
	expanded map[*Value]*Value
	values   map[string]Entry
	clips    []string
	nodes    int
	expands  int
}

func (r *resolver) clipf(format string, args ...any) {
	r.clips = append(r.clips, fmt.Sprintf(format, args...))
}

// expandValue 惰性展开节点，同一节点只触发一次后端往返。
func (r *resolver) expandValue(v *Value) *Value {
	if v == nil || v.Expand == nil {
		return v
	}
	if got, ok := r.expanded[v]; ok {
		return got
	}
	got, err := v.Expand()
	r.expands++
	if errors.Is(err, ErrOptimizedOut) {
		optimized := &Value{Type: v.Type, Optimized: true}
		r.expanded[v] = optimized
		return optimized
	}
	if err != nil || got == nil {
		r.expanded[v] = v
		return v
	}
	r.expanded[v] = got
	return got
}

func (r *resolver) resolveOne(raw string) error {
	p, err := ParsePath(raw)
	if err != nil {
		return err
	}
	node, err := r.snap.Scope(p.Frame, p.Scope)
	if err != nil {
		return err
	}
	key := p.Scope
	if p.Frame != 0 {
		key = fmt.Sprintf("frame[%d].%s", p.Frame, p.Scope)
	}

	depth := 0
	for _, part := range p.Parts {
		depth++
		node = r.expandValue(node)
		if node == nil {
			return ErrNotFound
		}
		if part.Wild {
			r.emitChildren(node, key, depth)
			return nil
		}
		if part.Name != "" {
			child, ferr := node.Field(part.Name)
			if ferr != nil {
				return ferr
			}
			node = child
			key += "." + part.Name
		}
		node = r.expandValue(node)
		switch {
		case part.HasIndex:
			child, ierr := node.Item(part.Index)
			if ierr != nil {
				return ierr
			}
			node = child
			key = fmt.Sprintf("%s[%d]", key, part.Index)
		case part.HasRange:
			r.emitRange(node, key, part, depth)
			return nil
		}
	}
	return r.emit(node, key, depth)
}

func (r *resolver) emit(node *Value, key string, depth int) error {
	if node == nil {
		r.values[key] = Entry{Error: ErrNotFound.Error()}
		return nil
	}
	r.nodes++
	node = r.expandValue(node)
	if node.Optimized {
		r.values[key] = Entry{Type: node.Type, Optimized: true, Error: ErrOptimizedOut.Error()}
		return nil
	}
	if node.IsScalar() {
		value, clipped := r.budget.ClipString(node.Display)
		if clipped {
			r.clipf("%s: 字符串已截断（原长 %d 字符）", key, len([]rune(node.Display)))
		}
		r.values[key] = Entry{Type: node.Type, Value: value}
		return nil
	}
	if depth < r.budget.Depth {
		// 类型与总量属于元信息，不受深度预算裁剪：展开子项时也回填一条
		// 父节点概要，调用方无需遍历子项即可判断规模。
		r.values[key] = Entry{Type: node.Type, Total: node.Len()}
		r.emitChildren(node, key, depth)
		return nil
	}
	ref, err := r.snap.AddRef(node)
	if err != nil {
		return err
	}
	r.values[key] = Entry{Type: node.Type, Total: node.Len(), Ref: ref}
	return nil
}

func (r *resolver) emitChildren(node *Value, key string, depth int) {
	node = r.expandValue(node)
	if node == nil {
		r.values[key] = Entry{Error: ErrNotFound.Error()}
		return
	}
	names := childKeys(node)
	if len(names) == 0 {
		// 后端未提供可枚举子节点，退化为可继续展开的引用。
		if ref, err := r.snap.AddRef(node); err == nil {
			r.values[key] = Entry{Type: node.Type, Total: node.Len(), Ref: ref}
			return
		}
		r.values[key] = Entry{Type: node.Type, Total: node.Len()}
		return
	}
	if len(names) > r.budget.MaxItems {
		r.clipf("%s: 已返回 %d/%d 项", key, r.budget.MaxItems, len(names))
		names = names[:r.budget.MaxItems]
	}
	for _, name := range names {
		child, err := node.Field(name)
		if err != nil {
			r.values[childKey(key, name, node.Kind)] = entryForError(err)
			continue
		}
		_ = r.emit(child, childKey(key, name, node.Kind), depth+1)
	}
}

// childKey 按容器类别生成子节点键：序列用下标形式，对象用成员形式。
func childKey(base, name string, kind Kind) string {
	if kind == KindList {
		return base + "[" + name + "]"
	}
	return base + "." + name
}

func (r *resolver) emitRange(node *Value, key string, part Part, depth int) {
	if node == nil {
		r.values[key] = Entry{Error: ErrNotFound.Error()}
		return
	}
	total := node.Len()
	if part.From >= total {
		r.clipf("%s%s: 起始索引超出上限，实际总长 %d", key, rangeLabel(part), total)
		r.values[key] = Entry{Type: node.Type, Total: total}
		return
	}
	if !part.Open && part.To >= total {
		r.clipf("%s%s: 超出上限，实际总长 %d", key, rangeLabel(part), total)
	}
	upper := part.To
	if upper >= total {
		upper = total - 1
	}
	if upper-part.From+1 > r.budget.MaxItems {
		upper = part.From + r.budget.MaxItems - 1
		r.clipf("%s%s: 受预算限制只返回 %d 项（共 %d 项）", key, rangeLabel(part), r.budget.MaxItems, total)
	}
	for i := part.From; i <= upper; i++ {
		child, err := node.Item(i)
		if err != nil {
			r.values[key+"["+strconv.Itoa(i)+"]"] = entryForError(err)
			continue
		}
		// 区间与索引两种写法的展开层级保持一致，避免同一数据因写法不同而结果不同。
		_ = r.emit(child, fmt.Sprintf("%s[%d]", key, i), depth)
	}
}

// rangeLabel 生成区间的人类可读形式，开放区间显示为 [*]。
func rangeLabel(part Part) string {
	if part.Open {
		return "[*]"
	}
	return fmt.Sprintf("[%d..%d]", part.From, part.To)
}

// childKeys 返回可枚举子节点的稳定顺序键。
func childKeys(node *Value) []string {
	if node == nil {
		return nil
	}
	if node.Kind == KindList {
		n := len(node.Items)
		if node.Total > n {
			r := make([]string, 0, n)
			for i := 0; i < n; i++ {
				r = append(r, strconv.Itoa(i))
			}
			return r
		}
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, strconv.Itoa(i))
		}
		return out
	}
	return node.Members()
}
