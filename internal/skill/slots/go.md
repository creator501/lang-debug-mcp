## description
调试 Go 程序（goroutine 并发、panic、interface 与泛型、channel）时使用：栈上只显示当前 goroutine、interface 展开成类型指针、优化构建变量显示 `<optimized out>`。`go build`/`go test` 编译失败、依赖下载失败等构建期问题不适用本 skill。

## triggers
- `panic: runtime error: invalid memory address or nil pointer dereference` 落在业务行，需要看该行指针、interface 或 map 的实际值。
- 程序卡住并报 `fatal error: all goroutines are asleep`，需要确认阻塞点与 wait 关系。
- 断点命中但 slice/map 内容与预期不符，或 channel 收发的停靠位置不在预期。

## preconditions
- dlv 在 PATH（或 `LANGDEBUG_DLV` 指定）；`go run` 形态还需要 go 工具链，编译由 dlv 负责。
- 变量可读需关优化与内联：`-gcflags="all=-N -l"`（dlv debug 形态默认带上，自己 build 时要显式加）。
- 二进制不要 strip：`-ldflags="-s -w"` 会去掉符号，栈上只剩地址。

## session
- 启动命令两种：`["go","run","./cmd/app"]`（dlv debug 形态，由 dlv 编译）或已编译二进制 `["./bin/app"]`（exec 形态）；`go build`、`go test` 等其它子命令会被拒绝。
- 入口停止落在 `main.main`：dlv 自带的 entry 停止在运行时初始化之前（占位线程，无栈无变量），工具会自动推进到 `main.main`，因此入口快照能看到 `main.main → runtime.main` 的栈。
- 只显示当前 goroutine 的栈；切换 goroutine 需要 dlv CLI，本工具不做。
- attach 连的是已经起好的 dlv dap 适配器端口（`dlv dap --listen=127.0.0.1:<端口>`）；把 dlv 附着到某个进程要用 dlv 自己的 `attach` 子命令，本工具不做。
- `go run` 的相对包路径按 `cwd` 补成绝对路径，会话建在哪个目录都一致。

## build
- 工具不代跑 `go build`/`go test`：先构建再给二进制，或用 `go run <包>` 让 dlv 编译。
- 调试构建：`go build -gcflags="all=-N -l" -o bin/app ./cmd/app`；发布构建不要覆盖这份产物，重新构建会丢调试信息。
- 多模块/workspace：`go.work` 工程把 `cwd` 设成 workspace 根，包路径写相对形式（`./services/api/cmd/app`）；单模块给模块根。
- 依赖与 vendor：`-mod=vendor`、`GOFLAGS`、私有仓库凭据通过会话 `env` 传入，构建期问题先在构建里解决。
- 测试二进制可用 `go test -c`：`go test -c -o bin/app.test ./pkg` 后当可执行文件启动，用 `-test.run TestX` 限制用例。
- 交叉编译（`GOOS`/`GOARCH` 不同）的产物本机 dlv 跑不起来，先确认产物与调试机同平台。

## read-traps
- slice 是 `{ptr,len,cap}` 三字段：`len` 之后是脏数据，按 `cap` 读会看到旧值。
- `string` 是 `{ptr,len}` 结构而不是 C 字符串；子串与原串共享底层数组，长度决定内容。
- interface 是 `{type,data}` 两字段：nil 接口两个字段都为 0；`error` 里包着 typed nil 时接口本身非 nil，要展开 `data` 才看得出来。
- map 无序、内部是 bucket 数组：不要按显示顺序推断插入顺序；并发写会直接 `fatal error: concurrent map writes`，断点来不及停。
- 泛型实例的类型参数出现在符号名里（`Box[int]`），同名函数的不同实例是不同帧。
- 内联函数的帧会被折叠到调用方；`-l` 关闭内联后才看得到真实调用链。

## mutate-bounds
- 只接受基础字面量（数字、布尔、字符串、nil）：结构体、slice、map 的整体替换不支持，改字段要展开到具体路径。
- 字符串是不可变对象，改值会生成新字符串，原引用不变；依赖指针身份的判断会失效。
- 改 slice 的 `len` 会让视图与底层数组不一致，后续 append 可能覆盖其他数据。
- channel 的内部索引（`sendx`/`recvx`）与缓冲计数改动会让收发错乱，不要用改值模拟收发。
- 优化构建下变量在寄存器里，改值可能只改到寄存器副本，读回值不变属正常。

## antipatterns
- **现象**：`fatal error: all goroutines are asleep`，但快照里只有一条栈 → **误判**：以为其他 goroutine 没跑 → **正确做法**：本工具只显示当前 goroutine，用日志或 dlv CLI 的 `goroutines` 辅助，别把「没有其他栈」当成「没有其他协程」。
- **现象**：interface 显示为 nil，调用方说传了值 → **误判**：以为参数丢了 → **正确做法**：展开 `{type,data}`，区分 typed nil 与真 nil（`(*T)(nil)` 装进接口后接口非 nil）。
- **现象**：release 产物里变量全是 `<optimized out>` → **误判**：以为内存被踩 → **正确做法**：用 `-gcflags="all=-N -l"` 重编，优化后的值不可恢复。

## profiling
- CPU 与堆用 `pprof`：`go tool pprof` 读 `-cpuprofile`/`-memprofile`，采样是统计值，短函数会被低估。
- `block`/`mutex` profile 需要显式打开（`runtime.SetBlockProfileRate`、`runtime.SetMutexProfileFraction`），否则为空。
- 调度与 GC 停顿用 `go tool trace`，它不能替代 CPU 采样。
- 没有采样条件时用带命中序号的断点统计调用次数，并在结论里标注这是计数不是采样。
