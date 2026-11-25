package platform

import (
	"path/filepath"
	"runtime"
	"strings"
)

// MSYSToWindows 把 MSYS/Cygwin 风格路径转换为 Windows 本地路径。
//
// 调试后端（尤其是 Windows 上通过 MSYS2 分发的 gdb）回传的文件名常为
// /c/Users/... 或 /cygdrive/c/Users/... 形式，与本地 C:\Users\... 不一致，
// 会导致断点匹配失败。第二个返回值表示是否发生了转换。
func MSYSToWindows(p string) (string, bool) {
	if len(p) < 2 || p[0] != '/' {
		return p, false
	}
	rest := p[1:]
	if strings.HasPrefix(rest, "cygdrive/") {
		rest = strings.TrimPrefix(rest, "cygdrive/")
	}
	drive := rest[0]
	if !isASCIIAlpha(drive) {
		return p, false
	}
	rest = rest[1:]
	if rest != "" && rest[0] != '/' {
		return p, false
	}
	converted := strings.ToUpper(string(drive)) + ":" + strings.ReplaceAll(rest, "/", `\`)
	return converted, true
}

// NormalizePath 把路径归一为当前平台的本地形式。
//
// 这是核心必经环节：所有来自调试后端的文件名、所有用户提供的路径，在参与
// 比较或展示前都必须经过本函数。
func NormalizePath(p string) string {
	if p == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		if converted, ok := MSYSToWindows(p); ok {
			p = converted
		}
		p = strings.ReplaceAll(p, "/", `\`)
		p = filepath.Clean(p)
		if len(p) >= 2 && p[1] == ':' {
			p = strings.ToUpper(p[:1]) + p[1:]
		}
		return p
	}
	return filepath.Clean(p)
}

// SamePath 报告两个路径是否指向同一位置。
//
// Windows 上大小写不敏感，Unix 上敏感；比较前双方都会被归一。
func SamePath(a, b string) bool {
	na, nb := NormalizePath(a), NormalizePath(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(na, nb)
	}
	return na == nb
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
