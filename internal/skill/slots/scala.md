## description
调试 Scala 程序（lazy val、object 单例、Future/Actor 并发）时使用：栈帧里全是 `$anonfun$`、单例字段看不到、Future 回调与业务线程不是同一个。纯 sbt 依赖解析失败不适用本 skill。

## triggers
- 栈帧出现 `App$$anonfun$1.apply`、`App$.foo$lzycompute`，业务方法名被 lambda 类名遮住。
- `object` 单例的字段读不到，或初始化时机与预期不符（`MODULE$` 尚未构造）。
- `Future`/Actor 回调里命中断点，但线程名属于 `scala-execution-context` 之类线程池。

## preconditions
- 用 `scalac -g:vars`（Scala 3 用 `-g`）编译，否则只有行号没有变量表，`locals` 会读不到。
- 运行期类路径需要 `scala-library.jar`。
- `object App { def main }` 的入口类是 `App`（编译器同时生成 `App$`，`MODULE$` 持有单例）。

## session
- 启动命令形如 `java -cp build/classes:scala-library.jar App`（Windows 用 `;` 分隔）。
- 主类只有转发方法，方法体在 `App$`；断点按源文件基名 `App.scala` 匹配，会自动落到 `App$` 的方法上。
- 单例字段在 `statics` 作用域里看不到 `MODULE$` 成员，需要展开 `locals.this` 或 `object` 实例。

## build
- sbt：`sbt compile`，产物在 `target/scala-<版本>/classes`；要单包运行用 `sbt assembly`（需 assembly 插件）出 `target/scala-<版本>/*-assembly-*.jar`。
- 依赖 classpath：`sbt "export runtime:fullClasspath"` 打印完整类路径，可直接接到 `-cp` 后。
- 启动命令只能是 java：`["java","-cp","target/scala-2.13/classes:<依赖>","App"]`；交互式 `sbt` 不能当启动命令。
- 在 sbt 内调试：`sbt -jvm-debug 5005 "run"` 会以 suspend 方式等调试器，之后用 attach 连 5005。
- 混合 Java/Scala 工程的 Java 产物可能在 `target/java-classes`，`-cp` 要把两处都列上。

## read-traps
- `object` 单例的字段挂在 `MODULE$` 实例上；`statics` 作用域看不到它们，要展开单例实例本身。
- `lazy val` 生成 `xxx` 字段加 `bitmap$0` 位图；字段为 `null` 不一定表示未求值，要先看 bitmap 位。
- `val` 在构造器里赋值，`case class` 的字段名与 `component1` 一一对应，栈帧里出现 `copy`/`apply` 多为编译器生成。
- 闭包捕获的变量被编译成 `anonfun` 类字段（`$outer`、`x$1`），`locals` 里看不到原名，要在闭包对象上展开。
- Scala 3 的 `given`/`inline` 参数在调用点被内联，断点处通常没有槽位。
- `Future` 的栈是异步栈：命中时的线程与提交任务的线程不同，因果链要靠 `ExecutionContext` 与回调注册顺序还原。

## mutate-bounds
- `val` 字段可写但破坏不变量；`case class` 改字段后 `equals`/`hashCode`/`copy` 结果会自相矛盾。
- 改 `lazy val` 的值不会让 `bitmap$0` 失效，后续访问仍返回新写入的值，但初始化逻辑被跳过。
- `Future` 内的局部变量在闭包里，改闭包字段只影响该回调实例。
- 单例 `MODULE$` 的字段写入是全局可见的，会影响同进程后续所有用例。

## antipatterns
- **现象**：在 Future 回调里下断点，线程栈里没有调用方 → **误判**：以为调用链断了 → **正确做法**：先看提交任务的线程与回调注册顺序，用断点记录两侧线程名。
- **现象**：`object` 初始化前的字段读到 `null` → **误判**：以为字段没赋值 → **正确做法**：确认 `MODULE$` 是否已构造，必要时停在 `<clinit>`。
- **现象**：断点未生效且提示源文件未加载 → **误判**：以为文件名要写 `App$.scala` → **正确做法**：仍用 `App.scala`，解析器会查 `App$`。

## profiling
- JVM 侧剖析器对 Scala 无特殊要求；注意 lambda 类名会影响热点聚合，按源文件与行号归并后再看排序。
- Actor/Future 场景用 wall-clock 采样，否则等待时间会被算成空闲。
- 没有剖析工具时，用命中序号断点统计调用次数，并在结论里说明是采样结果。
