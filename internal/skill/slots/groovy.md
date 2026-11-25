## description
调试 Groovy 程序（动态脚本、GString、闭包、AST 转换）时使用：报告的行号与源码对不上、脚本类名是文件名、`def` 变量类型是 Object。纯 Gradle/Groovy 插件加载失败不适用本 skill。

## triggers
- 脚本或 AST 转换后的行号与源文件不一致（报错行指向生成代码）。
- 断点落在 `run()`/`main()` 里，但源码里没有对应方法。
- `GString` 与 `String` 比较结果不符，或用 `==` 判断时结果与预期不同。

## preconditions
- 用 `groovyc -g` 编译，保证行号表与变量表；`groovy` 运行脚本时默认带调试信息。
- 脚本类名由文件名决定（`App.groovy` → `App`），继承 `groovy.lang.Script`，入口是 `run()` 或 `main()`。
- 运行期类路径需要 `groovy.jar`（或 `groovy-all`）。

## session
- 启动命令形如 `java -cp build/classes:groovy.jar App`；脚本方式可用 `groovy App.groovy`。
- 断点下在脚本语句行上，命中点通常在 `run()` 方法里；`main` 只是引导。
- 动态调用点（`CallSite`）会把方法分派缓存下来，栈帧里会出现 `invoke`/`call` 层，业务逻辑在下一层。

## build
- Gradle：`./gradlew build`，Groovy 产物在 `build/classes/groovy/main`；也可 `groovyc -g` 直接编译到自定目录。
- `groovy` 启动器不是 java，会被工具拒绝；脚本调试用 `["java","-cp","groovy-all.jar","groovy.ui.GroovyMain","App.groovy"]`。
- `./gradlew run --debug-jvm` 以 suspend 方式等调试器（默认 5005），用 attach 连该端口。
- 调试 Gradle 插件或 `buildSrc` 要 attach 到 Gradle 自身 JVM（在 `GRADLE_OPTS` 里注入 JDWP），不要在会话里启动 `gradle`。
- AST 转换生成的代码行号不落在源脚本上，构建后先用一次断点确认实际停靠行。

## read-traps
- AST 转换（`@CompileStatic`、`@Memoized`、外部 AST 脚本）会把代码挪进生成方法，调试器给出的行号可能指向生成代码或空行；定位以 `SourceFile` 与栈帧中的类名为准。
- `def` 变量声明类型是 `Object`，取到的值是运行时对象：数字可能是 `Integer`/`BigDecimal`，字符串可能是 `GStringImpl`。
- `GStringImpl` 不是 `String`，`==` 在 Groovy 里走 `equals` 语义，与 Java 的引用比较不同；要看真实类型再判断。
- 闭包是 `Closure` 子类，捕获变量在 `delegate`/`owner`/`thisObject` 上，`locals` 里只有闭包对象。
- 脚本类的字段是脚本局部变量（`@Field` 或顶层 `def`），展开脚本实例才能看到。
- `@CompileStatic` 与否决定变量是走动态调用还是直连字节码，栈深度会不同。

## mutate-bounds
- 动态分派的缓存（`CallSite`）在改值后不会失效，改类型可能让后续调用走错重载。
- `GString` 字段写入字符串时需要匹配运行时类型，写入 `String` 与 `GString` 行为不同。
- 脚本实例字段与闭包捕获是两份状态，改一处不一定影响另一处。
- AST 转换生成的方法没有源码槽位，改值无效。

## antipatterns
- **现象**：报错行号在源码里是空行或注释 → **误判**：以为源码过期 → **正确做法**：确认是否有 AST 转换或生成代码，用 `debug_inspect` 看 `this.class.name` 与栈帧。
- **现象**：`def x = 1` 的变量显示为对象且读写不一致 → **误判**：以为类型是 int → **正确做法**：按运行时类型（`Integer`/`BigDecimal`）判断，必要时显式声明类型。
- **现象**：断点设置成功但从不命中 → **误判**：以为行号选择器坏了 → **正确做法**：确认入口是不是脚本 `run()`，动态脚本源码需与会话工作目录一致。

## profiling
- JVM 剖析器可用；动态调用点会让热点分布偏向 `CallSite.invoke`，按源码行归并后再排序。
- 脚本场景下大量时间可能花在 `GroovyShell` 解析与类生成，先确认是否在反复编译脚本。
- 无剖析工具时用带命中序号的断点估算调用次数，并在结论中标注采样性质。
