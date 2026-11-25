package jvm

import (
	"context"
	"strconv"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// handleKind 是可写位置的类别。
type handleKind string

// 可写位置的类别。
const (
	handleSlot   handleKind = "slot"
	handleField  handleKind = "field"
	handleElem   handleKind = "elem"
	handleStatic handleKind = "static"
)

// parsedHandle 是解析后的可写位置。
type parsedHandle struct {
	kind     handleKind
	threadID uint64
	frameID  uint64
	slot     int
	objectID uint64
	typeID   uint64
	fieldID  uint64
	arrayID  uint64
	index    int
	sig      string
}

// Mutate 实现 backend.Debugger。
//
// 逐条执行、逐条报告；成功时回传原值，便于调用方判断是否需要恢复。
func (s *Session) Mutate(ops []backend.MutateOp) []backend.MutateResult {
	stop := s.CurrentStop()
	out := make([]backend.MutateResult, 0, len(ops))
	for _, op := range ops {
		out = append(out, s.mutateOne(stop, op))
	}
	return out
}

// mutateOne 执行一条改值。
func (s *Session) mutateOne(stop *backend.Stop, op backend.MutateOp) backend.MutateResult {
	res := backend.MutateResult{Path: op.Path}
	if stop == nil || stop.Snapshot == nil {
		res.Code = model.CodeStateNotStopped
		res.Reason = "目标未停止，无法改值"
		return res
	}

	node, err := s.locate(stop.Snapshot, op.Frame, op.Path)
	if err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		return res
	}
	handle, ok := parseHandle(node.Handle)
	if !ok {
		res.Code = model.CodeCapUnsupported
		res.Reason = "该位置不可直接写入（只能改原始类型、String 字段或局部变量）"
		res.Type = node.Type
		return res
	}

	ctx := context.Background()
	newValue, typ, err := literalValue(handle.sig, op.Value, func(text string) (uint64, error) {
		return s.wire.createString(ctx, text)
	})
	if err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		res.Type = node.Type
		return res
	}

	if err := s.writeHandle(ctx, handle, newValue); err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		return res
	}

	res.OK = true
	res.OldValue = displayOf(node)
	res.Type = typ
	return res
}

// writeHandle 按位置类别写入新值。
func (s *Session) writeHandle(ctx context.Context, h parsedHandle, v value) error {
	var err error
	switch h.kind {
	case handleSlot:
		err = s.wire.setFrameValues(ctx, h.threadID, h.frameID, []int{h.slot}, []value{v})
	case handleField:
		err = s.wire.setObjectValues(ctx, h.objectID, []uint64{h.fieldID}, []value{v})
	case handleElem:
		err = s.wire.setArrayValues(ctx, h.arrayID, h.index, []value{v})
	case handleStatic:
		err = s.wire.setStaticValues(ctx, h.typeID, []uint64{h.fieldID}, []value{v})
	default:
		return model.New(model.CodeCapUnsupported, "不支持写入该位置")
	}
	return wrapJDWP(err)
}

// displayOf 返回节点的可读原值。
func displayOf(v *model.Value) string {
	if v == nil {
		return ""
	}
	if v.IsScalar() {
		return v.Display
	}
	if v.Kind == model.KindList {
		return "<list len=" + strconv.Itoa(v.Len()) + ">"
	}
	return "<object>"
}

// locate 沿路径定位到最终节点。
func (s *Session) locate(snap *model.Snapshot, frame int, raw string) (*model.Value, error) {
	p, err := model.ParsePath(raw)
	if err != nil {
		return nil, err
	}
	useFrame := p.Frame
	if frame > 0 {
		useFrame = frame
	}
	cur, err := snap.Scope(useFrame, p.Scope)
	if err != nil {
		return nil, err
	}
	for _, part := range p.Parts {
		if part.Wild || part.HasRange {
			return nil, model.New(model.CodeInputInvalid, "改值路径不支持通配符与区间").
				WithFix("逐条给出确定路径，如 locals.retries")
		}
		cur, err = expandNode(cur)
		if err != nil {
			return nil, err
		}
		if part.Name != "" {
			child, ferr := cur.Field(part.Name)
			if ferr != nil {
				return nil, model.New(model.CodeInputNotFound, "路径 %s 不存在", raw).
					WithFix("先用 debug_inspect 确认可用的成员名")
			}
			cur = child
		}
		if part.HasIndex {
			cur, err = expandNode(cur)
			if err != nil {
				return nil, err
			}
			child, ierr := cur.Item(part.Index)
			if ierr != nil {
				return nil, model.New(model.CodeInputNotFound, "索引 %d 超出范围", part.Index)
			}
			cur = child
		}
	}
	return cur, nil
}

// expandNode 在节点尚未展开时触发一次展开。
func expandNode(v *model.Value) (*model.Value, error) {
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

// parseHandle 解析位置串。
//
// 形如 slot:1:2:0:I / field:123:45:Ljava/lang/String; / elem:123:0:[I /
// static:77:88:I（静态字段按声明类 + 字段定位）。
func parseHandle(raw string) (parsedHandle, bool) {
	parts := strings.Split(raw, ":")
	switch {
	case len(parts) == 5 && parts[0] == string(handleSlot):
		threadID, err1 := strconv.ParseUint(parts[1], 10, 64)
		frameID, err2 := strconv.ParseUint(parts[2], 10, 64)
		slot, err3 := strconv.Atoi(parts[3])
		if err1 != nil || err2 != nil || err3 != nil {
			return parsedHandle{}, false
		}
		return parsedHandle{kind: handleSlot, threadID: threadID, frameID: frameID, slot: slot, sig: parts[4]}, true
	case len(parts) == 4 && parts[0] == string(handleField):
		objectID, err1 := strconv.ParseUint(parts[1], 10, 64)
		fieldID, err2 := strconv.ParseUint(parts[2], 10, 64)
		if err1 != nil || err2 != nil {
			return parsedHandle{}, false
		}
		return parsedHandle{kind: handleField, objectID: objectID, fieldID: fieldID, sig: parts[3]}, true
	case len(parts) == 4 && parts[0] == string(handleStatic):
		typeID, err1 := strconv.ParseUint(parts[1], 10, 64)
		fieldID, err2 := strconv.ParseUint(parts[2], 10, 64)
		if err1 != nil || err2 != nil {
			return parsedHandle{}, false
		}
		return parsedHandle{kind: handleStatic, typeID: typeID, fieldID: fieldID, sig: parts[3]}, true
	case len(parts) == 4 && parts[0] == string(handleElem):
		arrayID, err1 := strconv.ParseUint(parts[1], 10, 64)
		index, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			return parsedHandle{}, false
		}
		return parsedHandle{kind: handleElem, arrayID: arrayID, index: index, sig: parts[3]}, true
	}
	return parsedHandle{}, false
}
