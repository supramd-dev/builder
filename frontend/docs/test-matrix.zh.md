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
version: 2

# 可选的默认值,合并进每个矩阵条目(map 按键合并,标量按条目覆盖)。
defaults:
  timeout: 3600                 # 单命令超时秒数(硬上限 4 小时)
  env:
    OMP_NUM_THREADS: "4"
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
    build:
      command: "cmake -DENABLE_MPI=OFF . && cmake --build ."
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      timeout: 600
      artifacts: "build/test_detail.xml"  # googletest 结果文件
    regression:
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
| version                      | 是       | 必须为 2。                                                          |
| defaults                     | 否       | 条目级默认值:timeout、env、build、unit、regression。                |
| presets                      | 否       | 公共回归用例(见下)。                                               |
| matrix                       | 是       | 一或多个条目;每个条目需要 tags 和至少一个阶段。                     |
| matrix[].tags                | 是       | 选择环境的标签(见[测试环境](#/docs/environments))。每个条目内必须唯一。 |
| matrix[].description         | 否       | 人类可读的标签。                                                    |
| matrix[].timeout             | 否       | 默认阶段超时秒数(默认 3600,上限 14400)。                          |
| matrix[].env                 | 否       | 为所有阶段导出的额外环境变量。                                      |
| matrix[].build               | 否       | 构建阶段(见下)。                                                   |
| matrix[].unit                | 否       | 单元测试阶段:至少有 command;可选 workdir、timeout、artifacts。    |
| matrix[].regression          | 否       | 回归测试选择:use / disable 引用预设。                              |
| unit.command                 | 是(每阶段) | shell 命令,或命令列表(见命令列表)。                       |
| unit.workdir                 | 否       | 命令的运行目录(见工作目录)。                                       |
| unit.artifacts                | 否       | 工件文件路径(或路径列表),runner 会在阶段结束后取回(见工件文件)。 |
| unit.timeout                 | 否       | 覆盖默认值的阶段超时。                                              |

构建阶段就是一条 shell 命令(没有内置的 cmake 支持 —— cmake/make/ninja/
脚本调用自己写):

| 字段           | 说明                                                              |
|----------------|-------------------------------------------------------------------|
| build.command  | 编译代码的 shell 命令(必填 —— 条目或 defaults 提供)。             |
| build.workdir  | 命令运行目录(语义与 unit/presets 相同)。                         |
| build.artifacts | 可选的文件路径(或列表):构建结束后 runner 取回并存到构建运行上,可在运行详情页下载。不做任何解析 —— 构建结论只看退出码。 |

示例 —— 通过 workdir 和内置的 `$MD_CODE_DIR` 变量做源外构建:

```yaml
build:
  command: 'cmake "$MD_CODE_DIR" -DCMAKE_BUILD_TYPE=Release && cmake --build . -j16'
  workdir: "build"   # 在 <code>/build 中运行
```

每个超时通过远程的 `timeout` 命令约束对应阶段;整个 SSH 会话的上限是各
阶段超时之和再加 15 分钟余量。

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
| presets.<名称>.description | 否 | 人类可读的标签。                                              |
| presets.<名称>.workdir   | 否   | 命令的运行目录(见工作目录)。                                 |
| presets.<名称>.timeout   | 否   | 用例超时(缺省回退到 defaults / 矩阵条目的 timeout)。          |
| presets.<名称>.artifacts | 否   | 要收集的工件文件(见工件文件)。                                |

每个被引用的预设都会成为任务图中**独立的子任务**(“regression: heat”),
在构建之后运行,拥有自己的超时、自己的日志,以及父回归运行下自己的
子测试运行。各用例相互独立 —— 某个用例失败不会中断其他用例 —— 矩阵格
汇总该条目的所有用例。

矩阵条目通过 `regression.use` 和 `regression.disable` 选择预设:

- **use**:要运行的预设名称列表。省略或为空时,**所有预设都会运行**
  (按名称排序)。
- **disable**:从已选集合中剔除的预设名称 —— 配合空的 use 使用很方便
  (“除 poisson 以外全部”)。
- `use` / `disable` 中出现 `presets` 里不存在的名称会校验失败。

### 用例的通过 / 失败判定

一个用例的成败完全由其**命令的退出状态**决定,别无其他:

- **退出码 0 → 通过**;任何非零退出码 → 失败。这包括超时(runner 用
  远程 `timeout` 包装命令,超时退出码为 124)和 workdir `cd` 失败。
- SSH 层面的失败(主机不可达、会话中断)同样判定为失败,传输错误会
  作为该用例的备注。
- 预设的 `artifacts` 文件**不会改变判定结果** —— 它们只作为该用例子
  运行的 artifact 存储(逐用例明细由浏览器解析)。这一点与 unit 阶段
  不同:unit 的工件文件解析出失败用例时,运行也会判为失败。
- `MD-BUILDER-SUMMARY:` 行只会成为用例的备注,无法把非零退出码变成
  通过。

条目的回归运行(矩阵格)对所有用例汇总:任一用例失败 → 格子显示 ✗,
全部通过 → ✓。各用例相互独立 —— 某个用例失败不会中断其他用例。当
clone 或 build 失败时,所有用例被记录为 **skipped**(⤼),备注为上游
错误。

## 命令列表

unit 阶段或回归预设的 `command` 可以是**单条命令,也可以是列表**:

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

yaml 的 `env` 变量在 MD_* 变量之后导出(要覆盖它们请用环境设置脚本,
见下)。

```yaml
unit:
  command: "$MD_CODE_DIR/build/unit_tests --gtest_output=xml:$MD_CODE_DIR/build/test_detail.xml"
```

## 环境设置脚本

每个**环境**(在站点上配置,而非 yaml 中)可以携带一个环境设置脚本 ——
脚本内容在环境设置表单中编辑、保存在服务器上。派发时它被写入任务目录,
文件名为 `md-builder-env-<hash>.sh`,并且被**每个阶段脚本**在阶段命令
之前 source(module 加载、编译器导出、virtualenv 激活等):

- **存在**:每个阶段脚本先执行 `. md-builder-env-<hash>.sh`;脚本导出的
  一切对 build、unit 和回归命令可见(它在导言的最后运行,因此可以覆盖
  内置变量和 yaml 变量)。
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

### 下载工件

运行(构建文件、unit / 回归结果文件)的每个已存储工件都可以在运行
详情页下载:单个文件逐一下载,或整包打成一个 zip
(`GET /api/test-runs/{id}/artifacts/zip`)。回归运行的 zip 包含所有
用例的文件,按 `cases/<用例名>/` 分目录存放。文件内容目前存于平台
数据库;S3 兼容对象存储(如 Garage)是规划中的存储后端 —— 无论哪种
后端,下载接口不变。

## 校验规则

- `version` 必须为 2;`matrix` 不能为空。
- 每个条目需要非空的 `tags` 以及 `unit` / `regression`(或其预设展开)
  中的至少一个,且带有 `command`。
- 条目之间不允许重复的标签组合。
- `build.command` 必填(条目自己的或 `defaults.build` 的)。
- 未知字段会被拒绝(例如已移除的 `build.generator` / `cmake_flags` /
  `threads`)。
- 每个预设必须带 `command`;`use` / `disable` 中的名称必须引用已定义
  的预设。

非法的 YAML 会使派发失败:推送仍被记录,`dispatchError` 出现在 webhook
响应中(见 [Webhooks](#/docs/webhooks)),但不会创建任何任务。

## Runner 做了什么

每个匹配的条目会成为一条任务图(见
[Runner 与任务](#/docs/runner-strategy)),各阶段按序运行:

1. **clone**:*服务器* 克隆被推送提交上的代码仓库,把工作树打包为
   tarball 并解压到环境上的 ~/.md-builder/tasks/<sha12>/code。环境
   设置脚本被写入任务目录。
2. **build**:生成的脚本导出 MD_COMMIT、MD_ENV_NAME、MD_ENV_TAGS、
   MD_TASK_DIR、MD_CODE_DIR 以及 yaml 的 env 变量,source 环境设置
   脚本(如果存在),然后在它的工作目录中运行构建阶段。
3. 若构建(或克隆)失败,依赖它的测试阶段会被标记为 skipped,仪表板
   显示 ✗。
4. **unit**:阶段命令在其工作目录中、受超时约束地运行;完整输出流入
   任务日志,结果被存为一条测试运行。
5. **regression:每个选中的预设一个子任务** —— 每个用例命令在构建之后
   运行(导出 MD_CASE),收集自己的结果文件并记录自己的子测试运行;矩阵格
   显示所有用例的汇总。

## 自定义摘要

阶段命令可以在 stdout 打印一行摘要:

```
MD-BUILDER-SUMMARY: all 8 tests passed, max rel err 3.2e-7
```

前缀之后的文本会成为仪表板上显示的运行摘要。没有这行时,摘要为退出码
加阶段日志的最后几行(截断到 500 字符)。对回归用例而言,这行摘要成为
该用例子运行的备注。
