package dap

import (
	"context"
	"strconv"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// variable 是 DAP variables 响应中的一项。
type variable struct {
	Name               string            `json:"name"`
	Value              string            `json:"value"`
	Type               string            `json:"type"`
	VariablesReference int               `json:"variablesReference"`
	EvaluateName       string            `json:"evaluateName"`
	PresentationHint   *presentationHint `json:"presentationHint"`
}

// presentationHint 是变量或作用域的展示提示。
type presentationHint struct {
	Kind       string   `json:"kind"`
	Attributes []string `json:"attributes"`
}

// debugpy 在容器与作用域里插入的分组伪变量：展开它们只会得到内建函数与
// 类型属性，对定位缺陷没有价值，反而会淹没真正的局部变量。
var pseudoVariables = map[string]bool{
	"special variables":  true,
	"function variables": true,
	"class variables":    true,
}

const (
	// moreVariableName 是 pydevd 分页时的续页标记。
	moreVariableName = "more"
	// lenVariableName 是容器长度伪变量，用于还原被分页隐藏的逻辑长度。
	lenVariableName = "len()"
	// maxVariablePages 是单次展开跟随续页的上限，避免超大容器拖垮一次取值。
	maxVariablePages = 5
)

// Variables 取回一个可展开节点的子变量，必要时跟随续页。
func (c *Client) Variables(ctx context.Context, ref int) ([]variable, error) {
	out := make([]variable, 0, 32)
	next := ref
	for page := 0; page < maxVariablePages && next > 0; page++ {
		var resp struct {
			Variables []variable `json:"variables"`
		}
		if err := c.Request(ctx, "variables", map[string]any{"variablesReference": next}, &resp); err != nil {
			if page == 0 {
				return nil, err
			}
			break
		}
		out = append(out, resp.Variables...)
		next = 0
		for _, v := range resp.Variables {
			if v.Name == moreVariableName {
				next = v.VariablesReference
				break
			}
		}
	}
	return out, nil
}

// valueDialect 是方言可选实现的值渲染差异。
//
// 协议层知道“这个容器有子项”，但判断哪些子项构成序列、成员名怎么规范化、
// 标量是否已被优化掉，属于语言与调试器的约定。
type valueDialect interface {
	// Sequence 报告子项是否按有序序列组装。
	Sequence(typ string, names []string) bool
	// Element 报告序列里的某个子项名是否算一个元素（内部字段不算）。
	Element(name string) bool
	// MemberName 规范化成员名（映射类型的键去引号等）。
	MemberName(typ, name string) string
	// Scalar 修饰标量值；返回 nil 表示用默认构造。
	Scalar(typ, display string) *model.Value
}

// defaultValues 是通用渲染规则：保持既有适配器（debugpy 等）的行为。
type defaultValues struct{}

func (defaultValues) Sequence(typ string, _ []string) bool { return isSequenceType(typ) }

func (defaultValues) Element(string) bool { return true }

func (defaultValues) MemberName(typ, name string) string {
	return fieldName(name, isDictType(typ))
}

func (defaultValues) Scalar(string, string) *model.Value { return nil }

// values 返回本会话生效的渲染规则。
func (s *Session) values() valueDialect {
	if d, ok := s.dialect.(valueDialect); ok {
		return d
	}
	return defaultValues{}
}

// variableToValue 把 DAP 变量转换为统一模型值。
//
// 复合值一律惰性：Expand 只在真正被路径访问到时才发起 variables 请求，
// 因此读一个深层字段不会先展开整棵对象图。
func (s *Session) variableToValue(v variable) *model.Value {
	typ := v.Type
	if typ == "" {
		typ = "object"
	}
	if v.VariablesReference > 0 {
		ref := v.VariablesReference
		val := &model.Value{
			Type:   typ,
			Kind:   kindForType(typ),
			Handle: v.EvaluateName,
		}
		val.Expand = func() (*model.Value, error) { return s.expandContainer(ref, typ) }
		return val
	}
	val := s.values().Scalar(typ, v.Value)
	if val == nil {
		val = &model.Value{Kind: model.KindScalar, Display: v.Value}
	}
	val.Type = typ
	val.Handle = v.EvaluateName
	return val
}

// expandContainer 取回子结果并组装为容器节点。
func (s *Session) expandContainer(ref int, typ string) (*model.Value, error) {
	vars, err := s.client.Variables(context.Background(), ref)
	if err != nil {
		return nil, err
	}
	return buildContainerWith(typ, vars, s.variableToValue, s.values()), nil
}

// scopeValue 构造一个按需展开的作用域节点。
func (s *Session) scopeValue(ref int, name string) *model.Value {
	v := &model.Value{Type: name, Kind: model.KindObject}
	v.Expand = func() (*model.Value, error) {
		vars, err := s.client.Variables(context.Background(), ref)
		if err != nil {
			return nil, err
		}
		out := buildContainerWith(name, vars, s.variableToValue, s.values())
		out.Type = name
		return out, nil
	}
	return v
}

// buildContainer 由子变量列表按默认规则组装容器节点。
//
// convert 作为参数传入，使该函数可以脱离会话做纯函数测试。
func buildContainer(typ string, vars []variable, convert func(variable) *model.Value) *model.Value {
	return buildContainerWith(typ, vars, convert, defaultValues{})
}

// buildContainerWith 按方言规则组装容器节点。
func buildContainerWith(typ string, vars []variable, convert func(variable) *model.Value, rules valueDialect) *model.Value {
	v := &model.Value{Type: typ, Kind: model.KindObject}
	children := make([]variable, 0, len(vars))
	names := make([]string, 0, len(vars))
	total := 0
	for _, child := range vars {
		switch {
		case child.Name == moreVariableName:
			continue
		case child.Name == lenVariableName:
			if n, err := strconv.Atoi(strings.TrimSpace(child.Value)); err == nil && n > total {
				total = n
			}
			continue
		case pseudoVariables[child.Name]:
			continue
		}
		children = append(children, child)
		names = append(names, child.Name)
	}

	if rules.Sequence(typ, names) {
		v.Kind = model.KindList
		v.Items = make([]*model.Value, 0, len(children))
		for _, child := range children {
			if !rules.Element(child.Name) {
				continue
			}
			v.Items = append(v.Items, convert(child))
		}
		if total < len(v.Items) {
			total = len(v.Items)
		}
		v.Total = total
		return v
	}

	v.Fields = make(map[string]*model.Value, len(children))
	order := make([]string, 0, len(children))
	for _, child := range children {
		name := rules.MemberName(typ, child.Name)
		if name == "" {
			continue
		}
		if _, exists := v.Fields[name]; !exists {
			order = append(order, name)
		}
		v.Fields[name] = convert(child)
	}
	v.Order = order
	if total < len(order) {
		total = len(order)
	}
	if total > 0 {
		v.Total = total
	}
	return v
}

// fieldName 把 DAP 变量名转换为路径可用的成员名。
//
// debugpy 的字典键以 repr 形式出现（字符串键带引号、数字键不带），而路径语法
// 使用裸名称；这里只去掉成对的引号，不做任何求值。
func fieldName(name string, dictLike bool) string {
	if !dictLike || len(name) < 2 {
		return name
	}
	first, last := name[0], name[len(name)-1]
	if first == last && (first == '\'' || first == '"') {
		return name[1 : len(name)-1]
	}
	return name
}

// isDictType 报告某类型是否按映射看待（键作为成员名）。
func isDictType(typ string) bool {
	switch typ {
	case "dict", "OrderedDict", "defaultdict", "mappingproxy", "Counter", "ChainMap":
		return true
	}
	return false
}

// isSequenceType 报告某类型是否按有序序列看待。
//
// 生成器与协程不在此列：它们没有可枚举的剩余元素，展开得到的是 gi_*/cr_*
// 内部属性，当作序列展示会误导调用方。
func isSequenceType(typ string) bool {
	switch typ {
	case "list", "tuple", "set", "frozenset", "range", "bytes", "bytearray",
		"memoryview", "deque", "dict_keys", "dict_values", "dict_items",
		"zip", "enumerate", "map", "filter", "reversed", "array":
		return true
	}
	return false
}

func kindForType(typ string) model.Kind {
	if isSequenceType(typ) {
		return model.KindList
	}
	return model.KindObject
}
