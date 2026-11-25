# 工具契约与后端实现（Node.js / Python / JVM / native）

本文记录当前已实现的工具面、参数、返回结构与能力边界。错误码见
[`error-codes.md`](./error-codes.md)。

## 工具总览

| 工具 | 作用 | 何时不该用 |
|---|---|---|
| `debug_env` | 探测语言与后端可用性 | — 任何会话前都应先调用 |
| `debug_session` | open / list / status / close | 不要用它设断点 |
| `debug_breakpoint` | set / update / remove / list | 不要在热路径挂无过滤断点 |
| `debug_run` | continue / step / pause / restart / terminate | 不要用单步替代条件断点 |
| `debug_inspect` | 批量读值 | 目标运行时会失败 |
| `debug_eval` | 表达式求值（有副作用） | 只读需求不要用它 |
| `debug_mutate` | 批量改值（实验性） | 不要当作修复手段 |
| `debug_output` | 读目标输出尾部 | attach 模式无输出 |

## 建立会话

```json
{"action": "open", "language": "nodejs", "mode": "launch",
 "command": ["node", "app.js"], "cwd": "/repo", "debugInfoCheck": "warn"}
```

`command` 必须是**程序原本的启动命令**：调试参数由工具注入
（`--inspect-brk=127.0.0.1:<空闲端口>`）。若入口不是直接用 `node` 启动
（如 `npm start`），工具会返回 `ENV_DEBUG_INFO` 并给出替代方案，因为 Node
不允许在 `NODE_OPTIONS` 中设置 `--inspect`。

返回：

```json
{"session":"sess1","language":"nodejs","mode":"launch","state":"stopped",
 "breakpoints":0,"snapshot":"s1",
 "stop":{"reason":"pause","thread":"main",
         "at":{"file":"E:\\repo\\app.js","line":1,"func":""},
         "frames":1,"scopes":["this"]},
 "capability":{"backend":"cdp","supported":["read","write","eval"],
               "unsupported":["tracepoint","stackSample","profile"],
               "limits":["不提供独立的参数作用域名，参数位于 locals 作用域"]},
 "warnings":[]}
```

要点：

- `stop` 与 `scopes` 是不受预算裁剪的元信息。
- `capability` 让调用方在执行前就知道哪些动作会得到 `CAP_UNSUPPORTED`。
- 入口停止时尚未进入用户函数，`scopes` 可能只有 `this`，要等断点命中才有 `locals`。

## 设置断点

```json
{"action":"set","session":"sess1","breakpoints":[
  {"file":"app.js","line":5,"condition":"i > 0"},
  {"file":"app.js","line":5,"logMessage":"i={i} total={total}"}
]}
```

- `condition` 由目标语言求值，为真才中断。
- `hitCondition`（`>=3`、`==2`）由工具侧计数，不满足时自动继续。
- `logMessage` 使该断点**不中断执行**，只记录一条带 `{表达式}` 插值的消息，
  可在 `debug_output` 中读到。这是追踪高频路径的正确手段。
- 返回的断点 `verified: false` 表示脚本尚未加载或该行无可执行语句。

## 一次往返完成「跑到某处并取数」

```json
{"action":"continue","session":"sess1","timeoutMs":10000,
 "until":{"breakpoint":"bp1","hits":2},
 "onHit":{"inspect":["locals.total","locals.items[0..1]"],"depth":2}}
```

命中返回：

```json
{"snapshot":"s2","hits":2,"breakpoint":"bp1",
 "stop":{"reason":"breakpoint","at":{"file":"...app.js","line":5,"func":"compute"},"scopes":["locals","this"]},
 "values":{"locals.total":{"t":"num","v":"0"},
           "locals.items":{"t":"list","n":2,"ref":"r1"}},
 "clipped":["locals.items[0..1]: 受预算限制只返回 20 项"]}
```

超时未命中返回 `{"running":true,"timeoutMs":10000,"hint":"..."}`，目标仍在运行，
可再次调用 `continue` 等待，**不要**因此重建会话。

## 批量读值

```json
{"session":"sess1","snapshot":"s2","paths":["locals","locals.items[0..4]"],"depth":2}
```

- 路径语法：`[frame[n].]<scope>.<成员>[索引][区间]`；`*` 表示全部成员，
  `items[*]` 表示按预算遍历全部元素。
- 作用域名由后端给出：Node.js 常见 `locals` / `block` / `closure` / `global` / `this`。
  **`let`/`const` 声明的块级变量在 `block` 作用域**，不在 `locals`；停止信息的
  `scopes` 字段会列出本次可用作用域。
- 扁平键返回，`t` 类型、`v` 值、`n` 总量、`ref` 可继续展开的引用。
- 发生裁剪时 `clipped` 会说明位置与总量；`ref` 只在同一快照内有效。

## 改值

```json
{"session":"sess1","snapshot":"s2",
 "ops":[{"path":"locals.total","value":"100"}]}
```

返回逐条结果并回传原值：

```json
{"results":[{"path":"locals.total","ok":true,"oldValue":"0","t":"num"}],
 "snapshot":"s3",
 "note":"已作废旧快照，请使用新快照标识继续取值；目标内存已被修改，后续行为可能偏离原始逻辑"}
```

失败时会区分 `TARGET_READ_ONLY`、`TARGET_IMMUTABLE`、`TARGET_OPTIMIZED_OUT`、
`INPUT_INVALID`（类型不匹配），不统一表述为“操作失败”。

## Node.js 后端能力边界

| 动作 | 支持 | 说明 |
|---|---|---|
| 读值 | 是 | 惰性展开，受预算约束 |
| 改值 | 是 | 通过 `Runtime.callFunctionOn` 严格模式赋值 |
| 求值 | 是 | `Debugger.evaluateOnCallFrame` |
| 采集点 | 否 | 用 `logMessage` 断点替代 |
| 栈采样 | 否 | 后续迭代 |
| 性能剖析 | 否 | 后续迭代，计划接入内置 CPU Profiler |
| 目标输出 | launch 模式 | 采集 stdout/stderr 尾部；attach 模式无 |

## Python 后端（debugpy）

协议层与 Node.js 共用同一套 DAP 客户端，语言差异只在适配器启动与 launch/attach
参数上。

**解释器探测**：`LANGDEBUG_PYTHON` → `PATH` 上的 `python` / `python3` / `py` →
当前目录的 `.venv` / `venv`。找到解释器但缺 debugpy 时，返回
`ENV_ADAPTER_MISSING` 并在 `installHint` 给出 `<解释器> -m pip install debugpy`。

**launch**：命令必须由 Python 解释器启动。工具把
`["python", "-u", "app.py", "--flag"]` 改写为 `program`+`args`（解释器参数进
`pythonArgs`），`["python", "-m", "pkg.mod"]` 改写为 `module`，
`["python", "-c", "..."]` 改写为 `code`；无法改写时返回 `ENV_DEBUG_INFO` 并建议
改用 attach。适配器以 `python -m debugpy.adapter` 启动（DAP over stdio），
目标输出作为 `output` 事件回传，`stopOnEntry` 让会话开在入口停止。

**attach**：目标自己承载 DAP，工具直接连 `127.0.0.1:<port>`，因此本机不需要
debugpy。目标需这样启动：

```bash
python -m debugpy --listen 127.0.0.1:5678 --wait-for-client app.py
```

`configurationDone` 被推迟到第一次 `continue`/`pause` 才发送，这样会话打开后、
目标放行前仍可设置断点（`--wait-for-client` 让目标停在首行之前）。attach 模式
不会终止目标：关闭会话只断开连接。

| 动作 | 支持 | 说明 |
|---|---|---|
| 读值 | 是 | 惰性展开；dict 键去掉引号并保持插入顺序 |
| 改值 | 是 | `setExpression`，只接受数字/布尔/字符串/None 字面量 |
| 求值 | 是 | `evaluate`（watch 上下文） |
| 采集点 | 否 | 用 `logMessage` 断点替代 |
| 栈采样 | 否 | 后续迭代 |
| 目标输出 | launch 模式 | attach 模式目标是独立控制台，不采集 |

已知读值差异：

- `special variables` / `function variables` / `class variables` 是适配器插入的
  分组伪变量，一律过滤，避免淹没真正的局部变量。
- 集合按序列展示（元素名是内存标识，只能用序号访问）；生成器/协程按对象展示，
  需要展开 `gi_frame` / `cr_frame` 才能看到内部状态。
- C 扩展帧没有源码与局部变量，栈帧 `file` 为空。
- 单次展开最多跟随 5 页（约 500 项），超大容器用区间取值。
- 命中条件由适配器在目标侧执行，因此 `hits` 是观察到的中断次数，不是该行真实
  执行次数。

## native 后端（lldb-dap 优先，gdb MI 降级）

协议层与 Node.js / Python 共用同一套 DAP 客户端；本包只负责找到 lldb-dap、把
启动命令原样交给它，以及声明 native 侧的读值与改值边界。lldb-dap 缺失时降级到
gdb MI（`backend=gdb-mi`），只保证基础能力。

**适配器探测**：`LANGDEBUG_LLDB_DAP` → `PATH` 上的 `lldb-dap` / `lldb-vscode` →
编译器所在目录（MSYS2 的 ucrt64 / mingw64 / clang64 与 gcc、clang 同目录）→
`<ProgramFiles>\LLVM\bin`。候选文件还要跑一次 `--version` 确认能启动：残缺安装
（缺 `liblldb.dll`）会被跳过，而不是让会话在 initialize 阶段超时。版本只探测一次，
进程内复用。

**调试信息校验**（`debug_session` 打开前，`debugInfoCheck=warn` 时生效）：

- 无用户源码行号（MinGW 的 crt 自带调试信息，`libgcc2.c`/`cygwin.S` 不算）→
  `ENV_DEBUG_INFO` 阻断，提示用 `-g` 重编。
- 二进制已 strip（`nm` 无符号）→ `ENV_DEBUG_INFO` 阻断。
- 缺 `.debug_info`、`-O2` 及以上的优化构建、源文件比二进制新 → 警告，不阻断。

**gdb MI 降级**：没有 lldb-dap 但有 `gdb` 时自动降级，`debug_env` 报告
`backend=gdb-mi` 并在 `notes` 写明能力子集；`debug_session` 返回的 `capability`
里 `write=false`，改值会直接拿到 `CAP_UNSUPPORTED`。降级路径只保证断点、继续/
单步、栈帧与变量读取；logpoint 与 `>N` 形式的命中条件不支持（给 `>=N` 时转成 gdb
的 ignore count）。两者都没有时返回 `ENV_ADAPTER_MISSING`，`missing` 同时列出
`lldb-dap` 与 `gdb`。

**launch**：命令行原样透传——首项是可执行文件，其余是它自己的参数，不注入任何
调试参数；相对路径按 `cwd` 补全并校验存在，缺失时返回 `INPUT_INVALID` 并带上
`gcc/clang -g -O0` 的重编建议。目标输出作为 `output` 事件回传，`stopOnEntry`
让会话开在入口停止。

**attach**：尚未实现（lldb-dap 的附着入口是进程标识，会话参数里只有端口与
启动命令），显式返回 `CAP_UNSUPPORTED`，不把“连上了但没附上”包装成成功。

| 动作 | lldb-dap | gdb MI 降级 | 说明 |
|---|---|---|---|
| 读值 | 是 | 是 | 惰性展开；`<optimized out>` 标记为 `optimized`，不当作路径错误 |
| 改值 | 是 | 否 | lldb 先 `setExpression`，不支持时回退 `evaluate` 赋值表达式 |
| 求值 | 是 | 是 | lldb 用 `evaluate`（watch 上下文），MI 用 `-data-evaluate-expression` |
| 采集点 | 否 | 否 | 用 `logMessage` 断点替代 |
| 栈采样 | 否 | 否 | 后续迭代 |

已知读值差异：

- 优化编译（`-O2` 及以上）会让变量显示 `<optimized out>`，这类值不可恢复，
  应改用 `-g -O0` 重编后复现。
- 指针只给地址与目标对象，`void *` 需要先确认实际类型再展开。
- 容器按子项名判定：出现 `[n]` 子项即按序列展示（C 定长数组、`std::vector`、
  `std::string`、Rust 的 `[T; N]`），`size`/`capacity`/`ptr` 这类内部字段不计入
  元素。Rust 的 `Vec`/`String`/`Option` 需要 lldb 的 Rust 格式化脚本，没有脚本时
  按内部字段（`buf`/`len`/`variant0`）展示，元素读取用 `debug_eval`。
- 改值失败按措辞归类：只读内存段 → `TARGET_READ_ONLY`，编译期常量 →
  `TARGET_IMMUTABLE`，变量被优化掉 → `TARGET_OPTIMIZED_OUT`，适配器无写能力 →
  `CAP_UNSUPPORTED`。

其他未实现后端的语言返回 `ENV_ADAPTER_MISSING`，并说明该语言尚无调试后端。

## 已验证的端到端行为

- 入口停止、断点命中、条件与命中次数过滤
- 批量取值与共享父节点只遍历一次
- 改值成功回传原值、旧快照强失效
- 类型不匹配拒绝写入
- logpoint 不中断执行且可插值局部变量
- 空闲回收后按需重建会话
- Python：launch 与 attach 均能命中首个断点；dict/list 取值、改值、求值、
  logpoint 原生输出（不中断）
- native：C / C++ / Rust 三个夹具均能命中首个断点并读取栈帧；定长数组按序列展示
  （`items[0]` 可读）；改值回传原值，刷新后的快照读到新值
- native 降级：强制关掉 lldb-dap 后自动走 gdb MI，入口停在 `main`、断点命中、
  `locals` 与数组元素可读，改值返回 `CAP_UNSUPPORTED`
- native 调试信息校验：无 `-g`、已 strip 的两类目标被阻断；`-O2` 与源码陈旧给出警告
