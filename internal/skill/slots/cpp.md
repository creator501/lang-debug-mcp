## description
调试 C++ 程序（模板、STL 容器、智能指针、虚函数分派）时使用：`std::string` 显示异常、容器内容与业务不符、异常抛出点找不到、优化后 lambda 无栈帧。编译或链接错误不适用本 skill。

## triggers
- STL 容器（`std::vector`/`std::map`/`std::string`）在调试器里显示为内部字段，看不到元素。
- 抛出的异常在 `catch` 处拦截，但不知道最初抛出的位置。
- 断点下在 lambda 里不生效，或栈帧只显示 `operator()`。

## preconditions
- 用 `-g -O0` 编译；发行版二进制若无 `-g` 只能看到符号名，读值基本不可用。
- 调试器优先 lldb-dap；lldb 自带 libstdc++/libc++ 的 pretty printer，能直接显示容器内容。
- 关闭 RTTI（`-fno-rtti`）后无法从基类指针还原动态类型，读值只能按静态类型看。

## session
- 例外：`catch throw` 类断点若后端不支持，改在可疑 `throw` 行下断点，或在 `__cxa_throw` 上停。
- 虚函数分派问题：在基类方法下断点，命中后看对象的 vtable 与动态类型，而不是只看静态类型。
- 模板实例化的符号很长，断点按源码行下，不要按符号名匹配。

## build
- CMake：`cmake -B build -DCMAKE_BUILD_TYPE=Debug -DCMAKE_CXX_FLAGS="-g -O0" && cmake --build build`；多配置生成器产物在 `build/Debug/app.exe`。
- Make：`make CXXFLAGS="-g -O0 -std=c++17"`；产物路径以工程实际输出为准（`build/`、`bin/`、`./app`）。
- 启动命令给产物：`["./build/app","--flag"]`；`cmake`、`make`、`ninja`、`conan`、`vcpkg` 都不是可执行文件。
- Release 也要行号时用 `-DCMAKE_BUILD_TYPE=RelWithDebInfo`，不要 `strip`；调试期不要开 `-D_GLIBCXX_DEBUG`，它会改变容器 ABI。
- 模板与内联会把帧折叠，构建时保留 `-fno-omit-frame-pointer` 才能看到完整调用链。
- native 后端不支持 attach，且默认只跟父进程；多进程工程要对子进程单独 launch。

## read-traps
- `std::string` 有 SSO：短字符串内容在对象内部，长字符串在堆上；两者显示相同，但直接改内部指针会破坏状态。
- `std::vector` 是三个指针（begin/end/capacity），元素内容要按 size 展开；`capacity` 之后的区域不是有效元素。
- `std::unique_ptr`/`shared_ptr` 显示的是包装对象，要解引用看 `get()` 指向的对象；`weak_ptr` 可能已过期。
- `std::map`/`unordered_map` 的节点结构是实现细节，按 pretty printer 的键值视图读，不要读 `_M_header`。
- 引用在调试器里本质是指针，绑定对象的生命周期结束后读到的内容是未定义值。
- 优化后 lambda 被内联进调用者，可能出现「没有对应栈帧」或变量被合并。

## mutate-bounds
- `const` 成员与虚表指针不要写：改 vtable 会让后续调用跳到错误实现并崩溃。
- 改 `std::string` 的内容要按 SSO/堆两种布局处理，直接写 `_M_p` 会导致析构时崩溃。
- `std::vector` 改 `size` 不改变已分配内存，但会让析构与遍历读到未构造对象。
- 智能指针的引用计数是原子量，改写会导致重复释放或泄漏。

## antipatterns
- **现象**：容器显示成一堆内部字段 → **误判**：以为调试器不支持 STL → **正确做法**：确认 pretty printer 是否加载（lldb 一般自带），按元素视图读。
- **现象**：异常在 `catch` 处才看到 → **误判**：以为没有抛出点信息 → **正确做法**：在可疑 `throw` 处下断点，或停在异常抛出钩子上看第一帧。
- **现象**：`-O2` 下 lambda 断点不生效 → **误判**：以为源码行写错 → **正确做法**：断在调用点或改 `-O0` 复现。

## profiling
- `perf record -g --call-graph dwarf` 可在有 `-g` 时还原内联栈；帧指针省略时 dwarf 更准但更慢。
- 堆问题用 heaptrack/ASan；采样剖析看不到内存越界。
- 无剖析工具时用带命中序号断点统计，并在结论中标注采样性质。
