---
name: nodejs-debug
description: 调试 Node.js 程序（V8、Promise 异步栈、TypeScript 源码映射）时使用：`Cannot read properties of undefined`、异步回调里断点看不到调用方、变量显示为纯对象而实际是 Map/Proxy。npm 安装失败、打包配置错误不适用本 skill。
---

# Node.js 调试技能

本 skill 由同一份模板加 Node.js 插槽展开，可独立加载：建立会话、检视、改值、追踪与剖析的指引都在本文件内，不需要再读其他 skill。

## 1. 触发信号

- `TypeError: Cannot read properties of undefined (reading 'x')`，需要确认该行接收者的实际结构与来源。
- Promise/async 回调里命中断点，栈里没有业务调用方（只有 `processTicksAndRejections`）。
- 变量在控制台打印成一个普通对象，但业务按 `Map`/`Set`/`Proxy` 使用。

## 2. 前置条件

- Node ≥16（CDP 调试域齐全），launch 时工具注入 `--inspect-brk` 并分配端口。
- TypeScript/ESM 需开启 sourcemap（`--enable-source-maps`），否则断点行号落在编译产物上。
- attach 需要目标以 `--inspect` 或 `--inspect-brk` 启动，且端口可达。

## 3. 建立会话

1. 先 `debug_env` 探测：确认调试器或适配器可用、版本可识别，并记下缺失项与安装建议。
2. `debug_session` 建立会话：launch 由工具注入调试参数，attach 复用已有调试端口；记下会话标识。
3. `debug_breakpoint` 设置断点，优先使用条件或命中序号，断点数量保持少量。
4. `debug_run` 的 continue/until 做一次性闭环：命中后同一响应里拿到位置与变量值。
5. 需要更多变量时 `debug_inspect` 批量取；需要改值时 `debug_mutate`，旧快照随即作废。
6. 结束时 `debug_session` 关闭会话。

- 启动命令 `node app.js` 或 `node --enable-source-maps dist/app.js`；入口停止发生在第一行用户代码之前。
- 断点可以下在源码路径上；ESM 与 CJS 的源文件解析不同，会话工作目录要给对。
- 修改代码后需要重启会话（restart），热更新不在支持范围。

## 4. 工程化项目

本工具不代跑构建，也不解析 `pom.xml`、`build.gradle`、`CMakeLists.txt`、`Makefile`、`build.sbt` 或 `package.json`：构建由你或 CI 先完成，会话只消费产物。

1. 用工程自己的构建命令产出带调试信息的产物：Debug 配置、保留符号、不要 strip、不要开高优化。
2. 找到该构建工具实际写入的产物路径（class 目录、jar、可执行文件或构建后的 JS），把它作为 `debug_session` 的启动命令，而不是构建工具本身。
3. 多模块/多包工程里，`cwd` 要与产物和源码的基准目录一致；命令中的相对路径都按 `cwd` 解析。
4. 需要在构建工具内部调试（跑测试、跑 `run` 任务）时，让构建工具以等待调试器的方式启动，再用 attach 连它的端口。
5. 构建描述文件里读不到的调试信息（classpath、sourcemap、`-g`）要显式确认，缺失时先补构建配置再排查代码。

- 先构建：`npm run build`、`pnpm build`、`yarn build`；TypeScript 常见产物在 `dist/`。
- 启动命令只能是 node：`["node","dist/app.js"]`；`npm`、`npx`、`pnpm`、`yarn` 会被拒绝（Node 不允许在 `NODE_OPTIONS` 里设 `--inspect`）。
- 开 sourcemap：构建时生成 `.map`，运行时 `node --enable-source-maps dist/app.js`，否则断点落在编译产物行号上。
- monorepo/workspace：`cwd` 指到具体包目录（`node packages/api/dist/index.js`）；不要用 `node_modules/.bin/*` 的包装脚本。
- TS 直跑仍是 node 启动：`node -r ts-node/register src/app.ts` 或 `node --import tsx src/app.ts`，工具可以注入调试参数。
- attach：`node --inspect=127.0.0.1:9229 dist/app.js` 后连 9229；`--inspect-brk` 才会在第一行暂停。

## 5. 读值陷阱

- `Map`/`Set` 与普通对象在调试器里都是对象：`Map` 要按 entries 展开，直接看自有属性会得到空。
- `Proxy` 与 getter 在展开时才求值，可能带副作用；只读关心字段，不要随手展开整个对象。
- `undefined` 与 `null` 是两种不同状态，`==` 宽松比较会掩盖差异。
- Promise 的内部状态（`[[PromiseState]]`）可读但不可改；`await` 之后的局部变量在闭包里。
- 大整数（`BigInt`）与 `Number` 精度不同，超过 2^53 的值不能按 Number 读。
- 模块单例的字段全局共享，改一处会影响同进程后续所有请求。

## 6. 改值边界

- `Object.freeze` 的对象写入会静默失败或被拒绝；先确认是否冻结。
- `const` 绑定不可重绑，但对象属性可写；改属性会立即影响持有者。
- getter-only 属性（无 setter）不能直接写，需要改底层字段或换实现。
- 类实例字段与原型上的字段是两层，写实例字段只遮蔽原型值。

## 7. 高频反模式

每条反模式按固定格式记录：**现象 → 误判 → 正确做法**。

- **现象**：异步回调里断点，栈上只有调度器 → **误判**：以为调用链丢了 → **正确做法**：开启 async stack trace（`--async-stack-traces`，Node 16+ 默认）或在任务入口断点。
- **现象**：`console.log` 打印出 `{}` 而业务说有数据 → **误判**：以为对象为空 → **正确做法**：对象可能是 `Map`/`Symbol` 键/不可枚举属性，用 inspect 展开看。
- **现象**：断点停在编译产物行上 → **误判**：以为源码与运行代码不一致 → **正确做法**：确认 sourcemap 是否开启、工作目录是否指向源码根。

## 8. 性能分析

- `node --cpu-prof app.js` 生成 `.cpuprofile`，可直接在 Chrome DevTools 打开；`--prof` 配合 `--prof-process` 适合长期运行进程。
- 异步耗时用 `--trace-events-enabled` 或 clinic.js 的 bubbleprof，单看 CPU 采样会把等待算成空闲。
- 没有剖析工具时用命中序号断点统计调用次数，并在结论中标注采样性质。

## 9. 流程纪律

1. 任何调试动作之前先探测环境；调试器缺失时先返回安装或开启指引，不要硬建会话。
2. 默认少量精确断点（一般 1–3 个），用条件或命中序号约束；不得以单步执行替代采集。
3. 在可能高频执行的位置设置采集点前，必须先设定速率上限或采样率。
4. 断点选「能观测到问题的最早位置」，不要一路下到用户报告的行号再回看。
5. 同一轮排查复用同一份快照标识；快照失效后重新停止再取，不要混用改前与改后的值。

## 10. 结论输出规范

给出结论时必须提供证据三元组：

- 位置：文件与行号，例如 `src/handler.py:42`。
- 观测值：该位置实际取到的值及其路径，例如 `locals.retries = 3`。
- 复现路径：从启动到该观测的最小步骤（会话模式、断点条件、运行动作）。

不得仅凭代码阅读下断言。尚未取得运行态证据时，写成「待验证假设」，并给出下一处要停下的位置。

## 11. 收尾

1. 调试结束关闭会话，释放后端进程与监听端口，不留下后台调试器。
2. 若本轮改过值，结论中标注目标进程内存已被修改，后续行为可能偏离原始逻辑，需要重启才能恢复。
3. 保留用过的断点与快照标识，便于他人复现。
