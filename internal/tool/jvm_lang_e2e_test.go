package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/jvm"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

const (
	kotlinFixture = "../../testdata/fixture/App.kt"
	scalaFixture  = "../../testdata/fixture/App.scala"
	groovyFixture = "../../testdata/fixture/App.groovy"
)

var (
	kotlinBuild fixtureBuild
	scalaBuild  fixtureBuild
	groovyBuild fixtureBuild
)

// fixtureBuild 缓存某个语言夹具的编译结果，避免每个测试都重编一次。
type fixtureBuild struct {
	once sync.Once
	dir  string
	skip string
}

// compile 编译夹具并返回 class 目录；编译器缺失或编译失败时跳过当前测试。
func (b *fixtureBuild) compile(t *testing.T, compiler, srcFile string, extra ...string) string {
	t.Helper()
	if _, err := exec.LookPath(compiler); err != nil {
		t.Skipf("跳过 %s 测试: 未找到 %s", compiler, compiler)
	}
	b.once.Do(func() {
		src, err := filepath.Abs(srcFile)
		if err != nil {
			b.skip = err.Error()
			return
		}
		dir, err := os.MkdirTemp("", "ldmcp-"+compiler)
		if err != nil {
			b.skip = err.Error()
			return
		}
		args := append(append([]string{}, extra...), "-d", dir, src)
		if out, cerr := runCompiler(compiler, args...); cerr != nil {
			b.skip = fmt.Sprintf("%s 编译失败: %v\n%s", compiler, cerr, out)
			return
		}
		b.dir = dir
	})
	if b.skip != "" {
		t.Skipf("跳过 %s 测试: %s", compiler, b.skip)
	}
	t.Logf("%s class 目录: %s", compiler, b.dir)
	return b.dir
}

// runCompiler 执行编译器；Windows 上 .bat/.cmd 必须经 cmd 转一手。
func runCompiler(name string, args ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	var cmd *exec.Cmd
	lower := strings.ToLower(path)
	if runtime.GOOS == "windows" && (strings.HasSuffix(lower, ".bat") || strings.HasSuffix(lower, ".cmd")) {
		cmd = exec.Command("cmd", append([]string{"/c", path}, args...)...)
	} else {
		cmd = exec.Command(path, args...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runtimeJar 找到编译器同级的运行期库（标准分发包的 lib/kotlinc 布局）。
//
// Kotlin/Scala 编译产物运行时仍需 stdlib；测试按“编译器旁 lib/”惯例查找，
// 也允许用环境变量覆盖，避免把本机路径写进代码。
func runtimeJar(t *testing.T, compiler, jarName, env string) string {
	t.Helper()
	if override := os.Getenv(env); override != "" {
		return override
	}
	path, err := exec.LookPath(compiler)
	if err != nil {
		t.Skipf("跳过：未找到 %s", compiler)
	}
	candidate, err := filepath.Abs(filepath.Join(filepath.Dir(path), "..", "lib", jarName))
	if err != nil {
		t.Skipf("跳过：无法解析 %s 路径", jarName)
	}
	if st, serr := os.Stat(candidate); serr != nil || st.IsDir() {
		t.Skipf("跳过：未找到 %s（可用 %s 指定）", jarName, env)
	}
	return candidate
}

// joinClassPath 拼接类路径，Linux/macOS 用冒号分隔。
func joinClassPath(entries ...string) string {
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	return strings.Join(entries, sep)
}

// sourceMarkerLine 返回包含标记的源码行号（1 起）。
func sourceMarkerLine(t *testing.T, path, marker string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	t.Fatalf("%s 中未找到标记 %s", path, marker)
	return 0
}

// openLangSession 按给定语言与启动命令建立调试会话。
func openLangSession(t *testing.T, lang backend.Language, command []string, cwd string) *Session {
	t.Helper()
	manager := NewManager(time.Minute)
	session, err := manager.Open(context.Background(), lang, jvm.Launcher{}, backend.LaunchSpec{
		Language: lang,
		Mode:     "launch",
		Command:  command,
		Cwd:      cwd,
		Timeout:  40 * time.Second,
	})
	if err != nil {
		t.Fatalf("建立 %s 会话失败: %v", lang, err)
	}
	return session
}

// TestJVMDebugInfoPerLanguage 验证四种 JVM 语言的夹具按同一套判据都能通过。
func TestJVMDebugInfoPerLanguage(t *testing.T) {
	javaDir := requireJVM(t)
	cwd := filepath.Dir(javaFixturePath(t))
	kotlinDir := kotlinBuild.compile(t, "kotlinc", kotlinFixture)
	scalaDir := scalaBuild.compile(t, "scalac", scalaFixture, "-g:vars")
	cases := []struct {
		lang    backend.Language
		cp      string
		main    string
		fixture string
	}{
		{backend.Java, javaDir, "App", javaFixture},
		{backend.Kotlin, joinClassPath(kotlinDir, runtimeJar(t, "kotlinc", "kotlin-stdlib.jar", "LDMCP_KOTLIN_STDLIB")), "AppKt", kotlinFixture},
		{backend.Scala, joinClassPath(scalaDir, runtimeJar(t, "scalac", "scala-library.jar", "LDMCP_SCALA_LIBRARY")), "App", scalaFixture},
		{backend.Groovy, groovyBuild.compile(t, "groovyc", groovyFixture), "App", groovyFixture},
	}
	for _, c := range cases {
		warnings, err := jvm.CheckDebugInfo(c.lang, []string{"java", "-cp", c.cp, c.main}, cwd)
		if err != nil {
			t.Fatalf("%s 夹具应带行号表，实际被拒绝: %v", c.lang, err)
		}
		if len(warnings) != 0 {
			t.Fatalf("%s 夹具不应有调试信息警告，实际: %v", c.lang, warnings)
		}
	}
}

// TestJVMDebugInfoBlockAndWarn 验证缺少行号表为 block、缺少变量表为 warn，
// 且修复命令按语言给出。
func TestJVMDebugInfoBlockAndWarn(t *testing.T) {
	requireJVM(t)
	src, err := filepath.Abs(javaFixture)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	cwd := filepath.Dir(src)
	langs := []struct {
		lang backend.Language
		hint string
	}{
		{backend.Java, "javac -g"},
		{backend.Kotlin, "kotlinc"},
		{backend.Scala, "scalac -g:vars"},
		{backend.Groovy, "groovyc -g"},
	}

	noLines := compileJavaWith(t, src, "-g:none")
	for _, c := range langs {
		warnings, cerr := jvm.CheckDebugInfo(c.lang, []string{"java", "-cp", noLines, "App"}, cwd)
		if cerr == nil {
			t.Fatalf("%s 缺少行号表必须判为 block，实际返回 warnings=%v", c.lang, warnings)
		}
		if model.CodeOf(cerr) != model.CodeEnvDebugInfo {
			t.Fatalf("%s 的 block 错误码应为 ENV_DEBUG_INFO，实际 %s", c.lang, model.CodeOf(cerr))
		}
		if !strings.Contains(cerr.Error(), c.hint) {
			t.Fatalf("%s 的修复建议应提到 %q，实际: %v", c.lang, c.hint, cerr)
		}
	}

	linesOnly := compileJavaWith(t, src, "-g:lines")
	// 有些 JDK 在 Linux 上会把 -g:lines 的行号表也省掉，此时无法构造出
	// 「有行号、无变量表」的产物；跳过而不是把 javac 的行为误报成产品缺陷。
	if _, cerr := jvm.CheckDebugInfo(backend.Java, []string{"java", "-cp", linesOnly, "App"}, cwd); cerr != nil && strings.Contains(cerr.Error(), "缺少行号表") {
		t.Skipf("当前 javac -g:lines 未生成行号表，无法验证变量表缺失降级: %v", cerr)
	}
	for _, c := range langs {
		warnings, cerr := jvm.CheckDebugInfo(c.lang, []string{"java", "-cp", linesOnly, "App"}, cwd)
		if cerr != nil {
			t.Fatalf("%s 缺少变量表应降级为 warn，实际被拒绝: %v", c.lang, cerr)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "局部变量表") || !strings.Contains(warnings[0], c.hint) {
			t.Fatalf("%s 的警告应说明变量表缺失并给出 %q，实际: %v", c.lang, c.hint, warnings)
		}
	}
}

// compileJavaWith 用指定调试信息级别编译 Java 夹具，返回 class 目录。
func compileJavaWith(t *testing.T, src string, debugFlag string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ldmcp-javac-info")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	if out, cerr := runCompiler("javac", debugFlag, "-encoding", "UTF-8", "-d", dir, src); cerr != nil {
		t.Fatalf("javac %s 编译失败: %v\n%s", debugFlag, cerr, out)
	}
	return dir
}

// TestKotlinBreakpointAndValues 在 Kotlin 夹具上验证断点命中与取值。
func TestKotlinBreakpointAndValues(t *testing.T) {
	classDir := kotlinBuild.compile(t, "kotlinc", kotlinFixture)
	fixture, err := filepath.Abs(kotlinFixture)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	stdlib := runtimeJar(t, "kotlinc", "kotlin-stdlib.jar", "LDMCP_KOTLIN_STDLIB")
	cp := joinClassPath(classDir, stdlib)
	session := openLangSession(t, backend.Kotlin, []string{"java", "-cp", cp, "AppKt"}, filepath.Dir(fixture))
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := sourceMarkerLine(t, fixture, "BREAK_HERE")
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	// 入口停止时主类尚未加载，断点先处于待解析状态，命中前的类加载会补上。
	t.Logf("Kotlin 断点 %s verified=%v note=%q", bp.ID, bp.Verified, bp.Note)
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q（reason=%s）", bp.ID, stop.BreakpointID, stop.Reason)
	}
	snap := session.ApplyStop(stop)
	assertKotlinOrScalaValues(t, snap, line)

	// Item.price 是普通实例字段，用来验证 ObjectReference.SetValues 的编码。
	mut := dbg.Mutate([]backend.MutateOp{{Path: "locals.items[0].price", Value: "11"}})
	if len(mut) != 1 || !mut[0].OK {
		t.Fatalf("实例字段改值应成功: %+v", mut)
	}
	refreshed, err := session.RefreshAfterMutation("kotlin-field")
	if err != nil {
		t.Fatalf("实例字段改值后刷新快照失败: %v", err)
	}
	mutRes, err := refreshed.Resolve([]string{"locals.items[0].price"}, model.DefaultBudget())
	if err != nil {
		t.Fatalf("实例字段取值失败: %v", err)
	}
	if got := mutRes.Values["locals.items[0].price"].Value; got != "11" {
		t.Fatalf("实例字段改值未生效，实际 %q", got)
	}
}

// TestScalaBreakpointAndValues 在 Scala 夹具上验证断点命中与取值。
func TestScalaBreakpointAndValues(t *testing.T) {
	classDir := scalaBuild.compile(t, "scalac", scalaFixture, "-g:vars")
	fixture, err := filepath.Abs(scalaFixture)
	if err != nil {
		t.Fatalf("解析夹具路径失败: %v", err)
	}
	lib := runtimeJar(t, "scalac", "scala-library.jar", "LDMCP_SCALA_LIBRARY")
	cp := joinClassPath(classDir, lib)
	session := openLangSession(t, backend.Scala, []string{"java", "-cp", cp, "App"}, filepath.Dir(fixture))
	defer session.Close()

	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("取后端失败: %v", err)
	}
	line := sourceMarkerLine(t, fixture, "BREAK_HERE")
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: fixture, Line: line})
	if err != nil {
		t.Fatalf("设置断点失败: %v", err)
	}
	t.Logf("Scala 断点 %s verified=%v note=%q", bp.ID, bp.Verified, bp.Note)
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("继续执行失败: %v", err)
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("应命中断点 %s，实际 %q（reason=%s）", bp.ID, stop.BreakpointID, stop.Reason)
	}
	snap := session.ApplyStop(stop)
	assertKotlinOrScalaValues(t, snap, line)
}

// assertKotlinOrScalaValues 断言两种语言的夹具在同一停止位置读到同样的取值。
func assertKotlinOrScalaValues(t *testing.T, snap *model.Snapshot, line int) {
	t.Helper()
	if top := snap.Frames[0]; top.Where.Line != line {
		t.Fatalf("停止行号 = %d，期望 %d", top.Where.Line, line)
	}
	budget := model.Budget{Depth: 6, MaxItems: 50}
	res, err := snap.Resolve([]string{"locals.total", "args.base", "locals.items", "locals.items[0].price"}, budget)
	if err != nil {
		t.Fatalf("取值失败: %v", err)
	}
	if got := res.Values["locals.total"].Value; got != "0" {
		t.Fatalf("total 初值应为 0，实际 %q", got)
	}
	if got := res.Values["args.base"].Value; got != "2" {
		t.Fatalf("base 应为 2，实际 %q", got)
	}
	if got := res.Values["locals.items"].Total; got != 2 {
		t.Fatalf("items 长度应为 2，实际 %d", got)
	}
	if got := res.Values["locals.items[0].price"].Value; got != "10" {
		t.Fatalf("items[0].price 应为 10，实际 %q", got)
	}
}
