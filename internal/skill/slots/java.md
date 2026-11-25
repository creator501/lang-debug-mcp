## description
调试 Java 程序（JVM/JDWP）时使用：空指针堆栈落在业务行、集合内容与预期不符、断点不生效或变量读不到。纯 javac 编译错误、依赖下载失败等非运行态问题不适用本 skill。

## triggers
- 堆栈里出现 `java.lang.NullPointerException`、`ClassCastException`、`ArrayIndexOutOfBoundsException`，需要确认出错那一行各变量的实际值。
- 断点显示未生效，提示源文件未加载或该行没有可执行代码。
- 集合（`ArrayList`/`HashMap`）打印出的内容与业务预期不一致，需要看到元素真实结构。

## preconditions
- 目标以 JDWP 代理启动：`-agentlib:jdwp=transport=dt_socket,server=y,suspend=y,address=<端口>`；工具在 launch 模式下自动注入。
- class 文件带行号表与局部变量表：`javac -g`（Maven/Gradle 的默认 debug 配置通常已包含）。
- 能读到源码：`-cp` 指向 class 目录，源码目录通过会话的工作目录给出，断点按文件基名匹配。

## session
- 顶层入口类用全限定名：`java -cp target/classes com.example.App`；单文件源码在 JDK 11+ 可直接 `java App.java`。
- 会话入口停在 VM 启动处，此时业务类尚未加载，断点是「待解析」状态，继续执行后由类加载事件补上。
- attach 到已运行进程需要它启动时就带 JDWP 参数；JVM 无法事后开启代理，失败时按返回的重启命令重新拉起。

## build
- Maven：`mvn -q -DskipTests package`；class 产物在 `target/classes`，可执行包在 `target/<artifact>-<version>.jar`。
- Gradle：`./gradlew build`；类目录在 `build/classes/java/main`，依赖 jar 在 `build/libs`。
- 启动命令只能是 java：`["java","-cp","target/classes:<依赖 jar，用 ; 或 : 分隔>","com.example.App"]`，或 `["java","-jar","target/app.jar"]`。
- 依赖 classpath 要自己拼：`mvn -q dependency:build-classpath -Dmdep.outputFile=cp.txt` 的结果接到 `-cp` 后；Gradle 用自定义任务导出 `runtimeClasspath`，或直接构建 fat jar。
- 多模块：类目录是 `service/target/classes`、`api/build/classes/java/main`，`-cp` 要把用到的模块列全；断点按源文件基名匹配，多模块同名类只会选中第一个并在返回里注明，看到该 note 时用全限定类名核对。
- 在构建工具里跑测试：`mvn -Dmaven.surefire.debug test`、`./gradlew test --debug-jvm` 会以 suspend 方式等调试器（默认 5005），此时用 attach 连该端口，不要再用 launch。

## read-traps
- 局部变量表按**槽位**记录：增强 for 循环变量、`try` 块里的临时变量会复用同一槽位，同一条语句里可能对应两个不同的名字，取值以命中的代码位置为准。
- 集合按后端识别的结构展开：`ArrayList`/`List.of`、`HashMap`/`LinkedHashMap`、`HashSet`、`LinkedList` 会展开成元素视图，其他集合按内部字段展示。
- `HashMap` 的键字段签名被擦除成 `Object`，键名以运行时类型还原；自定义对象作键时显示为「类型@标识」。
- 包装类型（`Integer`/`Long`/`Boolean`）展开为原始值；静态字段在 `statics` 作用域，实例字段里看不到。
- 字符串是对象：字段里存的是引用，长度与内容要用展开后的值，不要按 `char[]` 猜。
- 对象一律按字段渲染，不调用 `toString()`：它有副作用，返回值也不可信。

## mutate-bounds
- `final` 字段在调试器层面可写，但写入后破坏类不变量，后续逻辑可能异常；仅在确认影响面后使用。
- 改集合的内部字段（如 `ArrayList.size`）会让集合状态与元素数组不一致，之后的遍历可能越界。
- 局部变量槽位被复用时改值可能影响同一槽位的另一个变量；改完用新快照复核。
- 字符串字段需要一个新字符串对象，工具会为目标重新构造，原引用不变。

## antipatterns
- **现象**：日志顺序看起来就是执行顺序 → **误判**：把多线程交错的日志当因果 → **正确做法**：在疑似分支下条件断点，用命中时的线程名与栈帧确认。
- **现象**：循环里加无条件断点，命中成百上千次 → **误判**：以为业务在死循环 → **正确做法**：改成条件断点或按命中序号在第 N 次停下。
- **现象**：断点一直未生效 → **误判**：以为工具坏了 → **正确做法**：先确认 class 是否已加载、是否带 `-g` 行号表；入口停止时断点待解析属正常。

## profiling
- 先看能力清单：服务是否提供采集或剖析工具；没有就说明降级，不要编造工具调用。
- JVM 侧首选 JFR（`-XX:StartFlightRecording`）或 async-profiler（`-e cpu`/`-e wall`），二者都能按栈聚合自身耗时。
- 观察锁竞争用 `-e lock`；观察分配用 `-e alloc`，注意分配采样本身有开销。
