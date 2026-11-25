## description
调试 Kotlin 程序（协程、data class、委托属性）时使用：suspend 函数栈帧只剩状态机、lazy 值首次访问后才出现、data class 打印内容与字段不一致。纯 Gradle 构建失败或依赖解析失败不适用本 skill。

## triggers
- 栈帧里只有 `Foo$bar$1.invokeSuspend`、`ContinuationImpl.resumeWith`，看不到业务方法名。
- `by lazy` 的委托属性在断点处仍是 `null` 或 `Lazy` 对象，需要看它是否已被求值。
- `data class` 的 `toString` 输出与字段实际值不一致（自定义 `toString` 或字段被改过）。

## preconditions
- 用 `kotlinc -g` 编译（默认带行号表；显式 `-g` 保证变量表也在）。
- 运行期类路径需要 `kotlin-stdlib.jar`，否则会在 `main` 里抛 `NoClassDefFoundError: kotlin/...`。
- 顶层函数编译到 `<文件名>Kt` 类：`App.kt` 的 `main` 在 `AppKt` 里，入口类名要写 `AppKt`。

## session
- 启动命令形如 `java -cp build/classes:kotlin-stdlib.jar AppKt`（Windows 用 `;` 分隔）。
- 协程里的断点仍按源码行命中；停止后线程是 carrier 线程，需要用栈帧里的 `Continuation` 判断是哪个协程。
- `@JvmStatic`/`object` 单例的成员在 `Foo.INSTANCE` 上，静态字段用 `statics` 作用域看。

## build
- Gradle：`./gradlew build`；Kotlin 产物在 `build/classes/kotlin/main`，运行期还要 `kotlin-stdlib.jar`。
- 启动命令：`["java","-cp","build/classes/kotlin/main:<stdlib 与依赖>","AppKt"]`；顶层函数的类名是 `<文件名>Kt`。
- `./gradlew run --debug-jvm` 让 Gradle 以等待调试器的方式拉起 JVM（默认 5005），之后用 attach 连该端口。
- 多模块写 `:app:build`，产物在 `app/build/classes/kotlin/main`，`-cp` 要把被依赖模块一起带上。
- Kotlin/Gradle 默认带 `-g`（行号表与变量表），不要为减小编译产物关掉调试信息；增量编译不影响断点。

## read-traps
- 挂起函数被编译成状态机：局部变量搬进 `Continuation` 的字段，`locals` 里可能只有 `this` 与 `$continuation`，实际值要从 `label`、`$result` 与状态机字段里找。
- 顶层与扩展函数落在 `<文件名>Kt` 类；文件名与类名不一致时按 `SourceFile` 属性匹配断点。
- `by lazy` 生成 `xxx$delegate` 字段，值是 `SynchronizedLazyImpl`；先展开看 `_value` 是否已初始化，再判断业务值。
- `const val` 在编译期内联到调用方，断点处通常没有对应槽位；`val` 的属性若是自定义 getter，断点要下在 getter 里。
- `data class` 的 `componentN`、`copy` 是编译器生成的方法，栈帧里出现它们不代表业务逻辑。

## mutate-bounds
- `val` 字段在调试器层面可写，但写入后与构造期语义脱节，可能导致后续 `equals`/`hashCode` 不一致。
- 协程状态机的 `label`、`$result` 是调度状态，改写会让恢复执行落到错误分支，除定位问题外不要改。
- 内联函数（`inline`、`run`、`let`）的参数在调用方没有独立槽位，无法单独改值。
- 委托属性改 `xxx$delegate` 需要整体替换 `Lazy` 实现，直接改字段不会让 `lazy` 重新求值。

## antipatterns
- **现象**：在 `suspend fun` 里单步，跳转位置完全看不懂 → **误判**：以为源码与字节码不匹配 → **正确做法**：用断点加条件停在目标行，展开 Continuation 看状态。
- **现象**：`toString` 打印的字段值与 `debug_inspect` 不一致 → **误判**：以为工具读错 → **正确做法**：确认类是否自定义了 `toString`，结论以字段值为准。
- **现象**：断点未生效并提示类未加载 → **误判**：以为 `AppKt` 名字写错 → **正确做法**：确认入口类名是 `<文件名>Kt`，并检查 `-cp` 是否包含 stdlib。

## profiling
- 协程场景用 async-profiler 的 wall-clock 模式（`-e wall`）才能看到挂起等待；CPU 模式只看到 carrier 线程的忙等。
- JFR 里协程表现为主线程池上的短任务，按线程聚合会低估耗时，需按调用栈看。
- 在没有剖析工具时，用带命中序号的断点统计调用次数，并在结论中标注这是采样而非全量。
