package jvm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// CheckDebugInfo 校验 JVM 目标是否带够调试信息。
//
// 行号表缺失会让断点无法按行命中（block）；局部变量表缺失只会让变量读不到
// （warn）。四种 JVM 语言共用同一套字节码判据，但修复命令必须按语言给出，
// 否则 Kotlin/Scala 用户会拿到一条自己跑不了的 javac 命令。
// jar 启动与缺少 javap 时跳过校验并说明原因，而不是假装通过。
func CheckDebugInfo(lang backend.Language, command []string, cwd string) ([]string, error) {
	cmdInfo, err := parseJavaCommand(command)
	if err != nil {
		return nil, err
	}
	if cmdInfo.Jar != "" {
		return []string{"使用 -jar 启动：跳过 class 调试信息校验（jar 内容不在磁盘类路径上）"}, nil
	}

	base := cwd
	if base == "" {
		base, _ = os.Getwd()
	}
	classFile := findClassFile(cmdInfo, base)
	if classFile == "" {
		return []string{"未找到主类 " + cmdInfo.MainClass + " 的 class 文件，跳过调试信息校验；请先构建后再调试"}, nil
	}

	info := javaRuntime()
	if info.javap == "" {
		return []string{"未找到 javap，跳过调试信息校验"}, nil
	}
	cp := classPathArg(cmdInfo, base)
	output, jerr := runTool(info.javap, []string{"-p", "-l", "-cp", cp, cmdInfo.MainClass}, base, 10*time.Second)
	if jerr != nil {
		return []string{"javap 校验失败，跳过调试信息检查: " + jerr.Error()}, nil
	}
	// Scala 的 object、Kotlin 的伴生实现可能把真实方法体放在 `Main$` 里，主类只留
	// 转发方法。只查主类会把可调试目标误判为 block，因此把 `Main$` 一并纳入判据。
	if !strings.Contains(output, "LineNumberTable") {
		if alt, aerr := runTool(info.javap, []string{"-p", "-l", "-cp", cp, cmdInfo.MainClass + "$"}, base, 10*time.Second); aerr == nil {
			output += "\n" + alt
		}
	}
	fix := debugInfoFixFor(lang)
	if !strings.Contains(output, "LineNumberTable") {
		return nil, model.New(model.CodeEnvDebugInfo,
			"主类 %s 的字节码缺少行号表，断点无法按行命中", cmdInfo.MainClass).
			WithFix("用 " + fix + " 重新编译（不要关闭调试信息），再重新启动调试会话")
	}
	if !strings.Contains(output, "LocalVariableTable") {
		return []string{"主类缺少局部变量表：断点能命中，但 locals/args 读不到；建议用 " + fix + " 重新编译"}, nil
	}
	return nil, nil
}

// jvmDebugInfoFixes 是各语言补全调试信息的编译方式。
//
// 同一份字节码判据，但“怎么重编”按语言不同：把 javac 的答案给 Scala 用户等于
// 没有修复建议。
var jvmDebugInfoFixes = map[backend.Language]string{
	backend.Java:   "javac -g",
	backend.Kotlin: "kotlinc（Gradle/Maven 保持默认 debug 配置）",
	backend.Scala:  "scalac -g:vars",
	backend.Groovy: "groovyc -g",
}

// debugInfoFixFor 返回该语言补全调试信息的编译方式，未知语言按 Java 处理。
func debugInfoFixFor(lang backend.Language) string {
	if fix, ok := jvmDebugInfoFixes[lang]; ok {
		return fix
	}
	return jvmDebugInfoFixes[backend.Java]
}

// findClassFile 在类路径中定位主类的 class 文件。
func findClassFile(cmdInfo javaCommand, base string) string {
	rel := filepath.FromSlash(strings.ReplaceAll(cmdInfo.MainClass, ".", "/")) + ".class"
	for _, entry := range cmdInfo.effectiveClassPath() {
		dir := entry
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(base, dir)
		}
		candidate := filepath.Join(dir, rel)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// classPathArg 生成 javap 需要的绝对类路径。
func classPathArg(cmdInfo javaCommand, base string) string {
	entries := cmdInfo.effectiveClassPath()
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if filepath.IsAbs(entry) {
			out = append(out, entry)
			continue
		}
		out = append(out, filepath.Join(base, entry))
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// runTool 在限定时间内执行外部工具并返回合并输出。
func runTool(path string, args []string, dir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
