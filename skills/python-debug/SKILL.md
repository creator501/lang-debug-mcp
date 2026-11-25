---
name: python-debug
description: 调试 Python 程序（asyncio、生成器、C 扩展、闭包）时使用：KeyError/AttributeError 落在业务行、协程里断点跳转看不懂、变量显示 `<optimized>` 或读不到。pip 安装失败、语法错误等非运行态问题不适用本 skill。
---

# Python 调试技能

本 skill 由同一份模板加 Python 插槽展开，可独立加载：建立会话、检视、改值、追踪与剖析的指引都在本文件内，不需要再读其他 skill。

## 1. 触发信号

- 堆栈里出现 `KeyError`、`AttributeError`、`TypeError`，需要确认出错行上 dict/对象/slots 的实际内容。
- `async def` 里命中断点后单步，线程在多个协程间跳转，看不到预期的一行行执行。
- 生成器停在 `yield`，或用 `-O` 运行时 `assert` 与 `__debug__` 分支出错。

## 2. 前置条件

- 目标环境已安装 debugpy（`python -m debugpy --version`），工具在 launch 模式下自己引导，attach 需要目标先以 `--listen` 启动。
- 目标 Python 与调试器版本兼容（≥3.8）；venv/conda 环境要给出解释器绝对路径，避免装到系统 Python。
- **不要用 `-O`/`-OO` 启动**：`-O` 会删掉 `assert` 与 `__debug__` 分支，断点与取值和源码对不上；PYTHONOPTIMIZE 环境变量同理。
- 源码必须可读且路径可解析；打包进 zip/egg 的模块没有源码行号，只能看到模块名。

## 3. 建立会话

1. 先 `debug_env` 探测：确认调试器或适配器可用、版本可识别，并记下缺失项与安装建议。
2. `debug_session` 建立会话：launch 由工具注入调试参数，attach 复用已有调试端口；记下会话标识。
3. `debug_breakpoint` 设置断点，优先使用条件或命中序号，断点数量保持少量。
4. `debug_run` 的 continue/until 做一次性闭环：命中后同一响应里拿到位置与变量值。
5. 需要更多变量时 `debug_inspect` 批量取；需要改值时 `debug_mutate`，旧快照随即作废。
6. 结束时 `debug_session` 关闭会话。

- launch 时给 `python` 解释器与前缀参数（`-m`、`-X` 等），工具把 debugpy 引导参数插在解释器之后、模块之前。
- attach 用 `python -m debugpy --listen 127.0.0.1:<端口> --wait-for-client app.py`，再按端口 attach。
- 断点按文件基名匹配，工作目录给对才能还原相对路径；一处源码被多个进程加载时优先进程内那一个。

## 4. 工程化项目

本工具不代跑构建，也不解析 `pom.xml`、`build.gradle`、`CMakeLists.txt`、`Makefile`、`build.sbt` 或 `package.json`：构建由你或 CI 先完成，会话只消费产物。

1. 用工程自己的构建命令产出带调试信息的产物：Debug 配置、保留符号、不要 strip、不要开高优化。
2. 找到该构建工具实际写入的产物路径（class 目录、jar、可执行文件或构建后的 JS），把它作为 `debug_session` 的启动命令，而不是构建工具本身。
3. 多模块/多包工程里，`cwd` 要与产物和源码的基准目录一致；命令中的相对路径都按 `cwd` 解析。
4. 需要在构建工具内部调试（跑测试、跑 `run` 任务）时，让构建工具以等待调试器的方式启动，再用 attach 连它的端口。
5. 构建描述文件里读不到的调试信息（classpath、sourcemap、`-g`）要显式确认，缺失时先补构建配置再排查代码。

- 先准备环境：`python -m venv .venv` 后 `.venv/Scripts/python.exe -m pip install -e .`（POSIX 是 `.venv/bin/python`）；Poetry 用 `poetry install`，再用 `poetry env info -p` 找到解释器。
- 启动命令必须用该解释器：`[".venv/Scripts/python.exe","main.py"]` 或 `[…,"-m","pkg.module"]`；解释器没装 debugpy 会报 `ENV_ADAPTER_MISSING`。
- `pytest`、`poetry run`、`uv run`、`pipenv run` 都不是解释器，会被拒绝；改写成 `python -m pytest tests/test_x.py::test_y` 或 `python -m pkg`。
- 打包后的 console_scripts 入口要还原成真实模块，用 `-m` 或脚本路径启动，不要用安装生成的 `.exe` 包装器。
- 不要用 `-O`/`PYTHONOPTIMIZE`：断言被删、部分行不可断，调试结论会失真。
- zipapp 或单文件打包后运行路径与源码路径不一致，断点先确认能命中再排查。

## 5. 读值陷阱

- `dict` 保插入顺序：`dict.keys()` 的顺序就是插入顺序，不要按哈希顺序推理。
- 闭包变量存在 cell 里（`__closure__`），`locals` 里看到的是 cell 内容而非原变量；多层闭包要逐层展开。
- 生成器停在 `yield`，此时函数栈帧是暂停态：`locals` 是暂停点的值，`gi_frame` 与业务栈帧不是同一个。
- 协程（`async def`）的栈帧只在调度时存在；命中时的调用栈经过事件循环，因果链要靠 `await` 链与任务名还原。
- C 扩展帧（`builtins`、numpy 内部）没有 Python 变量，标记为不可进入，不要尝试展开。
- `__slots__` 的实例没有 `__dict__`，属性必须按槽位读取；`property` 会真正执行 getter，可能带副作用。
- 类属性与实例属性同名时，实例上读到的可能是类属性；要区分 `self.__class__` 上的值。

## 6. 改值边界

- 不可变类型（`str`、`int`、`tuple`）改值只能替换绑定，替换后原引用持有者看不到变化。
- 改 `__dict__` 里的项会影响属性查找顺序（数据描述符优先），property/slot 仍按描述符走。
- `list`/`dict` 按索引或键改元素是就地生效；改 `dict` 键会破坏正在进行的迭代（`RuntimeError: dictionary changed size`）。
- 生成器/协程的内部帧字段（`gi_frame`、`cr_await`）改写会破坏调度状态，除定位问题外不要动。

## 7. 高频反模式

每条反模式按固定格式记录：**现象 → 误判 → 正确做法**。

- **现象**：用 `print` 二分定位，输出顺序与代码顺序不符 → **误判**：以为分支没走到 → **正确做法**：在疑似分支下条件断点，用命中时栈帧确认。
- **现象**：`asyncio` 里单步，跳转在多个协程间来回 → **误判**：以为代码被乱序执行 → **正确做法**：用断点加条件（任务名/请求 id）停在目标协程，结论里写清事件循环调度。
- **现象**：变量读不到或显示异常 → **误判**：以为代码没执行 → **正确做法**：确认是否 `-O` 运行、是否为 C 扩展帧、是否被内联到 `assert` 分支。

## 8. 性能分析

- py-spy 是最省事的选择（`py-spy record -o prof.svg --pid <pid>`，无需改代码）；asyncio 用 `--idle` 观察等待。
- `cProfile` 适合单进程、可改代码的场景，注意它对递归与 C 扩展的统计偏差。
- 没有剖析器时用带命中序号的断点统计调用次数，并在结论中标注这是采样。

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
