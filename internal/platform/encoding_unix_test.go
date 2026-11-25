//go:build !windows

package platform

import "testing"

func TestDecodeOutputUTF8(t *testing.T) {
	const want = "中文输出 ok"
	if got := DecodeOutput([]byte(want)); got != want {
		t.Fatalf("UTF-8 输出应原样返回，得到 %q", got)
	}
}

func TestDecodeOutputInvalid(t *testing.T) {
	raw := []byte{0xff, 0xfe, 0xff}
	if got := DecodeOutput(raw); len(got) == 0 {
		t.Fatal("非法字节不应被丢弃为空串")
	}
}
