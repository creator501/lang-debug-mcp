package jvm

import (
	"context"
	"fmt"
)

// refType 是一个已加载的引用类型。
type refType struct {
	tag       byte
	id        uint64
	signature string
	status    uint32
}

// methodInfo 是一个方法。
type methodInfo struct {
	id        uint64
	name      string
	signature string
	mods      uint32
}

// fieldInfo 是一个字段。
type fieldInfo struct {
	id        uint64
	name      string
	signature string
	mods      uint32
}

// lineEntry 是行号表的一项。
type lineEntry struct {
	codeIndex uint64
	line      int
}

// varSlot 是局部变量表的一项。
type varSlot struct {
	codeIndex uint64
	name      string
	signature string
	length    int
	slot      int
}

// location 是一个可下断点的位置。
type location struct {
	classTag byte
	classID  uint64
	methodID uint64
	index    uint64
}

// frameInfo 是一个栈帧。
type frameInfo struct {
	id  uint64
	loc location
}

// idSizes 读取标识符宽度。
func (w *wire) idSizes(ctx context.Context) (sizes, error) {
	data, err := w.request(ctx, csVirtualMachine, 7, nil)
	if err != nil {
		return sizes{}, err
	}
	r := newReader(data, w.ids)
	return sizes{
		field:  int(r.u4()),
		method: int(r.u4()),
		object: int(r.u4()),
		refTyp: int(r.u4()),
		frame:  int(r.u4()),
	}, nil
}

// version 读取调试协议与 VM 版本。
func (w *wire) version(ctx context.Context) (description, vmVersion string, err error) {
	data, err := w.request(ctx, csVirtualMachine, 1, nil)
	if err != nil {
		return "", "", err
	}
	r := newReader(data, w.ids)
	description = r.str()
	r.skip(8) // jdwpMajor / jdwpMinor
	return description, r.str(), nil
}

// allClasses 列出全部已加载类。
func (w *wire) allClasses(ctx context.Context) ([]refType, error) {
	data, err := w.request(ctx, csVirtualMachine, 3, nil)
	if err != nil {
		return nil, err
	}
	r := newReader(data, w.ids)
	n := int(r.u4())
	out := make([]refType, 0, n)
	for i := 0; i < n; i++ {
		rt := refType{tag: r.u1(), id: r.id(w.ids.refTyp), signature: r.str(), status: r.u4()}
		out = append(out, rt)
	}
	return out, nil
}

// allThreads 列出全部线程。
func (w *wire) allThreads(ctx context.Context) ([]uint64, error) {
	data, err := w.request(ctx, csVirtualMachine, 4, nil)
	if err != nil {
		return nil, err
	}
	r := newReader(data, w.ids)
	n := int(r.u4())
	out := make([]uint64, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.id(w.ids.object))
	}
	return out, nil
}

// suspendAll 挂起整个虚拟机。
func (w *wire) suspendAll(ctx context.Context) error {
	_, err := w.request(ctx, csVirtualMachine, 8, nil)
	return err
}

// resumeAll 恢复整个虚拟机。
func (w *wire) resumeAll(ctx context.Context) error {
	_, err := w.request(ctx, csVirtualMachine, 9, nil)
	return err
}

// resumeThread 只恢复指定线程，用于放行被单线程挂起的事件（如类准备）。
func (w *wire) resumeThread(ctx context.Context, threadID uint64) error {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, threadID)
	_, err := w.request(ctx, csThreadReference, 3, bw.bytes())
	return err
}

// dispose 断开并让 VM 继续运行。
func (w *wire) dispose(ctx context.Context) error {
	_, err := w.request(ctx, csVirtualMachine, 6, nil)
	return err
}

// createString 在目标堆上创建字符串，用于把字符串字面量写入变量。
func (w *wire) createString(ctx context.Context, s string) (uint64, error) {
	bw := newWriter(w.ids)
	bw.str(s)
	data, err := w.request(ctx, csVirtualMachine, 11, bw.bytes())
	if err != nil {
		return 0, err
	}
	return newReader(data, w.ids).id(w.ids.object), nil
}

// sourceFile 取类对应的源文件名（不含路径）。
func (w *wire) sourceFile(ctx context.Context, typeID uint64) (string, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	data, err := w.request(ctx, csReferenceType, 7, bw.bytes())
	if err != nil {
		return "", err
	}
	return newReader(data, w.ids).str(), nil
}

// referenceSignature 取引用类型的签名。
func (w *wire) referenceSignature(ctx context.Context, typeID uint64) (string, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	data, err := w.request(ctx, csReferenceType, 1, bw.bytes())
	if err != nil {
		return "", err
	}
	return newReader(data, w.ids).str(), nil
}

// methods 列出类的方法。
func (w *wire) methods(ctx context.Context, typeID uint64) ([]methodInfo, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	data, err := w.request(ctx, csReferenceType, 5, bw.bytes())
	if err != nil {
		return nil, err
	}
	r := newReader(data, w.ids)
	n := int(r.u4())
	out := make([]methodInfo, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, methodInfo{
			id:        r.id(w.ids.method),
			name:      r.str(),
			signature: r.str(),
			mods:      r.u4(),
		})
	}
	return out, nil
}

// fields 列出类声明的字段。
func (w *wire) fields(ctx context.Context, typeID uint64) ([]fieldInfo, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	data, err := w.request(ctx, csReferenceType, 4, bw.bytes())
	if err != nil {
		return nil, err
	}
	r := newReader(data, w.ids)
	n := int(r.u4())
	out := make([]fieldInfo, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fieldInfo{
			id:        r.id(w.ids.field),
			name:      r.str(),
			signature: r.str(),
			mods:      r.u4(),
		})
	}
	return out, nil
}

// superclass 取父类；没有父类时返回 0。
func (w *wire) superclass(ctx context.Context, typeID uint64) (uint64, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	data, err := w.request(ctx, 3, 1, bw.bytes())
	if err != nil {
		return 0, err
	}
	return newReader(data, w.ids).id(w.ids.refTyp), nil
}

// lineTable 取方法的行号表。
func (w *wire) lineTable(ctx context.Context, typeID, methodID uint64) ([]lineEntry, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	bw.id(w.ids.method, methodID)
	data, err := w.request(ctx, csMethod, 1, bw.bytes())
	if err != nil {
		return nil, err
	}
	r := newReader(data, w.ids)
	r.skip(16) // start / end / 起始行保留不用
	n := int(r.u4())
	out := make([]lineEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, lineEntry{codeIndex: r.u8(), line: int(r.u4())})
	}
	return out, nil
}

// variableTable 取方法的局部变量表。
func (w *wire) variableTable(ctx context.Context, typeID, methodID uint64) (int, []varSlot, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	bw.id(w.ids.method, methodID)
	data, err := w.request(ctx, csMethod, 2, bw.bytes())
	if err != nil {
		return 0, nil, err
	}
	r := newReader(data, w.ids)
	argCount := int(r.u4())
	n := int(r.u4())
	out := make([]varSlot, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, varSlot{
			codeIndex: r.u8(),
			name:      r.str(),
			signature: r.str(),
			length:    int(r.u4()),
			slot:      int(r.u4()),
		})
	}
	return argCount, out, nil
}

// threadName 取线程名。
func (w *wire) threadName(ctx context.Context, threadID uint64) (string, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, threadID)
	data, err := w.request(ctx, csThreadReference, 1, bw.bytes())
	if err != nil {
		return "", err
	}
	return newReader(data, w.ids).str(), nil
}

// threadSuspended 报告线程是否处于挂起状态。
func (w *wire) threadSuspended(ctx context.Context, threadID uint64) (bool, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, threadID)
	data, err := w.request(ctx, csThreadReference, 4, bw.bytes())
	if err != nil {
		return false, err
	}
	r := newReader(data, w.ids)
	r.skip(4) // threadStatus
	return r.u4() != 0, nil
}

// frames 取线程的调用栈。
func (w *wire) frames(ctx context.Context, threadID uint64, start, length int) ([]frameInfo, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, threadID)
	bw.u4(uint32(start))
	bw.u4(uint32(length))
	data, err := w.request(ctx, csThreadReference, 6, bw.bytes())
	if err != nil {
		return nil, err
	}
	r := newReader(data, w.ids)
	n := int(r.u4())
	out := make([]frameInfo, 0, n)
	for i := 0; i < n; i++ {
		f := frameInfo{id: r.id(w.ids.frame)}
		f.loc = location{
			classTag: r.u1(),
			classID:  r.id(w.ids.refTyp),
			methodID: r.id(w.ids.method),
			index:    r.u8(),
		}
		out = append(out, f)
	}
	return out, nil
}

// objectRefType 取对象引用的实际类型。
func (w *wire) objectRefType(ctx context.Context, objectID uint64) (byte, uint64, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, objectID)
	data, err := w.request(ctx, csObjectReference, 1, bw.bytes())
	if err != nil {
		return 0, 0, err
	}
	r := newReader(data, w.ids)
	return r.u1(), r.id(w.ids.refTyp), nil
}

// objectValues 读取对象字段值。
func (w *wire) objectValues(ctx context.Context, objectID uint64, fieldIDs []uint64) ([]value, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, objectID)
	bw.u4(uint32(len(fieldIDs)))
	for _, id := range fieldIDs {
		bw.id(w.ids.field, id)
	}
	data, err := w.request(ctx, csObjectReference, 2, bw.bytes())
	if err != nil {
		return nil, err
	}
	return readValues(data, w.ids, len(fieldIDs))
}

// setObjectValues 写入对象字段值。
//
// SetValues 的字段值沿用数组区域的约定：字段类型已由 fieldID 确定，值不带标签。
func (w *wire) setObjectValues(ctx context.Context, objectID uint64, fieldIDs []uint64, vals []value) error {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, objectID)
	bw.u4(uint32(len(fieldIDs)))
	for i, id := range fieldIDs {
		bw.id(w.ids.field, id)
		bw.untagged(vals[i])
	}
	_, err := w.request(ctx, csObjectReference, 3, bw.bytes())
	return err
}

// staticValues 读取类静态字段的值。
func (w *wire) staticValues(ctx context.Context, typeID uint64, fieldIDs []uint64) ([]value, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	bw.u4(uint32(len(fieldIDs)))
	for _, id := range fieldIDs {
		bw.id(w.ids.field, id)
	}
	data, err := w.request(ctx, csReferenceType, 6, bw.bytes())
	if err != nil {
		return nil, err
	}
	return readValues(data, w.ids, len(fieldIDs))
}

// setStaticValues 写入类静态字段的值。
//
// 写静态字段只有 ClassType.SetValues(3/2) 一条命令；ReferenceType 命令集里没有
// SetValues（7 是 SourceFile），发错命令会被当成读取而“静默成功”。
func (w *wire) setStaticValues(ctx context.Context, typeID uint64, fieldIDs []uint64, vals []value) error {
	bw := newWriter(w.ids)
	bw.id(w.ids.refTyp, typeID)
	bw.u4(uint32(len(fieldIDs)))
	for i, id := range fieldIDs {
		bw.id(w.ids.field, id)
		bw.untagged(vals[i])
	}
	_, err := w.request(ctx, csClassType, 2, bw.bytes())
	return err
}

// stringValue 取字符串内容。
func (w *wire) stringValue(ctx context.Context, stringID uint64) (string, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, stringID)
	data, err := w.request(ctx, csStringReference, 1, bw.bytes())
	if err != nil {
		return "", err
	}
	return newReader(data, w.ids).str(), nil
}

// arrayLength 取数组长度。
func (w *wire) arrayLength(ctx context.Context, arrayID uint64) (int, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, arrayID)
	data, err := w.request(ctx, csArrayReference, 1, bw.bytes())
	if err != nil {
		return 0, err
	}
	return int(newReader(data, w.ids).u4()), nil
}

// arrayValues 读取数组元素。
func (w *wire) arrayValues(ctx context.Context, arrayID uint64, first, length int) ([]value, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, arrayID)
	bw.u4(uint32(first))
	bw.u4(uint32(length))
	data, err := w.request(ctx, csArrayReference, 2, bw.bytes())
	if err != nil {
		return nil, err
	}
	return readArrayValues(data, w.ids, length)
}

// setArrayValues 写入数组元素。
func (w *wire) setArrayValues(ctx context.Context, arrayID uint64, first int, vals []value) error {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, arrayID)
	bw.u4(uint32(first))
	bw.u4(uint32(len(vals)))
	for _, v := range vals {
		bw.untagged(v)
	}
	_, err := w.request(ctx, csArrayReference, 3, bw.bytes())
	return err
}

// frameSlot 是一个待读取的局部变量槽位。
//
// GetValues 的请求需要逐个槽位带上类型标签，虚拟机按该标签决定如何取值；
// 标签就是变量签名（JVM 类型描述符）的首字节。
type frameSlot struct {
	slot int
	tag  byte
}

// frameValues 读取栈帧槽位值。
func (w *wire) frameValues(ctx context.Context, threadID, frameID uint64, slots []frameSlot) ([]value, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, threadID)
	bw.id(w.ids.frame, frameID)
	bw.u4(uint32(len(slots)))
	for _, slot := range slots {
		bw.u4(uint32(slot.slot))
		bw.u1(slot.tag)
	}
	data, err := w.request(ctx, csStackFrame, 1, bw.bytes())
	if err != nil {
		return nil, err
	}
	return readValues(data, w.ids, len(slots))
}

// setFrameValues 写入栈帧槽位值。
func (w *wire) setFrameValues(ctx context.Context, threadID, frameID uint64, slots []int, vals []value) error {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, threadID)
	bw.id(w.ids.frame, frameID)
	bw.u4(uint32(len(slots)))
	for i, slot := range slots {
		bw.u4(uint32(slot))
		bw.value(vals[i])
	}
	_, err := w.request(ctx, csStackFrame, 2, bw.bytes())
	return err
}

// thisObject 取栈帧的 this 引用；静态方法返回 0。
func (w *wire) thisObject(ctx context.Context, threadID, frameID uint64) (uint64, error) {
	bw := newWriter(w.ids)
	bw.id(w.ids.object, threadID)
	bw.id(w.ids.frame, frameID)
	data, err := w.request(ctx, csStackFrame, 3, bw.bytes())
	if err != nil {
		return 0, err
	}
	return newReader(data, w.ids).id(w.ids.object), nil
}

// requestID 是事件请求的标识。
type requestID int32

// eventModifier 是一个事件修饰符。
type eventModifier struct {
	kind     byte
	thread   uint64
	class    uint64
	loc      location
	count    int32
	size     int32
	depth    int32
	tagged   bool
	caught   bool
	uncaught bool
}

// 修饰符种类。
const (
	modCount        = 1
	modThreadOnly   = 3
	modClassOnly    = 4
	modClassMatch   = 5
	modClassExclude = 6
	modLocation     = 7
	modException    = 8
	modStep         = 10
)

// setEvent 注册一个事件请求。
//
// 报文结构为：eventKind、挂起策略、修饰符个数、修饰符。请求标识由虚拟机在
// 应答里分配，请求报文中不携带。
func (w *wire) setEvent(ctx context.Context, kind byte, suspend byte, mods []eventModifier) (requestID, error) {
	bw := newWriter(w.ids)
	bw.u1(kind)
	bw.u1(suspend)
	bw.u4(uint32(len(mods)))
	writeModifiers(bw, mods)

	data, err := w.request(ctx, csEventRequest, 1, bw.bytes())
	if err != nil {
		return 0, err
	}
	return requestID(newReader(data, w.ids).u4()), nil
}

// writeModifiers 写入修饰符列表。
func writeModifiers(bw *jdwpWriter, mods []eventModifier) {
	for _, m := range mods {
		bw.u1(m.kind)
		switch m.kind {
		case modCount:
			bw.u4(uint32(m.count))
		case modThreadOnly:
			bw.id(bw.ids.object, m.thread)
		case modClassOnly:
			bw.id(bw.ids.refTyp, m.class)
		case modClassMatch, modClassExclude:
			bw.str("")
		case modLocation:
			bw.u1(m.loc.classTag)
			bw.id(bw.ids.refTyp, m.loc.classID)
			bw.id(bw.ids.method, m.loc.methodID)
			bw.u8(m.loc.index)
		case modException:
			bw.id(bw.ids.refTyp, m.class)
			bw.u1(boolByte(m.caught))
			bw.u1(boolByte(m.uncaught))
		case modStep:
			bw.id(bw.ids.object, m.thread)
			bw.u4(uint32(m.size))
			bw.u4(uint32(m.depth))
		}
	}
}

// boolByte 把布尔转换为 JDWP 的 0/1。
func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// clearEvent 注销一个事件请求。
func (w *wire) clearEvent(ctx context.Context, kind byte, id requestID) error {
	bw := newWriter(w.ids)
	bw.u1(kind)
	bw.u4(uint32(id))
	_, err := w.request(ctx, csEventRequest, 2, bw.bytes())
	return err
}

// readValues 读取一组带标签的值。
func readValues(data []byte, ids sizes, want int) ([]value, error) {
	r := newReader(data, ids)
	n := int(r.u4())
	if n < 0 || n > want {
		return nil, fmt.Errorf("JDWP 返回值数量异常: %d（期望 %d）", n, want)
	}
	out := make([]value, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.value())
	}
	return out, nil
}

// readArrayValues 读取数组区域。
//
// 应答结构与其它 GetValues 不同：先给组件类型标签与元素个数，随后按组件类别
// 给出元素——对象/数组元素逐个带标签，原始类型元素省略标签。
func readArrayValues(data []byte, ids sizes, want int) ([]value, error) {
	r := newReader(data, ids)
	comp := r.u1()
	n := int(r.u4())
	if n < 0 || n > want {
		return nil, fmt.Errorf("JDWP 数组元素数量异常: %d（期望 %d）", n, want)
	}
	out := make([]value, 0, n)
	for i := 0; i < n; i++ {
		tag := comp
		if isObjectTag(comp) {
			tag = r.u1()
		}
		out = append(out, r.untagged(tag))
	}
	return out, nil
}

// isObjectTag 报告标签是否代表引用类型。
func isObjectTag(tag byte) bool {
	switch tag {
	case tagObject, tagArray, tagString:
		return true
	}
	return false
}

// write 把一个值写入缓冲区。
func (w *jdwpWriter) value(v value) {
	w.u1(v.tag)
	w.untagged(v)
}

// writeUntagged 只写入值体，用于数组区域（元素标签由组件类型统一给出）。
func (w *jdwpWriter) untagged(v value) {
	switch v.tag {
	case tagBoolean, tagByte:
		w.u1(byte(v.num))
	case tagChar, tagShort:
		w.u2(uint16(v.num))
	case tagInt, tagFloat:
		w.u4(uint32(int32(v.num)))
	case tagLong, tagDouble:
		w.u8(uint64(v.num))
	case tagObject, tagString, tagArray:
		w.id(w.ids.object, v.ref)
	}
}
