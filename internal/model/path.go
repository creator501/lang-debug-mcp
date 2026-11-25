package model

import (
	"fmt"
	"strconv"
	"strings"
)

// Part 是取值路径中的一段。
//
// 支持的形式：name、*、name[3]、name[0..19]、[3]。
type Part struct {
	Name     string
	Wild     bool
	HasIndex bool
	Index    int
	HasRange bool
	From     int
	To       int
	// Open 表示 name[*] 形式：上界由集合长度与预算共同决定，不属于调用方指定越界。
	Open bool
}

// Path 是一条解析后的取值路径。
type Path struct {
	Raw   string
	Frame int
	Scope string
	Parts []Part
}

// ParsePath 解析取值路径。
//
// 路径形如 this.items[0..19]、frame[2].args.name、locals.*。省略作用域时默认
// 取 locals，省略 frame 时默认取当前栈帧。
func ParsePath(raw string) (Path, error) {
	p := Path{Raw: raw, Frame: 0, Scope: ScopeLocals}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return p, New(CodeInputInvalid, "路径为空")
	}
	tokens := splitPathSegments(trimmed)
	for i, token := range tokens {
		if token == "" {
			return p, New(CodeInputInvalid, "路径 %q 含空片段", raw)
		}
		if i == 0 {
			if idx, ok := frameSpec(token); ok {
				p.Frame = idx
				continue
			}
		}
		if p.Scope == ScopeLocals && len(p.Parts) == 0 && isScopeName(token) && !strings.Contains(token, "[") {
			p.Scope = token
			continue
		}
		part, err := parsePart(token)
		if err != nil {
			return p, err
		}
		p.Parts = append(p.Parts, part)
	}
	return p, nil
}

// maxRangeIndex 是 name[*] 的开放上界，实际范围由集合长度与预算共同决定。
const maxRangeIndex = 1 << 30

// splitPathSegments 按 '.' 切分路径，但忽略方括号内部的点。
//
// 区间写法（items[0..19]）含两个点，直接按 '.' 切分会把它切断。
func splitPathSegments(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '.':
			if depth != 0 {
				continue
			}
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func isScopeName(token string) bool {
	switch token {
	case ScopeLocals, ScopeArgs, ScopeThis, ScopeStatics:
		return true
	}
	return false
}

func frameSpec(token string) (int, bool) {
	inner, ok := bracket(token, "frame")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(inner)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// bracket 解析形如 prefix[...] 的片段。
func bracket(token, prefix string) (string, bool) {
	if !strings.HasPrefix(token, prefix+"[") || !strings.HasSuffix(token, "]") {
		return "", false
	}
	return token[len(prefix)+1 : len(token)-1], true
}

func parsePart(token string) (Part, error) {
	if token == "*" {
		return Part{Wild: true}, nil
	}
	part := Part{}
	name := token
	if idx := strings.IndexByte(token, '['); idx >= 0 {
		if !strings.HasSuffix(token, "]") {
			return part, New(CodeInputInvalid, "路径片段 %q 的括号未闭合", token)
		}
		name = token[:idx]
		selector := token[idx+1 : len(token)-1]
		if selector == "*" {
			// name[*] 表示按预算遍历全部元素。
			part.HasRange = true
			part.Open = true
			part.From = 0
			part.To = maxRangeIndex
			part.Name = name
			return part, nil
		}
		if strings.Contains(selector, "..") {
			bounds := strings.SplitN(selector, "..", 2)
			from, err1 := strconv.Atoi(strings.TrimSpace(bounds[0]))
			to, err2 := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err1 != nil || err2 != nil || from < 0 || to < from {
				return part, New(CodeInputInvalid, "路径片段 %q 的区间非法", token)
			}
			part.HasRange = true
			part.From = from
			part.To = to
		} else {
			n, err := strconv.Atoi(strings.TrimSpace(selector))
			if err != nil || n < 0 {
				return part, New(CodeInputInvalid, "路径片段 %q 的索引非法", token)
			}
			part.HasIndex = true
			part.Index = n
		}
	}
	part.Name = name
	if part.Name == "" && !part.HasIndex && !part.HasRange {
		return part, New(CodeInputInvalid, "路径片段 %q 缺少成员名", token)
	}
	return part, nil
}

// String 返回路径的可读描述。
func (p Path) String() string {
	if p.Parts == nil && p.Frame == 0 {
		return p.Raw
	}
	scope := p.Scope
	if scope == "" {
		scope = ScopeLocals
	}
	parts := make([]string, 0, len(p.Parts)+1)
	parts = append(parts, scope)
	for _, part := range p.Parts {
		parts = append(parts, part.String())
	}
	return fmt.Sprintf("frame[%d].%s", p.Frame, strings.Join(parts, "."))
}

// String 返回路径片段的可读描述。
func (p Part) String() string {
	if p.Wild {
		return "*"
	}
	out := p.Name
	switch {
	case p.Open:
		out += "[*]"
	case p.HasRange:
		out += fmt.Sprintf("[%d..%d]", p.From, p.To)
	case p.HasIndex:
		out += fmt.Sprintf("[%d]", p.Index)
	}
	return out
}
