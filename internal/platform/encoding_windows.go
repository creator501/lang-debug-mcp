//go:build windows

package platform

import (
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 代码页编号：系统 ANSI 代码页与 OEM（控制台）代码页。
const (
	cpACP = 0
	cpOEM = 1
)

// procWideCharToMultiByte 用于测试侧构造非 UTF-8 字节流；x/sys/windows 未导出该封装。
var procWideCharToMultiByte = windows.NewLazySystemDLL("kernel32.dll").NewProc("WideCharToMultiByte")

// DecodeOutput 把子进程输出的原始字节转换为 UTF-8 文本。
//
// Windows 控制台与部分运行时的输出不是 UTF-8（如中文系统上的 GBK），直接
// 按字节透传会在展示层产生乱码。这里依次尝试 UTF-8、系统 ANSI 代码页、
// OEM 代码页，全部失败时按原始字节返回（不丢数据，交由调用方判断）。
func DecodeOutput(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	for _, cp := range []uint32{cpACP, cpOEM} {
		if s, ok := decodeCodePage(b, cp); ok {
			return s
		}
	}
	return string(b)
}

func decodeCodePage(b []byte, cp uint32) (string, bool) {
	n, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return "", false
	}
	buf := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), &buf[0], n); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}

// encodeCodePage 把 UTF-8 文本编码为指定代码页，仅用于测试与自检。
func encodeCodePage(s string, cp uint32) []byte {
	if s == "" {
		return nil
	}
	u, err := windows.UTF16FromString(strings.ReplaceAll(s, "\x00", ""))
	if err != nil || len(u) <= 1 {
		return nil
	}
	n, _, _ := procWideCharToMultiByte.Call(
		uintptr(cp), 0,
		uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1),
		0, 0, 0, 0,
	)
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	if _, _, _ = procWideCharToMultiByte.Call(
		uintptr(cp), 0,
		uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1),
		uintptr(unsafe.Pointer(&out[0])), n, 0, 0,
	); len(out) == 0 {
		return nil
	}
	return out
}
