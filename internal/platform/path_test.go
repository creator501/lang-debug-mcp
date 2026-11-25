package platform

import (
	"runtime"
	"testing"
)

func TestMSYSToWindows(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"盘符形式", "/c/Users/dev/main.c", `C:\Users\dev\main.c`, true},
		{"盘符大写形式", "/D/tmp/a.log", `D:\tmp\a.log`, true},
		{"cygdrive 形式", "/cygdrive/e/work/app", `E:\work\app`, true},
		{"仅盘符", "/c", "C:", true},
		{"非盘符路径", "/usr/bin/gdb", "", false},
		{"多字母首段", "/ab/c", "", false},
		{"已是本地路径", `C:\Users\dev`, "", false},
		{"相对路径", "src/main.c", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := MSYSToWindows(c.in)
			if ok != c.ok {
				t.Fatalf("MSYSToWindows(%q) 转换标志 = %v, 期望 %v", c.in, ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("MSYSToWindows(%q) = %q, 期望 %q", c.in, got, c.want)
			}
			if !ok && got != c.in {
				t.Fatalf("未转换时应原样返回，得到 %q", got)
			}
		})
	}
}

func TestNormalizePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		cases := map[string]string{
			`C:/Users/dev/../dev/main.c`: `C:\Users\dev\main.c`,
			`/c/Users/dev/main.c`:        `C:\Users\dev\main.c`,
			`c:\users\dev\main.c`:        `C:\users\dev\main.c`,
		}
		for in, want := range cases {
			if got := NormalizePath(in); got != want {
				t.Fatalf("NormalizePath(%q) = %q, 期望 %q", in, got, want)
			}
		}
		return
	}
	cases := map[string]string{
		"/home/dev/../dev/main.c": "/home/dev/main.c",
		"/c/Users/dev/main.c":     "/c/Users/dev/main.c",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Fatalf("NormalizePath(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestSamePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		if !SamePath(`C:\Users\Dev\a.c`, `/c/Users/Dev/a.c`) {
			t.Fatal("同一文件的本地形式与 MSYS 形式应判定为相同")
		}
		if !SamePath(`C:\Users\Dev\a.c`, `c:/users/dev/a.c`) {
			t.Fatal("Windows 路径比较应大小写不敏感")
		}
		if SamePath(`C:\a\b.c`, `C:\a\c.c`) {
			t.Fatal("不同文件不应判定为相同")
		}
		return
	}
	if !SamePath(`/home/dev/a.c`, `/home/dev/./a.c`) {
		t.Fatal("归一后应判定为相同")
	}
	if SamePath(`/home/dev/a.c`, `/home/dev/A.c`) {
		t.Fatal("Unix 路径比较应大小写敏感")
	}
}
