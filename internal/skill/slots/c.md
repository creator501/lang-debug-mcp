## description
调试 C 程序（段错误、优化后变量丢失、宏展开行号错乱）时使用：崩溃栈指向可疑行、变量显示为 `<optimized out>`、数组与指针内容需要确认。链接失败、头文件缺失等构建期问题不适用本 skill。

## triggers
- 进程收到 `SIGSEGV`/`SIGABRT`，需要知道崩溃点上指针与缓冲区长度是否匹配。
- 变量显示 `<optimized out>`，或读到与源码逻辑矛盾的旧值。
- 宏展开后的报错行指向头文件，或同一行在不同编译单元表现不一致。

## preconditions
- 用 `-g -O0` 编译（调试期关闭优化）；`-O2` 下仍可带 `-g`，但要接受变量被优化掉。
- 后端优先 lldb-dap；缺失时自动降级到 gdb MI（`backend=gdb-mi`），此时只有断点、栈与变量读取，改值、logpoint 与 `>N` 命中条件不可用，改用 `debug_eval` 求值。
- 需要符号与源码：二进制未 strip，源码路径在会话工作目录内可解析。

## session
- 启动命令给出完整 argv，工具在 launch 模式下保持原参数并附加调试器入口；stdin/stdout 不重定向。
- 段错误定位：先用 `debug_run` 跑到崩溃停止点，再逐帧看参数；不要从 `main` 一路单步。
- 多进程/`fork` 场景默认只跟父进程，子进程需要单独 attach，结论里注明跟随的是哪个 pid。

## build
- CMake：`cmake -B build -DCMAKE_BUILD_TYPE=Debug && cmake --build build`；产物在 `build/app`（Makefile/Ninja）或 `build/Debug/app.exe`（MSVC 多配置生成器）。
- Make：`make CFLAGS="-g -O0"`（或工程自带的 `make debug` 目标），产物常见于 `./app`、`./a.out`、`bin/app`。
- 启动命令给产物路径：`["./build/app","--flag"]`；`cmake`、`make`、`ninja` 会被当成不存在的可执行文件拒绝。
- 工具会拒绝已 strip 的二进制，并在 `-O2`、源文件比二进制新时给警告：按提示重新构建，不要在旧产物上排查。
- 调试期用 `-g3` 保留宏信息、`-fno-omit-frame-pointer` 保留完整栈；发布构建不要覆盖调试构建。
- native 后端不支持 attach，工程里的守护进程只能由工具 launch 托管，或改用 core dump 离线分析。

## read-traps
- 指针只给出地址与目标对象，`char *` 按字符串读，`void *` 需要先确认实际类型再展开。
- 数组作为参数会退化为指针，`sizeof` 在函数内不可信；长度要另找变量或看分配点。
- `<optimized out>` 是不可恢复状态：不要用邻近变量的值推测，改为 `-O0` 重编或用 `printf` 之外的手段（如临时守卫变量）。
- 结构体的 padding 字段不在源码里，偏移与调试器显示可能不一致，按字段名读而不是按偏移算。
- 宏与内联函数会把行号归到定义处，栈帧行号指向头文件属正常。

## mutate-bounds
- 字符串字面量在只读段，写入会 `SIGSEGV`；改字符串要用堆上的缓冲区。
- `const` 与编译期常量（`static const int`）可能已被内联到调用点，改存储不影响已生成的代码。
- 寄存器分配后的局部变量可能不可写；报「无法写入」时不要反复尝试。
- 越界写能成功但会破坏堆，后续崩溃点与真正的问题点无关，改值前先确认边界。

## antipatterns
- **现象**：崩溃栈指向 `memcpy`/`free` 内部 → **误判**：以为库有 bug → **正确做法**：看上一层业务帧的缓冲区指针与长度，崩溃点只是受害者。
- **现象**：`-O2` 下变量值明显不对 → **误判**：以为内存被踩 → **正确做法**：先排除优化导致的旧值/寄存器复用，必要时 `-O0` 复现。
- **现象**：变量显示为地址，无法判断内容 → **误判**：以为读值失败 → **正确做法**：确认指针类型与指向对象的真实类型，再按类型展开。

## profiling
- `perf record -g` 后 `perf report`，需要 `-fno-omit-frame-pointer` 或 `--call-graph dwarf` 才能拿到完整栈。
- 内存问题用 valgrind/ASan（`-fsanitize=address`）定位，采样剖析看不到越界写。
- 无剖析工具时用带命中序号的断点统计调用次数，并在结论中标注采样性质。
