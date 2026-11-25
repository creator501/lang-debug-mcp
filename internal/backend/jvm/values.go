package jvm

import (
	"context"
	"strconv"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// 取值分页上限：单个复合值一次最多取回的成员数。
const (
	maxArrayPage = 100
	maxMapPage   = 100
	maxListChain = 100
	maxString    = 200
)

// makeValue 把 JDWP 值包装为语言无关节点。
//
// handle 是改值用的定位串（形如 slot:1:2:0:I），只对可写位置有意义；复合节
// 点的展开是惰性的，展开时才会真正读取内存。
func (s *Session) makeValue(ctx context.Context, sig string, v value, handle string) *model.Value {
	if v.isReference() {
		if v.ref == 0 {
			return &model.Value{Type: typeName(sig), Kind: model.KindScalar, Display: "null"}
		}
		if isStringSig(sig) {
			text, err := s.wire.stringValue(ctx, v.ref)
			if err != nil {
				return &model.Value{Type: "String", Kind: model.KindScalar, Display: `"<读取失败>"`}
			}
			return &model.Value{Type: "String", Kind: model.KindScalar, Display: quoteText(text)}
		}
		if isArraySig(sig) {
			return s.arrayValue(ctx, sig, v.ref)
		}
		return s.objectValue(ctx, sig, v.ref)
	}
	return &model.Value{Type: typeName(sig), Kind: model.KindScalar, Display: v.display(), Handle: handle}
}

// quoteText 把字符串截断并加引号，避免把整段文本塞进快照。
func quoteText(text string) string {
	if len(text) > maxString {
		text = text[:maxString] + "…"
	}
	return strconv.Quote(text)
}

// arrayValue 构造数组节点。
func (s *Session) arrayValue(ctx context.Context, sig string, ref uint64) *model.Value {
	n, err := s.wire.arrayLength(ctx, ref)
	if err != nil {
		return &model.Value{Type: typeName(sig), Kind: model.KindScalar, Display: "<数组不可读>"}
	}
	elem := elementSig(sig)
	node := &model.Value{Type: typeName(sig), Kind: model.KindList, Total: n}
	node.Expand = func() (*model.Value, error) {
		page := n
		if page > maxArrayPage {
			page = maxArrayPage
		}
		vals, verr := s.wire.arrayValues(context.Background(), ref, 0, page)
		if verr != nil {
			return nil, verr
		}
		items := make([]*model.Value, 0, len(vals))
		for i, val := range vals {
			items = append(items, s.makeValue(context.Background(), elem, val,
				"elem:"+u2s(ref)+":"+strconv.Itoa(i)+":"+elem))
		}
		return &model.Value{Type: typeName(sig), Kind: model.KindList, Total: n, Items: items}, nil
	}
	return node
}

// objectValue 构造对象节点，识别常见集合类型后按序列或映射展示。
func (s *Session) objectValue(ctx context.Context, sig string, ref uint64) *model.Value {
	name := className(sig)
	if _, typeID, err := s.wire.objectRefType(ctx, ref); err == nil {
		if actual := s.classNameOf(ctx, typeID); actual != "" {
			name = actual
			if wrapped := s.boxedScalar(ctx, ref, typeID, actual); wrapped != nil {
				return wrapped
			}
			if kv := s.collectionValue(ctx, ref, typeID, actual); kv != nil {
				return kv
			}
			return s.plainObject(ctx, ref, typeID, actual)
		}
	}
	return &model.Value{Type: shortClassName(name), Kind: model.KindObject, Handle: "obj:" + u2s(ref)}
}

// staticGroup 是同一声明类的静态字段集合。
type staticGroup struct {
	owner  uint64
	fields []staticField
}

// groupStaticFields 按声明类聚合静态字段，减少 GetValues 的往返次数。
func groupStaticFields(fields []staticField) []staticGroup {
	index := make(map[uint64]int, len(fields))
	out := make([]staticGroup, 0, len(fields))
	for _, f := range fields {
		at, ok := index[f.owner]
		if !ok {
			at = len(out)
			index[f.owner] = at
			out = append(out, staticGroup{owner: f.owner})
		}
		out[at].fields = append(out[at].fields, f)
	}
	return out
}

// staticHandle 生成静态字段的写入句柄。
func staticHandle(owner, fieldID uint64, sig string) string {
	return "static:" + u2s(owner) + ":" + u2s(fieldID) + ":" + sig
}

// staticScope 把类的静态字段做成一个可展开作用域。
//
// 静态字段属于类而不是对象，按声明类分组读取；同名成员以子类为准（父类同名项
// 不覆盖），这样继承链上的静态状态都能看到且不产生歧义。
func (s *Session) staticScope(ctx context.Context, typeID uint64) *model.Value {
	fields := s.staticFieldsOf(ctx, typeID)
	if len(fields) == 0 {
		return nil
	}
	label := shortClassName(s.classNameOf(ctx, typeID))
	node := &model.Value{Type: label, Kind: model.KindObject, Total: len(fields)}
	node.Expand = func() (*model.Value, error) {
		ectx := context.Background()
		out := &model.Value{
			Type:   label,
			Kind:   model.KindObject,
			Total:  len(fields),
			Fields: make(map[string]*model.Value, len(fields)),
		}
		for _, group := range groupStaticFields(fields) {
			ids := make([]uint64, 0, len(group.fields))
			for _, f := range group.fields {
				ids = append(ids, f.info.id)
			}
			vals, err := s.wire.staticValues(ectx, group.owner, ids)
			if err != nil {
				continue
			}
			for i, f := range group.fields {
				if i >= len(vals) {
					break
				}
				if _, dup := out.Fields[f.info.name]; dup {
					continue
				}
				handle := staticHandle(group.owner, f.info.id, f.info.signature)
				out.Fields[f.info.name] = s.makeValue(ectx, f.info.signature, vals[i], handle)
			}
		}
		return out, nil
	}
	return node
}

// boxedTypes 是包装类型到原始类型名的映射。
var boxedTypes = map[string]string{
	"java.lang.Boolean":   "boolean",
	"java.lang.Byte":      "byte",
	"java.lang.Character": "char",
	"java.lang.Short":     "short",
	"java.lang.Integer":   "int",
	"java.lang.Long":      "long",
	"java.lang.Float":     "float",
	"java.lang.Double":    "double",
}

// boxedScalar 把包装类型展开为原始值。
//
// 调试时最常看的是数值本身，把 Integer 显示成带 value 字段的对象会让每条路径
// 都多一层；包装类型的 value 字段仍可通过改值句柄写回。
func (s *Session) boxedScalar(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	prim, ok := boxedTypes[name]
	if !ok {
		return nil
	}
	inner, field, iok := s.readFieldValue(ctx, ref, typeID, "value")
	if !iok {
		return nil
	}
	return &model.Value{
		Type:    prim,
		Kind:    model.KindScalar,
		Display: inner.display(),
		Handle:  "field:" + u2s(ref) + ":" + u2s(field.id) + ":" + field.signature,
	}
}

// plainObject 按声明字段展开普通对象。
func (s *Session) plainObject(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	node := &model.Value{Type: shortClassName(name), Kind: model.KindObject, Handle: "obj:" + u2s(ref)}
	node.Expand = func() (*model.Value, error) {
		fields := s.fieldsOf(context.Background(), typeID)
		ids := make([]uint64, 0, len(fields))
		for _, f := range fields {
			ids = append(ids, f.id)
		}
		vals, err := s.wire.objectValues(context.Background(), ref, ids)
		if err != nil {
			return nil, err
		}
		out := &model.Value{Type: shortClassName(name), Kind: model.KindObject, Order: make([]string, 0, len(fields))}
		out.Fields = make(map[string]*model.Value, len(fields))
		for i, f := range fields {
			if i >= len(vals) {
				break
			}
			child := s.makeValue(context.Background(), f.signature, vals[i],
				"field:"+u2s(ref)+":"+u2s(f.id)+":"+f.signature)
			out.Fields[f.name] = child
			out.Order = append(out.Order, f.name)
		}
		return out, nil
	}
	return node
}

// collectionValue 识别常见集合类型并给出结构化视图。
func (s *Session) collectionValue(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	switch name {
	case "java.util.ArrayList":
		return s.arrayBackedList(ctx, ref, typeID, name, "elementData", "size")
	case "java.util.Arrays$ArrayList":
		return s.arrayBackedList(ctx, ref, typeID, name, "a", "")
	case "java.util.ImmutableCollections$ListN":
		return s.arrayBackedList(ctx, ref, typeID, name, "elements", "")
	case "java.util.ImmutableCollections$List12":
		return s.smallList(ctx, ref, typeID, name)
	case "java.util.LinkedList":
		return s.linkedList(ctx, ref, typeID, name)
	case "java.util.HashMap", "java.util.LinkedHashMap":
		return s.mapValue(ctx, ref, typeID, name)
	case "java.util.HashSet", "java.util.LinkedHashSet":
		return s.setValue(ctx, ref, typeID, name)
	case "java.util.ImmutableCollections$SetN":
		return s.setFromArray(ctx, ref, typeID, name, "elements")
	case "java.util.ImmutableCollections$Set12":
		return s.smallSet(ctx, ref, typeID, name)
	}
	return nil
}

// arrayBackedList 展示以数组为后端的列表（ArrayList 及其同类）。
func (s *Session) arrayBackedList(ctx context.Context, ref, typeID uint64, name, arrayField, sizeField string) *model.Value {
	arr, af, ok := s.readFieldValue(ctx, ref, typeID, arrayField)
	sig := af.signature
	if !ok || !isArraySig(sig) || arr.ref == 0 {
		return nil
	}
	total, err := s.wire.arrayLength(ctx, arr.ref)
	if err != nil {
		return nil
	}
	if sizeField != "" {
		if sv, _, sok := s.readFieldValue(ctx, ref, typeID, sizeField); sok && sv.tag == tagInt {
			total = int(int32(sv.num))
		}
	}
	if total < 0 {
		total = 0
	}
	elem := elementSig(sig)
	arrID := arr.ref
	node := &model.Value{Type: shortClassName(name), Kind: model.KindList, Total: total}
	node.Expand = func() (*model.Value, error) {
		page := total
		if page > maxArrayPage {
			page = maxArrayPage
		}
		vals, verr := s.wire.arrayValues(context.Background(), arrID, 0, page)
		if verr != nil {
			return nil, verr
		}
		items := make([]*model.Value, 0, len(vals))
		for i, val := range vals {
			items = append(items, s.makeValue(context.Background(), elem, val,
				"elem:"+u2s(arrID)+":"+strconv.Itoa(i)+":"+elem))
		}
		return &model.Value{Type: shortClassName(name), Kind: model.KindList, Total: total, Items: items}, nil
	}
	return node
}

// smallList 展示 List.of(a) / List.of(a, b) 这类小列表。
func (s *Session) smallList(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	return s.fieldList(ctx, ref, typeID, name, []string{"e0", "e1"})
}

// fieldList 按固定字段名展示元素，空引用视为结束。
func (s *Session) fieldList(ctx context.Context, ref, typeID uint64, name string, names []string) *model.Value {
	type entry struct {
		val value
		sig string
	}
	entries := make([]entry, 0, len(names))
	for _, field := range names {
		val, f, ok := s.readFieldValue(ctx, ref, typeID, field)
		if !ok || (val.isReference() && val.ref == 0) {
			continue
		}
		entries = append(entries, entry{val: val, sig: f.signature})
	}
	node := &model.Value{Type: shortClassName(name), Kind: model.KindList, Total: len(entries)}
	node.Expand = func() (*model.Value, error) {
		items := make([]*model.Value, 0, len(entries))
		for i := range entries {
			items = append(items, s.makeValue(context.Background(), entries[i].sig, entries[i].val, ""))
		}
		return &model.Value{Type: shortClassName(name), Kind: model.KindList, Total: len(entries), Items: items}, nil
	}
	return node
}

// linkedList 沿 next 链遍历 LinkedList。
func (s *Session) linkedList(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	items, total := s.chainItems(ctx, ref, typeID, "first", "item", "next")
	node := &model.Value{Type: shortClassName(name), Kind: model.KindList, Total: total}
	node.Expand = func() (*model.Value, error) {
		return &model.Value{Type: shortClassName(name), Kind: model.KindList, Total: total, Items: items}, nil
	}
	return node
}

// chainItems 遍历单链表字段，返回元素与总长度（长度受 maxListChain 限制）。
func (s *Session) chainItems(ctx context.Context, ref, typeID uint64, firstField, itemField, nextField string) ([]*model.Value, int) {
	cur, _, ok := s.readFieldValue(ctx, ref, typeID, firstField)
	if !ok || cur.ref == 0 {
		return nil, 0
	}
	nodeType := uint64(0)
	if _, id, err := s.wire.objectRefType(ctx, cur.ref); err == nil {
		nodeType = id
	}
	items := make([]*model.Value, 0, 8)
	for i := 0; i < maxListChain && cur.ref != 0; i++ {
		item, itemFieldInfo, iok := s.readFieldValue(ctx, cur.ref, nodeType, itemField)
		if iok {
			items = append(items, s.makeValue(ctx, itemFieldInfo.signature, item,
				"field:"+u2s(cur.ref)+":"+u2s(itemFieldInfo.id)+":"+itemFieldInfo.signature))
		}
		next, _, nok := s.readFieldValue(ctx, cur.ref, nodeType, nextField)
		if !nok || next.ref == 0 {
			break
		}
		cur = next
	}
	return items, len(items)
}

// mapValue 展开 HashMap 的桶数组与冲突链。
func (s *Session) mapValue(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	table, tf, ok := s.readFieldValue(ctx, ref, typeID, "table")
	if !ok || !isArraySig(tf.signature) || table.ref == 0 {
		return &model.Value{Type: shortClassName(name), Kind: model.KindObject}
	}
	node := &model.Value{Type: shortClassName(name), Kind: model.KindObject, Total: 0}
	node.Expand = func() (*model.Value, error) {
		out := &model.Value{Type: shortClassName(name), Kind: model.KindObject}
		out.Fields = make(map[string]*model.Value)
		entries := s.mapEntries(context.Background(), table.ref)
		for _, e := range entries {
			key := e.key
			if _, exists := out.Fields[key]; exists {
				key += "#" + strconv.Itoa(len(out.Fields))
			}
			out.Fields[key] = e.val
			out.Order = append(out.Order, key)
		}
		out.Total = len(out.Order)
		return out, nil
	}
	return node
}

// mapEntry 是映射中的一对键值。
type mapEntry struct {
	key string
	val *model.Value
}

// mapEntries 遍历桶数组，取出键与值。
func (s *Session) mapEntries(ctx context.Context, tableID uint64) []mapEntry {
	length, err := s.wire.arrayLength(ctx, tableID)
	if err != nil || length == 0 {
		return nil
	}
	page := length
	if page > maxMapPage {
		page = maxMapPage
	}
	buckets, err := s.wire.arrayValues(ctx, tableID, 0, page)
	if err != nil {
		return nil
	}
	out := make([]mapEntry, 0, len(buckets))
	for _, bucket := range buckets {
		node := bucket
		for hops := 0; hops < 64 && node.ref != 0; hops++ {
			nodeType := uint64(0)
			if _, id, terr := s.wire.objectRefType(ctx, node.ref); terr == nil {
				nodeType = id
			}
			keyVal, keyField, kok := s.readFieldValue(ctx, node.ref, nodeType, "key")
			if kok {
				entry := mapEntry{key: s.keyText(ctx, keyField.signature, keyVal)}
				if valVal, valField, vok := s.readFieldValue(ctx, node.ref, nodeType, "value"); vok {
					entry.val = s.makeValue(ctx, valField.signature, valVal,
						"field:"+u2s(node.ref)+":"+u2s(valField.id)+":"+valField.signature)
				}
				out = append(out, entry)
			}
			next, _, nok := s.readFieldValue(ctx, node.ref, nodeType, "next")
			if !nok {
				break
			}
			node = next
		}
		if len(out) >= maxMapPage {
			break
		}
	}
	return out
}

// keyText 把映射键渲染为字段名。
//
// HashMap 的键字段签名被擦除成 Object，因此不能只看声明签名：这里按运行时
// 类型还原字符串与包装类型，其它对象退化为“类型@标识”。
func (s *Session) keyText(ctx context.Context, sig string, v value) string {
	if !v.isReference() {
		return v.display()
	}
	if v.ref == 0 {
		return "null"
	}
	if isStringSig(sig) {
		if text, err := s.wire.stringValue(ctx, v.ref); err == nil {
			return text
		}
	}
	_, typeID, err := s.wire.objectRefType(ctx, v.ref)
	if err != nil {
		return "@" + u2s(v.ref)
	}
	actual := s.classNameOf(ctx, typeID)
	if actual == "java.lang.String" {
		if text, serr := s.wire.stringValue(ctx, v.ref); serr == nil {
			return text
		}
	}
	if boxedTypes[actual] != "" {
		if inner, _, iok := s.readFieldValue(ctx, v.ref, typeID, "value"); iok {
			return inner.display()
		}
	}
	if actual != "" {
		return shortClassName(actual) + "@" + u2s(v.ref)
	}
	return "@" + u2s(v.ref)
}

// setValue 展示 HashSet 的键集合。
func (s *Session) setValue(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	inner, _, ok := s.readFieldValue(ctx, ref, typeID, "map")
	if !ok || inner.ref == 0 {
		return &model.Value{Type: shortClassName(name), Kind: model.KindList}
	}
	node := &model.Value{Type: shortClassName(name), Kind: model.KindList}
	node.Expand = func() (*model.Value, error) {
		mapType := uint64(0)
		if _, id, err := s.wire.objectRefType(context.Background(), inner.ref); err == nil {
			mapType = id
		}
		table, tableField, tok := s.readFieldValue(context.Background(), inner.ref, mapType, "table")
		if !tok || !isArraySig(tableField.signature) || table.ref == 0 {
			return &model.Value{Type: shortClassName(name), Kind: model.KindList}, nil
		}
		entries := s.mapEntries(context.Background(), table.ref)
		items := make([]*model.Value, 0, len(entries))
		for _, e := range entries {
			items = append(items, &model.Value{Type: "key", Kind: model.KindScalar, Display: e.key})
		}
		return &model.Value{Type: shortClassName(name), Kind: model.KindList, Total: len(items), Items: items}, nil
	}
	return node
}

// setFromArray 展示以数组为后端的集合。
func (s *Session) setFromArray(ctx context.Context, ref, typeID uint64, name, arrayField string) *model.Value {
	list := s.arrayBackedList(ctx, ref, typeID, name, arrayField, "")
	if list == nil {
		return &model.Value{Type: shortClassName(name), Kind: model.KindList}
	}
	return list
}

// smallSet 展示 Set.of(a) / Set.of(a, b)。
func (s *Session) smallSet(ctx context.Context, ref, typeID uint64, name string) *model.Value {
	return s.fieldList(ctx, ref, typeID, name, []string{"e0", "e1"})
}

// readFieldValue 沿继承链按名读取实例字段。
func (s *Session) readFieldValue(ctx context.Context, objID, typeID uint64, name string) (value, fieldInfo, bool) {
	for _, f := range s.fieldsOf(ctx, typeID) {
		if f.name != name {
			continue
		}
		vals, err := s.wire.objectValues(ctx, objID, []uint64{f.id})
		if err != nil || len(vals) == 0 {
			return value{}, f, false
		}
		return vals[0], f, true
	}
	return value{}, fieldInfo{}, false
}

// u2s 把标识渲染为十进制串。
func u2s(v uint64) string { return strconv.FormatUint(v, 10) }

// trimSig 去掉签名里的泛型信息（JDWP 给的是擦除后签名，这里只做兜底）。
func trimSig(sig string) string {
	if idx := strings.IndexByte(sig, '<'); idx >= 0 {
		return sig[:idx] + ";"
	}
	return sig
}
