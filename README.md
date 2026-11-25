# lang-debug-mcp

**A debugging MCP server for AI agents — 11 languages behind one tool interface.**

[中文说明](./README.zh-CN.md)

Most coding agents can only read source and guess. `lang-debug-mcp` gives them a real debugger: set breakpoints, run to a location, read actual values, step, evaluate, mutate, collect execution traces and profile — then report **position, observed value, and reproduction path** instead of speculation.

It speaks MCP over stdio and drives the native debugger of each language family (V8 Inspector, debugpy, JDWP, lldb-dap / gdb MI, dlv dap). Nothing about your code is uploaded anywhere; the target process runs on your machine.

## Features

- **10 tools** covering the whole loop: probe environment → open session → breakpoints → run/step → inspect → mutate → output → tracepoints → profiling.
- **One round trip per stop**: `continue` accepts `until` (breakpoint + hit count) and `onHit` (paths to read right after the hit), so "run there and read these values" is a single call.
- **Honest capability reporting**: every session returns a `capability` block up front (`read` / `write` / `eval` / limits), so an agent knows before acting whether an action will end in `CAP_UNSUPPORTED`.
- **Snapshot semantics**: a snapshot id is valid while the target is stopped and is invalidated by mutation or resume — modified and unmodified values can never be mixed silently.
- **Budgeted reads**: deep structures are clipped with an explicit `clipped` note telling you what was left out and how big it is.
- **Non-interactive tracepoints**: hit, record `{expression}` values, resume immediately; aggregate and detail views report dropped events instead of hiding them.
- **Structured errors**: `ENV_*`, `INPUT_*`, `STATE_*`, `TARGET_*`, `CAP_UNSUPPORTED` with concrete fix hints (see [docs/error-codes.md](./docs/error-codes.md)).
- **Bundled skills**: per-language debugging playbooks in [`skills/`](./skills/README.md), generated from one template so their shared rules stay identical.

## Supported languages

| Language | Backend | Launch | Attach | You need |
|---|---|---|---|---|
| Node.js | V8 Inspector (CDP over WebSocket) | Yes | Yes | `node` on `PATH` |
| Python | debugpy (DAP over stdio) | Yes | Yes | interpreter with `debugpy` installed |
| Java / Kotlin / Scala / Groovy | JDWP | Yes | Yes | JDK; classes compiled with debug info (`-g`) |
| C / C++ / Rust | lldb-dap (DAP) | Yes | No | debug build; falls back to gdb MI |
| Cangjie | lldb-dap (DAP) | Yes | No | `cjc` (compiled with `-g`) |
| Go | dlv dap | Yes | Yes | `dlv` + Go toolchain |

Backend selection, degraded modes and per-language limits are described in [docs/tool-contract.md](./docs/tool-contract.md).

## Requirements

- **Go 1.27.3+** to build (the version declared in `go.mod`).
- A debugger for the language you want to debug (see the table above).
- The project is developed and end-to-end tested on Windows. Linux and macOS run the same Go code and DAP/CDP clients but have not been verified case by case — please open an issue with your platform and toolchain versions.

## Install

```bash
git clone https://github.com/creator501/lang-debug-mcp.git
cd lang-debug-mcp
go build -o lang-debug-mcp ./cmd/lang-debug-mcp
./lang-debug-mcp -version
```

`go install ./cmd/lang-debug-mcp` also works from the checkout.

## Connect your MCP client

The server speaks MCP over **stdio** and takes no arguments. Add it to your client's MCP configuration (field names differ slightly per client: `mcpServers` / `servers`):

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

If the binary is not on `PATH`, use its absolute path in `command`. Then ask your agent for a debug task, or call the tools directly.

## Quick start

A Node.js walkthrough; other languages keep the same shape.

1. **Probe the environment** — do this before any session.

```json
{"language": "nodejs"}
```

`debug_env` reports which backend is available, its version, and install hints for what is missing.

2. **Open a session** with the program's *own* launch command — no debug flags.

```json
{"action": "open", "language": "nodejs", "mode": "launch",
 "command": ["node", "app.js"], "cwd": "/repo"}
```

The tool injects `--inspect-brk=127.0.0.1:<free port>` and returns a stopped session (`state: "stopped"`) plus the current snapshot id.

3. **Set a breakpoint** — prefer a condition or hit count over a hot line.

```json
{"action": "set", "session": "sess1",
 "breakpoints": [{"file": "app.js", "line": 5, "condition": "i > 0"}]}
```

4. **Run and read in one call**.

```json
{"action": "continue", "session": "sess1", "timeoutMs": 10000,
 "until": {"breakpoint": "bp1", "hits": 2},
 "onHit": {"inspect": ["locals.total", "locals.items[0..1]"], "depth": 2}}
```

The response carries the stop position and the requested values. A timeout returns `{"running": true}` — call `continue` again instead of rebuilding the session.

5. **Dig deeper, change a value, or close**.

```json
{"session": "sess1", "snapshot": "s2", "paths": ["locals", "locals.items[0..4]"], "depth": 2}
{"session": "sess1", "snapshot": "s2", "ops": [{"path": "locals.total", "value": "100"}]}
{"action": "close", "session": "sess1"}
```

Mutation returns the previous values and invalidates the old snapshot; use the new snapshot id.

## Tools

| Tool | What it does |
|---|---|
| `debug_env` | List supported languages and probe backend availability, version, missing pieces and install hints |
| `debug_session` | `open` / `list` / `status` / `close`; launch injects debug flags, attach reuses a debug port |
| `debug_breakpoint` | `set` / `update` / `remove` / `list`; conditions, hit counts, and logpoints that never stop the target |
| `debug_run` | `continue` (with `until` + `onHit`), `step` (into/over/out), `pause`, `restart`, `terminate` |
| `debug_inspect` | Batch-read variable paths from one snapshot, merged traversal, budgeted |
| `debug_eval` | Evaluate an expression in a frame (may execute code and change state) |
| `debug_mutate` | Batch-write values, returns old values, reports why a write is impossible |
| `debug_output` | Tail of the target's stdout/stderr, including logpoint records |
| `debug_tracepoint` | Non-interactive collection: `add` / `events` / `stop` / `timeline` (align with a profile) |
| `debug_profile` | `sample` (interrupt sampling), `start`/`stop` (external profilers), `import` (folded stacks / `.cpuprofile`) |

Path syntax for `debug_inspect` / `onHit`: `[frame[n].]<scope>.<member>[index][range]`, with `*` for all members and `items[0..19]` for ranges. Scope names are backend-provided (`locals`, `block`, `closure`, `statics`, `this`, …).

## Skills for agents

[`skills/`](./skills/README.md) contains one playbook per language (`nodejs-debug`, `python-debug`, `java-debug`, `go-debug`, `cangjie-debug`, …), plus a symptom → skill selection table. They are generated from a single template, so the shared workflow (evidence triple, one round trip per stop, discipline) is identical everywhere; only language-specific traps differ.

```bash
go run ./cmd/skillgen -root .          # regenerate
go run ./cmd/skillgen -root . -check   # verify no drift
```

Copy the directories you need into your client's skills folder (e.g. a project-level `.codebuddy/skills/`).

## Environment variables

| Variable | Purpose |
|---|---|
| `LANGDEBUG_PYTHON` | Interpreter used to host debugpy |
| `LANGDEBUG_DLV` | Absolute path to `dlv` |
| `LANGDEBUG_LLDB_DAP` | Absolute path to `lldb-dap` |
| `LANGDEBUG_CJC` | Absolute path to `cjc` |
| `MSYS2_ROOT` | MSYS2 root when lldb-dap lives outside the default location |

## Documentation

- [docs/tool-contract.md](./docs/tool-contract.md) — tool parameters, return shapes, per-backend capabilities
- [docs/error-codes.md](./docs/error-codes.md) — error codes and fix hints
- [docs/tracing-profiling.md](./docs/tracing-profiling.md) — tracepoints, sampling and timeline alignment
- [skills/README.md](./skills/README.md) — skill package and symptom selection table

## Project layout

```
cmd/lang-debug-mcp   MCP server entry point (stdio)
cmd/skillgen         skill generator and consistency checker
internal/backend     language backends: nodejs, python, jvm, native, cangjie, golang, dap
internal/tool        the 10 MCP tools, session state, snapshots, tracing, profiling
internal/model       snapshot/value model, state machine, error codes
internal/skill       skill template, slots and generation
skills/              generated per-language SKILL.md files
docs/                tool contract, error codes, tracing/profiling
```

## Limits and safety

- `attach` is not implemented for the native backend (C/C++/Rust/Cangjie); those targets must be launched by the tool.
- For Go, `attach` connects to an already running `dlv dap` adapter port; attaching Delve to a process is done with Delve's own CLI.
- JDWP does not support expression evaluation or conditional breakpoints — JVM sessions report `eval` as unsupported.
- With the gdb MI fallback (no lldb-dap), writes are disabled and only `>=N` hit conditions are supported.
- Optimized builds make values `<optimized out>` and unrecoverable; use a debug build.
- Reads are budgeted (depth, collection size, string length) and clipped items are reported explicitly.
- **This server starts processes, injects debugger flags and can modify target memory.** Use it only on code you own and in environments you control; do not point it at production processes.

## Development

```bash
go test ./...                          # unit + end-to-end; suites skip when a language toolchain is missing
go run ./cmd/skillgen -root . -check   # skills must not drift from the template
```

## License

Licensed under the [Apache License, Version 2.0](./LICENSE).
