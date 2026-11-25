package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
)

// sectionMap 把渲染结果按章节名索引。
func sectionMap(t *testing.T, content string) map[string]Section {
	t.Helper()
	out := make(map[string]Section)
	for _, sec := range parseSections(content) {
		out[sec.Title] = sec
	}
	return out
}

// TestRenderAllSkills 验证每种受支持语言都能展开，且固定章节齐全且非空。
func TestRenderAllSkills(t *testing.T) {
	langs := Languages()
	if len(langs) != len(backend.Supported) {
		t.Fatalf("skill 语言数应与受支持语言一致（%d），实际 %d", len(backend.Supported), len(langs))
	}
	for _, l := range langs {
		content, err := Render(l)
		if err != nil {
			t.Fatalf("%s 展开失败: %v", l.Slug, err)
		}
		if !strings.Contains(content, "name: "+l.DirName()) {
			t.Fatalf("%s 的 frontmatter 缺少 name", l.Slug)
		}
		sections := sectionMap(t, content)
		for _, title := range requiredChapters {
			sec, ok := sections[title]
			if !ok {
				t.Fatalf("%s 缺少章节「%s」", l.Slug, title)
			}
			if strings.TrimSpace(sec.Body) == "" {
				t.Fatalf("%s 的章节「%s」为空", l.Slug, title)
			}
		}
	}
}

// TestSkillCoversEveryBackendLanguage 验证每个受支持后端语言都有一份 skill。
//
// 后端清单是工具能力的唯一事实来源，新增语言却忘了加 skill 时，模型就无从
// 按现象定位到该语言的流程指引，因此两边必须一一对应。
func TestSkillCoversEveryBackendLanguage(t *testing.T) {
	have := make(map[string]bool, len(Languages()))
	for _, l := range Languages() {
		have[l.Slug] = true
	}
	for _, lang := range backend.Supported {
		if !have[string(lang)] {
			t.Fatalf("受支持语言 %s 缺少对应的 skill 插槽", lang)
		}
	}
}

// TestSharedSectionsIdentical 验证共享段落在各产物中逐字一致。
func TestSharedSectionsIdentical(t *testing.T) {
	shared := []string{"流程纪律", "结论输出规范", "收尾"}
	langs := Languages()
	base, err := Render(langs[0])
	if err != nil {
		t.Fatalf("展开 %s 失败: %v", langs[0].Slug, err)
	}
	baseSec := sectionMap(t, base)
	for _, l := range langs[1:] {
		content, err := Render(l)
		if err != nil {
			t.Fatalf("展开 %s 失败: %v", l.Slug, err)
		}
		sec := sectionMap(t, content)
		for _, title := range shared {
			if baseSec[title].Body != sec[title].Body {
				t.Fatalf("%s 与 %s 的共享章节「%s」不一致", l.Slug, langs[0].Slug, title)
			}
		}
	}
}

// TestDifferencesOnlyInSlots 验证去掉插槽正文后骨架完全一致。
func TestDifferencesOnlyInSlots(t *testing.T) {
	langs := Languages()
	base, err := Skeleton(langs[0])
	if err != nil {
		t.Fatalf("取骨架失败: %v", err)
	}
	if !strings.Contains(base, "{{slot:") {
		t.Fatal("骨架应保留插槽占位符")
	}
	for _, l := range langs[1:] {
		skel, err := Skeleton(l)
		if err != nil {
			t.Fatalf("取 %s 骨架失败: %v", l.Slug, err)
		}
		if skel != base {
			t.Fatalf("%s 与 %s 的骨架不一致，说明差异超出了插槽章节", l.Slug, langs[0].Slug)
		}
	}
}

// TestCheckPassesOnGeneratedTree 验证生成产物可通过一致性校验。
func TestCheckPassesOnGeneratedTree(t *testing.T) {
	root := t.TempDir()
	changed, err := Generate(root)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if want := len(Languages()) + 1; len(changed) != want {
		t.Fatalf("首次生成应产出 %d 个 skill 与一份 README（共 %d 个文件），实际 %d 个",
			len(Languages()), want, len(changed))
	}
	issues, err := Check(root)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("生成产物应通过校验，实际: %v", issues)
	}
	// 再生成一次不应有任何变更。
	again, err := Generate(root)
	if err != nil {
		t.Fatalf("二次生成失败: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("二次生成应无变更，实际 %v", again)
	}
}

// TestCheckDetectsTamper 验证篡改共享段落后校验失败并指出 skill 与章节。
func TestCheckDetectsTamper(t *testing.T) {
	root := t.TempDir()
	if _, err := Generate(root); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	target := filepath.Join(root, "skills", "kotlin-debug", "SKILL.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取产物失败: %v", err)
	}
	tampered := strings.Replace(string(data), "不得以单步执行替代采集。", "用单步替代采集即可。", 1)
	if tampered == string(data) {
		t.Fatal("未找到要篡改的共享段落文本")
	}
	if err := os.WriteFile(target, []byte(tampered), 0o644); err != nil {
		t.Fatalf("写入篡改内容失败: %v", err)
	}

	issues, err := Check(root)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("篡改后校验必须失败")
	}
	found := false
	for _, issue := range issues {
		if issue.Skill == "kotlin-debug" && issue.Section == "流程纪律" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应指出 kotlin-debug 的「流程纪律」章节被改动，实际: %v", issues)
	}
}

// TestCheckDetectsMissingFile 验证缺少产物文件时校验失败。
func TestCheckDetectsMissingFile(t *testing.T) {
	root := t.TempDir()
	if _, err := Generate(root); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "skills", "rust-debug", "SKILL.md")); err != nil {
		t.Fatalf("删除产物失败: %v", err)
	}
	issues, err := Check(root)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if len(issues) != 1 || issues[0].Skill != "rust-debug" || !strings.Contains(issues[0].Detail, "缺少产物文件") {
		t.Fatalf("应报告 rust-debug 缺文件，实际: %v", issues)
	}
}

// TestSlotsCarryLanguageTraps 验证各语言插槽写的是该语言特有的坑，而非通用常识。
func TestSlotsCarryLanguageTraps(t *testing.T) {
	cases := []struct {
		slug     string
		chapter  string
		keywords []string
	}{
		{"python", "preconditions", []string{"-O", "debugpy"}},
		{"python", "read-traps", []string{"cell", "生成器", "协程"}},
		{"java", "read-traps", []string{"槽位", "HashMap"}},
		{"kotlin", "read-traps", []string{"Continuation", "lazy"}},
		{"scala", "read-traps", []string{"lazy val", "MODULE$"}},
		{"groovy", "read-traps", []string{"GString", "行号"}},
		{"c", "read-traps", []string{"<optimized out>", "指针"}},
		{"cpp", "read-traps", []string{"SSO", "vector"}},
		{"rust", "read-traps", []string{"Option", "&str"}},
		{"nodejs", "read-traps", []string{"Map", "Proxy"}},
		{"java", "build", []string{"mvn", "gradlew"}},
		{"kotlin", "build", []string{"gradlew", "kotlin-stdlib"}},
		{"scala", "build", []string{"sbt", "runtime:fullClasspath"}},
		{"groovy", "build", []string{"groovyc", "GroovyMain"}},
		{"c", "build", []string{"cmake", "make"}},
		{"cpp", "build", []string{"CMAKE_CXX_FLAGS", "-O0"}},
		{"rust", "build", []string{"cargo", "target/debug"}},
		{"python", "build", []string{"poetry", "-m pytest"}},
		{"nodejs", "build", []string{"npm run build", "--enable-source-maps"}},
		{"go", "build", []string{"go build", "go.work"}},
		{"cangjie", "build", []string{"cjc", "cjpm"}},
	}
	for _, c := range cases {
		slots, err := slotText(c.slug)
		if err != nil {
			t.Fatalf("读取 %s 插槽失败: %v", c.slug, err)
		}
		body := slots[c.chapter]
		for _, kw := range c.keywords {
			if !strings.Contains(body, kw) {
				t.Fatalf("%s 的「%s」应提到 %q，实际内容不含该关键字", c.slug, c.chapter, kw)
			}
		}
	}
}

// TestSlotsCoverEveryTemplateSlot 验证每份插槽都填满模板要求的键，且正文非空。
func TestSlotsCoverEveryTemplateSlot(t *testing.T) {
	want := []string{
		"description", "triggers", "preconditions", "session", "build",
		"read-traps", "mutate-bounds", "antipatterns", "profiling",
	}
	for _, l := range Languages() {
		slots, err := slotText(l.Slug)
		if err != nil {
			t.Fatalf("读取 %s 插槽失败: %v", l.Slug, err)
		}
		for _, key := range want {
			body, ok := slots[key]
			if !ok {
				t.Fatalf("%s 插槽缺少键 %q", l.Slug, key)
			}
			if strings.TrimSpace(body) == "" {
				t.Fatalf("%s 插槽的 %q 为空", l.Slug, key)
			}
		}
	}
}

// TestSelectionTableCoversAllLanguages 验证选择表覆盖全部语言且都排除非调试场景。
func TestSelectionTableCoversAllLanguages(t *testing.T) {
	list := Selections()
	if len(list) == 0 {
		t.Fatal("选择表不能为空")
	}
	known := make(map[string]bool, len(Languages()))
	for _, l := range Languages() {
		known[l.DirName()] = false
	}
	for _, sel := range list {
		if _, ok := known[sel.Skill]; !ok {
			t.Fatalf("选择表指向不存在的 skill: %s", sel.Skill)
		}
		known[sel.Skill] = true
		if strings.TrimSpace(sel.Symptom) == "" {
			t.Fatalf("%s 的条目缺少现象描述", sel.Skill)
		}
		if strings.TrimSpace(sel.NotApply) == "" {
			t.Fatalf("%s 的条目缺少不适用声明", sel.Skill)
		}
	}
	for skill, covered := range known {
		if !covered {
			t.Fatalf("选择表未覆盖 %s，模型无法从现象定位到该 skill", skill)
		}
	}
}

// TestGenerateWritesSkillReadme 验证 README 与各 skill 同源生成且含选择表。
func TestGenerateWritesSkillReadme(t *testing.T) {
	root := t.TempDir()
	if _, err := Generate(root); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, readmeName))
	if err != nil {
		t.Fatalf("应生成 %s: %v", readmeName, err)
	}
	text := string(data)
	for _, want := range []string{"## 安装", "## 更新", "## 从现象选 skill", "go run ./cmd/skillgen"} {
		if !strings.Contains(text, want) {
			t.Fatalf("README 缺少 %q 段落", want)
		}
	}
	for _, l := range Languages() {
		if !strings.Contains(text, "`"+l.DirName()+"`") {
			t.Fatalf("README 的选择表未包含 %s", l.DirName())
		}
	}
}

// TestCheckDetectsMissingReadme 验证缺少 README 时校验失败。
func TestCheckDetectsMissingReadme(t *testing.T) {
	root := t.TempDir()
	if _, err := Generate(root); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if err := os.Remove(filepath.Join(root, readmeName)); err != nil {
		t.Fatalf("删除 README 失败: %v", err)
	}
	issues, err := Check(root)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if len(issues) != 1 || issues[0].Skill != "README" {
		t.Fatalf("应只报告 README 缺失，实际: %v", issues)
	}
}
