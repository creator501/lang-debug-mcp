---
name: scala-debug
description: 调试 Scala 程序（lazy val、object 单例、Future/Actor 并发）时使用：栈帧里全是 `$anonfun$`、单例字段看不到、Future 回调与业务线程不是同一个。纯 sbt 依赖解析失败不适用本 skill。
---

# Scala 调试技能

本 skill 由同一份模板加 Scala 插槽展开，可独立加载：建立会话、检视、改值、追踪与剖析的指引都在本文件内，不需要再读其他 skill。

## 1. 触发信号

- 栈帧出现 `App$$anonfun$1.apply`、`App$.foo$lzycompute`，业务方法名被 lambda 类名遮住。
- `object` 单例的字段读不到，或初始化时机与预期不符（`MODULE$` 尚未构造）。
- `Future`/Actor 回调里命中断点，但线程名属于 `scala-execution-context` 之类线程池。

## 2. 前置条件

- 用 `scalac -g:vars`（Scala 3 用 `-g`）编译，否则只有行号没有变量表，`locals` 会读不到。
- 运行期类路径需要 `scala-library.jar`。
- `object App { def main }` 的入口类是 `App`（编译器同时生成 `App$`，`MODULE$` 持有单例）。

## 3. 建立会话

1. 先 `debug_env` 探测：确认调试器或适配器可用、版本可识别，并记下缺失项与安装建议。
2. `debug_session` 建立会话：launch 由工具注入调试参数，attach 复用已有调试端口；记下会话标识。
3. `debug_breakpoint` 设置断点，优先使用条件或命中序号，断点数量保持少量。
4. `debug_run` 的 continue/until 做一次性闭环：命中后同一响应里拿到位置与变量值。
5. 需要更多变量时 `debug_inspect` 批量取；需要改值时 `debug_mutate`，旧快照随即作废。
6. 结束时 `debug_session` 关闭会话。

- 启动命令形如 `java -cp build/classes:scala-library.jar App`（Windows 用 `;` 分隔）。
- 主类只有转发方法，方法体在 `App$`；断点按源文件基名 `App.scala` 匹配，会自动落到 `App$` 的方法上。
- 单例字段在 `statics` 作用域里看不到 `MODULE$` 成员，需要展开 `locals.this` 或 `object` 实例。

## 4. 工程化项目

本工具不代跑构建，也不解析 `pom.xml`、`build.gradle`、`CMakeLists.txt`、`Makefile`、`build.sbt` 或 `package.json`：构建由你或 CI 先完成，会话只消费产物。

1. 用工程自己的构建命令产出带调试信息的产物：Debug 配置、保留符号、不要 strip、不要开高优化。
2. 找到该构建工具实际写入的产物路径（class 目录、jar、可执行文件或构建后的 JS），把它作为 `debug_session` 的启动命令，而不是构建工具本身。
3. 多模块/多包工程里，`cwd` 要与产物和源码的基准目录一致；命令中的相对路径都按 `cwd` 解析。
4. 需要在构建工具内部调试（跑测试、跑 `run` 任务）时，让构建工具以等待调试器的方式启动，再用 attach 连它的端口。
5. 构建描述文件里读不到的调试信息（classpath、sourcemap、`-g`）要显式确认，缺失时先补构建配置再排查代码。

- sbt：`sbt compile`，产物在 `target/scala-<版本>/classes`；要单包运行用 `sbt assembly`（需 assembly 插件）出 `target/scala-<版本>/*-assembly-*.jar`。
- 依赖 classpath：`sbt "export runtime:fullClasspath"` 打印完整类路径，可直接接到 `-cp` 后。
- 启动命令只能是 java：`["java","-cp","target/scala-2.13/classes:<依赖>","App"]`；交互式 `sbt` 不能当启动命令。
- 在 sbt 内调试：`sbt -jvm-debug 5005 "run"` 会以 suspend 方式等调试器，之后用 attach 连 5005。
- 混合 Java/Scala 工程的 Java 产物可能在 `target/java-classes`，`-cp` 要把两处都列上。

## 5. 读值陷阱

- `object` 单例的字段挂在 `MODULE$` 实例上；`statics` 作用域看不到它们，要展开单例实例本身。
- `lazy val` 生成 `xxx` 字段加 `bitmap$0` 位图；字段为 `null` 不一定表示未求值，要先看 bitmap 位。
- `val` 在构造器里赋值，`case class` 的字段名与 `component1` 一一对应，栈帧里出现 `copy`/`apply` 多为编译器生成。
- 闭包捕获的变量被编译成 `anonfun` 类字段（`$outer`、`x$1`），`locals` 里看不到原名，要在闭包对象上展开。
- Scala 3 的 `given`/`inline` 参数在调用点被内联，断点处通常没有槽位。
- `Future` 的栈是异步栈：命中时的线程与提交任务的线程不同，因果链要靠 `ExecutionContext` 与回调注册顺序还原。

## 6. 改值边界

- `val` 字段可写但破坏不变量；`case class` 改字段后 `equals`/`hashCode`/`copy` 结果会自相矛盾。
- 改 `lazy val` 的值不会让 `bitmap$0` 失效，后续访问仍返回新写入的值，但初始化逻辑被跳过。
- `Future` 内的局部变量在闭包里，改闭包字段只影响该回调实例。
- 单例 `MODULE$` 的字段写入是全局可见的，会影响同进程后续所有用例。

## 7. 高频反模式

每条反模式按固定格式记录：**现象 → 误判 → 正确做法**。

- **现象**：在 Future 回调里下断点，线程栈里没有调用方 → **误判**：以为调用链断了 → **正确做法**：先看提交任务的线程与回调注册顺序，用断点记录两侧线程名。
- **现象**：`object` 初始化前的字段读到 `null` → **误判**：以为字段没赋值 → **正确做法**：确认 `MODULE$` 是否已构造，必要时停在 `<clinit>`。
- **现象**：断点未生效且提示源文件未加载 → **误判**：以为文件名要写 `App$.scala` → **正确做法**：仍用 `App.scala`，解析器会查 `App$`。

## 8. 性能分析

- JVM 侧剖析器对 Scala 无特殊要求；注意 lambda 类名会影响热点聚合，按源文件与行号归并后再看排序。
- Actor/Future 场景用 wall-clock 采样，否则等待时间会被算成空闲。
- 没有剖析工具时，用命中序号断点统计调用次数，并在结论里说明是采样结果。

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
