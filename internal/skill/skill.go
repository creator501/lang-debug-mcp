// Package skill 由同一份模板加各语言插槽展开调试 skill，并提供产物一致性校验。
//
// 设计约束来自 spec：全部语言 skill 必须由同一模板生成，共享段落逐字一致，
// 语言差异只允许出现在插槽章节内，且固定八个章节齐全且非空。
package skill

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed template.md
var templateFS embed.FS

//go:embed slots/*.md
var slotFS embed.FS

// Language 是一个语言 skill 的身份信息。
type Language struct {
	Slug string // 目录与文件用的短名，如 java
	Name string // 展示名，如 Java
}

// Section 是解析出来的一个二级章节。
type Section struct {
	Title string
	Body  string
	Line  int
}

// Issue 是一条一致性校验问题。
type Issue struct {
	Skill   string // 产物标识，如 java-debug
	Section string // 章节标题；为空表示文件级问题
	Detail  string
}

// String 返回便于直接打印的问题描述。
func (i Issue) String() string {
	if i.Section == "" {
		return fmt.Sprintf("%s: %s", i.Skill, i.Detail)
	}
	return fmt.Sprintf("%s §%s: %s", i.Skill, i.Section, i.Detail)
}

// requiredChapters 是 spec 规定的固定章节骨架。
var requiredChapters = []string{
	"触发信号", "前置条件", "建立会话", "工程化项目", "读值陷阱", "改值边界", "高频反模式", "性能分析",
}

// languages 是全部受支持语言，顺序与 backend.Supported 保持一致，生成与校验都按此顺序。
var languages = []Language{
	{Slug: "java", Name: "Java"},
	{Slug: "kotlin", Name: "Kotlin"},
	{Slug: "scala", Name: "Scala"},
	{Slug: "groovy", Name: "Groovy"},
	{Slug: "c", Name: "C"},
	{Slug: "cpp", Name: "C++"},
	{Slug: "rust", Name: "Rust"},
	{Slug: "cangjie", Name: "Cangjie"},
	{Slug: "python", Name: "Python"},
	{Slug: "nodejs", Name: "Node.js"},
	{Slug: "go", Name: "Go"},
}

var (
	slotPattern  = regexp.MustCompile(`\{\{slot:([a-z-]+)\}\}`)
	headingPat   = regexp.MustCompile(`^##\s+(.*)$`)
	numberPrefix = regexp.MustCompile(`^\d+\.\s*`)
)

// Languages 返回全部语言（副本，避免调用方改动内部顺序）。
func Languages() []Language { return append([]Language(nil), languages...) }

// DirName 返回 skill 所在的目录名，例如 java-debug。
func (l Language) DirName() string { return l.Slug + "-debug" }

// Path 返回相对仓库根的 SKILL.md 位置。
func (l Language) Path() string { return filepath.Join("skills", l.DirName(), "SKILL.md") }

// Render 展开模板，返回该语言 skill 的完整内容。
func Render(l Language) (string, error) {
	tmpl, err := templateText()
	if err != nil {
		return "", err
	}
	slots, err := slotText(l.Slug)
	if err != nil {
		return "", err
	}
	out, err := expand(tmpl, l, slots)
	if err != nil {
		return "", err
	}
	return out, nil
}

// templateText 读取内嵌模板。
func templateText() (string, error) {
	data, err := templateFS.ReadFile("template.md")
	if err != nil {
		return "", fmt.Errorf("读取模板失败: %w", err)
	}
	return string(data), nil
}

// slotText 读取并解析某语言的插槽文件，返回“章节名 → 正文”。
func slotText(slug string) (map[string]string, error) {
	data, err := slotFS.ReadFile("slots/" + slug + ".md")
	if err != nil {
		return nil, fmt.Errorf("读取 %s 插槽失败: %w", slug, err)
	}
	sections := parseSections(string(data))
	slots := make(map[string]string, len(sections))
	for _, sec := range sections {
		if sec.Body == "" {
			return nil, fmt.Errorf("%s 插槽的「%s」章节为空", slug, sec.Title)
		}
		slots[sec.Title] = sec.Body
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("%s 插槽没有任何章节", slug)
	}
	return slots, nil
}

// expand 用语言信息与插槽正文填充模板。
func expand(tmpl string, l Language, slots map[string]string) (string, error) {
	var missing []string
	out := slotPattern.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := slotPattern.FindStringSubmatch(m)[1]
		body, ok := slots[key]
		if !ok {
			missing = append(missing, key)
			return m
		}
		return body
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s 插槽缺少模板要求的章节: %s", l.Slug, strings.Join(missing, ", "))
	}
	out = strings.ReplaceAll(out, "{{slug}}", l.Slug)
	out = strings.ReplaceAll(out, "{{name}}", l.Name)
	if strings.Contains(out, "{{") {
		return "", fmt.Errorf("%s 展开后仍有未替换的占位符", l.Slug)
	}
	return out, nil
}

// skeleton 把插槽正文换回占位符，用于验证“语言差异仅出现在插槽章节”。
func skeleton(rendered string, slots map[string]string, l Language) string {
	out := rendered
	for key, body := range slots {
		out = strings.ReplaceAll(out, body, "{{slot:"+key+"}}")
	}
	out = strings.ReplaceAll(out, "name: "+l.DirName(), "name: {{slug}}-debug")
	out = strings.ReplaceAll(out, "# "+l.Name+" 调试技能", "# {{name}} 调试技能")
	out = strings.ReplaceAll(out, "加 "+l.Name+" 插槽", "加 {{name}} 插槽")
	return out
}

// Skeleton 返回去掉插槽差异后的骨架内容（跨语言应完全一致）。
func Skeleton(l Language) (string, error) {
	tmpl, err := templateText()
	if err != nil {
		return "", err
	}
	slots, err := slotText(l.Slug)
	if err != nil {
		return "", err
	}
	rendered, err := expand(tmpl, l, slots)
	if err != nil {
		return "", err
	}
	return skeleton(rendered, slots, l), nil
}

// parseSections 按二级标题切分 Markdown，标题去掉序号后作为章节名。
func parseSections(text string) []Section {
	lines := strings.Split(text, "\n")
	var out []Section
	var cur *Section
	var body []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
		out = append(out, *cur)
		body = nil
	}
	for i, line := range lines {
		if m := headingPat.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Section{Title: strings.TrimSpace(numberPrefix.ReplaceAllString(m[1], "")), Line: i + 1}
			continue
		}
		if cur != nil {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// Generate 写入各语言产物与 README，返回发生变更的相对路径。
func Generate(root string) ([]string, error) {
	changed := make([]string, 0, len(languages))
	for _, l := range languages {
		content, err := Render(l)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(root, l.Path())
		if old, err := os.ReadFile(path); err == nil && string(old) == content {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("创建 %s 失败: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("写入 %s 失败: %w", path, err)
		}
		changed = append(changed, filepath.ToSlash(l.Path()))
	}
	readme := RenderReadme()
	readmePath := filepath.Join(root, readmeName)
	if old, err := os.ReadFile(readmePath); err != nil || string(old) != readme {
		if err := os.MkdirAll(filepath.Dir(readmePath), 0o755); err != nil {
			return nil, fmt.Errorf("创建 %s 失败: %w", filepath.Dir(readmePath), err)
		}
		if err := os.WriteFile(readmePath, []byte(readme), 0o644); err != nil {
			return nil, fmt.Errorf("写入 %s 失败: %w", readmePath, err)
		}
		changed = append(changed, filepath.ToSlash(readmeName))
	}
	return changed, nil
}

// Check 校验磁盘产物是否与模板展开结果一致、章节是否齐全，并给出具体位置。
func Check(root string) ([]Issue, error) {
	var issues []Issue
	baseSkeleton := ""
	for _, l := range languages {
		skillName := l.DirName()
		expected, err := Render(l)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(root, l.Path())
		data, err := os.ReadFile(path)
		if err != nil {
			issues = append(issues, Issue{Skill: skillName, Detail: "缺少产物文件 " + filepath.ToSlash(l.Path())})
			continue
		}
		actual := string(data)

		skel, err := Skeleton(l)
		if err != nil {
			return nil, err
		}
		if baseSkeleton == "" {
			baseSkeleton = skel
		} else if skel != baseSkeleton {
			issues = append(issues, Issue{Skill: skillName, Detail: "共享段落与模板不一致"})
		}

		if actual != expected {
			issues = append(issues, Issue{Skill: skillName,
				Section: firstDiffSection(expected, actual),
				Detail:  "内容与模板展开结果不一致（请重新运行 skillgen）"})
		}

		sections := parseSections(actual)
		byTitle := make(map[string]Section, len(sections))
		for _, sec := range sections {
			byTitle[sec.Title] = sec
		}
		for _, title := range requiredChapters {
			sec, ok := byTitle[title]
			if !ok {
				issues = append(issues, Issue{Skill: skillName, Section: title, Detail: "缺少固定章节"})
				continue
			}
			if sec.Body == "" {
				issues = append(issues, Issue{Skill: skillName, Section: title, Detail: "章节内容为空"})
			}
		}
	}
	issues = append(issues, checkReadme(root)...)
	return issues, nil
}

// checkReadme 校验 skill 包说明与生成结果一致，并检查选择表覆盖全部语言。
func checkReadme(root string) []Issue {
	var issues []Issue
	data, err := os.ReadFile(filepath.Join(root, readmeName))
	if err != nil {
		issues = append(issues, Issue{Skill: "README", Detail: "缺少产物文件 " + readmeName})
	} else if string(data) != RenderReadme() {
		issues = append(issues, Issue{Skill: "README", Detail: "内容与生成结果不一致（请重新运行 skillgen）"})
	}
	issues = append(issues, coverageIssues()...)
	return issues
}

// coverageIssues 检查“现象 → skill”选择表是否覆盖全部语言且排除非调试场景。
func coverageIssues() []Issue {
	var issues []Issue
	known := make(map[string]bool, len(languages))
	for _, l := range languages {
		known[l.DirName()] = true
	}
	covered := make(map[string]bool, len(languages))
	for _, sel := range selections {
		if sel.Symptom == "" || sel.Skill == "" {
			issues = append(issues, Issue{Skill: "README", Section: "选择表", Detail: "存在空白现象或未指定 skill 的条目"})
			continue
		}
		if !known[sel.Skill] {
			issues = append(issues, Issue{Skill: "README", Section: "选择表",
				Detail: fmt.Sprintf("条目指向不存在的 skill: %s", sel.Skill)})
			continue
		}
		if strings.TrimSpace(sel.NotApply) == "" {
			issues = append(issues, Issue{Skill: sel.Skill, Section: "选择表", Detail: "缺少不适用声明"})
		}
		covered[sel.Skill] = true
	}
	for _, l := range languages {
		if !covered[l.DirName()] {
			issues = append(issues, Issue{Skill: l.DirName(), Section: "选择表",
				Detail: "选择表未覆盖该语言，模型无法从现象定位到本 skill"})
		}
	}
	return issues
}

// firstDiffSection 返回两份内容第一个不同的章节名，找不到章节时返回空。
func firstDiffSection(expected, actual string) string {
	exp := parseSections(expected)
	act := parseSections(actual)
	for i := range exp {
		if i >= len(act) {
			return exp[i].Title
		}
		if exp[i].Title != act[i].Title || exp[i].Body != act[i].Body {
			return exp[i].Title
		}
	}
	if len(act) > len(exp) {
		return act[len(exp)].Title
	}
	return ""
}
