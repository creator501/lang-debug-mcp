# 错误码契约

本文件是 MCP 工具失败返回的对外契约。任何新增后端适配都必须复用既有错误码，
不得为同一类失败发明新码，否则调用方无法跨语言做分支处理。

## 返回结构

失败时工具返回 `isError: true`，文本内容为以下 JSON：

```json
{
  "code": "ENV_ADAPTER_MISSING",
  "message": "缺少 debugpy",
  "fix": "pip install debugpy",
  "context": { "language": "python", "backend": "debugpy" }
}
```

- `code`：跨后端稳定的错误码，调用方按码分支。
- `message`：人类可读原因，可以包含细节，不保证稳定。
- `fix`：可执行的下一步。**缺失此字段视为契约缺陷**：调用方拿到错误却不知道该做什么。
- `context`：定位信息（会话、快照、语言、后端），用于继续排查。

## 错误码分组

| 分组 | 含义 | 调用方应采取的动作 |
|---|---|---|
| `ENV_*` | 运行环境或适配器缺失、权限不足、调试信息不足 | 告知用户安装或重新编译，通常附带可直接执行的命令 |
| `STATE_*` | 会话或快照状态不匹配 | 调整调用时机（如先停止再取值），或重建会话 |
| `CAP_*` | 当前后端不具备该能力 | 换能力更强的后端，或放弃该动作 |
| `INPUT_*` | 参数或路径非法 | 修正入参后重试，不要重试相同入参 |
| `TARGET_*` | 目标进程侧问题（退出、崩溃、只读、已优化） | 重启目标或降低优化级别 |
| `INTERNAL_*` | 适配器协议异常 | 视为工具缺陷，收集日志上报 |

## 明细

| 错误码 | 触发条件 | `fix` 必须包含 |
|---|---|---|
| `ENV_ADAPTER_MISSING` | 语言适配器不存在（如未装 debugpy、无 java-debug） | 安装命令 |
| `ENV_TOOL_MISSING` | 外部工具不存在（如缺少 async-profiler、py-spy） | 安装命令或替代方案 |
| `ENV_PERMISSION` | 附着目标权限不足 | 所需权限说明 |
| `ENV_DEBUG_INFO` | 调试信息缺失或不足，按三级校验判定 | 重新编译的具体参数 |
| `STATE_INVALID_TRANSITION` | 会话状态机拒绝了该转换 | 当前状态与允许的目标状态 |
| `STATE_NOT_STOPPED` | 在运行态请求只有停止态才能完成的动作 | 达成停止态的方法（断点或中断） |
| `STATE_NOT_RUNNING` | 在停止态请求只有运行态才能完成的动作 | 恢复运行的调用方式 |
| `STATE_STALE_SNAPSHOT` | 引用了已失效快照及其引用标识 | 重新取值以获得新快照 |
| `STATE_DEAD_SESSION` | 会话后端已崩溃或目标已退出 | 重建会话 |
| `STATE_UNKNOWN_SESSION` | 会话标识不存在 | 当前可用会话列表 |
| `CAP_UNSUPPORTED` | 后端不支持该动作 | 后端名与已知能力限制 |
| `INPUT_INVALID` | 参数、路径语法非法 | 正确的写法示例 |
| `INPUT_NOT_FOUND` | 路径、引用、栈帧不在当前快照内 | 可用取值范围 |
| `TARGET_EXITED` | 目标进程已退出 | 重启方式 |
| `TARGET_CRASHED` | 目标进程异常终止 | 退出码或信号，以及重启方式 |
| `TARGET_OPTIMIZED_OUT` | 变量因优化不存在于调试信息 | 降低优化级别重编 |
| `TARGET_READ_ONLY` | 目标位于只读内存区 | 说明不可写原因 |
| `TARGET_IMMUTABLE` | 编译期不可变（`final` / `const`） | 说明不可写原因 |
| `INTERNAL_PROTOCOL` | 适配器返回了无法解析的内容 | 要求收集日志 |

## 后端失败类别到错误码的映射

适配层只上报“发生了什么”（`FailureKind`），由统一映射表决定错误码：

| 失败类别 | 错误码 |
|---|---|
| `adapter_missing` | `ENV_ADAPTER_MISSING` |
| `tool_missing` | `ENV_TOOL_MISSING` |
| `permission` | `ENV_PERMISSION` |
| `not_stopped` | `STATE_NOT_STOPPED` |
| `not_running` | `STATE_NOT_RUNNING` |
| `unsupported` | `CAP_UNSUPPORTED` |
| `target_exited` | `TARGET_EXITED` |
| `target_crashed` | `TARGET_CRASHED` |
| `protocol` | `INTERNAL_PROTOCOL` |

规则：四个后端（JVM / native / Python / Node）对同类失败必须上报相同的
`FailureKind`。若某后端无法区分，应上报保守类别，不得新增私有码。

## 不受预算裁剪的元信息

以下内容无论返回体预算多紧都必须完整返回：错误码与 `fix`、停止位置与栈帧总
数、集合总量与被裁剪区间、快照失效原因。裁剪只作用于变量值与集合元素。
