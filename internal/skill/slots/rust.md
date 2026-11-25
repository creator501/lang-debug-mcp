## description
调试 Rust 程序（`Option`/`Result` 包裹、借用与生命周期、release 内联）时使用：panic 栈缺少业务帧、变量显示成枚举标签、`String` 与 `&str` 看到的内容不完整。cargo 依赖或编译期借用检查报错不适用本 skill。

## triggers
- 进程 panic，需要知道 panic 那一行上 `Result` 的实际错误值或 `Option` 是否为 `None`。
- 变量在调试器里显示成枚举的判别值，看不到内部数据（`Option<i32>` 只显示标签）。
- release 构建下断点不生效，或栈帧被内联掉。

## preconditions
- debug 构建默认带 `-g`；release 需在 `Cargo.toml` 里 `[profile.release] debug = true` 才有调试信息。
- 调试器 lldb（或 code-lldb）配合 rust pretty printer；缺失时 `Option`/`Vec` 会显示成内部布局。
- panic 要看到栈需 `RUST_BACKTRACE=1`；`panic = "abort"` 时无 unwinding 栈。

## session
- 启动命令 `./target/debug/app` 或 `cargo run`（后者需给出 cargo 路径与 `--bin`，工具不解析 Cargo 项目结构）。
- panic 断点：后端不支持 `catch panic` 时，在可疑 `unwrap`/`expect` 行下断点，先看被包裹的值。
- 异步运行时（tokio）下断点命中的是 worker 线程，因果链要靠 `spawn` 位置与任务名还原。

## build
- cargo：`cargo build`，dev profile 默认 `debug = true`，产物在 `target/debug/<bin>`。
- release 也要调试信息得在 `Cargo.toml` 写 `[profile.release] debug = true`，并确认没有 `strip = "symbols"`。
- 启动命令给产物：`["target/debug/app","--flag"]`；`cargo run`、`cargo test` 不是可执行文件，会被拒绝。
- workspace 多 crate：`cargo build -p app`，产物统一在根 `target/debug/`；`cwd` 给 workspace 根，断点用绝对路径。
- 调试测试：`cargo test --no-run` 会打印测试二进制路径，直接 launch 它并传 `--exact tests::case`。
- native 后端不支持 attach，`cargo watch` 之类的热重载进程只能由工具托管或手动重启会话。

## read-traps
- `Option<T>`/`Result<T, E>` 是枚举：先看判别值（`Some`/`None`、`Ok`/`Err`），再展开对应分支的负载。
- `&str` 是 (指针, 长度) 二元组，`String` 是 `Vec<u8>` 三元组；按长度截取才是真实内容，不要按 C 字符串读。
- `Box`/`Arc`/`Rc` 需要解引用看目标；`Arc` 的引用计数是原子量。
- 借用结束后（NLL）变量可能被提前 drop，调试器显示「不可用」是真实状态，不是读值失败。
- 元组的字段名是 `.0`/`.1`，结构体字段与源码一致；`#[repr(C)]` 之外的内存布局不保证。
- 优化后多个变量共用寄存器，读到的值是「某个时刻」的值，不是当前语句的值。

## mutate-bounds
- 改不可变绑定的值会破坏借用保证：后续代码可能读到与编译器假设不符的状态。
- `static`/`const` 的存储写入不影响已内联的常量使用点。
- 改 `Mutex`/`RwLock` 内部状态可能让锁永远不释放，导致后续线程挂死。
- 枚举改判别值而不改负载会造出非法状态，随后 `match` 可能 panic。

## antipatterns
- **现象**：`Option` 只显示 `Some`/`None`，看不到值 → **误判**：以为读值失败 → **正确做法**：展开枚举负载，或确认 pretty printer 是否加载。
- **现象**：release 下断点不命中 → **误判**：以为源码行选错 → **正确做法**：确认 release 是否带 `debug = true`，内联后改断在调用者。
- **现象**：变量显示「不可用」→ **误判**：以为工具丢了变量 → **正确做法**：确认是否已被 drop（NLL 提前释放），把断点前移到使用点之前。

## profiling
- `perf record` 配合 `--call-graph dwarf`；`cargo flamegraph` 可直接产出火焰图。
- 异步程序用 tokio-console 观察任务等待与轮询耗时，采样剖析会把等待算成空闲。
- 无剖析工具时用命中序号断点统计调用次数，并在结论中标注采样性质。
