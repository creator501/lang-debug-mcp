## description
调试 Node.js 程序（V8、Promise 异步栈、TypeScript 源码映射）时使用：`Cannot read properties of undefined`、异步回调里断点看不到调用方、变量显示为纯对象而实际是 Map/Proxy。npm 安装失败、打包配置错误不适用本 skill。

## triggers
- `TypeError: Cannot read properties of undefined (reading 'x')`，需要确认该行接收者的实际结构与来源。
- Promise/async 回调里命中断点，栈里没有业务调用方（只有 `processTicksAndRejections`）。
- 变量在控制台打印成一个普通对象，但业务按 `Map`/`Set`/`Proxy` 使用。

## preconditions
- Node ≥16（CDP 调试域齐全），launch 时工具注入 `--inspect-brk` 并分配端口。
- TypeScript/ESM 需开启 sourcemap（`--enable-source-maps`），否则断点行号落在编译产物上。
- attach 需要目标以 `--inspect` 或 `--inspect-brk` 启动，且端口可达。

## session
- 启动命令 `node app.js` 或 `node --enable-source-maps dist/app.js`；入口停止发生在第一行用户代码之前。
- 断点可以下在源码路径上；ESM 与 CJS 的源文件解析不同，会话工作目录要给对。
- 修改代码后需要重启会话（restart），热更新不在支持范围。

## build
- 先构建：`npm run build`、`pnpm build`、`yarn build`；TypeScript 常见产物在 `dist/`。
- 启动命令只能是 node：`["node","dist/app.js"]`；`npm`、`npx`、`pnpm`、`yarn` 会被拒绝（Node 不允许在 `NODE_OPTIONS` 里设 `--inspect`）。
- 开 sourcemap：构建时生成 `.map`，运行时 `node --enable-source-maps dist/app.js`，否则断点落在编译产物行号上。
- monorepo/workspace：`cwd` 指到具体包目录（`node packages/api/dist/index.js`）；不要用 `node_modules/.bin/*` 的包装脚本。
- TS 直跑仍是 node 启动：`node -r ts-node/register src/app.ts` 或 `node --import tsx src/app.ts`，工具可以注入调试参数。
- attach：`node --inspect=127.0.0.1:9229 dist/app.js` 后连 9229；`--inspect-brk` 才会在第一行暂停。

## read-traps
- `Map`/`Set` 与普通对象在调试器里都是对象：`Map` 要按 entries 展开，直接看自有属性会得到空。
- `Proxy` 与 getter 在展开时才求值，可能带副作用；只读关心字段，不要随手展开整个对象。
- `undefined` 与 `null` 是两种不同状态，`==` 宽松比较会掩盖差异。
- Promise 的内部状态（`[[PromiseState]]`）可读但不可改；`await` 之后的局部变量在闭包里。
- 大整数（`BigInt`）与 `Number` 精度不同，超过 2^53 的值不能按 Number 读。
- 模块单例的字段全局共享，改一处会影响同进程后续所有请求。

## mutate-bounds
- `Object.freeze` 的对象写入会静默失败或被拒绝；先确认是否冻结。
- `const` 绑定不可重绑，但对象属性可写；改属性会立即影响持有者。
- getter-only 属性（无 setter）不能直接写，需要改底层字段或换实现。
- 类实例字段与原型上的字段是两层，写实例字段只遮蔽原型值。

## antipatterns
- **现象**：异步回调里断点，栈上只有调度器 → **误判**：以为调用链丢了 → **正确做法**：开启 async stack trace（`--async-stack-traces`，Node 16+ 默认）或在任务入口断点。
- **现象**：`console.log` 打印出 `{}` 而业务说有数据 → **误判**：以为对象为空 → **正确做法**：对象可能是 `Map`/`Symbol` 键/不可枚举属性，用 inspect 展开看。
- **现象**：断点停在编译产物行上 → **误判**：以为源码与运行代码不一致 → **正确做法**：确认 sourcemap 是否开启、工作目录是否指向源码根。

## profiling
- `node --cpu-prof app.js` 生成 `.cpuprofile`，可直接在 Chrome DevTools 打开；`--prof` 配合 `--prof-process` 适合长期运行进程。
- 异步耗时用 `--trace-events-enabled` 或 clinic.js 的 bubbleprof，单看 CPU 采样会把等待算成空闲。
- 没有剖析工具时用命中序号断点统计调用次数，并在结论中标注采样性质。
