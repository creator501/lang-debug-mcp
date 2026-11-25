package jvm

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// bp 是会话内的断点状态。
type bp struct {
	id       string
	spec     backend.BreakpointSpec
	verified bool
	hits     int
	note     string
	req      requestID
	hasReq   bool
	loc      location
}

// AddBreakpoint 实现 backend.Debugger。
//
// JDWP 的断点请求要求类已准备，因此类尚未加载时先登记为未验证断点，等
// ClassPrepare 事件到达后再自动解析。
func (s *Session) AddBreakpoint(spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if err := s.ensureUsable(); err != nil {
		return backend.Breakpoint{}, err
	}
	if strings.TrimSpace(spec.File) == "" || spec.Line <= 0 {
		return backend.Breakpoint{}, model.New(model.CodeInputInvalid, "断点需要文件与行号").
			WithFix(`示例: {"file": "src/main/java/App.java", "line": 12}`)
	}
	if spec.Condition != "" || spec.HitCondition != "" {
		return backend.Breakpoint{}, model.New(model.CodeCapUnsupported,
			"后端 %s 不支持条件断点", backendName).
			WithFix("把条件写进目标代码（如 if 判断后调用一个空方法并在该方法上下断点），" +
				"或先无条件断点，再用 debug_inspect 观察变量")
	}
	if spec.LogMessage != "" {
		return backend.Breakpoint{}, model.New(model.CodeCapUnsupported,
			"后端 %s 不支持 logpoint", backendName).
			WithFix("在目标代码里加一行日志，或用无条件断点配合 debug_inspect")
	}

	s.addSourceDir(spec.File)
	s.mu.Lock()
	s.bpSeq++
	id := fmt.Sprintf("bp%d", s.bpSeq)
	s.mu.Unlock()

	b := &bp{id: id, spec: spec}
	if err := s.attachBreakpoint(context.Background(), b); err != nil {
		return backend.Breakpoint{}, err
	}
	s.mu.Lock()
	s.bps = append(s.bps, b)
	out := toBreakpoint(b)
	s.mu.Unlock()
	return out, nil
}

// attachBreakpoint 解析位置并注册事件请求；解析不到时登记为待加载。
func (s *Session) attachBreakpoint(ctx context.Context, b *bp) error {
	loc, actual, note, err := s.resolveLocation(ctx, b.spec.File, b.spec.Line)
	if err != nil {
		return err
	}
	if loc == nil {
		s.mu.Lock()
		b.verified, b.note = false, note
		s.mu.Unlock()
		s.ensureClassPrepare(ctx)
		return nil
	}
	req, err := s.wire.setEvent(ctx, evBreakpoint, suspendAll, []eventModifier{{kind: modLocation, loc: *loc}})
	if err != nil {
		return wrapJDWP(err)
	}
	s.mu.Lock()
	b.req, b.hasReq, b.verified, b.loc = req, true, true, *loc
	if actual != 0 && actual != b.spec.Line {
		b.note = note
	}
	s.mu.Unlock()
	return nil
}

// UpdateBreakpoint 实现 backend.Debugger。
func (s *Session) UpdateBreakpoint(id string, spec backend.BreakpointSpec) (backend.Breakpoint, error) {
	if err := s.ensureUsable(); err != nil {
		return backend.Breakpoint{}, err
	}
	s.mu.Lock()
	target := s.findBP(id)
	s.mu.Unlock()
	if target == nil {
		return backend.Breakpoint{}, model.New(model.CodeInputNotFound, "断点 %s 不存在", id).
			WithFix("调用 debug_breakpoint 的 list 动作确认标识")
	}
	if strings.TrimSpace(spec.File) == "" || spec.Line <= 0 {
		return backend.Breakpoint{}, model.New(model.CodeInputInvalid, "断点需要文件与行号")
	}
	if spec.Condition != "" || spec.HitCondition != "" || spec.LogMessage != "" {
		return backend.Breakpoint{}, model.New(model.CodeCapUnsupported,
			"后端 %s 不支持条件断点与 logpoint", backendName)
	}

	s.clearBPRequest(context.Background(), target)
	s.addSourceDir(spec.File)
	s.mu.Lock()
	target.spec = spec
	target.verified, target.note = false, ""
	s.mu.Unlock()
	if err := s.attachBreakpoint(context.Background(), target); err != nil {
		return backend.Breakpoint{}, err
	}
	s.mu.Lock()
	out := toBreakpoint(target)
	s.mu.Unlock()
	return out, nil
}

// RemoveBreakpoint 实现 backend.Debugger。
func (s *Session) RemoveBreakpoint(id string) error {
	s.mu.Lock()
	target := s.findBP(id)
	if target != nil {
		s.bps = removeBP(s.bps, target)
	}
	s.mu.Unlock()
	if target == nil {
		return model.New(model.CodeInputNotFound, "断点 %s 不存在", id).
			WithFix("调用 debug_breakpoint 的 list 动作确认标识")
	}
	s.clearBPRequest(context.Background(), target)
	s.maybeClearClassPrepare(context.Background())
	return nil
}

// ListBreakpoints 实现 backend.Debugger。
func (s *Session) ListBreakpoints() []backend.Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]backend.Breakpoint, 0, len(s.bps))
	for _, b := range s.bps {
		out = append(out, toBreakpoint(b))
	}
	return out
}

// toBreakpoint 转换为对外结构。
func toBreakpoint(b *bp) backend.Breakpoint {
	return backend.Breakpoint{
		ID:       b.id,
		Spec:     b.spec,
		Verified: b.verified,
		Hits:     b.hits,
		Note:     b.note,
	}
}

// findBP 按标识查找断点（调用方需持锁）。
func (s *Session) findBP(id string) *bp {
	for _, b := range s.bps {
		if b.id == id {
			return b
		}
	}
	return nil
}

// removeBP 从切片中删除断点。
func removeBP(list []*bp, target *bp) []*bp {
	out := list[:0]
	for _, b := range list {
		if b != target {
			out = append(out, b)
		}
	}
	return out
}

// clearBPRequest 注销断点对应的事件请求。
func (s *Session) clearBPRequest(ctx context.Context, b *bp) {
	s.mu.Lock()
	req, active := b.req, b.hasReq
	b.hasReq, b.verified = false, false
	s.mu.Unlock()
	if active {
		_ = s.wire.clearEvent(ctx, evBreakpoint, req)
	}
}

// breakpointByRequest 按事件请求找回断点。
func (s *Session) breakpointByRequest(req requestID) *bp {
	if req == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bps {
		if b.hasReq && b.req == req {
			return b
		}
	}
	return nil
}

// hit 记录一次命中。
func (s *Session) hit(req requestID) {
	b := s.breakpointByRequest(req)
	if b == nil {
		return
	}
	s.mu.Lock()
	b.hits++
	s.mu.Unlock()
}

// resolveLocation 把文件与行号解析为可下断点的位置。
//
// JDWP 只能按类定位，因此这里用“源文件名 + 行号表”匹配：先找同名源文件的类，
// 再在方法行号表里找该行（找不到时退到其后最近的可执行行）。
func (s *Session) resolveLocation(ctx context.Context, file string, line int) (*location, int, string, error) {
	base := filepath.Base(file)
	classes, err := s.wire.allClasses(ctx)
	if err != nil {
		return nil, 0, "", wrapJDWP(err)
	}
	matches := 0
	for _, rt := range classes {
		if rt.tag == 3 {
			continue // 数组类没有源码
		}
		if src := s.sourceFileOf(ctx, rt.id); !strings.EqualFold(src, base) {
			continue
		}
		matches++
		loc, actual, ok := s.findLine(ctx, rt.id, line)
		if !ok {
			continue
		}
		note := ""
		if actual != line {
			note = fmt.Sprintf("%s 第 %d 行没有可执行代码，已改到第 %d 行", base, line, actual)
		}
		if matches > 1 {
			note = strings.TrimSpace(note + "；同名源文件存在多个，已选 " + s.classNameOf(ctx, rt.id))
		}
		return &loc, actual, note, nil
	}
	return nil, 0, fmt.Sprintf("源文件 %s 尚未加载，或第 %d 行没有可执行代码", base, line), nil
}

// findLine 在类的方法行号表里定位行号。
func (s *Session) findLine(ctx context.Context, typeID uint64, line int) (location, int, bool) {
	ci, err := s.info(ctx, typeID)
	if err != nil {
		return location{}, 0, false
	}
	for _, methodID := range ci.order {
		m := ci.methods[methodID]
		if m.mods&(0x0100|0x0400) != 0 { // ACC_NATIVE / ACC_ABSTRACT
			continue
		}
		lines, lerr := s.lineTableOf(ctx, typeID, methodID)
		if lerr != nil || len(lines) == 0 {
			continue
		}
		bestLine, bestIndex, found := 0, uint64(0), false
		for _, entry := range lines {
			if entry.line == line {
				bestLine, bestIndex, found = entry.line, entry.codeIndex, true
				break
			}
		}
		if !found {
			for _, entry := range lines {
				if entry.line > line && (!found || entry.line < bestLine) {
					bestLine, bestIndex, found = entry.line, entry.codeIndex, true
				}
			}
		}
		if found {
			return location{
				classTag: ci.tag,
				classID:  typeID,
				methodID: methodID,
				index:    bestIndex,
			}, bestLine, true
		}
	}
	return location{}, 0, false
}

// ensureClassPrepare 注册类加载事件，用于解析尚未加载的断点。
func (s *Session) ensureClassPrepare(ctx context.Context) {
	s.mu.Lock()
	active := s.cpActive
	s.mu.Unlock()
	if active {
		return
	}
	id, err := s.wire.setEvent(ctx, evClassPrepare, suspendEventThread, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.cpReq, s.cpActive = id, true
	s.mu.Unlock()
}

// maybeClearClassPrepare 在没有待解析断点时取消类加载事件。
func (s *Session) maybeClearClassPrepare(ctx context.Context) {
	s.mu.Lock()
	pending := false
	for _, b := range s.bps {
		if !b.hasReq {
			pending = true
			break
		}
	}
	req, active := s.cpReq, s.cpActive
	if !pending {
		s.cpActive = false
	}
	s.mu.Unlock()
	if pending || !active {
		return
	}
	_ = s.wire.clearEvent(ctx, evClassPrepare, req)
}

// resolvePending 在类加载后解析待生效断点。
func (s *Session) resolvePending(ctx context.Context) {
	s.mu.Lock()
	var pending []*bp
	for _, b := range s.bps {
		if !b.hasReq {
			pending = append(pending, b)
		}
	}
	s.mu.Unlock()
	for _, b := range pending {
		if err := s.attachBreakpoint(ctx, b); err != nil {
			s.mu.Lock()
			b.note = "断点解析失败: " + err.Error()
			s.mu.Unlock()
		}
	}
	s.maybeClearClassPrepare(ctx)
}
