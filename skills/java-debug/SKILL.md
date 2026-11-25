---
name: java-debug
description: 调试 Java 程序（JVM/JDWP）时使用：空指针堆栈落在业务行、集合内容与预期不符、断点不生效或变量读不到。纯 javac 编译错误、依赖下载失败等非运行态问题不适用本 skill。
---

# Java 调试技能

本 skill 由同一份模板加 Java 插槽展开，可独立加载：建立会话、检视、改值、追踪与剖析的指引都在本文件内，不需要再读其他 skill。

## 1. 触发信号

- 堆栈里出现 `java.lang.NullPointerException`、`ClassCastException`、`ArrayIndexOutOfBoundsException`，需要确认出错那一行各变量的实际值。
- 断点显示未生效，提示源文件未加载或该行没有可执行代码。
- 集合（`ArrayList`/`HashMap`）打印出的内容与业务预期不一致，需要看到元素真实结构。

## 2. 前置条件

- 目标以 JDWP 代理启动：`-agentlib:jdwp=transport=dt_socket,server=y,suspend=y,address=<端口>`；工具在 launch 模式下自动注入。
- class 文件带行号表与局部变量表：`javac -g`（Maven/Gradle 的默认 debug 配置通常已包含）。
- 能读到源码：`-cp` 指向 class 目录，源码目录通过会话的工作目录给出，断点按文件基名匹配。

## 3. 建立会话

1. 先 `debug_env` 探测：确认调试器或适配器可用、版本可识别，并记下缺失项与安装建议。
2. `debug_session` 建立会话：launch 由工具注入调试参数，attach 复用已有调试端口；记下会话标识。
3. `debug_breakpoint` 设置断点，优先使用条件或命中序号，断点数量保持少量。
4. `debug_run` 的 continue/until 做一次性闭环：命中后同一响应里拿到位置与变量值。
5. 需要更多变量时 `debug_inspect` 批量取；需要改值时 `debug_mutate`，旧快照随即作废。
6. 结束时 `debug_session` 关闭会话。

- 顶层入口类用全限定名：`java -cp target/classes com.example.App`；单文件源码在 JDK 11+ 可直接 `java App.java`。
- 会话入口停在 VM 启动处，此时业务类尚未加载，断点是「待解析」状态，继续执行后由类加载事件补上。
- attach 到已运行进程需要它启动时就带 JDWP 参数；JVM 无法事后开启代理，失败时按返回的重启命令重新拉起。

## 4. 工程化项目

本工具不代跑构建，也不解析 `pom.xml`、`build.gradle`、`CMakeLists.txt`、`Makefile`、`build.sbt` 或 `package.json`：构建由你或 CI 先完成，会话只消费产物。

1. 用工程自己的构建命令产出带调试信息的产物：Debug 配置、保留符号、不要 strip、不要开高优化。
2. 找到该构建工具实际写入的产物路径（class 目录、jar、可执行文件或构建后的 JS），把它作为 `debug_session` 的启动命令，而不是构建工具本身。
3. 多模块/多包工程里，`cwd` 要与产物和源码的基准目录一致；命令中的相对路径都按 `cwd` 解析。
4. 需要在构建工具内部调试（跑测试、跑 `run` 任务）时，让构建工具以等待调试器的方式启动，再用 attach 连它的端口。
5. 构建描述文件里读不到的调试信息（classpath、sourcemap、`-g`）要显式确认，缺失时先补构建配置再排查代码。

- Maven：`mvn -q -DskipTests package`；class 产物在 `target/classes`，可执行包在 `target/<artifact>-<version>.jar`。
- Gradle：`./gradlew build`；类目录在 `build/classes/java/main`，依赖 jar 在 `build/libs`。
- 启动命令只能是 java：`["java","-cp","target/classes:<依赖 jar，用 ; 或 : 分隔>","com.example.App"]`，或 `["java","-jar","target/app.jar"]`。
- 依赖 classpath 要自己拼：`mvn -q dependency:build-classpath -Dmdep.outputFile=cp.txt` 的结果接到 `-cp` 后；Gradle 用自定义任务导出 `runtimeClasspath`，或直接构建 fat jar。
- 多模块：类目录是 `service/target/classes`、`api/build/classes/java/main`，`-cp` 要把用到的模块列全；断点按源文件基名匹配，多模块同名类只会选中第一个并在返回里注明，看到该 note 时用全限定类名核对。
- 在构建工具里跑测试：`mvn -Dmaven.surefire.debug test`、`./gradlew test --debug-jvm` 会以 suspend 方式等调试器（默认 5005），此时用 attach 连该端口，不要再用 launch。

## 5. 读值陷阱

- 局部变量表按**槽位**记录：增强 for 循环变量、`try` 块里的临时变量会复用同一槽位，同一条语句里可能对应两个不同的名字，取值以命中的代码位置为准。
- 集合按后端识别的结构展开：`ArrayList`/`List.of`、`HashMap`/`LinkedHashMap`、`HashSet`、`LinkedList` 会展开成元素视图，其他集合按内部字段展示。
- `HashMap` 的键字段签名被擦除成 `Object`，键名以运行时类型还原；自定义对象作键时显示为「类型@标识」。
- 包装类型（`Integer`/`Long`/`Boolean`）展开为原始值；静态字段在 `statics` 作用域，实例字段里看不到。
- 字符串是对象：字段里存的是引用，长度与内容要用展开后的值，不要按 `char[]` 猜。
- 对象一律按字段渲染，不调用 `toString()`：它有副作用，返回值也不可信。

## 6. 改值边界

- `final` 字段在调试器层面可写，但写入后破坏类不变量，后续逻辑可能异常；仅在确认影响面后使用。
- 改集合的内部字段（如 `ArrayList.size`）会让集合状态与元素数组不一致，之后的遍历可能越界。
- 局部变量槽位被复用时改值可能影响同一槽位的另一个变量；改完用新快照复核。
- 字符串字段需要一个新字符串对象，工具会为目标重新构造，原引用不变。

## 7. 高频反模式

每条反模式按固定格式记录：**现象 → 误判 → 正确做法**。

- **现象**：日志顺序看起来就是执行顺序 → **误判**：把多线程交错的日志当因果 → **正确做法**：在疑似分支下条件断点，用命中时的线程名与栈帧确认。
- **现象**：循环里加无条件断点，命中成百上千次 → **误判**：以为业务在死循环 → **正确做法**：改成条件断点或按命中序号在第 N 次停下。
- **现象**：断点一直未生效 → **误判**：以为工具坏了 → **正确做法**：先确认 class 是否已加载、是否带 `-g` 行号表；入口停止时断点待解析属正常。

## 8. 性能分析

- 先看能力清单：服务是否提供采集或剖析工具；没有就说明降级，不要编造工具调用。
- JVM 侧首选 JFR（`-XX:StartFlightRecording`）或 async-profiler（`-e cpu`/`-e wall`），二者都能按栈聚合自身耗时。
- 观察锁竞争用 `-e lock`；观察分配用 `-e alloc`，注意分配采样本身有开销。

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
