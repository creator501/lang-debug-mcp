## description
调试 Python 程序（asyncio、生成器、C 扩展、闭包）时使用：KeyError/AttributeError 落在业务行、协程里断点跳转看不懂、变量显示 `<optimized>` 或读不到。pip 安装失败、语法错误等非运行态问题不适用本 skill。

## triggers
- 堆栈里出现 `KeyError`、`AttributeError`、`TypeError`，需要确认出错行上 dict/对象/slots 的实际内容。
- `async def` 里命中断点后单步，线程在多个协程间跳转，看不到预期的一行行执行。
- 生成器停在 `yield`，或用 `-O` 运行时 `assert` 与 `__debug__` 分支出错。

## preconditions
- 目标环境已安装 debugpy（`python -m debugpy --version`），工具在 launch 模式下自己引导，attach 需要目标先以 `--listen` 启动。
- 目标 Python 与调试器版本兼容（≥3.8）；venv/conda 环境要给出解释器绝对路径，避免装到系统 Python。
- **不要用 `-O`/`-OO` 启动**：`-O` 会删掉 `assert` 与 `__debug__` 分支，断点与取值和源码对不上；PYTHONOPTIMIZE 环境变量同理。
- 源码必须可读且路径可解析；打包进 zip/egg 的模块没有源码行号，只能看到模块名。

## session
- launch 时给 `python` 解释器与前缀参数（`-m`、`-X` 等），工具把 debugpy 引导参数插在解释器之后、模块之前。
- attach 用 `python -m debugpy --listen 127.0.0.1:<端口> --wait-for-client app.py`，再按端口 attach。
- 断点按文件基名匹配，工作目录给对才能还原相对路径；一处源码被多个进程加载时优先进程内那一个。

## build
- 先准备环境：`python -m venv .venv` 后 `.venv/Scripts/python.exe -m pip install -e .`（POSIX 是 `.venv/bin/python`）；Poetry 用 `poetry install`，再用 `poetry env info -p` 找到解释器。
- 启动命令必须用该解释器：`[".venv/Scripts/python.exe","main.py"]` 或 `[…,"-m","pkg.module"]`；解释器没装 debugpy 会报 `ENV_ADAPTER_MISSING`。
- `pytest`、`poetry run`、`uv run`、`pipenv run` 都不是解释器，会被拒绝；改写成 `python -m pytest tests/test_x.py::test_y` 或 `python -m pkg`。
- 打包后的 console_scripts 入口要还原成真实模块，用 `-m` 或脚本路径启动，不要用安装生成的 `.exe` 包装器。
- 不要用 `-O`/`PYTHONOPTIMIZE`：断言被删、部分行不可断，调试结论会失真。
- zipapp 或单文件打包后运行路径与源码路径不一致，断点先确认能命中再排查。

## read-traps
- `dict` 保插入顺序：`dict.keys()` 的顺序就是插入顺序，不要按哈希顺序推理。
- 闭包变量存在 cell 里（`__closure__`），`locals` 里看到的是 cell 内容而非原变量；多层闭包要逐层展开。
- 生成器停在 `yield`，此时函数栈帧是暂停态：`locals` 是暂停点的值，`gi_frame` 与业务栈帧不是同一个。
- 协程（`async def`）的栈帧只在调度时存在；命中时的调用栈经过事件循环，因果链要靠 `await` 链与任务名还原。
- C 扩展帧（`builtins`、numpy 内部）没有 Python 变量，标记为不可进入，不要尝试展开。
- `__slots__` 的实例没有 `__dict__`，属性必须按槽位读取；`property` 会真正执行 getter，可能带副作用。
- 类属性与实例属性同名时，实例上读到的可能是类属性；要区分 `self.__class__` 上的值。

## mutate-bounds
- 不可变类型（`str`、`int`、`tuple`）改值只能替换绑定，替换后原引用持有者看不到变化。
- 改 `__dict__` 里的项会影响属性查找顺序（数据描述符优先），property/slot 仍按描述符走。
- `list`/`dict` 按索引或键改元素是就地生效；改 `dict` 键会破坏正在进行的迭代（`RuntimeError: dictionary changed size`）。
- 生成器/协程的内部帧字段（`gi_frame`、`cr_await`）改写会破坏调度状态，除定位问题外不要动。

## antipatterns
- **现象**：用 `print` 二分定位，输出顺序与代码顺序不符 → **误判**：以为分支没走到 → **正确做法**：在疑似分支下条件断点，用命中时栈帧确认。
- **现象**：`asyncio` 里单步，跳转在多个协程间来回 → **误判**：以为代码被乱序执行 → **正确做法**：用断点加条件（任务名/请求 id）停在目标协程，结论里写清事件循环调度。
- **现象**：变量读不到或显示异常 → **误判**：以为代码没执行 → **正确做法**：确认是否 `-O` 运行、是否为 C 扩展帧、是否被内联到 `assert` 分支。

## profiling
- py-spy 是最省事的选择（`py-spy record -o prof.svg --pid <pid>`，无需改代码）；asyncio 用 `--idle` 观察等待。
- `cProfile` 适合单进程、可改代码的场景，注意它对递归与 C 扩展的统计偏差。
- 没有剖析器时用带命中序号的断点统计调用次数，并在结论中标注这是采样。
