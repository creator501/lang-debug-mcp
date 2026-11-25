package nodejs

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// locator 描述一个可写位置：父容器、成员名或索引、以及当前值。
type locator struct {
	parent  *model.Value
	name    string
	index   int
	isIndex bool
	value   *model.Value
}

// Mutate 实现 backend.Debugger。
func (a *Adapter) Mutate(ops []backend.MutateOp) []backend.MutateResult {
	out := make([]backend.MutateResult, 0, len(ops))
	for _, op := range ops {
		out = append(out, a.mutateOne(op))
	}
	return out
}

func (a *Adapter) mutateOne(op backend.MutateOp) backend.MutateResult {
	res := backend.MutateResult{Path: op.Path}

	stop := a.CurrentStop()
	if stop == nil || stop.Snapshot == nil {
		res.Code = model.CodeStateNotStopped
		res.Reason = "目标未停止，无法改值"
		return res
	}

	loc, err := a.locate(stop.Snapshot, op.Frame, op.Path)
	if err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		return res
	}
	if loc.parent == nil || loc.parent.Handle == "" {
		res.Code = model.CodeInputInvalid
		res.Reason = "目标位置不可写：缺少可写的父容器"
		return res
	}

	arg, err := literalToArg(loc.value.Type, op.Value)
	if err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		res.Type = loc.value.Type
		return res
	}

	oldValue := loc.value.Display
	if err := a.setProperty(loc, arg); err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		return res
	}

	res.OK = true
	res.OldValue = oldValue
	res.Type = loc.value.Type
	return res
}

// locate 沿路径定位到可写位置，按需展开父节点。
func (a *Adapter) locate(snap *model.Snapshot, frame int, raw string) (*locator, error) {
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

	loc := &locator{}
	for _, part := range p.Parts {
		if part.Wild || part.HasRange {
			return nil, model.New(model.CodeInputInvalid, "改值路径不支持通配符与区间").
				WithFix("逐条给出确定路径，如 this.retries")
		}
		expanded, err := a.ensureExpanded(cur)
		if err != nil {
			return nil, err
		}
		if part.Name != "" {
			loc.parent = expanded
			loc.name = part.Name
			loc.isIndex = false
			child, ferr := expanded.Field(part.Name)
			if ferr != nil {
				if errors.Is(ferr, model.ErrOptimizedOut) {
					return nil, model.New(model.CodeTargetOptimized, "变量 %s 已不存在于调试信息中", part.Name)
				}
				return nil, model.New(model.CodeInputNotFound, "路径 %s 不存在", raw).
					WithFix("先用 debug_inspect 确认可用的成员名")
			}
			cur = child
		}
		if part.HasIndex {
			expanded, err = a.ensureExpanded(cur)
			if err != nil {
				return nil, err
			}
			loc.parent = expanded
			loc.isIndex = true
			loc.index = part.Index
			child, ierr := expanded.Item(part.Index)
			if ierr != nil {
				return nil, model.New(model.CodeInputNotFound, "索引 %d 超出范围", part.Index)
			}
			cur = child
		}
	}
	loc.value = cur
	return loc, nil
}

func (a *Adapter) setProperty(loc *locator, arg map[string]any) error {
	fn := "function(k,v){'use strict'; this[k]=v}"
	args := []map[string]any{{"value": loc.name}}
	if loc.isIndex {
		fn = "function(i,v){'use strict'; this[i]=v}"
		args = []map[string]any{{"value": loc.index}}
	}
	args = append(args, arg)

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var res struct {
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := a.client.call(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            loc.parent.Handle,
		"functionDeclaration": fn,
		"arguments":           args,
		"returnByValue":       true,
	}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		detail := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil && res.ExceptionDetails.Exception.Description != "" {
			detail = res.ExceptionDetails.Exception.Description
		}
		if isReadOnlyMessage(detail) {
			return model.New(model.CodeTargetReadOnly, "目标属性不可写: %s", detail)
		}
		return model.New(model.CodeInputInvalid, "写入失败: %s", detail)
	}
	return nil
}

func isReadOnlyMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "read only") ||
		strings.Contains(lower, "readonly") ||
		strings.Contains(lower, "not extensible") ||
		strings.Contains(lower, "getter")
}

// literalToArg 把字面量转换为 CDP 调用参数。
//
// 只接受与目标类型匹配的字面量：不做隐式转换，也不把字符串再次当作表达式求值。
func literalToArg(typ, literal string) (map[string]any, error) {
	switch typ {
	case "num":
		if _, err := strconv.ParseFloat(literal, 64); err != nil {
			return nil, model.New(model.CodeInputInvalid, "目标类型为数字，无法写入 %q", literal).
				WithFix("给出数字字面量，如 42 或 3.14")
		}
		return map[string]any{"value": json.Number(literal)}, nil
	case "bool":
		if literal != "true" && literal != "false" {
			return nil, model.New(model.CodeInputInvalid, "目标类型为布尔，无法写入 %q", literal).
				WithFix("使用 true 或 false")
		}
		return map[string]any{"value": literal == "true"}, nil
	case "str":
		return map[string]any{"value": literal}, nil
	case "null":
		return map[string]any{"value": nil}, nil
	case "undefined":
		return map[string]any{}, nil
	}
	return nil, model.New(model.CodeCapUnsupported, "不支持向类型 %s 写入字面量", typ).
		WithFix("只支持数字、布尔、字符串、null、undefined")
}

// Eval 实现 backend.Debugger。
func (a *Adapter) Eval(in backend.EvalInput) (backend.EvalOutput, error) {
	if in.Snapshot == nil {
		return backend.EvalOutput{}, model.New(model.CodeStateNotStopped, "目标未停止，无法求值")
	}
	frame, err := in.Snapshot.Frame(in.Frame)
	if err != nil {
		return backend.EvalOutput{}, err
	}
	if frame.BackendID == "" {
		return backend.EvalOutput{}, model.New(model.CodeCapUnsupported, "当前栈帧不支持求值").
			WithFix("改用 debug_inspect 读取变量")
	}
	display, err := a.evaluate(context.Background(), frame.BackendID, in.Expression)
	if err != nil {
		return backend.EvalOutput{}, err
	}
	return backend.EvalOutput{
		Type:       "expr",
		Value:      display,
		SideEffect: strings.Contains(in.Expression, "("),
	}, nil
}
