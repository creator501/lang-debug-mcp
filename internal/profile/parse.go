package profile

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Format 是已支持的原始格式标识。
const (
	FormatFolded = "folded"
	FormatV8     = "v8"
)

// Folded 支持的折叠栈格式来源：py-spy 的 raw、async-profiler 的 collapsed、
// perf 经 stackcollapse-perf 处理后的输出，以及 lldb 采样脚本的折叠输出。
//
// 这些来源的差异只在帧文本上：有的带 `文件:行`，有的只有符号名；解析时
// 能取到多少就记多少，不为了对齐字段而编造位置。
func ParseFolded(r io.Reader) ([]Sample, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	samples := make([]Sample, 0, 256)
	lineno := 0
	for scanner.Scan() {
		lineno++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		stack, weight, err := splitFolded(line)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行无法解析: %w", lineno, err)
		}
		frames := make([]Frame, 0, 8)
		parts := strings.Split(stack, ";")
		// 折叠栈从根到叶排列，统一翻转成叶到根。
		for i := len(parts) - 1; i >= 0; i-- {
			if frame := parseFoldedFrame(parts[i]); frame.Func != "" {
				frames = append(frames, frame)
			}
		}
		if len(frames) == 0 {
			continue
		}
		samples = append(samples, Sample{Frames: frames, Weight: weight})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取折叠栈失败: %w", err)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("折叠栈里没有任何可用样本（期望每行形如 `a;b;c 12`）")
	}
	return samples, nil
}

// splitFolded 把一行折叠栈拆成“栈”与“样本数”；样本数在最后一个空格之后。
func splitFolded(line string) (string, int, error) {
	idx := strings.LastIndex(line, " ")
	if idx <= 0 || idx == len(line)-1 {
		return "", 0, fmt.Errorf("缺少样本数列")
	}
	weight, err := strconv.Atoi(strings.TrimSpace(line[idx+1:]))
	if err != nil || weight <= 0 {
		return "", 0, fmt.Errorf("样本数不是正整数: %q", line[idx+1:])
	}
	return strings.TrimSpace(line[:idx]), weight, nil
}

// foldedFramePattern 匹配 `函数名 (文件:行)` 形式；取不到位置时只保留函数名。
func parseFoldedFrame(text string) Frame {
	text = strings.TrimSpace(text)
	if text == "" {
		return Frame{}
	}
	open := strings.LastIndex(text, " (")
	if open > 0 && strings.HasSuffix(text, ")") {
		name := strings.TrimSpace(text[:open])
		loc := text[open+2 : len(text)-1]
		file, line := splitLocation(loc)
		return Frame{Func: name, File: file, Line: line}
	}
	return Frame{Func: text}
}

// splitLocation 拆 `文件:行`；行号取不到时整段当文件名。
func splitLocation(loc string) (string, int) {
	idx := strings.LastIndex(loc, ":")
	if idx <= 0 || idx == len(loc)-1 {
		return loc, 0
	}
	line, err := strconv.Atoi(loc[idx+1:])
	if err != nil || line <= 0 {
		return loc, 0
	}
	return loc[:idx], line
}

// v8Profile 是 V8 `.cpuprofile`（Chrome DevTools 格式）的最小结构。
type v8Profile struct {
	Nodes []struct {
		ID        int `json:"id"`
		CallFrame struct {
			FunctionName string `json:"functionName"`
			URL          string `json:"url"`
			LineNumber   int    `json:"lineNumber"`
		} `json:"callFrame"`
		HitCount int   `json:"hitCount"`
		Children []int `json:"children"`
	} `json:"nodes"`
	Samples []int `json:"samples"`
}

// ParseV8 解析 V8 CPU profile 的节点图：用父子关系重建调用栈。
//
// 有 samples 时逐样本累计（更准）；只有 hitCount 时按节点命中数累计，
// 这种情况下栈仍可还原，但样本总数等于命中数之和。
func ParseV8(data []byte) ([]Sample, error) {
	var raw v8Profile
	if err := json.Unmarshal(bytes.TrimSpace(data), &raw); err != nil {
		return nil, fmt.Errorf("解析 V8 剖析结果失败: %w", err)
	}
	if len(raw.Nodes) == 0 {
		return nil, fmt.Errorf("V8 剖析结果里没有节点（期望 .cpuprofile 的 nodes 字段）")
	}
	nodes := make(map[int]int, len(raw.Nodes)) // id → 下标
	parent := make(map[int]int, len(raw.Nodes))
	for i, node := range raw.Nodes {
		nodes[node.ID] = i
		for _, child := range node.Children {
			parent[child] = node.ID
		}
	}
	stackOf := func(id int) []Frame {
		frames := make([]Frame, 0, 16)
		for cur := id; cur != 0; cur = parent[cur] {
			idx, ok := nodes[cur]
			if !ok {
				break
			}
			node := raw.Nodes[idx]
			if node.CallFrame.FunctionName == "(root)" || node.CallFrame.FunctionName == "" && node.CallFrame.URL == "" {
				break
			}
			frames = append(frames, Frame{
				Func: node.CallFrame.FunctionName,
				File: urlToFile(node.CallFrame.URL),
				Line: node.CallFrame.LineNumber + 1,
			})
		}
		return frames
	}

	samples := make([]Sample, 0, len(raw.Samples))
	if len(raw.Samples) > 0 {
		for _, id := range raw.Samples {
			frames := stackOf(id)
			if len(frames) == 0 {
				continue
			}
			samples = append(samples, Sample{Frames: frames, Weight: 1})
		}
	} else {
		for _, node := range raw.Nodes {
			if node.HitCount <= 0 {
				continue
			}
			frames := stackOf(node.ID)
			if len(frames) == 0 {
				continue
			}
			samples = append(samples, Sample{Frames: frames, Weight: node.HitCount})
		}
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("V8 剖析结果里没有可用样本")
	}
	return samples, nil
}

// urlToFile 把 V8 的脚本地址转成文件路径；无法识别时原样保留。
func urlToFile(url string) string {
	if url == "" {
		return ""
	}
	url = strings.TrimPrefix(url, "file://")
	if len(url) > 2 && url[0] == '/' && url[2] == ':' {
		// Windows：/C:/dir/app.js → C:/dir/app.js
		url = url[1:]
	}
	return url
}

// Parse 按格式解析剖析数据。
func Parse(format string, data []byte) ([]Sample, error) {
	switch format {
	case FormatFolded:
		return ParseFolded(bytes.NewReader(data))
	case FormatV8:
		return ParseV8(data)
	}
	return nil, fmt.Errorf("不支持的剖析格式 %q（可用: %s、%s）", format, FormatFolded, FormatV8)
}
