package skill

import (
	"fmt"
	"strings"
)

// Selection 是一条“现象 → skill”的选择表条目。
//
// 选择表是 skill 包的入口：模型先按现象选中一份 skill，再按 skill 内的
// 流程纪律执行。NotApply 显式排除非调试场景（构建失败、依赖安装失败等），
// 避免把编译期问题误当成运行态问题去建会话。
type Selection struct {
	// Symptom 是用户可复述的现象，尽量贴原始报错或可见行为。
	Symptom string
	// Skill 是目标 skill 的目录名，如 java-debug。
	Skill string
	// NotApply 是本条不适用的场景。
	NotApply string
}

// selections 是覆盖全部语言的现象选择表，顺序与 languages 一致。
var selections = []Selection{
	{
		Symptom:  "断点停在业务行，`locals` 里只看到 `this`，集合内容与业务预期不符",
		Skill:    "java-debug",
		NotApply: "Maven/Gradle 依赖下载失败、编译期类型错误不适用",
	},
	{
		Symptom:  "协程里单步会在多个 `suspend` 函数间跳，栈上看不到业务调用方",
		Skill:    "kotlin-debug",
		NotApply: "kotlinc 编译错误、协程库版本冲突不适用",
	},
	{
		Symptom:  "`lazy val` 初始化时机与预期不符，`Option`/`Future` 打印成内部结构",
		Skill:    "scala-debug",
		NotApply: "sbt 依赖解析失败、隐式转换编译报错不适用",
	},
	{
		Symptom:  "脚本行号与源码错位，`methodMissing` 看不出实际落到哪个方法",
		Skill:    "groovy-debug",
		NotApply: "GroovyShell 语法错误、类路径缺失不适用",
	},
	{
		Symptom:  "崩溃后栈上只有 `??`，或优化构建里变量显示 `<optimized out>`",
		Skill:    "c-debug",
		NotApply: "链接期 undefined reference、头文件缺失不适用",
	},
	{
		Symptom:  "基类指针读值看不到派生类成员，`std::vector` 只显示三个指针",
		Skill:    "cpp-debug",
		NotApply: "模板实例化失败、ABI 不匹配的链接错误不适用",
	},
	{
		Symptom:  "panic 栈只有 `rust_begin_unwind`，`Option`/`Result` 显示成内部枚举布局",
		Skill:    "rust-debug",
		NotApply: "cargo 依赖解析失败、借用检查等编译期错误不适用",
	},
	{
		Symptom:  "仓颉程序崩溃后栈上只有地址或 `??`，`Option`/class 显示成底层字段",
		Skill:    "cangjie-debug",
		NotApply: "cjc 编译错误、SDK 未安装、cjpm 依赖解析失败不适用",
	},
	{
		Symptom:  "`KeyError`/`AttributeError` 落在业务行，需要看该行 dict 或对象的实际内容",
		Skill:    "python-debug",
		NotApply: "pip 安装失败、语法错误与 `-O` 删除断言之外的非运行态问题不适用",
	},
	{
		Symptom:  "`Cannot read properties of undefined`，异步回调里断点看不到业务调用方",
		Skill:    "nodejs-debug",
		NotApply: "npm 安装失败、打包或转译配置错误不适用",
	},
	{
		Symptom:  "`fatal error: all goroutines are asleep`，栈上只有当前 goroutine，找不到阻塞点",
		Skill:    "go-debug",
		NotApply: "`go build`/`go test` 编译失败、依赖下载与 vendor 配置问题不适用",
	},
}

// Selections 返回选择表副本，避免调用方改动内部顺序。
func Selections() []Selection { return append([]Selection(nil), selections...) }

// readmeName 是 skill 包说明文件（相对仓库根）。
const readmeName = "skills/README.md"

// ReadmePath 返回 skill 包说明文件的位置。
func ReadmePath() string { return readmeName }

// RenderReadme 生成 skill 包的说明：安装方式、更新方式与现象到 skill 的选择表。
//
// 该文件与各语言 SKILL.md 同源生成，避免文档里写的安装步骤与实际产物漂移。
func RenderReadme() string {
	var b strings.Builder
	b.WriteString("# 调试 skill 包\n\n")
	b.WriteString("本目录由 `go run ./cmd/skillgen -root .` 生成，请勿手改：" +
		"各语言 `SKILL.md` 的共享段落逐字一致，语言差异只允许出现在各文件的插槽章节，`-check` 会校验这一点。\n\n")
	b.WriteString("## 安装\n\n")
	b.WriteString("1. 生成或更新产物：`go run ./cmd/skillgen -root .`。\n")
	b.WriteString("2. 把需要的 `skills/<语言>-debug/` 目录复制到客户端的 skills 目录" +
		"（项目级 `.codebuddy/skills/`，或客户端约定的用户级目录）。\n")
	b.WriteString("3. 确认 `SKILL.md` 的 frontmatter `name` 与目录名一致（如 `java-debug`），客户端按此加载。\n")
	b.WriteString("4. 同时把 MCP 服务端配好：skill 只是流程指引，实际动作由 `debug_env`、`debug_session` 等工具执行。\n\n")
	b.WriteString("## 更新\n\n")
	b.WriteString("- 改共享流程（会话步骤、流程纪律、证据三元组、收尾）：编辑 `internal/skill/template.md`。\n")
	b.WriteString("- 改语言差异（触发信号、前置条件、工程化项目、读值陷阱、改值边界、反模式、性能分析）：" +
		"编辑 `internal/skill/slots/<slug>.md`，章节名保持不变。\n")
	b.WriteString("- 重新生成后校验：`go run ./cmd/skillgen -root . -check`；" +
		"校验会指出哪个 skill 的哪一章节与模板不一致、缺失或为空。\n\n")
	b.WriteString("## 从现象选 skill\n\n")
	b.WriteString("先按可信现象定位 skill；括号内的“不适用”情形不要用调试流程处理，先解决构建或依赖问题。\n\n")
	b.WriteString("问题出在构建阶段（构建工具报错、产物缺失、classpath 或 sourcemap 没生成、二进制被 strip）时，" +
		"先看该 skill 的「工程化项目」章节：那里写了各构建工具的产物路径、启动命令与调试信息开关，" +
		"补好构建配置再建会话，不要把构建问题当运行态问题排查。\n\n")
	b.WriteString("| 现象 | skill | 不适用 |\n|---|---|---|\n")
	for _, sel := range selections {
		fmt.Fprintf(&b, "| %s | `%s` | %s |\n", sel.Symptom, sel.Skill, sel.NotApply)
	}
	b.WriteString("\n覆盖全部受支持语言：")
	names := make([]string, 0, len(languages))
	for _, l := range languages {
		names = append(names, l.DirName())
	}
	b.WriteString(strings.Join(names, "、"))
	b.WriteString("。\n")
	return b.String()
}
