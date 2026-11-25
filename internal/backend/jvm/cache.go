package jvm

import (
	"context"
	"strings"
	"sync"
)

// lineKey 是行号表与变量表的缓存键。
type lineKey struct {
	classID  uint64
	methodID uint64
}

// methodVars 是方法的局部变量表信息。
type methodVars struct {
	argCount int
	slots    []varSlot
}

// classInfo 是类的元信息缓存。
type classInfo struct {
	tag        byte
	name       string
	source     string
	fields     []fieldInfo
	methods    map[uint64]methodInfo
	order      []uint64
	supers     []uint64
	lineTables map[uint64][]lineEntry
	varTables  map[uint64]*methodVars
}

// cache 缓存类元信息与行号表。
//
// 断点解析与取值都依赖这些低频变化的元信息，缓存可按类名与行号表键精确定位，
// 避免每次展开对象都重新拉一遍类结构。
type cache struct {
	mu      sync.Mutex
	classes map[uint64]*classInfo
	sources map[uint64]string
}

func newCache() *cache {
	return &cache{classes: make(map[uint64]*classInfo), sources: make(map[uint64]string)}
}

// sourceFileOf 只取源文件名，不加载完整类结构（断点解析会遍历上千个类）。
func (s *Session) sourceFileOf(ctx context.Context, typeID uint64) string {
	s.cache.mu.Lock()
	if src, ok := s.cache.sources[typeID]; ok {
		s.cache.mu.Unlock()
		return src
	}
	s.cache.mu.Unlock()

	src, err := s.wire.sourceFile(ctx, typeID)
	if err != nil {
		src = ""
	}
	s.cache.mu.Lock()
	s.cache.sources[typeID] = src
	s.cache.mu.Unlock()
	return src
}

// info 返回类元信息，必要时补齐。
func (s *Session) info(ctx context.Context, typeID uint64) (*classInfo, error) {
	s.cache.mu.Lock()
	ci, ok := s.cache.classes[typeID]
	s.cache.mu.Unlock()
	if ok {
		return ci, nil
	}

	sig, err := s.wire.referenceSignature(ctx, typeID)
	if err != nil {
		return nil, err
	}
	ci = &classInfo{
		tag:        1,
		name:       className(sig),
		methods:    make(map[uint64]methodInfo),
		lineTables: make(map[uint64][]lineEntry),
		varTables:  make(map[uint64]*methodVars),
	}
	ci.source = s.sourceFileOf(ctx, typeID)
	methods, merr := s.wire.methods(ctx, typeID)
	if merr == nil {
		for _, m := range methods {
			ci.methods[m.id] = m
			ci.order = append(ci.order, m.id)
		}
	}
	if fields, ferr := s.wire.fields(ctx, typeID); ferr == nil {
		ci.fields = fields
	}

	// 逐级上溯父类，保证字段与方法可解析到继承链。
	cur := typeID
	for i := 0; i < 32; i++ {
		sup, err := s.wire.superclass(ctx, cur)
		if err != nil || sup == 0 {
			break
		}
		ci.supers = append(ci.supers, sup)
		cur = sup
	}

	s.cache.mu.Lock()
	s.cache.classes[typeID] = ci
	s.cache.mu.Unlock()
	return ci, nil
}

// classNameOf 返回类的全名。
func (s *Session) classNameOf(ctx context.Context, typeID uint64) string {
	ci, err := s.info(ctx, typeID)
	if err != nil {
		return ""
	}
	return ci.name
}

// sourceOf 返回类的源文件名。
func (s *Session) sourceOf(ctx context.Context, typeID uint64) string {
	ci, err := s.info(ctx, typeID)
	if err != nil {
		return ""
	}
	return ci.source
}

// methodOf 返回方法元信息。
func (s *Session) methodOf(ctx context.Context, typeID, methodID uint64) (methodInfo, bool) {
	ci, err := s.info(ctx, typeID)
	if err != nil {
		return methodInfo{}, false
	}
	if m, ok := ci.methods[methodID]; ok {
		return m, true
	}
	for _, sup := range ci.supers {
		supInfo, serr := s.info(ctx, sup)
		if serr != nil {
			continue
		}
		if m, ok := supInfo.methods[methodID]; ok {
			return m, true
		}
	}
	return methodInfo{}, false
}

// fieldsOf 返回对象可读的实例字段（含继承，静态字段除外）。
func (s *Session) fieldsOf(ctx context.Context, typeID uint64) []fieldInfo {
	ci, err := s.info(ctx, typeID)
	if err != nil {
		return nil
	}
	out := make([]fieldInfo, 0, len(ci.fields))
	out = appendInstanceFields(out, ci.fields)
	for _, sup := range ci.supers {
		supInfo, serr := s.info(ctx, sup)
		if serr != nil {
			continue
		}
		out = appendInstanceFields(out, supInfo.fields)
	}
	return out
}

// staticField 是一个静态字段及其声明类。
//
// 静态字段必须按“声明类 + 字段”成对读取（ReferenceType.GetValues 只认自己声明的
// 字段），所以这里保留归属类，取值时按类分组。
type staticField struct {
	owner uint64
	info  fieldInfo
}

// staticFieldsOf 返回类的静态字段（含继承链），供 statics 作用域使用。
func (s *Session) staticFieldsOf(ctx context.Context, typeID uint64) []staticField {
	ci, err := s.info(ctx, typeID)
	if err != nil {
		return nil
	}
	out := make([]staticField, 0, len(ci.fields))
	out = appendStaticFields(out, typeID, ci.fields)
	for _, sup := range ci.supers {
		supInfo, serr := s.info(ctx, sup)
		if serr != nil {
			continue
		}
		out = appendStaticFields(out, sup, supInfo.fields)
	}
	return out
}

// appendStaticFields 追加静态字段，过滤编译器生成的 `$` 开头成员。
func appendStaticFields(dst []staticField, owner uint64, fields []fieldInfo) []staticField {
	for _, f := range fields {
		if f.mods&0x0008 == 0 || strings.HasPrefix(f.name, "$") { // ACC_STATIC
			continue
		}
		dst = append(dst, staticField{owner: owner, info: f})
	}
	return dst
}

// appendInstanceFields 追加非静态字段。
func appendInstanceFields(dst []fieldInfo, fields []fieldInfo) []fieldInfo {
	for _, f := range fields {
		if f.mods&0x0008 != 0 { // ACC_STATIC
			continue
		}
		dst = append(dst, f)
	}
	return dst
}

// lineTableOf 返回方法的行号表。
func (s *Session) lineTableOf(ctx context.Context, classID, methodID uint64) ([]lineEntry, error) {
	ci, err := s.info(ctx, classID)
	if err != nil {
		return nil, err
	}
	s.cache.mu.Lock()
	lines, ok := ci.lineTables[methodID]
	s.cache.mu.Unlock()
	if ok {
		return lines, nil
	}
	lines, err = s.wire.lineTable(ctx, classID, methodID)
	if err != nil {
		return nil, err
	}
	s.cache.mu.Lock()
	ci.lineTables[methodID] = lines
	s.cache.mu.Unlock()
	return lines, nil
}

// varTableOf 返回方法的局部变量表。
func (s *Session) varTableOf(ctx context.Context, classID, methodID uint64) (*methodVars, error) {
	ci, err := s.info(ctx, classID)
	if err != nil {
		return nil, err
	}
	s.cache.mu.Lock()
	vars, ok := ci.varTables[methodID]
	s.cache.mu.Unlock()
	if ok {
		return vars, nil
	}
	argCount, slots, err := s.wire.variableTable(ctx, classID, methodID)
	if err != nil {
		return nil, err
	}
	vars = &methodVars{argCount: argCount, slots: slots}
	s.cache.mu.Lock()
	ci.varTables[methodID] = vars
	s.cache.mu.Unlock()
	return vars, nil
}

// methodName 返回方法名，用于栈帧显示。
func (s *Session) methodName(ctx context.Context, classID, methodID uint64) string {
	if m, ok := s.methodOf(ctx, classID, methodID); ok {
		return m.name
	}
	return "<unknown>"
}

// lineAt 返回方法内某字节码偏移对应的源码行。
func (s *Session) lineAt(ctx context.Context, classID, methodID, index uint64) int {
	lines, err := s.lineTableOf(ctx, classID, methodID)
	if err != nil || len(lines) == 0 {
		return 0
	}
	line := lines[0].line
	for _, entry := range lines {
		if entry.codeIndex > index {
			break
		}
		line = entry.line
	}
	return line
}
