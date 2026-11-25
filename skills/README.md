# 调试 skill 包

本目录由 `go run ./cmd/skillgen -root .` 生成，请勿手改：各语言 `SKILL.md` 的共享段落逐字一致，语言差异只允许出现在各文件的插槽章节，`-check` 会校验这一点。

## 安装

1. 生成或更新产物：`go run ./cmd/skillgen -root .`。
2. 把需要的 `skills/<语言>-debug/` 目录复制到客户端的 skills 目录（项目级 `.codebuddy/skills/`，或客户端约定的用户级目录）。
3. 确认 `SKILL.md` 的 frontmatter `name` 与目录名一致（如 `java-debug`），客户端按此加载。
4. 同时把 MCP 服务端配好：skill 只是流程指引，实际动作由 `debug_env`、`debug_session` 等工具执行。

## 更新

- 改共享流程（会话步骤、流程纪律、证据三元组、收尾）：编辑 `internal/skill/template.md`。
- 改语言差异（触发信号、前置条件、工程化项目、读值陷阱、改值边界、反模式、性能分析）：编辑 `internal/skill/slots/<slug>.md`，章节名保持不变。
- 重新生成后校验：`go run ./cmd/skillgen -root . -check`；校验会指出哪个 skill 的哪一章节与模板不一致、缺失或为空。

## 从现象选 skill

先按可信现象定位 skill；括号内的“不适用”情形不要用调试流程处理，先解决构建或依赖问题。

问题出在构建阶段（构建工具报错、产物缺失、classpath 或 sourcemap 没生成、二进制被 strip）时，先看该 skill 的「工程化项目」章节：那里写了各构建工具的产物路径、启动命令与调试信息开关，补好构建配置再建会话，不要把构建问题当运行态问题排查。

| 现象 | skill | 不适用 |
|---|---|---|
| 断点停在业务行，`locals` 里只看到 `this`，集合内容与业务预期不符 | `java-debug` | Maven/Gradle 依赖下载失败、编译期类型错误不适用 |
| 协程里单步会在多个 `suspend` 函数间跳，栈上看不到业务调用方 | `kotlin-debug` | kotlinc 编译错误、协程库版本冲突不适用 |
| `lazy val` 初始化时机与预期不符，`Option`/`Future` 打印成内部结构 | `scala-debug` | sbt 依赖解析失败、隐式转换编译报错不适用 |
| 脚本行号与源码错位，`methodMissing` 看不出实际落到哪个方法 | `groovy-debug` | GroovyShell 语法错误、类路径缺失不适用 |
| 崩溃后栈上只有 `??`，或优化构建里变量显示 `<optimized out>` | `c-debug` | 链接期 undefined reference、头文件缺失不适用 |
| 基类指针读值看不到派生类成员，`std::vector` 只显示三个指针 | `cpp-debug` | 模板实例化失败、ABI 不匹配的链接错误不适用 |
| panic 栈只有 `rust_begin_unwind`，`Option`/`Result` 显示成内部枚举布局 | `rust-debug` | cargo 依赖解析失败、借用检查等编译期错误不适用 |
| 仓颉程序崩溃后栈上只有地址或 `??`，`Option`/class 显示成底层字段 | `cangjie-debug` | cjc 编译错误、SDK 未安装、cjpm 依赖解析失败不适用 |
| `KeyError`/`AttributeError` 落在业务行，需要看该行 dict 或对象的实际内容 | `python-debug` | pip 安装失败、语法错误与 `-O` 删除断言之外的非运行态问题不适用 |
| `Cannot read properties of undefined`，异步回调里断点看不到业务调用方 | `nodejs-debug` | npm 安装失败、打包或转译配置错误不适用 |
| `fatal error: all goroutines are asleep`，栈上只有当前 goroutine，找不到阻塞点 | `go-debug` | `go build`/`go test` 编译失败、依赖下载与 vendor 配置问题不适用 |

覆盖全部受支持语言：java-debug、kotlin-debug、scala-debug、groovy-debug、c-debug、cpp-debug、rust-debug、cangjie-debug、python-debug、nodejs-debug、go-debug。
