---
name: rust-debug
description: 调试 Rust 程序（`Option`/`Result` 包裹、借用与生命周期、release 内联）时使用：panic 栈缺少业务帧、变量显示成枚举标签、`String` 与 `&str` 看到的内容不完整。cargo 依赖或编译期借用检查报错不适用本 skill。
---

# Rust 调试技能

本 skill 由同一份模板加 Rust 插槽展开，可独立加载：建立会话、检视、改值、追踪与剖析的指引都在本文件内，不需要再读其他 skill。

## 1. 触发信号

- 进程 panic，需要知道 panic 那一行上 `Result` 的实际错误值或 `Option` 是否为 `None`。
- 变量在调试器里显示成枚举的判别值，看不到内部数据（`Option<i32>` 只显示标签）。
- release 构建下断点不生效，或栈帧被内联掉。

## 2. 前置条件

- debug 构建默认带 `-g`；release 需在 `Cargo.toml` 里 `[profile.release] debug = true` 才有调试信息。
- 调试器 lldb（或 code-lldb）配合 rust pretty printer；缺失时 `Option`/`Vec` 会显示成内部布局。
- panic 要看到栈需 `RUST_BACKTRACE=1`；`panic = "abort"` 时无 unwinding 栈。

## 3. 建立会话

1. 先 `debug_env` 探测：确认调试器或适配器可用、版本可识别，并记下缺失项与安装建议。
2. `debug_session` 建立会话：launch 由工具注入调试参数，attach 复用已有调试端口；记下会话标识。
3. `debug_breakpoint` 设置断点，优先使用条件或命中序号，断点数量保持少量。
4. `debug_run` 的 continue/until 做一次性闭环：命中后同一响应里拿到位置与变量值。
5. 需要更多变量时 `debug_inspect` 批量取；需要改值时 `debug_mutate`，旧快照随即作废。
6. 结束时 `debug_session` 关闭会话。

- 启动命令 `./target/debug/app` 或 `cargo run`（后者需给出 cargo 路径与 `--bin`，工具不解析 Cargo 项目结构）。
- panic 断点：后端不支持 `catch panic` 时，在可疑 `unwrap`/`expect` 行下断点，先看被包裹的值。
- 异步运行时（tokio）下断点命中的是 worker 线程，因果链要靠 `spawn` 位置与任务名还原。

## 4. 工程化项目

本工具不代跑构建，也不解析 `pom.xml`、`build.gradle`、`CMakeLists.txt`、`Makefile`、`build.sbt` 或 `package.json`：构建由你或 CI 先完成，会话只消费产物。

1. 用工程自己的构建命令产出带调试信息的产物：Debug 配置、保留符号、不要 strip、不要开高优化。
2. 找到该构建工具实际写入的产物路径（class 目录、jar、可执行文件或构建后的 JS），把它作为 `debug_session` 的启动命令，而不是构建工具本身。
3. 多模块/多包工程里，`cwd` 要与产物和源码的基准目录一致；命令中的相对路径都按 `cwd` 解析。
4. 需要在构建工具内部调试（跑测试、跑 `run` 任务）时，让构建工具以等待调试器的方式启动，再用 attach 连它的端口。
5. 构建描述文件里读不到的调试信息（classpath、sourcemap、`-g`）要显式确认，缺失时先补构建配置再排查代码。

- cargo：`cargo build`，dev profile 默认 `debug = true`，产物在 `target/debug/<bin>`。
- release 也要调试信息得在 `Cargo.toml` 写 `[profile.release] debug = true`，并确认没有 `strip = "symbols"`。
- 启动命令给产物：`["target/debug/app","--flag"]`；`cargo run`、`cargo test` 不是可执行文件，会被拒绝。
- workspace 多 crate：`cargo build -p app`，产物统一在根 `target/debug/`；`cwd` 给 workspace 根，断点用绝对路径。
- 调试测试：`cargo test --no-run` 会打印测试二进制路径，直接 launch 它并传 `--exact tests::case`。
- native 后端不支持 attach，`cargo watch` 之类的热重载进程只能由工具托管或手动重启会话。

## 5. 读值陷阱

- `Option<T>`/`Result<T, E>` 是枚举：先看判别值（`Some`/`None`、`Ok`/`Err`），再展开对应分支的负载。
- `&str` 是 (指针, 长度) 二元组，`String` 是 `Vec<u8>` 三元组；按长度截取才是真实内容，不要按 C 字符串读。
- `Box`/`Arc`/`Rc` 需要解引用看目标；`Arc` 的引用计数是原子量。
- 借用结束后（NLL）变量可能被提前 drop，调试器显示「不可用」是真实状态，不是读值失败。
- 元组的字段名是 `.0`/`.1`，结构体字段与源码一致；`#[repr(C)]` 之外的内存布局不保证。
- 优化后多个变量共用寄存器，读到的值是「某个时刻」的值，不是当前语句的值。

## 6. 改值边界

- 改不可变绑定的值会破坏借用保证：后续代码可能读到与编译器假设不符的状态。
- `static`/`const` 的存储写入不影响已内联的常量使用点。
- 改 `Mutex`/`RwLock` 内部状态可能让锁永远不释放，导致后续线程挂死。
- 枚举改判别值而不改负载会造出非法状态，随后 `match` 可能 panic。

## 7. 高频反模式

每条反模式按固定格式记录：**现象 → 误判 → 正确做法**。

- **现象**：`Option` 只显示 `Some`/`None`，看不到值 → **误判**：以为读值失败 → **正确做法**：展开枚举负载，或确认 pretty printer 是否加载。
- **现象**：release 下断点不命中 → **误判**：以为源码行选错 → **正确做法**：确认 release 是否带 `debug = true`，内联后改断在调用者。
- **现象**：变量显示「不可用」→ **误判**：以为工具丢了变量 → **正确做法**：确认是否已被 drop（NLL 提前释放），把断点前移到使用点之前。

## 8. 性能分析

- `perf record` 配合 `--call-graph dwarf`；`cargo flamegraph` 可直接产出火焰图。
- 异步程序用 tokio-console 观察任务等待与轮询耗时，采样剖析会把等待算成空闲。
- 无剖析工具时用命中序号断点统计调用次数，并在结论中标注采样性质。

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
