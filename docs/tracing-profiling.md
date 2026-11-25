# 执行流追踪与性能剖析

本文只讲三件事：怎么用采集点（tracepoint）拿精确事实、怎么用中断采样/外部剖析拿统计热点、以及在能力缺失时该退到哪一步。示例基于本仓库的夹具 `testdata/fixture/trace.js` 与 `testdata/fixture/busy.js`。

## 1. 三条路径怎么选

| 需求 | 用什么 | 数据性质 | 对目标的影响 |
|---|---|---|---|
| 某个位置到底执行了几次、当时变量是什么 | `debug_tracepoint` 的 `add` + `events` | 精确事件（受采样率与丢弃计数约束） | 每命中一次短暂中断再恢复 |
| 时间花在哪些函数上、调用关系如何 | `debug_profile` 的 `sample` | 统计值，不是精确调用次数 | 每个采样间隔停顿一次 |
| 需要接近生产级的 CPU 采样 | `debug_profile` 的 `start`/`stop`（外部剖析器）或 `import` | 取决于来源，仍标注精度 | 由外部剖析器附着采集 |

结论里必须写清用的是哪条路径，以及“统计值”还是“精确事件”。把采样数说成调用次数是错误结论。

## 2. 采集点：非停止采集

### 2.1 定义与取值

```json
{"action":"add","session":"s1","file":"testdata/fixture/trace.js","line":6,
 "template":["locals.total"],"rate":1,"pointLimit":200}
```

- `template` 是命中时记录的路径表达式，取值受预算裁剪（超限时事件标 `truncated`）。
- `kind` 取 `instant`（默认）/`enter`/`exit`；`enter` 与 `exit` 成对时在 `exit` 事件上给出耗时，口径是墙上时间。
- 命中后立即恢复目标，不提供停在原地的交互。若命中的是普通断点（不是采集点），采集会停下并把控制权交回，此时用 `debug_run` 交互。

### 2.2 开销模型

三个上限互相独立，任何一个生效都会在 `stats` / `counters` 里留下计数：

| 参数 | 默认 | 作用 | 超出时 |
|---|---|---|---|
| `rate` | 1（全量） | 每 N 次命中记录 1 条 | 计入 `skipped` |
| `pointLimit` | 500 | 单点累计记录条数 | 计入 `dropped` |
| `rateLimit` | 200 条/秒 | 全局每秒记录上限（固定 1 秒窗口） | 计入 `dropped` |
| 环形缓冲 | 2000 条 | 未读取时保留的事件数 | 丢弃最旧一条并计入 `dropped` |

热路径（每秒上万次命中）**必须先降频**：只设 `template` 不设 `rate` 会让目标在断点与恢复之间反复停顿，同时大部分事件被速率上限丢掉。

### 2.3 查询

```json
{"action":"events","session":"s1","aggregate":true}
{"action":"events","session":"s1","aggregate":false,"eventLimit":5,"from":1,"to":20}
```

- 聚合视图按采集点返回次数与耗时统计（`count` / `duration.minNs|avgNs|maxNs`）。
- 明细视图默认只给 20 条，`total` 是命中总数；看到的是前若干条，不是全部。

## 3. 中断采样：`debug_profile` 的 `sample`

```json
{"action":"sample","session":"s1","intervalMs":50,"samples":40,"top":10,"focus":"spin"}
```

- `intervalMs` 越小越准，但停顿越频繁：`<10ms` 记“高”、`10–50ms` 记“中”、`>50ms` 记“低”，报告里的 `overhead` 字段如实写出。
- 只给三种视图：`hot`（自身样本最高）、`cumulative`（函数出现在栈上的样本数）、`callers`（`focus` 指定的函数的调用来源及占比），`total*` + `offset` 支持翻页。
- 报告固定包含“统计值非精确计数”“每次采样会短暂停顿目标”的标注。
- 采集进行中（`debug_tracepoint` 在跑）不能同时采样：两者都要驱动目标运行，会互相打断。

## 4. 外部剖析：`start` / `stop` / `status` / `import`

### 4.1 直接调用（start/stop）

```json
{"action":"start","session":"s1","pid":12345,"durationMs":3000,"timeoutMs":8000}
{"action":"status","session":"s1"}
{"action":"stop","session":"s1","top":10,"focus":"parse"}
```

- `pid` 必填：调试端口不是进程号，用系统工具查（Windows `tasklist | findstr app`，Linux `pgrep -f app`）。
- 目标处于停止态时拒绝开始（规范要求）。
- 超时会终止剖析器并返回**已采集的部分数据**，`partial=true`，`notes` 里说明是部分结果。
- 会话关闭时会把仍在跑的剖析进程一起结束，不留残留采样器。

| 语言 | 剖析器 | 实际命令 | 输出格式 |
|---|---|---|---|
| Python | py-spy | `py-spy record --format raw --output out.folded --pid <pid> --duration <秒>` | folded |
| Java/Kotlin/Scala/Groovy | async-profiler | `asprof -e cpu -d <秒> -o collapsed -f out.folded <pid>` | folded |
| Node.js | 无独立采样器 | 用 `node --cpu-prof` 启动，再 `import` | v8 |
| C/C++/Rust（Windows） | 无 | 用 `sample` 做中断采样 | — |
| C/C++/Rust（Linux） | perf | `perf record -F 99 -g -p <pid> -- sleep <秒>` 后折叠 | folded |

### 4.2 导入已有文件（import）

```json
{"action":"import","format":"folded","path":"out.folded","focus":"parse"}
{"action":"import","format":"v8","path":"CPU.20260101.120000.1234.0.001.cpuprofile"}
```

`import` 不需要会话，是纯解析：

- `folded`：每行 `a;b;c 12`，也接受 `func (file.py:12)` 形式的位置后缀；py-spy、async-profiler、`stackcollapse-perf.pl` 的输出都属于这一族。
- `v8`：`.cpuprofile` 的节点图，按 `children` 反查父链还原“叶→根”栈；只有 `hitCount` 时按命中数加权。

两种来源都会归一成同一份报告：`hot` / `cumulative` / `callers` / `edges`，并把运行时噪声帧（`(root)`、`(program)`、`(idle)`、`[unknown]` 等）计入 `filtered`，不静默丢弃。

### 4.3 Node 的 `.cpuprofile` 怎么来

```powershell
node --cpu-prof --cpu-prof-dir .\profdist testdata/fixture/trace.js
# 产物形如 .\profdist\CPU.20260101.120000.1234.0.001.cpuprofile
```

再用 `import`（`format=v8`）导入。把 `--cpu-prof-dir` 指向可写目录，否则 profile 落在当前工作目录，容易被误认为源码文件。

## 5. 双源对齐：`debug_tracepoint` 的 `timeline`

```json
{"action":"timeline","session":"s1"}
```

- 前提：同一会话里先用 `add` 采集过事件，再用 `debug_profile sample` 采过样本；缺任一侧都会报错并给出下一步动作。
- 输出以会话内墙上时间为基线，桶宽默认等于采样间隔（跨度大时按 40 桶上限放大）。
- `alignment.matched` 是两源都出现的函数（带事件数与叶子样本数）；`traceOnly` / `profileOnly` 是只出现在一侧的部分，`notes` 给出判断方向：
  - 采集点函数不在采样栈中 → 该路径不在采样窗口，或采样密度不足；
  - 采样热点没有采集点事件 → 未插桩路径，或短函数/运行时内部帧噪声。

## 6. 降级矩阵

| 情形 | 行为 | 下一步 |
|---|---|---|
| 没有 lldb-dap（native） | `debug_env` 报 `backend=gdb-mi`，采集点/采样仍可用，改值与 logpoint 不可用 | 装 LLVM 或 MSYS2 lldb；`MSYS2_ROOT` / `LANGDEBUG_LLDB_DAP` 指定路径 |
| 没有 py-spy / asprof | `start` 返回 `ENV_ADAPTER_MISSING`，带安装命令与该语言替代方案 | 装工具，或改用 `sample` / `import` |
| Windows 上 native 想外部剖析 | 不提供 perf，直接降级为 `sample` 中断采样 | 接受统计精度；或到 Linux 上 `perf record` 后 `import` |
| Node 想要外部采样 | 无独立采样器，用 `--cpu-prof` + `import` | 见 4.3 |
| 高优化二进制（`-O2`/release） | 变量可能 `<optimized out>`，采样仍可用但栈可能内联 | 用 `-g -O0` 重编，或接受内联后的热点归属 |
| JVM 缺局部变量表 | 只剩行号，`locals` 读不到 | `javac -g` / `kotlinc -g` / `scalac -g:vars` / `groovyc -g` 重编 |

## 7. 自检命令

### Windows（PowerShell，本机已执行通过）

```powershell
# 1. 精确事件：循环 20 次应记录 20 条
go test ./internal/tool/ -run 'TestTracepointCollectsAndContinues' -v

# 2. 过载保护：速率上限下丢弃计数必须 > 0
go test ./internal/tool/ -run 'TestTracepointRateLimitDrops' -v

# 3. 中断采样与三视图
go test ./internal/tool/ -run 'TestProfileInterruptSampling|TestProfileCumulativeAndCallerViews' -v

# 4. 双源对齐
go test ./internal/tool/ -run 'TestTracepointTimeline' -v

# 5. 归一化解析（折叠栈与 .cpuprofile）
go test ./internal/profile/ ./internal/tool/ -run 'TestParseFolded|TestParseV8|TestProfileImport' -v
```

### Linux（bash，需在装有 Go 与 perf 的机器上执行）

```bash
go test ./internal/tool/ -run 'TestTracepoint|TestProfile' -v
perf record -F 99 -g -p "$(pgrep -f app)" -- sleep 3
perf script | stackcollapse-perf.pl > out.folded   # 来自 FlameGraph 仓库
# 然后 debug_profile import(format=folded, path=out.folded)
```

## 8. 常见误用

- 把采样数当调用次数：`cumulative.samples` 是“采样瞬间在该函数栈上的次数”，不是执行次数。
- 热路径不降频就挂采集点：目标会被反复中断，事件也会被速率上限丢掉，看到的只是被截断的一小段。
- 只看 `hot` 就下结论：短函数会被调用者吞掉，需要 `cumulative` 和 `callers` 一起看。
- 以为 `filtered` 里的帧不算样本：它们确实被排除了，所以有效样本数小于剖析器报告的原始样本数，占比分母用的是有效样本。
- 在目标停止态请求剖析：规范禁止，工具会直接报 `STATE_NOT_STOPPED` 而不是启动后空跑。
