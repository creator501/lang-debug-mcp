# lang-debug-mcp

**面向 AI Agent 的调试 MCP 服务端 —— 11 种语言，一套工具接口。**

[English](./README.md)

多数编码助手只能读源码猜结论。`lang-debug-mcp` 给它一个真实调试器：下断点、跑到指定位置、读实际值、单步、求值、改值、采集执行流、做性能剖析，最后给出**位置、观测值、复现路径**，而不是「看起来应该是」。

服务端通过 stdio 讲 MCP 协议，按语言族驱动原生调试器（V8 Inspector、debugpy、JDWP、lldb-dap / gdb MI、dlv dap）。代码不上传任何地方，被调试进程跑在你本机。

## 特性

- **10 个工具**覆盖完整闭环：探测环境 → 建会话 → 断点 → 运行/单步 → 读值 → 改值 → 读输出 → 采集点 → 性能剖析。
- **一次往返完成一次停止**：`continue` 支持 `until`（断点 + 命中序号）与 `onHit`（命中后立刻要读的路径），「跑到那里并读这些值」只需一次调用。
- **能力如实上报**：会话建立时返回 `capability`（`read` / `write` / `eval` / 限制清单），Agent 在执行前就知道某个动作会不会以 `CAP_UNSUPPORTED` 结束。
- **快照语义**：快照在目标停止期间有效，改值或恢复运行即失效，改前与改后的值不会被悄悄混用。
- **有预算的读值**：深层结构会裁剪，并明确给出 `clipped` 说明——裁剪了哪里、总量多少。
- **非交互采集点**：命中即记录 `{表达式}` 值并立刻恢复目标；聚合与明细视图会如实报告丢弃事件数。
- **结构化错误**：`ENV_*`、`INPUT_*`、`STATE_*`、`TARGET_*`、`CAP_UNSUPPORTED`，每类都带可执行修复建议（见 [docs/error-codes.md](./docs/error-codes.md)）。
- **内置技能包**：每种语言一份调试流程（[`skills/`](./skills/README.md)），由同一模板生成，共享纪律逐字一致。

## 支持的语言

| 语言 | 后端 | launch | attach | 需要准备 |
|---|---|---|---|---|
| Node.js | V8 Inspector（WebSocket 上的 CDP） | 支持 | 支持 | `node` 在 `PATH` |
| Python | debugpy（stdio 上的 DAP） | 支持 | 支持 | 装了 `debugpy` 的解释器 |
| Java / Kotlin / Scala / Groovy | JDWP | 支持 | 支持 | JDK；class 编译带 `-g` |
| C / C++ / Rust | lldb-dap（DAP） | 支持 | 不支持 | Debug 构建；缺失时降级 gdb MI |
| 仓颉（Cangjie） | lldb-dap（DAP） | 支持 | 不支持 | `cjc`，用 `-g` 编译 |
| Go | dlv dap | 支持 | 支持 | `dlv` + Go 工具链 |

后端选择、降级模式与各语言限制见 [docs/tool-contract.md](./docs/tool-contract.md)。

## 环境要求

- 构建需要 **Go 1.27.3+**（即 `go.mod` 声明的版本）。
- 要调试的语言需要对应的调试器（见上表）。
- 项目在 Windows 上开发并跑通端到端测试。Linux 与 macOS 走同一套 Go 代码与 DAP/CDP 客户端，但未逐项验证——遇到问题请带平台与工具链版本提 issue。

## 安装

```bash
git clone https://github.com/creator501/lang-debug-mcp.git
cd lang-debug-mcp
go build -o lang-debug-mcp ./cmd/lang-debug-mcp
./lang-debug-mcp -version
```

也可以在仓库目录内执行 `go install ./cmd/lang-debug-mcp`。

## 接入 MCP 客户端

服务端通过 **stdio** 讲 MCP，不需要参数。把它加进客户端的 MCP 配置（不同客户端字段名略有差异：`mcpServers` / `servers`）：

```json
{
  "mcpServers": {
    "lang-debug": {
      "command": "lang-debug-mcp",
      "args": []
    }
  }
}
```

二进制不在 `PATH` 时，`command` 写绝对路径。之后即可让 Agent 执行调试任务，或直接调用工具。

## 快速开始

以 Node.js 为例，其它语言流程相同。

1. **探测环境** —— 任何会话之前先做。

```json
{"language": "nodejs"}
```

`debug_env` 会返回可用的后端、版本，以及缺失项的安装建议。

2. **建立会话**，启动命令用程序**原本的**命令，不要带调试参数。

```json
{"action": "open", "language": "nodejs", "mode": "launch",
 "command": ["node", "app.js"], "cwd": "/repo"}
```

工具会注入 `--inspect-brk=127.0.0.1:<空闲端口>`，返回已停下的会话（`state: "stopped"`）与当前快照标识。

3. **设断点** —— 优先用条件或命中序号，避免热路径无过滤断点。

```json
{"action": "set", "session": "sess1",
 "breakpoints": [{"file": "app.js", "line": 5, "condition": "i > 0"}]}
```

4. **一次调用完成「跑过去 + 取数」**。

```json
{"action": "continue", "session": "sess1", "timeoutMs": 10000,
 "until": {"breakpoint": "bp1", "hits": 2},
 "onHit": {"inspect": ["locals.total", "locals.items[0..1]"], "depth": 2}}
```

响应里同时给出停止位置与所读的值。超时会返回 `{"running": true}`——再调一次 `continue` 即可，不要重建会话。

5. **继续深入、改值或收尾**。

```json
{"session": "sess1", "snapshot": "s2", "paths": ["locals", "locals.items[0..4]"], "depth": 2}
{"session": "sess1", "snapshot": "s2", "ops": [{"path": "locals.total", "value": "100"}]}
{"action": "close", "session": "sess1"}
```

改值会回传原值并使旧快照作废，请用返回的新快照标识继续。

## 工具一览

| 工具 | 作用 |
|---|---|
| `debug_env` | 列出受支持语言，探测后端可用性、版本、缺失项与安装建议 |
| `debug_session` | `open` / `list` / `status` / `close`；launch 注入调试参数，attach 复用调试端口 |
| `debug_breakpoint` | `set` / `update` / `remove` / `list`；支持条件、命中序号与不中断执行的 logpoint |
| `debug_run` | `continue`（可带 `until` 与 `onHit`）、`step`（into/over/out）、`pause`、`restart`、`terminate` |
| `debug_inspect` | 在同一快照内批量读值，合并遍历，受预算约束 |
| `debug_eval` | 在指定栈帧内求值（会执行代码，可能有副作用） |
| `debug_mutate` | 批量改值，回传原值，并说明某次写入为何无法完成 |
| `debug_output` | 目标 stdout/stderr 的尾部，含 logpoint 记录 |
| `debug_tracepoint` | 非交互采集：`add` / `events` / `stop` / `timeline`（与剖析结果对齐） |
| `debug_profile` | `sample`（中断采样）、`start`/`stop`（外部剖析器）、`import`（折叠栈 / `.cpuprofile`） |

`debug_inspect` 与 `onHit` 的路径语法：`[frame[n].]<作用域>.<成员>[索引][区间]`，`*` 表示全部成员，`items[0..19]` 表示区间。作用域名由后端给出（`locals`、`block`、`closure`、`statics`、`this` 等）。

## 给 Agent 用的技能包

[`skills/`](./skills/README.md) 里每种语言一份流程（`nodejs-debug`、`python-debug`、`java-debug`、`go-debug`、`cangjie-debug` 等），另有一张「现象 → skill」选择表。它们由同一模板生成，共享流程（证据三元组、一次往返一次停止、流程纪律）逐字一致，只有语言特有的坑不同。

```bash
go run ./cmd/skillgen -root .          # 重新生成
go run ./cmd/skillgen -root . -check   # 校验产物无漂移
```

把需要的目录复制到客户端的 skills 目录（例如项目级 `.codebuddy/skills/`）。

## 环境变量

| 变量 | 作用 |
|---|---|
| `LANGDEBUG_PYTHON` | 承载 debugpy 的 Python 解释器 |
| `LANGDEBUG_DLV` | `dlv` 可执行文件路径 |
| `LANGDEBUG_LLDB_DAP` | `lldb-dap` 可执行文件路径 |
| `LANGDEBUG_CJC` | `cjc` 可执行文件路径 |
| `MSYS2_ROOT` | lldb-dap 位于非默认安装位置时的 MSYS2 根目录 |

## 文档

- [docs/tool-contract.md](./docs/tool-contract.md) —— 工具参数、返回结构、各后端能力边界
- [docs/error-codes.md](./docs/error-codes.md) —— 错误码与修复建议
- [docs/tracing-profiling.md](./docs/tracing-profiling.md) —— 采集点、采样与时间线对齐
- [skills/README.md](./skills/README.md) —— 技能包与「现象选 skill」表

## 目录结构

```
cmd/lang-debug-mcp   MCP 服务端入口（stdio）
cmd/skillgen         技能生成器与一致性校验
internal/backend     各语言后端：nodejs、python、jvm、native、cangjie、golang、dap
internal/tool        10 个 MCP 工具，会话状态、快照、采集、剖析
internal/model       快照/值模型、状态机、错误码
internal/skill       技能模板、插槽与生成
skills/              生成好的各语言 SKILL.md
docs/                工具契约、错误码、采集与剖析
```

## 限制与安全

- native 后端（C/C++/Rust/仓颉）不支持 `attach`，只能由工具 launch 托管。
- Go 的 `attach` 连的是已经在运行的 `dlv dap` 适配器端口；把 Delve 附着到某个进程要用 Delve 自己的 CLI。
- JDWP 不支持表达式求值与条件断点，JVM 会话的 `eval` 能力为不支持。
- 降级到 gdb MI（没有 lldb-dap）时不能改值，命中条件只支持 `>=N`。
- 优化构建下变量显示 `<optimized out>` 且不可恢复，请用 Debug 构建。
- 读值有预算（深度、集合大小、字符串长度），被裁剪的项会明确说明。
- **本服务会启动进程、注入调试参数，并能修改目标内存。** 只在你拥有且受控的环境中使用，不要指向生产进程。

## 开发

```bash
go test ./...                          # 单元 + 端到端；缺对应语言工具链时相关用例会跳过
go run ./cmd/skillgen -root . -check   # 校验 skill 产物与模板没有漂移
```

## 许可

仓库目前尚未声明开源协议。发布前请补充 `LICENSE`（如 MIT、Apache-2.0），并同步更新本节。
