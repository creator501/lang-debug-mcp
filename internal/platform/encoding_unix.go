//go:build !windows

package platform

import "unicode/utf8"

// DecodeOutput 把子进程输出的原始字节转换为 UTF-8 文本。
//
// Unix 平台的进程输出约定为 UTF-8；非法字节序列按原始字节返回，避免丢失
// 诊断信息。
func DecodeOutput(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return string(b)
}
