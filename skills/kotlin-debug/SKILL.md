---
name: kotlin-debug
description: 调试 Kotlin 程序（协程、data class、委托属性）时使用：suspend 函数栈帧只剩状态机、lazy 值首次访问后才出现、data class 打印内容与字段不一致。纯 Gradle 构建失败或依赖解析失败不适用本 skill。
---

# Kotlin 调试技能

本 skill 由同一份模板加 Kotlin 插槽展开，可独立加载：建立会话、检视、改值、追踪与剖析的指引都在本文件内，不需要再读其他 skill。

## 1. 触发信号

- 栈帧里只有 `Foo$bar$1.invokeSuspend`、`ContinuationImpl.resumeWith`，看不到业务方法名。
- `by lazy` 的委托属性在断点处仍是 `null` 或 `Lazy` 对象，需要看它是否已被求值。
- `data class` 的 `toString` 输出与字段实际值不一致（自定义 `toString` 或字段被改过）。

## 2. 前置条件

- 用 `kotlinc -g` 编译（默认带行号表；显式 `-g` 保证变量表也在）。
- 运行期类路径需要 `kotlin-stdlib.jar`，否则会在 `main` 里抛 `NoClassDefFoundError: kotlin/...`。
- 顶层函数编译到 `<文件名>Kt` 类：`App.kt` 的 `main` 在 `AppKt` 里，入口类名要写 `AppKt`。

## 3. 建立会话

1. 先 `debug_env` 探测：确认调试器或适配器可用、版本可识别，并记下缺失项与安装建议。
2. `debug_session` 建立会话：launch 由工具注入调试参数，attach 复用已有调试端口；记下会话标识。
3. `debug_breakpoint` 设置断点，优先使用条件或命中序号，断点数量保持少量。
4. `debug_run` 的 continue/until 做一次性闭环：命中后同一响应里拿到位置与变量值。
5. 需要更多变量时 `debug_inspect` 批量取；需要改值时 `debug_mutate`，旧快照随即作废。
6. 结束时 `debug_session` 关闭会话。

- 启动命令形如 `java -cp build/classes:kotlin-stdlib.jar AppKt`（Windows 用 `;` 分隔）。
- 协程里的断点仍按源码行命中；停止后线程是 carrier 线程，需要用栈帧里的 `Continuation` 判断是哪个协程。
- `@JvmStatic`/`object` 单例的成员在 `Foo.INSTANCE` 上，静态字段用 `statics` 作用域看。

## 4. 工程化项目

本工具不代跑构建，也不解析 `pom.xml`、`build.gradle`、`CMakeLists.txt`、`Makefile`、`build.sbt` 或 `package.json`：构建由你或 CI 先完成，会话只消费产物。

1. 用工程自己的构建命令产出带调试信息的产物：Debug 配置、保留符号、不要 strip、不要开高优化。
2. 找到该构建工具实际写入的产物路径（class 目录、jar、可执行文件或构建后的 JS），把它作为 `debug_session` 的启动命令，而不是构建工具本身。
3. 多模块/多包工程里，`cwd` 要与产物和源码的基准目录一致；命令中的相对路径都按 `cwd` 解析。
4. 需要在构建工具内部调试（跑测试、跑 `run` 任务）时，让构建工具以等待调试器的方式启动，再用 attach 连它的端口。
5. 构建描述文件里读不到的调试信息（classpath、sourcemap、`-g`）要显式确认，缺失时先补构建配置再排查代码。

- Gradle：`./gradlew build`；Kotlin 产物在 `build/classes/kotlin/main`，运行期还要 `kotlin-stdlib.jar`。
- 启动命令：`["java","-cp","build/classes/kotlin/main:<stdlib 与依赖>","AppKt"]`；顶层函数的类名是 `<文件名>Kt`。
- `./gradlew run --debug-jvm` 让 Gradle 以等待调试器的方式拉起 JVM（默认 5005），之后用 attach 连该端口。
- 多模块写 `:app:build`，产物在 `app/build/classes/kotlin/main`，`-cp` 要把被依赖模块一起带上。
- Kotlin/Gradle 默认带 `-g`（行号表与变量表），不要为减小编译产物关掉调试信息；增量编译不影响断点。

## 5. 读值陷阱

- 挂起函数被编译成状态机：局部变量搬进 `Continuation` 的字段，`locals` 里可能只有 `this` 与 `$continuation`，实际值要从 `label`、`$result` 与状态机字段里找。
- 顶层与扩展函数落在 `<文件名>Kt` 类；文件名与类名不一致时按 `SourceFile` 属性匹配断点。
- `by lazy` 生成 `xxx$delegate` 字段，值是 `SynchronizedLazyImpl`；先展开看 `_value` 是否已初始化，再判断业务值。
- `const val` 在编译期内联到调用方，断点处通常没有对应槽位；`val` 的属性若是自定义 getter，断点要下在 getter 里。
- `data class` 的 `componentN`、`copy` 是编译器生成的方法，栈帧里出现它们不代表业务逻辑。

## 6. 改值边界

- `val` 字段在调试器层面可写，但写入后与构造期语义脱节，可能导致后续 `equals`/`hashCode` 不一致。
- 协程状态机的 `label`、`$result` 是调度状态，改写会让恢复执行落到错误分支，除定位问题外不要改。
- 内联函数（`inline`、`run`、`let`）的参数在调用方没有独立槽位，无法单独改值。
- 委托属性改 `xxx$delegate` 需要整体替换 `Lazy` 实现，直接改字段不会让 `lazy` 重新求值。

## 7. 高频反模式

每条反模式按固定格式记录：**现象 → 误判 → 正确做法**。

- **现象**：在 `suspend fun` 里单步，跳转位置完全看不懂 → **误判**：以为源码与字节码不匹配 → **正确做法**：用断点加条件停在目标行，展开 Continuation 看状态。
- **现象**：`toString` 打印的字段值与 `debug_inspect` 不一致 → **误判**：以为工具读错 → **正确做法**：确认类是否自定义了 `toString`，结论以字段值为准。
- **现象**：断点未生效并提示类未加载 → **误判**：以为 `AppKt` 名字写错 → **正确做法**：确认入口类名是 `<文件名>Kt`，并检查 `-cp` 是否包含 stdlib。

## 8. 性能分析

- 协程场景用 async-profiler 的 wall-clock 模式（`-e wall`）才能看到挂起等待；CPU 模式只看到 carrier 线程的忙等。
- JFR 里协程表现为主线程池上的短任务，按线程聚合会低估耗时，需按调用栈看。
- 在没有剖析工具时，用带命中序号的断点统计调用次数，并在结论中标注这是采样而非全量。

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
