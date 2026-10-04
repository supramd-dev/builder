# 测试矩阵(md-builder.yaml)

将 md-builder.yaml 放在**代码仓库根目录** —— 它随代码一起演进,改动它
的推送会改变派发行为。每次推送时,服务器都会在被推送的提交上读取它
(git show <sha>:md-builder.yaml),因此矩阵的改动从引入它的那个提交
开始生效。

md-builder 源码树中的
[md-builder.example.yaml](https://github.com/genshen/md-builder/blob/main/md-builder.example.yaml)
是一份带完整注释、可直接复制修改的模板。

## 完整示例

```yaml
version: 3

# 可选的默认值,合并进每个矩阵条目(map 按键合并,标量按条目覆盖)。
defaults:
  timeout: 3600                 # 单命令超时秒数(硬上限 4 小时);
                                # 超过它的命令以 timeout 状态上报
  env:
    OMP_NUM_THREADS: "4"
  variables:                    # 模板,在环境上展开
    BUILD_ROOT: "$MD_CODE_DIR/build"
  build:
    # 一条普通 shell 命令 —— cmake/make/脚本,项目用什么就写什么。
    command: "cmake -DCMAKE_BUILD_TYPE=Release . && cmake --build . -j8"

# 回归测试预设:被矩阵条目引用的公共测试用例。每个预设就是一个
# 回归用例 —— 如何运行它、收集哪些工件文件。
presets:
  heat:
    description: 热传导方程收敛性
    command: "python3 run_heat.py"
    workdir: "regression/heat"        # 相对代码目录
    timeout: 1800
    artifacts: "out.xml"
  poisson:
    command: "python3 run_poisson.py --nt 200"
    artifacts: ["poisson.xml", "poisson.log"]

# 必填:矩阵。每个条目声明环境必须携带的标签;派发时为每个条目挑选
# 一个匹配且已启用的环境。
matrix:
  - tags: [cpu]
    description: Generic CPU build
    env:
      CC: gcc
      CXX: g++
    variables:                    # 按键合并到 defaults.variables 之上
      CMAKE_FLAGS: "-DENABLE_MPI=OFF"
    build:
      command: 'cmake -B "$BUILD_ROOT" $CMAKE_FLAGS . && cmake --build "$BUILD_ROOT"'
      description: "用 gcc 和 cmake 构建代码"
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      description: "运行单元测试"
      timeout: 600
      artifacts: "build/test_detail.xml"  # googletest 结果文件
    regression:
      description: "运行回归测试"        # 整个回归阶段的标签
      use: [heat, poisson]          # 在此条目上运行哪些预设(留空 = 全部)
  - tags: [gpu, cuda]
    env:
      CC: clang
    build:
      command: "./build.sh --cuda"   # 任意构建工具,不限于 cmake
    unit:
      command: "ctest --test-dir build -L unit"
    regression:
      disable: [heat]           # 运行除 heat 以外的所有预设
```

## 字段参考

| 字段                         | 必填     | 说明                                                               |
|------------------------------|----------|--------------------------------------------------------------------|
| version                      | 是       | 必须为 3。                                                          |
| defaults                     | 否       | 条目级默认值:timeout、env、variables、build、unit、regression。     |
| presets                      | 否       | 公共回归用例(见下)。                                               |
| matrix                       | 是       | 一或多个条目;每个条目需要 tags 和至少一个阶段。                     |
| matrix[].tags                | 是       | 选择环境的标签(见[测试环境](#/docs/environments))。每个条目内必须唯一。 |
| matrix[].description         | 否       | 人类可读的标签。                                                    |
| matrix[].timeout             | 否       | 默认阶段超时秒数(默认 3600,上限 14400)。                          |
| matrix[].env                 | 否       | 为所有阶段导出的额外环境变量 —— 字面量值。                          |
| matrix[].variables           | 否       | 该条目各阶段的额外变量,合并到 defaults.variables 之上 —— 模板值(见[变量](#变量))。 |
| matrix[].build               | 否       | 构建阶段(见下)。                                                   |
| matrix[].unit                | 否       | 单元测试阶段:至少有 command;可选 description、workdir、timeout、artifacts。 |
| matrix[].regression          | 否       | 回归测试选择:use / disable 引用预设;可选整个阶段的 description。  |
| unit.command                 | 是(每阶段) | shell 命令,或命令列表(见命令列表)。                       |
| unit.description             | 否       | 阶段的人类可读标签:显示在其运行详情页,且每次触发运行时都会存入数据库(描述可能随 yaml 变更)。 |
| unit.workdir                 | 否       | 命令的运行目录(见工作目录)。                                       |
| unit.artifacts                | 否       | 工件文件路径(或路径列表),runner 会在阶段结束后取回(见工件文件)。 |
| unit.timeout                 | 否       | 覆盖默认值的阶段超时。                                              |
| regression.description       | 否       | 整个回归阶段(容器节点)的人类可读标签;每个用例另带自己预设的 description。 |

构建阶段是一条 shell 命令 —— 或者像测试阶段一样是命令列表(没有内置的
cmake 支持 —— cmake/make/ninja/脚本调用自己写):

| 字段           | 说明                                                              |
|----------------|-------------------------------------------------------------------|
| build.command  | 编译代码的 shell 命令(必填 —— 条目或 defaults 提供),或命令列表(见命令列表)。 |
| build.description | 构建阶段的人类可读标签:显示在其运行详情页,且每次触发运行时都会存入数据库(缺省回退到 defaults.build.description)。 |
| build.workdir  | 命令运行目录(语义与 unit/presets 相同)。                         |
| build.artifacts | 可选的文件路径(或列表):构建结束后 runner 取回并存到构建运行上,可在运行详情页下载。不做任何解析 —— 构建结论只看退出码。 |

示例 —— 通过 workdir 和内置的 `$MD_CODE_DIR` 变量做源外构建:

```yaml
build:
  command: 'cmake "$MD_CODE_DIR" -DCMAKE_BUILD_TYPE=Release && cmake --build . -j16'
  workdir: "build"   # 在 <code>/build 中运行
```

每条阶段命令都由远程的 `timeout` 命令约束,上限是阶段的 timeout(默认
3600 秒,硬上限 4 小时);该阶段自己的 SSH 会话在此基础上再加 5 分钟余量。

命令超出超时被杀的阶段,状态是独立的 **`timeout`**,而不是普通的
`failed` —— 结果一样,但页面显示的是原因:

- 仪表盘格子与运行详情页显示「⏱ timeout」,有自己的颜色;
- 运行的错误与摘要写明超时与命令(`timed out after 30m0s: make -j8`,
  摘要还会带上日志尾部);
- 日志在命令被杀的位置追加一行
  `task timed out: timed out after 30m0s: make -j8`(命令自己的输出保留
  在这行之前);
- 它下游的阶段被标记为 **skipped**,原因是
  「upstream task build timed out」,与失败时一致。

远程 `timeout` 包装触发时退出码为 124,runner 正是靠它把超时与「命令自己
退出 124」区分开(后者实际不会发生)。忽略 SIGTERM、连会话余量也一起耗尽的
命令会被会话切断 —— 状态与措辞相同。

## 回归测试预设

回归测试在顶层 `presets` 映射中**只定义一次**,再由各矩阵条目引用 ——
同一个用例不需要在每个平台上重复:

```yaml
presets:
  heat:
    description: 热传导方程收敛性
    command: "python3 run_heat.py"
    workdir: "regression/heat"
    timeout: 1800
    artifacts: "out.xml"
```

| 字段                     | 必填 | 说明                                                        |
|--------------------------|------|--------------------------------------------------------------|
| presets.<名称>.command   | 是   | 运行该用例的 shell 命令,或命令列表(见命令列表)。                |
| presets.<名称>.description | 否 | 用例的人类可读标签:显示在该用例自己的任务页(及其各次运行)上;每次触发运行时都会存入数据库。 |
| presets.<名称>.workdir   | 否   | 命令的运行目录(见工作目录)。                                 |
| presets.<名称>.timeout   | 否   | 用例超时(缺省回退到 defaults / 矩阵条目的 timeout)。          |
| presets.<名称>.artifacts | 否   | 要收集的工件文件(见工件文件)。                                |

每个被引用的预设都会成为任务图中**独立的任务**(“regression: heat”),
嵌在该阶段的虚拟容器之下,在构建之后运行,拥有自己的超时、自己的日志、
自己的尝试历史和自己的一条运行 —— 正是这一点让它能单独上报、单独下载、
单独重跑。各用例相互独立 —— 某个用例失败不会中断其他用例 —— 矩阵格
展示的是容器对该条目所有用例的汇总。

矩阵条目通过 `regression.use` 和 `regression.disable` 选择预设:

- **use**:要运行的预设名称列表。省略或为空时,**所有预设都会运行**
  (按名称排序)。
- **disable**:从已选集合中剔除的预设名称 —— 配合空的 use 使用很方便
  (“除 poisson 以外全部”)。
- `use` / `disable` 中出现 `presets` 里不存在的名称会校验失败。

### 用例的通过 / 失败判定

一个用例的成败完全由其**命令的退出状态**决定,别无其他:

- **退出码 0 → 通过**;任何非零退出码 → 失败。超时是唯一有独立状态的
  非零退出码:**`timeout`**,由 runner 直接记录(而不是从退出码推导),
  workdir `cd` 失败则只是普通失败。
- SSH 层面的失败(主机不可达、会话中断)同样判定为失败,传输错误会
  作为该用例的备注。
- 预设的 `artifacts` 文件**不会改变判定结果** —— 它们只作为该用例
  自己那条运行的 artifact 存储(逐用例明细由浏览器解析)。这一点与
  unit 阶段不同:unit 的工件文件解析出失败用例时,运行也会判为失败。
- 用例自己那条运行只计作**一个**测试:命令退出码为 0 即 `1/1 passed`,
  否则 `0/1 failed`。这个计数就是判定结果本身,而不是从工件文件里解析
  出来的用例数(逐用例明细由浏览器解析那些文件得到)。
- `MD-BUILDER-SUMMARY:` 行只会成为用例的备注,无法把非零退出码变成
  通过。

矩阵格展示的是**容器**的状态,也就是其下各用例的汇总:任一用例失败则
该阶段失败,格子里写明是哪些("3/4 cases passed; failed: heat");全部
通过则阶段通过;还没被认领完时读作 "2/4 cases passed; 2 queued"。
如果容器下的失败**全部**是超时,它读作 `timeout` 并写明是哪些用例
("1/4 cases passed; timed out: poisson");超时与失败混合时读作 `failed`,
但两种原因都会被列出。各用例相互独立 —— 某个用例失败不会中断其他用例。
当 clone 或 build 未能通过时 —— 它失败了、超时了,或它自己就被上报为
`skipped` —— 所有用例被标记为 **skipped**,摘要即上游错误(各自还有
一行日志);如果所有用例都被跳过,该阶段本身也读作 `skipped`,而不是
`passed`。

## 命令列表

构建阶段、unit 阶段或回归预设的 `command` 都可以是**单条命令,也可以是列表**:

```yaml
command: "make data && ctest -L unit"       # 标量:一条 bash -c 命令
command: ["make data", "ctest -L unit"]     # 列表:两条命令
command: |                                  # 块标量:仍是一条命令
  ./configure --enable-mpi
  make -j8
```

- **标量**形式是单次 `bash -c` 调用 —— 用 `&&`、`;`、管道或块标量
  自由组合。
- **列表**形式按顺序执行每一项(每条命令有自己的 `timeout` 包装),
  并用 `&&` 串联:**任一条命令失败即停止** —— 后续命令不再执行,
  该用例以失败命令的退出码判定失败。
- 空白项会被忽略(方便把块标量按行拆成列表)。

列表形式等价于在单条字符串里用 `&&` 串联,只是可读性更好
(也省得在 yaml 里写 `&&`)。

## 内置环境变量

每个阶段脚本(build、unit、回归用例)都会在阶段命令运行之前导出以下
变量,命令可以直接使用:

| 变量          | 含义                                                        |
|---------------|--------------------------------------------------------------|
| MD_COMMIT     | 被测试提交的完整 SHA。                                        |
| MD_ENV_NAME   | 阶段运行所在环境的名称。                                      |
| MD_ENV_TAGS   | 该环境的逗号分隔标签。                                        |
| MD_TASK_DIR   | 远程任务目录(~/.md-builder/tasks/<sha12>)。                  |
| MD_CODE_DIR   | 代码目录,即 MD_TASK_DIR/code。                               |
| MD_CASE       | 用例名称(仅回归用例脚本导出)。                                |
| MD_SECRET_TOKEN | 站点的 secret token(设置 → 仓库),配置后才导出 —— 见下。     |

yaml 的 `env` 变量在 MD_* 变量之后导出(要覆盖它们请用环境设置脚本,
见下)。

```yaml
unit:
  command: "$MD_CODE_DIR/build/unit_tests --gtest_output=xml:$MD_CODE_DIR/build/test_detail.xml"
```

这些引用之所以能展开,是因为命令本身运行在环境上的阶段脚本里,那里
已经导出了 MD_*。相反,`env` 的值是**字面量**,永远不会被展开 ——
需要展开请用[变量](#变量)。

### Secret token(MD_SECRET_TOKEN)

命令经常需要凭证 —— 私有软件源、工件存储、付费软件的 license 服务器
—— 这些不能提交进代码仓库。站点的 **secret token**(设置 → 仓库 →
“命令用 Secret token”)解决这个问题:配置后,它会(与其他 MD_* 变量
一样)导出到每个阶段脚本(build、unit、回归用例),变量名为
`MD_SECRET_TOKEN`:

```yaml
build:
  command: "cmake -DFETCH_TOKEN=\"$MD_SECRET_TOKEN\" . && cmake --build . -j8"
```

```yaml
presets:
  eos-table:
    command: "curl -sS -H \"Authorization: Bearer $MD_SECRET_TOKEN\" -o eos.tbl https://data.internal/eos.tbl && ./check_eos eos.tbl"
```

它与仓库 access token 一样是只写的:设置表单只显示是否已配置,不显示
值本身。若命令把它回显出来(`env`、`set -x`、`curl -v`),runner 会在
写入任务日志前把每一处出现都替换为 `REDACTED`。未配置时该变量为未设
置状态。

## 变量

`defaults` 和每个矩阵条目都可以声明 **`variables`**:一组具名值,该条目
的各阶段命令可以像普通 shell 变量一样使用。它们是 `env` 的*可展开*版本,
区别很重要:

- **`env`** 的值是字面量。`BUILD_ROOT: "$MD_CODE_DIR/build"` 写在 `env`
  下,导出的就是这段文本(单引号包裹),没有任何东西会展开它:命令里读到
  的 `$BUILD_ROOT` 是一个中间带 `$` 的路径。
- **`variables`** 的值是模板,由阶段脚本在**环境上**、阶段运行时展开。

展开时会替换三类引用:

1. 内置的 `MD_*` 变量(`$MD_CODE_DIR`、`$MD_COMMIT` 等);
2. 该条目的**其他变量** —— 按依赖顺序导出,因此一个变量可以写在另一个
   变量之上;
3. **该环境**允许的**主机**环境变量(*Runner Envs* → 该行的 **Edit**
   表单 —— 在所有者修改之前是 `HOME`、`USER`、`LOGNAME`、`PATH`、
   `SHELL`、`TMPDIR`)。每台主机公开自己的名字,因此同一份 yaml 可能
   在一台机器上展开、在另一台机器上保持字面量。该条目自己的 `env`
   导出的名字只要在名单上也算:变量在 `env` 之后导出。

其余引用一律保持字面量:写成 `$MD_CODEDIR` 这类笔误,命令收到的就是
`$MD_CODEDIR` 这段文本,同时阶段日志里会出现

```
warning: variables.<name>: $MD_CODEDIR is not a built-in variable, a variables entry or an allowed environment variable; kept literally
```

因此路径不会悄悄丢掉 `$`。

```yaml
defaults:
  variables:
    BUILD_ROOT: "$MD_CODE_DIR/build"

matrix:
  - tags: [cpu]
    variables:
      CMAKE_FLAGS: "-DCMAKE_BUILD_TYPE=Release -DENABLE_MPI=OFF"
      UNIT_XML: "$BUILD_ROOT/tests/unit.xml"   # BUILD_ROOT 先展开
    build:
      command: 'cmake -B "$BUILD_ROOT" $CMAKE_FLAGS . && cmake --build "$BUILD_ROOT" -j8'
    unit:
      command: "./build/unit_tests --gtest_output=xml:$UNIT_XML"
```

条目按键继承 `defaults.variables`,冲突时条目优先,与 `env` 完全一致。

### 值只是数据,不是代码

一个值被渲染为**恰好一个 shell 词**:可展开的引用变成双引号包裹的
`${NAME}`,其余部分单引号包裹。因此值里的 `$(…)`、反引号、引号或换行
都只是文本:`INJECT: "$(touch /tmp/x)"` 导出的就是这段字符串,命令里
`echo "$INJECT"` 会把它打印出来而不是执行它 —— yaml 无法借 `variables`
把 shell 语法注入阶段脚本。

值里想要一个字面 `$` 就写 `$$`(`PRICE: "5$$ per run"`);后面不构成
变量名的 `$`(`50$`、`${ }`)原样保留。

### 导言的导出顺序

每个阶段脚本按以下顺序构建环境:

1. 内置 `MD_*` 变量;
2. 条目的 `env`(字面量,单引号包裹);
3. **环境设置脚本**(source,见下);
4. 条目的 `variables`(模板,在此展开);
5. `cd` 进入工作目录。

变量在环境设置脚本之后展开,因此变量可以建立在脚本导出的内容之上
(module 加载、`module load` 推入的主机路径等)—— 同名时变量覆盖脚本的
导出。`cd` 在最后,所以 `workdir` 也可以引用变量。

### 允许哪些主机变量

某个仓库的 yaml 可以读取主机的哪些环境变量,由**主机所有者**决定,而不是
仓库决定:名单保存在环境本身上 —— *Runner Envs* → 该行的 **Edit** 表单,
**Expandable host variables** —— 因此同一站点的两台机器可以公开不同的名字
(见[测试环境](#/docs/environments))。新环境从一份最小默认名单开始
(`HOME`、`USER`、`PATH` 等)。不在名单上的名字保持字面量,与写错名字完全
相同;所有者也可以清空名单,表示一个都不允许。

### 校验

变量在读取 yaml 时就会校验,因此有问题的条目会让派发失败并指出出错的
名字(commit 行上的 `dispatchError`),而不是生成一份取值依赖导出顺序的
脚本:

- 名字必须是 shell 标识符(`[A-Za-z_][A-Za-z0-9_]*`);
- 不能以 `MD_` 开头 —— 该前缀保留给总是会展开的内置变量;
- 不能与同一条目的 `env` 重名 —— 同一个名字会有两种语义不同的值,只能
  留在其中一处;
- 两个变量之间不能形成循环引用。

## 环境设置脚本

每个**环境**(在站点上配置,而非 yaml 中)可以携带一个环境设置脚本 ——
脚本内容在环境设置表单中编辑、保存在服务器上。派发时它被写入任务目录,
文件名为 `md-builder-env-<hash>.sh`,并且被**每个阶段脚本**在阶段命令
之前 source(module 加载、编译器导出、virtualenv 激活等):

- **存在**:每个阶段脚本先执行 `. md-builder-env-<hash>.sh`;脚本导出的
  一切对 build、unit 和回归命令可见。它在内置 `MD_*` 和 yaml 的 `env`
  之后、yaml 的 `variables` 之前运行:可以覆盖前两者,但同名的变量会
  覆盖它(见[导言的导出顺序](#导言的导出顺序))。
- **不存在**:阶段脚本记录一条警告后照常运行。

见[测试环境](#/docs/environments)。

## 工作目录

`build.workdir`、`unit.workdir` 和 `presets.<名称>.workdir` 设置阶段命令
的运行目录:

- **留空**(默认):代码目录(MD_CODE_DIR)。
- **相对路径**:MD_CODE_DIR/<workdir> —— 目录必须已存在于仓库中
  (runner 不会创建它)。
- **绝对路径**:在远程主机上原样使用。

源外构建只是 workdir 加一条以 `"$MD_CODE_DIR"` 为参数的配置命令
(见上面的 build 示例)。

## 工件文件

阶段命令(unit 或回归预设)可以产出结构化的结果文件 —— 默认是 googletest
的 XML(`--gtest_output=xml:`)或 JSON(`--gtest_output=json:`)格式,
由测试程序自己写出。一次运行可能产出**多个**结果文件;`artifacts` 接受
单个路径或路径列表:

```yaml
unit:
  command: "./build/unit_tests --gtest_output=xml:build/test_detail.xml"
  artifacts: "build/test_detail.xml"
```

```yaml
unit:
  command: "ctest --output-junit junit.xml && ./build/extra_tests --gtest_output=json:build/extra.json"
  artifacts: ["build/test_detail.xml", "build/extra.json"]
```

配置了 `artifacts` 时,runner 会在命令结束后逐个取回这些文件,并:

- 将每个文件内容**原样**存为各自独立的运行 artifact;
- 仅从各文件根节点属性中提取聚合计数(总数 / 失败 / 跳过),跨所有
  文件**求和**后用于矩阵格。

每个测试项的明细 —— 名称、状态、耗时、失败信息 —— 由**浏览器**在打开
运行详情页时解析展示;服务器不解析单个测试项。所有已存储的工件文件
都按默认名称/格式解析;缺失或无法识别的文件会被跳过(阶段日志会注
明),不影响运行结果 —— 运行仍记录命令的退出状态。路径相对于该阶段的
工作目录(绝对路径也可以)。

对 **unit** 阶段,解析出的计数参与判定:工件文件中出现失败用例时,
即使命令以 0 退出,运行也判为失败(ctest 一类的包装可能吞掉测试程序
的退出码)。对**回归预设**,工件文件仅用于展示 —— 用例的成败由其命令
的退出状态决定(见“用例的通过 / 失败判定”)。

### 构建工件

**build** 阶段同样支持 `artifacts` 字段,但语义不同:文件**原样**存
储、永不解析 —— 构建的结论只由命令退出码决定(从 build 取回的形似
结果的文件不会把格子翻成失败)。

```yaml
build:
  command: "cmake . && ninja"
  artifacts: ["build/.ninja_log", "build/compile_commands.json"]
```

构建留下的任意文件都可以 —— 日志、`compile_commands.json`、体积报
告。与测试阶段相同,路径相对于 build 的 workdir,每个文件取回时以
8 MiB 为上限。

### 绘图工件(`*.plot.json` / `*.plotly.json`)

回归用例的 artifacts 里可以包含**绘图图表**:文件名以 `.plot.json`
或 `.plotly.json` 结尾、内容是一个 [Plotly] 图表文档 —— `data` 轨迹
数组加可选的 `layout` 对象:

```yaml
presets:
  heat:
    command: "python3 run_heat.py --plot drift.plot.json"
    workdir: "regression/heat"
    artifacts: "drift.plot.json"
```

```json
{
  "data": [
    { "x": [1, 2, 3], "y": [2, 6, 3], "type": "scatter", "mode": "lines+markers" }
  ],
  "layout": { "width": 320, "height": 240, "title": "A Fancy Plot" }
}
```

打开运行详情页时,每个绘图工件都会被取回并渲染为可交互的图表(缩放、
悬浮、图例开关 —— 标准的 Plotly 工具栏)。文档几乎原样透传:[Plotly
支持的每种轨迹类型](https://plotly.com/javascript/)(scatter、bar、
heatmap、3D surface 等)都可用;文件里的 `layout` 优先于默认值。格式
坏的文件在页面内显示错误信息,仍可从工件表下载。

**尺寸。** 高度由文件说了算:`layout.height` 写多少就画多少(手写导出
把 `height` 放在**根级**也认),只在 120–2000 像素这个宽松区间内取
值,免得一条细线或一个失控的数字被照单全收。文件没写高度时用 480 ——
比 plotly 自己的 450 高一些,因为这一页的图表横跨一千多像素,默认值
再矮就成了压扁的一条。宽度始终跟随容器:文件里的 `layout.width` 会被
丢掉,而不是让它撑出页面。每张图上方的标签会写明实际画出来的高度和它
的来处 —— `800 px (from the file)`、`480 px (default)`、或
`(capped from 4000)` —— 于是“图看着不对”当场就有答案,不必去翻 JSON。

**判定只看文件名**(不分大小写):只认这两个后缀,因为另一种做法 ——
取回每个 JSON 工件再看它的形状 —— 会把整个运行的结果文件都下载一遍,
并为了确认“这不是图”而拖进 3.5 MB 的渲染器。用别的名字存的图
(`results/nvt-compare.plotly.json` 可以;`results/figure.json` 不行)
就只是工件表里的一个普通文件。

绘图文件在其他方面就是普通工件:原样存储、8 MiB 上限、随 zip 一起
下载,且与用例的判定无关(退出码说了算,一如既往)。unit 和 build
运行同样支持 —— 详情页遇到它们就会画图。

[Plotly]: https://plotly.com/javascript/

### HTML 工件(`*.html`)

名字以 `.html`(或 `.htm`)结尾的工件就按它本来的样子显示:在工件表
里点这一行的 *View*,预览弹窗直接给出渲染后的页面 —— 弹窗里可以切到
*Source* 看源码、可以新标签页单独打开,并且有一行字说明它跑在沙箱里。
因此一个写出 Plotly HTML 导出、覆盖率报告或自包含结果页的测试,除了
把它列进 `artifacts` 之外什么都不用做:

```yaml
unit:
  command: "pytest --html=report/report.html"
  artifacts: ["report/report.html"]
```

判定规则与绘图一致 —— 只认这两个后缀,弹窗里也体现了这一点(这两类
文件打开时默认就是 *Preview*)。渲染用的接口是
`GET /api/test-artifacts/{id}/raw`,即工件接口“用来看、而不是存下来”的
那一种:字节与下载完全相同,只是换上一个浏览器会直接渲染的类型,
HTML 还会加上沙箱。

**为什么必须沙箱。** 工件是构建产物:内容来自被测代码,不是 md-builder
自己。若以本站 origin 渲染它,页面里的脚本就能带着你的会话去调 API
(会话 cookie 是 HttpOnly,那只是让它读不到,并没有让它用不了)。所以
响应带上 `Content-Security-Policy: sandbox allow-scripts …`,且**不含**
`allow-same-origin`,iframe 上也重复同一份清单:页面自己的脚本照跑
(Plotly 导出必须能跑,那正是它值得被当作页面的原因),但它落在一个
opaque origin 里 —— 读不到 cookie 与本地存储,发出的请求也不带凭据。
少数靠 `localStorage` 记 UI 状态的报告页在预览里会退化,那种情况下把
文件下载下来本地打开即可。SVG 出于同样的原因**故意不做**预览(它同样
能带脚本),压缩包与二进制也只走下载。

HTML 工件在其他方面同样是普通工件:原样存储、8 MiB 上限、随 zip 一起
下载;与绘图一样,build / unit / 回归运行都支持,且从不影响阶段的判定。

### Markdown 工件(`*.md`)

名字以 `.md`(或 `.markdown`)结尾的工件,在预览弹窗里直接给出渲染后的
文档 —— 标题、列表、表格、代码块、链接 —— 并可以切到 *Source* 看纯
文本:

```yaml
unit:
  command: "./tools/report.sh > report/summary.md"
  artifacts: ["report/summary.md"]
```

渲染由 md-builder 自己完成(内置文档用的同一个渲染器),它直接构造
DOM 节点:文件的字节从不作为标记交给浏览器,所以 `.md` 不需要沙箱
(文件里没有任何东西能对页面执行脚本),也只有这两个后缀会得到渲染
视图。渲染只是阅读上的便利:文件仍然原样存储、原样下载。

与绘图、页面一样,它属于展示类工件:不是矩阵会统计的结果格式,列上
它不会改变阶段的判定。运行自己的说明、脚本写出的总结、逐用例的报告,
就是这样到达读这次运行的人面前的。

### 下载工件

运行(构建文件、unit / 回归结果文件、画图文件)的每个已存储工件都可以
在运行详情页下载:单个文件逐一下载,或把该次尝试的整包打成一个 zip
(`GET /api/test-runs/{id}/artifacts/zip`)。没有产出任何工件的运行下载
zip 会得到 `404`,而不是一个空归档。

要打包**整个测试**时用的是任务级 zip:
`GET /api/tasks/{id}/artifacts/zip` 会把该任务及其下每个节点最新尝试的
工件打在一起 —— 任务自己的文件在归档根目录,每个后代的文件放在以它
命名的目录下(`regression-heat/…`,由节点 key 把 `:` 换成 `-` 得来)。
因此向回归容器请求,就得到一个包含所有用例文件的归档;向单个用例请求,
就只得到那个用例的文件。已退休的节点不包含在内:它们的文件属于更早的
图形态。

文件内容存放在所配置的 S3 兼容对象存储中(见
[对象存储](#/docs/object-storage));对象已不存在时返回 `404`,后端故障
返回 `502`。

## 校验规则

- `version` 必须为 3;`matrix` 不能为空。
- 每个条目需要非空的 `tags` 以及 `unit` / `regression`(或其预设展开)
  中的至少一个,且带有 `command`。
- 条目之间不允许重复的标签组合。
- `build.command` 必填(条目自己的或 `defaults.build` 的)。
- 未知字段会被拒绝(例如已移除的 `build.generator` / `cmake_flags` /
  `threads`)。
- 每个预设必须带 `command`;`use` / `disable` 中的名称必须引用已定义
  的预设。
- `variables` 的名字必须是 shell 标识符,不能以 `MD_` 开头,不能与同一
  条目的 `env` 重名,也不能形成循环引用(见[变量](#变量))。

非法的 YAML 会使派发失败:推送仍被记录,`dispatchError` 出现在 webhook
响应中(见 [Webhooks](#/docs/webhooks)),但不会创建任何任务。同一条
消息也会存到 commit 行上,因此仪表板会在该 commit 的 **graph** 列显示
它(一个警告三角:悬停或点击查看文本)—— 没有任何条目匹配到已启用
环境时也是如此。

由于匹配是全站范围的,某一列可能是别人注册的机器:仪表板的每个环境列都会
在环境名下方标出它的 owner(见[测试环境](#/docs/environments))。

## Runner 做了什么

每个匹配的条目会成为一条任务图(见
[Runner 与任务](#/docs/runner-strategy)),各阶段按序运行:

1. **clone**:*服务器* 克隆被推送提交上的代码仓库,把工作树打包为
   tarball 并解压到环境上的 ~/.md-builder/tasks/<sha12>/code。环境
   设置脚本被写入任务目录。
2. **build**:生成的脚本导出 MD_COMMIT、MD_ENV_NAME、MD_ENV_TAGS、
   MD_TASK_DIR、MD_CODE_DIR 以及 yaml 的 env 变量,source 环境设置
   脚本(如果存在),导出展开后的 yaml 变量,然后在它的工作目录中运行
   构建阶段。
3. 若构建(或克隆)未能通过 —— 它失败了、超时了,或它自己就被上报为
   `skipped` —— 依赖它的测试阶段会被标记为 **skipped**,原因写在
   摘要里、日志中有一行,仪表板如实呈现。
4. **unit**:阶段命令在其工作目录中、受超时约束地运行;完整输出流入
   任务日志,结果被存为该次尝试的测试运行。
5. **regression:每个选中的预设一个任务** —— 每个用例命令在构建之后
   运行(导出 MD_CASE),收集自己的结果文件并记录自己那条运行;矩阵格
   展示容器对所有用例的汇总。

图的终止也可能与代码无关:当同一版本的新一次派发在站点的 `fork_cancel`
策略下把它丢掉时,它尚未结束的阶段会读作 **cancelled** —— 它们从未被
判定,因此单元格、图与运行页都如实如此呈现,而不是显示为失败(见
[站点配置 → 重复 commit](#/docs/site-configuration))。

## 自定义摘要

阶段命令可以在 stdout 打印一行摘要:

```
MD-BUILDER-SUMMARY: all 8 tests passed, max rel err 3.2e-7
```

前缀之后的文本会成为仪表板上显示的运行摘要。没有这行时,摘要为退出码
加阶段日志的最后几行(截断到 500 字符)。对回归用例而言,这行摘要成为
该用例那条运行的摘要。超时的阶段例外:摘要保留超时的措辞,被杀命令
临终前的输出只是摘要的尾部,而不是摘要本身。
