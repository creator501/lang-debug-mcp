---
name: go-debug
description: 调试 Go 程序（goroutine 并发、panic、interface 与泛型、channel）时使用：栈上只显示当前 goroutine、interface 展开成类型指针、优化构建变量显示 `<optimized out>`。`go build`/`go test` 编译失败、依赖下载失败等构建期问题不适用本 skill。
---

# Go 调试技能

本 skill 由同一份模板加 Go 插槽展开，可独立加载：建立会话、检视、改值、追踪与剖析的指引都在本文件内，不需要再读其他 skill。

## 1. 触发信号

- `panic: runtime error: invalid memory address or nil pointer dereference` 落在业务行，需要看该行指针、interface 或 map 的实际值。
- 程序卡住并报 `fatal error: all goroutines are asleep`，需要确认阻塞点与 wait 关系。
- 断点命中但 slice/map 内容与预期不符，或 channel 收发的停靠位置不在预期。

## 2. 前置条件

- dlv 在 PATH（或 `LANGDEBUG_DLV` 指定）；`go run` 形态还需要 go 工具链，编译由 dlv 负责。
- 变量可读需关优化与内联：`-gcflags="all=-N -l"`（dlv debug 形态默认带上，自己 build 时要显式加）。
- 二进制不要 strip：`-ldflags="-s -w"` 会去掉符号，栈上只剩地址。

## 3. 建立会话

1. 先 `debug_env` 探测：确认调试器或适配器可用、版本可识别，并记下缺失项与安装建议。
2. `debug_session` 建立会话：launch 由工具注入调试参数，attach 复用已有调试端口；记下会话标识。
3. `debug_breakpoint` 设置断点，优先使用条件或命中序号，断点数量保持少量。
4. `debug_run` 的 continue/until 做一次性闭环：命中后同一响应里拿到位置与变量值。
5. 需要更多变量时 `debug_inspect` 批量取；需要改值时 `debug_mutate`，旧快照随即作废。
6. 结束时 `debug_session` 关闭会话。

- 启动命令两种：`["go","run","./cmd/app"]`（dlv debug 形态，由 dlv 编译）或已编译二进制 `["./bin/app"]`（exec 形态）；`go build`、`go test` 等其它子命令会被拒绝。
- 入口停止落在 `main.main`：dlv 自带的 entry 停止在运行时初始化之前（占位线程，无栈无变量），工具会自动推进到 `main.main`，因此入口快照能看到 `main.main → runtime.main` 的栈。
- 只显示当前 goroutine 的栈；切换 goroutine 需要 dlv CLI，本工具不做。
- attach 连的是已经起好的 dlv dap 适配器端口（`dlv dap --listen=127.0.0.1:<端口>`）；把 dlv 附着到某个进程要用 dlv 自己的 `attach` 子命令，本工具不做。
- `go run` 的相对包路径按 `cwd` 补成绝对路径，会话建在哪个目录都一致。

## 4. 工程化项目

本工具不代跑构建，也不解析 `pom.xml`、`build.gradle`、`CMakeLists.txt`、`Makefile`、`build.sbt` 或 `package.json`：构建由你或 CI 先完成，会话只消费产物。

1. 用工程自己的构建命令产出带调试信息的产物：Debug 配置、保留符号、不要 strip、不要开高优化。
2. 找到该构建工具实际写入的产物路径（class 目录、jar、可执行文件或构建后的 JS），把它作为 `debug_session` 的启动命令，而不是构建工具本身。
3. 多模块/多包工程里，`cwd` 要与产物和源码的基准目录一致；命令中的相对路径都按 `cwd` 解析。
4. 需要在构建工具内部调试（跑测试、跑 `run` 任务）时，让构建工具以等待调试器的方式启动，再用 attach 连它的端口。
5. 构建描述文件里读不到的调试信息（classpath、sourcemap、`-g`）要显式确认，缺失时先补构建配置再排查代码。

- 工具不代跑 `go build`/`go test`：先构建再给二进制，或用 `go run <包>` 让 dlv 编译。
- 调试构建：`go build -gcflags="all=-N -l" -o bin/app ./cmd/app`；发布构建不要覆盖这份产物，重新构建会丢调试信息。
- 多模块/workspace：`go.work` 工程把 `cwd` 设成 workspace 根，包路径写相对形式（`./services/api/cmd/app`）；单模块给模块根。
- 依赖与 vendor：`-mod=vendor`、`GOFLAGS`、私有仓库凭据通过会话 `env` 传入，构建期问题先在构建里解决。
- 测试二进制可用 `go test -c`：`go test -c -o bin/app.test ./pkg` 后当可执行文件启动，用 `-test.run TestX` 限制用例。
- 交叉编译（`GOOS`/`GOARCH` 不同）的产物本机 dlv 跑不起来，先确认产物与调试机同平台。

## 5. 读值陷阱

- slice 是 `{ptr,len,cap}` 三字段：`len` 之后是脏数据，按 `cap` 读会看到旧值。
- `string` 是 `{ptr,len}` 结构而不是 C 字符串；子串与原串共享底层数组，长度决定内容。
- interface 是 `{type,data}` 两字段：nil 接口两个字段都为 0；`error` 里包着 typed nil 时接口本身非 nil，要展开 `data` 才看得出来。
- map 无序、内部是 bucket 数组：不要按显示顺序推断插入顺序；并发写会直接 `fatal error: concurrent map writes`，断点来不及停。
- 泛型实例的类型参数出现在符号名里（`Box[int]`），同名函数的不同实例是不同帧。
- 内联函数的帧会被折叠到调用方；`-l` 关闭内联后才看得到真实调用链。

## 6. 改值边界

- 只接受基础字面量（数字、布尔、字符串、nil）：结构体、slice、map 的整体替换不支持，改字段要展开到具体路径。
- 字符串是不可变对象，改值会生成新字符串，原引用不变；依赖指针身份的判断会失效。
- 改 slice 的 `len` 会让视图与底层数组不一致，后续 append 可能覆盖其他数据。
- channel 的内部索引（`sendx`/`recvx`）与缓冲计数改动会让收发错乱，不要用改值模拟收发。
- 优化构建下变量在寄存器里，改值可能只改到寄存器副本，读回值不变属正常。

## 7. 高频反模式

每条反模式按固定格式记录：**现象 → 误判 → 正确做法**。

- **现象**：`fatal error: all goroutines are asleep`，但快照里只有一条栈 → **误判**：以为其他 goroutine 没跑 → **正确做法**：本工具只显示当前 goroutine，用日志或 dlv CLI 的 `goroutines` 辅助，别把「没有其他栈」当成「没有其他协程」。
- **现象**：interface 显示为 nil，调用方说传了值 → **误判**：以为参数丢了 → **正确做法**：展开 `{type,data}`，区分 typed nil 与真 nil（`(*T)(nil)` 装进接口后接口非 nil）。
- **现象**：release 产物里变量全是 `<optimized out>` → **误判**：以为内存被踩 → **正确做法**：用 `-gcflags="all=-N -l"` 重编，优化后的值不可恢复。

## 8. 性能分析

- CPU 与堆用 `pprof`：`go tool pprof` 读 `-cpuprofile`/`-memprofile`，采样是统计值，短函数会被低估。
- `block`/`mutex` profile 需要显式打开（`runtime.SetBlockProfileRate`、`runtime.SetMutexProfileFraction`），否则为空。
- 调度与 GC 停顿用 `go tool trace`，它不能替代 CPU 采样。
- 没有采样条件时用带命中序号的断点统计调用次数，并在结论里标注这是计数不是采样。

## 9. 流程纪律

1. 任何调试动作之前先探测环境；调试器缺失时先返回安装或开启指引，不要硬建会话。
2. 默认少量精确断点（一般 1–3 个），用条件或命中序号约束；不得以单步执行替代采集。
3. 在可能高频执行的位置设置采集点前，必须先设定速率上限或采样率。
4. 断点选「能观测到问题的最早位置」，不要一路下到用户报告的行号再回看。
5. 同一轮排查复用同一份快照标识；快照失效后重新停止再取，不要混用改前与改后的值。

## 10. 结论输出规范

给出结论时必须提供证据三元组：

- 位置：文件与行号，例如 `src/handler.py:42`。
- 观测值：该位置实际取到的值及其路径，例如 `locals.retries = 3`。
- 复现路径：从启动到该观测的最小步骤（会话模式、断点条件、运行动作）。

不得仅凭代码阅读下断言。尚未取得运行态证据时，写成「待验证假设」，并给出下一处要停下的位置。

## 11. 收尾

1. 调试结束关闭会话，释放后端进程与监听端口，不留下后台调试器。
2. 若本轮改过值，结论中标注目标进程内存已被修改，后续行为可能偏离原始逻辑，需要重启才能恢复。
3. 保留用过的断点与快照标识，便于他人复现。
