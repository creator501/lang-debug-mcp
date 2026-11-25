//go:build windows

package platform

import (
	"testing"
	"unicode/utf8"
)

func TestDecodeOutputUTF8(t *testing.T) {
	const want = "中文输出 ok"
	if got := DecodeOutput([]byte(want)); got != want {
		t.Fatalf("合法 UTF-8 应原样返回，得到 %q", got)
	}
}

func TestDecodeOutputNonUTF8(t *testing.T) {
	const want = "中文输出测试"
	enc := encodeCodePage(want, cpACP)
	if len(enc) == 0 {
		t.Skip("当前系统无法用 ANSI 代码页编码测试文本")
	}
	if utf8.Valid(enc) {
		t.Skip("当前系统 ANSI 代码页即 UTF-8，无法构造非 UTF-8 输入")
	}
	if got, ok := decodeCodePage(enc, cpACP); !ok || got != want {
		t.Skip("当前系统 ANSI 代码页不是可逆的多字节编码")
	}
	if got := DecodeOutput(enc); got != want {
		t.Fatalf("非 UTF-8 输出应归一为 %q，得到 %q", want, got)
	}
}

func TestDecodeOutputInvalid(t *testing.T) {
	raw := []byte{0xff, 0xfe, 0xff}
	if got := DecodeOutput(raw); len(got) == 0 {
		t.Fatal("非法字节不应被丢弃为空串")
	}
}
