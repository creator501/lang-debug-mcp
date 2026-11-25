package nodejs

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// remoteObject 是 CDP 的值描述。
type remoteObject struct {
	Type                string          `json:"type"`
	Subtype             string          `json:"subtype"`
	ClassName           string          `json:"className"`
	Value               json.RawMessage `json:"value"`
	UnserializableValue string          `json:"unserializableValue"`
	Description         string          `json:"description"`
	ObjectID            string          `json:"objectId"`
}

// propertyDescriptor 是 Runtime.getProperties 的成员描述。
type propertyDescriptor struct {
	Name       string        `json:"name"`
	Value      *remoteObject `json:"value"`
	Writable   bool          `json:"writable"`
	Enumerable bool          `json:"enumerable"`
	IsOwn      bool          `json:"isOwn"`
}

// remoteToValue 把 CDP 值转换为统一模型值。
//
// 复合值一律惰性：只有真正被路径访问到时才发起 getProperties，避免读一个
// 字段就展开整棵对象图。
func (a *Adapter) remoteToValue(ro remoteObject) *model.Value {
	switch ro.Type {
	case "string":
		var s string
		_ = json.Unmarshal(ro.Value, &s)
		return &model.Value{Type: "str", Kind: model.KindScalar, Display: strconv.Quote(s)}
	case "number":
		display := string(ro.Value)
		if ro.UnserializableValue != "" {
			display = ro.UnserializableValue
		}
		return &model.Value{Type: "num", Kind: model.KindScalar, Display: display}
	case "boolean":
		return &model.Value{Type: "bool", Kind: model.KindScalar, Display: string(ro.Value)}
	case "undefined":
		return &model.Value{Type: "undefined", Kind: model.KindScalar, Display: "undefined"}
	case "bigint":
		return &model.Value{Type: "bigint", Kind: model.KindScalar, Display: ro.UnserializableValue}
	case "symbol":
		return &model.Value{Type: "symbol", Kind: model.KindScalar, Display: ro.Description}
	case "function":
		name := ro.Description
		if name == "" {
			name = "function"
		}
		return &model.Value{Type: "func", Kind: model.KindScalar, Display: name}
	case "object":
		return a.objectToValue(ro)
	}
	return &model.Value{Type: ro.Type, Kind: model.KindScalar, Display: ro.Description}
}

func (a *Adapter) objectToValue(ro remoteObject) *model.Value {
	switch ro.Subtype {
	case "null":
		return &model.Value{Type: "null", Kind: model.KindScalar, Display: "null"}
	case "error":
		return &model.Value{Type: "error", Kind: model.KindScalar, Display: ro.Description}
	}

	isList := ro.Subtype == "array" || ro.Subtype == "typedarray"
	typ := ro.ClassName
	if typ == "" {
		typ = "object"
	}
	if isList {
		typ = "list"
	}
	v := &model.Value{
		Type:   typ,
		Kind:   model.KindObject,
		Handle: ro.ObjectID,
	}
	if isList {
		v.Kind = model.KindList
		v.Total = arrayLength(ro.Description)
	}
	objectID := ro.ObjectID
	list := isList
	if objectID != "" {
		v.Expand = func() (*model.Value, error) { return a.expandObject(objectID, list, typ) }
	}
	return v
}

// arrayLength 从 "Array(3)" 形式的描述中取出长度。
func arrayLength(desc string) int {
	open := strings.LastIndexByte(desc, '(')
	closeIdx := strings.LastIndexByte(desc, ')')
	if open < 0 || closeIdx <= open+1 {
		return 0
	}
	n, err := strconv.Atoi(desc[open+1 : closeIdx])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// expandObject 取回对象的成员。
func (a *Adapter) expandObject(objectID string, list bool, typ string) (*model.Value, error) {
	if objectID == "" {
		return nil, model.New(model.CodeInputNotFound, "该节点不可展开")
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var resp struct {
		Result []propertyDescriptor `json:"result"`
	}
	if err := a.client.call(ctx, "Runtime.getProperties", map[string]any{
		"objectId":               objectID,
		"ownProperties":          true,
		"accessorPropertiesOnly": false,
	}, &resp); err != nil {
		return nil, err
	}

	out := &model.Value{Type: typ, Kind: model.KindObject, Handle: objectID}
	if list {
		out.Kind = model.KindList
	}
	fields := make(map[string]*model.Value, len(resp.Result))
	type indexed struct {
		idx int
		val *model.Value
	}
	var ordered []indexed
	for _, prop := range resp.Result {
		if prop.Value == nil {
			continue
		}
		child := a.remoteToValue(*prop.Value)
		fields[prop.Name] = child
		if list {
			if idx, ok := parseIndexName(prop.Name); ok {
				ordered = append(ordered, indexed{idx: idx, val: child})
			}
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].idx < ordered[j].idx })
	if list {
		out.Items = make([]*model.Value, 0, len(ordered))
		for _, item := range ordered {
			out.Items = append(out.Items, item.val)
		}
		if out.Total < len(out.Items) {
			out.Total = len(out.Items)
		}
	}
	out.Fields = fields
	return out, nil
}

func parseIndexName(name string) (int, bool) {
	if name == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return 0, false
		}
		n = n*10 + int(name[i]-'0')
	}
	return n, true
}

// ensureExpanded 在节点尚未展开时触发一次展开。
func (a *Adapter) ensureExpanded(v *model.Value) (*model.Value, error) {
	if v == nil || v.Expand == nil {
		return v, nil
	}
	expanded, err := v.Expand()
	if err != nil {
		return nil, err
	}
	if expanded == nil {
		return v, nil
	}
	return expanded, nil
}
