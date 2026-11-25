package tool

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// langEvidence 是一份九语言验收记录：位置、观测值与复现路径。
//
// 三项缺一不可：没有位置就无法复核，没有观测值就退化成读代码猜测，
// 没有复现路径则结论不可重现。
type langEvidence struct {
	Language   string            `json:"language"`
	Location   string            `json:"location"`
	Line       int               `json:"line"`
	Breakpoint string            `json:"breakpoint"`
	Values     map[string]string `json:"values"`
	Missing    []string          `json:"missing,omitempty"`
	Reproduce  []string          `json:"reproduce"`
}

// langCase 是一种语言的验收准备：建会话并给出断点位置与取值表达式。
type langCase struct {
	name      string
	reproduce []string
	prepare   func(t *testing.T) (session *Session, file string, line int, exprs []string)
}

// nineLanguageCases 返回九种语言的验收用例。
func nineLanguageCases() []langCase {
	cases := []langCase{
		{
			name:      "nodejs",
			reproduce: []string{"node testdata/fixture/trace.js"},
			prepare: func(t *testing.T) (*Session, string, int, []string) {
				script := filepath.ToSlash(filepath.Join("..", "..", "testdata", "fixture", "trace.js"))
				line := lineOfMarker(t, "../../testdata/fixture/trace.js", "TRACE_HERE")
				return openNodeScript(t, script), script, line, []string{"locals.total"}
			},
		},
		{
			name:      "python",
			reproduce: []string{"python -m pip install debugpy", "python testdata/fixture/app.py"},
			prepare: func(t *testing.T) (*Session, string, int, []string) {
				interp := requireDebugpy(t)
				return openPythonSession(t, interp), pythonFixturePath(t), pythonBreakLine(t),
					[]string{"locals.total", "locals.items"}
			},
		},
		{
			name:      "java",
			reproduce: []string{"javac -g -d out testdata/fixture/App.java", "java -cp out App"},
			prepare: func(t *testing.T) (*Session, string, int, []string) {
				classDir := requireJVM(t)
				return openJVMSession(t, classDir), javaFixturePath(t), javaBreakLine(t),
					[]string{"locals.total"}
			},
		},
		{
			name:      "kotlin",
			reproduce: []string{"kotlinc -d out testdata/fixture/App.kt", "java -cp out AppKt"},
			prepare: func(t *testing.T) (*Session, string, int, []string) {
				classDir := kotlinBuild.compile(t, "kotlinc", kotlinFixture)
				stdlib := runtimeJar(t, "kotlinc", "kotlin-stdlib.jar", "LDMCP_KOTLIN_STDLIB")
				fixture, err := filepath.Abs(kotlinFixture)
				if err != nil {
					t.Fatalf("解析 Kotlin 夹具失败: %v", err)
				}
				session := openLangSession(t, backend.Kotlin,
					[]string{"java", "-cp", joinClassPath(classDir, stdlib), "AppKt"}, filepath.Dir(fixture))
				return session, fixture, sourceMarkerLine(t, kotlinFixture, "BREAK_HERE"), []string{"locals.total"}
			},
		},
		{
			name:      "scala",
			reproduce: []string{"scalac -g:vars -d out testdata/fixture/App.scala", "java -cp out App"},
			prepare: func(t *testing.T) (*Session, string, int, []string) {
				classDir := scalaBuild.compile(t, "scalac", scalaFixture, "-g:vars")
				lib := runtimeJar(t, "scalac", "scala-library.jar", "LDMCP_SCALA_LIBRARY")
				fixture, err := filepath.Abs(scalaFixture)
				if err != nil {
					t.Fatalf("解析 Scala 夹具失败: %v", err)
				}
				session := openLangSession(t, backend.Scala,
					[]string{"java", "-cp", joinClassPath(classDir, lib), "App"}, filepath.Dir(fixture))
				return session, fixture, sourceMarkerLine(t, scalaFixture, "BREAK_HERE"), []string{"locals.total"}
			},
		},
		{
			name:      "groovy",
			reproduce: []string{"groovyc -g -d out testdata/fixture/App.groovy", "java -cp \"out;<groovy-lib>/*\" App"},
			prepare: func(t *testing.T) (*Session, string, int, []string) {
				classDir := groovyBuild.compile(t, "groovyc", groovyFixture)
				lib := groovyLibDir(t)
				fixture, err := filepath.Abs(groovyFixture)
				if err != nil {
					t.Fatalf("解析 Groovy 夹具失败: %v", err)
				}
				session := openLangSession(t, backend.Groovy,
					[]string{"java", "-cp", joinClassPath(classDir, filepath.Join(lib, "*")), "App"},
					filepath.Dir(fixture))
				return session, fixture, sourceMarkerLine(t, groovyFixture, "BREAK_HERE"),
					[]string{"locals.total", "locals.items"}
			},
		},
	}
	for _, fixture := range nativeFixtures() {
		fixture := fixture
		cases = append(cases, langCase{
			name: string(fixture.lang),
			reproduce: []string{
				strings.Join(append(append([]string{}, fixture.compile...), "-o", "app", fixture.source), " "),
			},
			prepare: func(t *testing.T) (*Session, string, int, []string) {
				if _, err := exec.LookPath(fixture.compile[0]); err != nil {
					t.Skipf("未安装 %s，跳过 %s 验收", fixture.compile[0], fixture.lang)
				}
				return fixture.open(t), fixture.sourcePath(t), fixture.breakLine(t), []string{"locals.total"}
			},
		})
	}
	return cases
}

// groovyLibDir 从 groovyc 位置推出 Groovy 的 lib 目录，供运行时类路径使用。
func groovyLibDir(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("groovyc")
	if err != nil {
		t.Skipf("未找到 groovyc，跳过 Groovy 验收")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(path), "..", "lib"))
}

// TestAcceptanceNineLanguages 对九种语言各跑一次「建会话 → 断点 → 批量取数」，
// 并校验每份记录都含位置、观测值与复现路径。
func TestAcceptanceNineLanguages(t *testing.T) {
	for _, c := range nineLanguageCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			session, file, line, exprs := c.prepare(t)
			defer session.Close()
			record := collectEvidence(t, c.name, session, file, line, exprs, c.reproduce)
			if record.Location == "" || record.Line == 0 {
				t.Fatalf("%s 的记录缺少位置: %+v", c.name, record)
			}
			if len(record.Values) == 0 {
				t.Fatalf("%s 的记录缺少观测值: %+v", c.name, record)
			}
			if len(record.Reproduce) == 0 {
				t.Fatalf("%s 的记录缺少复现路径: %+v", c.name, record)
			}
			if record.Breakpoint == "" {
				t.Fatalf("%s 的记录缺少断点标识: %+v", c.name, record)
			}
			t.Logf("%s 证据: 位置=%s:%d 观测=%v 复现=%v",
				c.name, record.Location, record.Line, record.Values, record.Reproduce)
		})
	}
}

// collectEvidence 建断点、跑到命中并批量取值，产出一份验收记录。
func collectEvidence(t *testing.T, name string, session *Session, file string, line int, exprs, reproduce []string) *langEvidence {
	t.Helper()
	ctx := context.Background()
	dbg, err := session.Debugger(ctx)
	if err != nil {
		t.Fatalf("%s 取后端失败: %v", name, err)
	}
	bp, err := dbg.AddBreakpoint(backend.BreakpointSpec{File: file, Line: line})
	if err != nil {
		t.Fatalf("%s 设置断点失败: %v", name, err)
	}
	session.MarkRunning()
	stop, err := dbg.Continue(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("%s 未命中断点: %v\n输出:\n%s", name, err, dbg.Output(30))
	}
	if stop.BreakpointID != bp.ID {
		t.Fatalf("%s 应命中断点 %s，实际 %q", name, bp.ID, stop.BreakpointID)
	}
	snap := session.ApplyStop(stop)
	res, err := snap.Resolve(exprs, model.DefaultBudget())
	if err != nil {
		t.Fatalf("%s 批量取值失败: %v", name, err)
	}
	record := &langEvidence{
		Language:   name,
		Location:   file,
		Line:       line,
		Breakpoint: bp.ID,
		Values:     make(map[string]string, len(exprs)),
		Reproduce:  reproduce,
	}
	for _, expr := range exprs {
		if got := res.Values[expr].Value; got != "" {
			record.Values[expr] = got
			continue
		}
		record.Missing = append(record.Missing, expr)
	}
	return record
}
