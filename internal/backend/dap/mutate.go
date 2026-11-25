package dap

import (
	"context"
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
	// frame 是路径实际生效的栈帧序号。
	frame int
}

// Mutate 实现 backend.Debugger。
//
// 批量改值不承诺原子性：逐条执行、逐条报告，成功时回传原值以便恢复现场。
func (s *Session) Mutate(ops []backend.MutateOp) []backend.MutateResult {
	out := make([]backend.MutateResult, 0, len(ops))
	for _, op := range ops {
		out = append(out, s.mutateOne(op))
	}
	return out
}

func (s *Session) mutateOne(op backend.MutateOp) backend.MutateResult {
	res := backend.MutateResult{Path: op.Path}

	stop := s.CurrentStop()
	if stop == nil || stop.Snapshot == nil {
		res.Code = model.CodeStateNotStopped
		res.Reason = "目标未停止，无法改值"
		return res
	}

	loc, err := s.locate(stop.Snapshot, op.Frame, op.Path)
	if err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		return res
	}
	// DAP 改值依赖适配器给出的可写表达式（如 d['a']、obj.field）；
	// 没有表达式说明该位置只能读。
	if loc.value == nil || loc.value.Handle == "" {
		res.Code = model.CodeCapUnsupported
		res.Reason = "该位置没有可写表达式"
		return res
	}

	expression, err := literalToExpr(loc.value.Type, op.Value)
	if err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		res.Type = loc.value.Type
		return res
	}

	frame, err := stop.Snapshot.Frame(loc.frame)
	if err != nil {
		res.Code = model.CodeOf(err)
		res.Reason = err.Error()
		return res
	}
	frameID, convErr := strconv.Atoi(frame.BackendID)
	if convErr != nil {
		res.Code = model.CodeCapUnsupported
		res.Reason = "当前栈帧不支持改值"
		return res
	}

	oldValue := loc.value.Display
	if err := s.writeValue(context.Background(), frameID, loc.value.Handle, expression); err != nil {
		err = s.explainWriteError(loc.value.Type, err)
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
func (s *Session) locate(snap *model.Snapshot, frame int, raw string) (*locator, error) {
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

	loc := &locator{frame: useFrame}
	for _, part := range p.Parts {
		if part.Wild || part.HasRange {
			return nil, model.New(model.CodeInputInvalid, "改值路径不支持通配符与区间").
				WithFix("逐条给出确定路径，如 locals.retries")
		}
		expanded, err := ensureExpanded(cur)
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
			expanded, err = ensureExpanded(cur)
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

// ensureExpanded 在节点尚未展开时触发一次展开。
func ensureExpanded(v *model.Value) (*model.Value, error) {
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

// setExpression 用适配器的表达式赋值能力写入新值。
func (s *Session) setExpression(ctx context.Context, frameID int, expression, value string) error {
	return s.client.Request(ctx, "setExpression", map[string]any{
		"expression": expression,
		"value":      value,
		"frameId":    frameID,
	}, nil)
}

// writeValue 写入新值。
//
// DAP 没有统一的写值请求：debugpy 一类适配器实现了 setExpression，lldb-dap
// 只认 evaluate 里的赋值表达式。先试 setExpression，失败后退回 evaluate；
// 两条路径都失败才报错，并把回退失败的原因一并给出，便于区分“不支持”与
// “写不进去”（只读段、编译期常量）。
func (s *Session) writeValue(ctx context.Context, frameID int, expression, value string) error {
	primaryErr := s.setExpression(ctx, frameID, expression, value)
	if primaryErr == nil {
		return nil
	}
	if err := s.assignByEval(ctx, frameID, expression+" = "+value); err == nil {
		return nil
	} else {
		return model.New(model.CodeOf(primaryErr), "%s（回退 evaluate 赋值失败: %v）",
			primaryErr.Error(), err)
	}
}

// assignByEval 通过 evaluate 的赋值表达式写入。
//
// 用 repl 上下文：lldb-dap 在 watch 上下文里只求值不执行赋值。
func (s *Session) assignByEval(ctx context.Context, frameID int, assignment string) error {
	return s.client.Request(ctx, "evaluate", map[string]any{
		"expression": assignment,
		"frameId":    frameID,
		"context":    "repl",
	}, nil)
}

// writeErrorMapper 是方言可选实现的写值失败解释器。
//
// 适配器只会返回一段文本，方言知道目标语言的语义（只读段、编译期常量、
// 已优化掉的存储），因此由方言把文本归类成稳定错误码。
type writeErrorMapper interface {
	// MapWriteError 归类写值失败；返回 nil 表示由通用层原样上报。
	MapWriteError(typ string, err error) error
}

// explainWriteError 让方言解释写值失败原因；方言未实现时原样返回。
func (s *Session) explainWriteError(typ string, err error) error {
	mapper, ok := s.dialect.(writeErrorMapper)
	if !ok {
		return err
	}
	if mapped := mapper.MapWriteError(typ, err); mapped != nil {
		return mapped
	}
	return err
}

// literalToExpr 把调用方给出的字面量转换为目标语言的赋值表达式。
//
// 只接受与目标类型匹配的字面量：不做隐式转换，也不把字符串再次当作表达式。
func literalToExpr(typ, literal string) (string, error) {
	switch typ {
	case "int", "float":
		if _, err := strconv.ParseFloat(strings.TrimSpace(literal), 64); err != nil {
			return "", model.New(model.CodeInputInvalid, "目标类型为数字，无法写入 %q", literal).
				WithFix("给出数字字面量，如 42 或 3.14")
		}
		// 整数写法与类型一致，避免 3.0 被写入 int 时报错。
		if typ == "int" {
			if n, err := strconv.ParseInt(strings.TrimSpace(literal), 10, 64); err == nil {
				return strconv.FormatInt(n, 10), nil
			}
		}
		return strings.TrimSpace(literal), nil
	case "bool":
		switch strings.TrimSpace(literal) {
		case "True", "true":
			return "True", nil
		case "False", "false":
			return "False", nil
		}
		return "", model.New(model.CodeInputInvalid, "目标类型为布尔，无法写入 %q", literal).
			WithFix("使用 True 或 False")
	case "str":
		return strconv.Quote(literal), nil
	case "NoneType":
		if strings.TrimSpace(literal) == "None" || strings.TrimSpace(literal) == "null" {
			return "None", nil
		}
		return "", model.New(model.CodeInputInvalid, "目标类型为 None，只能写入 None")
	}
	return "", model.New(model.CodeCapUnsupported, "不支持向类型 %s 写入字面量", typ).
		WithFix("只支持数字、布尔、字符串与 None；容器内容请逐项修改")
}

// Eval 实现 backend.Debugger。
func (s *Session) Eval(in backend.EvalInput) (backend.EvalOutput, error) {
	if in.Snapshot == nil {
		return backend.EvalOutput{}, model.New(model.CodeStateNotStopped, "目标未停止，无法求值")
	}
	frame, err := in.Snapshot.Frame(in.Frame)
	if err != nil {
		return backend.EvalOutput{}, err
	}
	frameID, convErr := strconv.Atoi(frame.BackendID)
	if convErr != nil {
		return backend.EvalOutput{}, model.New(model.CodeCapUnsupported, "当前栈帧不支持求值").
			WithFix("改用 debug_inspect 读取变量")
	}
	var resp struct {
		Result string `json:"result"`
		Type   string `json:"type"`
	}
	if err := s.client.Request(context.Background(), "evaluate", map[string]any{
		"expression": in.Expression,
		"frameId":    frameID,
		"context":    "watch",
	}, &resp); err != nil {
		return backend.EvalOutput{}, err
	}
	typ := resp.Type
	if typ == "" {
		typ = "expr"
	}
	return backend.EvalOutput{
		Type:       typ,
		Value:      resp.Result,
		SideEffect: strings.Contains(in.Expression, "("),
	}, nil
}
