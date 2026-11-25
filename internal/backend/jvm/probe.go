package jvm

import (
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// Prober 探测 JVM 后端可用性。
type Prober struct {
	// Lang 是该探测器负责的语言；为空时按 Java 处理。
	Lang backend.Language
}

// Language 实现 backend.Prober。
func (p Prober) Language() backend.Language {
	if p.Lang == "" {
		return backend.Java
	}
	return p.Lang
}

// Backend 实现 backend.Prober。
func (p Prober) Backend() string { return backendName }

// javaInfo 是本机 JDK 的探测结果。
type javaInfo struct {
	path    string
	version string
	javap   string
	err     error
}

var (
	javaOnce sync.Once
	javaEnv  javaInfo
)

// javaRuntime 探测 java 与 javap，进程内只执行一次。
func javaRuntime() javaInfo {
	javaOnce.Do(func() {
		path, err := exec.LookPath("java")
		if err != nil {
			javaEnv = javaInfo{err: model.New(model.CodeEnvToolMissing, "PATH 中没有 java").
				WithFix("安装 JDK 17 或更高版本，并把 bin 目录加入 PATH")}
			return
		}
		out, _ := exec.Command(path, "-version").CombinedOutput()
		javap, _ := exec.LookPath("javap")
		javaEnv = javaInfo{path: path, version: parseJavaVersion(string(out)), javap: javap}
	})
	return javaEnv
}

// javaVersionPattern 匹配 java -version 输出中的版本号。
var javaVersionPattern = regexp.MustCompile(`version "([^"]+)"`)

// parseJavaVersion 从 java -version 输出中提取版本。
func parseJavaVersion(output string) string {
	if m := javaVersionPattern.FindStringSubmatch(output); len(m) == 2 {
		return m[1]
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return ""
}

// Probe 实现 backend.Prober。
func (p Prober) Probe() backend.Status {
	st := backend.Status{Language: p.Language(), Backend: backendName}
	info := javaRuntime()
	if info.path == "" {
		st.Missing = []string{"java"}
		if info.err != nil {
			if coded, ok := info.err.(*model.Error); ok {
				st.InstallHint = coded.Fix
			}
		}
		return st
	}
	st.Available = true
	st.Version = info.version
	st.Notes = []string{
		"解释器: " + info.path,
		"协议: JDWP（-agentlib:jdwp），不依赖 java-debug 适配器",
	}
	if info.javap == "" {
		st.Notes = append(st.Notes, "未找到 javap：无法校验字节码的调试信息")
	} else {
		st.Notes = append(st.Notes, "javap: "+info.javap+"（用于校验行号表与局部变量表）")
	}
	return st
}
