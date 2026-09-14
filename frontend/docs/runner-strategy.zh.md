# Runner 与任务

服务器内嵌 runner 组件:任务图模型、调度器以及通往测试环境的 SSH 传输。
原先的 sshcheck 包(SSH 传输)已并入其中。

## 派发

**任务图**由三种触发方式之一创建 —— webhook 推送、手动 yaml 矩阵派发、
手动自定义命令派发。三者共用同一套图模型、调度器和环境执行流程;区别
在于提交与各阶段命令如何确定,以及重复触发时的记录方式。

- **webhook 派发**(自动):向已配置代码仓库的推送(见
  [Webhooks](#/docs/webhooks))会在**被推送的提交**上读取
  `md-builder.yaml`,按标签把条目匹配到已启用的环境,每个条目创建一条
  图。图以(提交, 环境)为键:重复推送同一提交 —— 或在修改环境标签或
  YAML 后通过 `POST /api/jobs` 重新派发 —— 会重排既有图:子任务与日志
  按新快照重建,新图不再包含的阶段的测试运行会被删除。
- **手动 yaml 派发**(**Run command** 页面的 *Manual test* 标签的
  第一部分 —— 一个 ref 输入框 + 一个按钮 —— 或
  `POST /api/jobs/manual-yaml`):按需执行的 webhook 流程。解析站点配置
  代码仓库的一个 ref(分支 / 标签 / 提交 id,留空 = HEAD),提交行按
  **webhook 同样的去重方式**记录,然后与推送完全一致地派发该提交上的
  yaml 矩阵:图同样以(提交, 环境)为键,重复触发同一 ref 会以最新快照
  重排这些图(采纳 yaml 或环境标签的修改),而不会新增矩阵行。图标记
  `trigger: 2`。
- **手动派发**(**Run command** 页面的 *Manual test* 标签,或
  `POST /api/jobs/manual`,见[仪表板与 API](#/docs/dashboard)):用户
  自定义的测试 —— 不需要 YAML,也不需要推送。仓库默认取站点配置的
  代码仓库(也可以填写其他地址覆盖);可选的分支 / 标签 / 提交会在派发
  前解析为具体提交(留空 = HEAD);构建 / 单元 / 回归命令来自表单:
  留空的阶段直接不进入图(矩阵单元格显示"—"),每个阶段按默认超时
  (1 小时)运行。可以勾选任意已启用环境的子集(默认全选),每个环境
  创建一条图。与 webhook 推送不同,每次手动派发都会记录**一条新的
  commit 行**:重跑同一 ref 时,每次尝试都有自己的矩阵行(commit
  message 携带派发时间),同一(repo, sha)的旧行会被标记为
  **superseded**(已过期)—— 仍显示在仪表板上,但置灰。

任务的 `trigger` 列记录图的创建方式 —— `0` = webhook,`1` = 手动
(自定义命令),`2` = 手动 yaml ——
并透出到任务页面和矩阵上(手动创建的图 —— `1` 或 `2` —— 带一个小的
"M" / "manual" 徽标)。

一条任务图 = 一个 root 任务加一小组子任务 DAG:

```
root (test <sha> on <environment>)
 └─ clone repositories          # 服务器克隆,经 SSH 上传 tar
     └─ build                   # 构建命令,在其 workdir 中
         ├─ unit tests          # 每阶段命令,独立超时
         ├─ regression: heat    # 每个选中的预设一个子任务
         └─ regression: poisson
```

- 每个节点都是 `tasks` 表中的一行;`kind` 列区分 root / clone / build /
  unit / regression(列表是开放的 —— 未来的类别,例如性能测试,只需
  增加一个常量和一个执行器)。依赖关系以 JSON 任务 ID 存储。
- 回归阶段在构图时展开:条目选中的每个预设(`regression.use` 减去
  `disable`;见[测试矩阵](#/docs/test-matrix))都成为名为
  `regression: <预设>` 的独立子任务,依赖 build,拥有自己的命令、
  workdir、超时和结果文件。一个条目的所有用例共享每个(环境、提交)的
  **一条**回归运行:每个用例记录自己的行(状态、备注、耗时),其结果
  文件链接到该行。
- 每个节点保存其配置的**快照**,因此后续修改 YAML 或手动重新派发都不
  影响已在运行的图。
- 子任务失败时,所有(传递地)依赖它的任务被标记为 **skipped**;仪表板
  仍会为被跳过的测试阶段显示 ✗ 单元格。完全不属于图的阶段(手动派发
  中留空的阶段命令)则显示"—" —— 它从未被请求执行。

## 执行池

- 默认 2 个 worker goroutine 原子地认领就绪的子任务并运行,每 2 秒轮询
  一次。子任务在其全部依赖完成后即为就绪。可用 `MD_BUILDER_WORKERS`
  环境变量调整;`MD_BUILDER_DISABLE_WORKER=1` 完全关闭执行池(派发仍会
  记录任务)。
- 服务器崩溃后遗留的 `running` 任务在启动时被重置为 pending 并重新
  执行。

## 每个子任务

- **clone**:*服务器* 克隆被推送提交上的代码仓库(测试输入内置于其中,
  或由代码自行获取),打包为 tarball 并经 SSH 流式传到环境(解压为
  `~/.md-builder/tasks/<sha12>/code`),随后把环境的环境设置脚本(若
  已配置)写入任务目录。远程主机**不需要 git,也不需要任何仓库访问
  权限**。
- **build**:根据配置快照生成的 bash 脚本经 SSH 流式送到环境
  (`bash -s`),在阶段的工作目录中运行构建命令
  (设置了 `build.workdir` 时为源外构建),由远程 `timeout` 命令按配置
  的超时约束(细节见[测试矩阵](#/docs/test-matrix))。构建结果会记录
  为一条 "build" 测试运行,显示在仪表板的构建矩阵上。
- **unit**:同样方式在其工作目录中运行阶段命令。阶段的完整输出流入
  任务日志;阶段可以打印一行 `MD-BUILDER-SUMMARY: <text>` 来自行声明
  一段式结论,否则存储退出码加日志尾部。结果被记为一条测试运行(见
  [仪表板与报告](#/docs/dashboard))。
- **regression: <预设>**:每个用例一个子任务 —— 预设的命令在预设的
  工作目录中、导出 `MD_CASE` 后运行;用例的结果文件被收集并链接到
  用例行。每个用例把自己的行记入共享的回归运行;单元格显示汇总。

每个阶段脚本都经过相同的导言:

```bash
export MD_COMMIT=... MD_ENV_NAME=... MD_ENV_TAGS=...
export MD_TASK_DIR=~/.md-builder/tasks/<sha12> MD_CODE_DIR=$MD_TASK_DIR/code
export MD_CASE=...                    # 仅回归用例子任务
export <条目 env 映射>
ENV_SCRIPT="$MD_TASK_DIR/md-builder-env-<hash>.sh"
if [ -f "$ENV_SCRIPT" ]; then . "$ENV_SCRIPT"
else echo "warning: env script $ENV_SCRIPT not found; continuing without it" >&2; fi
cd "$MD_CODE_DIR/<workdir>"           # 空 workdir 即代码目录
<受 timeout 约束的阶段命令>
```

环境脚本最后被 source,因此它可以覆盖已导出的变量(见
[测试环境](#/docs/environments))。每个阶段都是独立的 SSH 会话,这也
是导言对每个阶段重复执行的原因。

## 日志

每个子任务的输出增量存入 `task_logs` 表(每块一行:至少每 2 秒或每
32 KiB 一块)。前端轮询 `GET /api/tasks/{id}/log?after=<seq>` 获取新块,
因此运行中的任务可以实时跟随。日志按任务封顶 8 MiB;完整日志保存在
服务器端,界面显示尾部。

## 任务 API

- `GET /api/tasks/{id}` —— 单个任务;root 附带其子任务列表以及
  提交/环境上下文。
- `GET /api/tasks/{id}/log?after=<seq>` —— 给定序号之后的日志块
  (增量式,用于实时跟随)。

对尚未报告运行的图(排队 / 运行中 / 报告前失败),仪表板矩阵单元格会
链接到任务详情。

## 结果处理

每个测试阶段都会为仪表板记录一条运行:状态、摘要、聚合计数,以及
—— 当阶段配置了 `results` 文件时 —— 每个结果文件各自存为一条原样
artifact。图的所有子任务都完成时 root 为 done,否则为 failed。

- SSH 连接失败、构建失败或阶段命令以非零退出时,子任务进入
  **failed**(错误记录在任务及其详情页上)—— *测试本身* 的通过/失败
  体现在仪表板上,而不是任务状态里。
- **单元测试运行**在命令非零退出 **或** 结果文件解析出失败用例时为
  failed(ctest 一类的包装可能吞掉测试程序的退出码)。
- **回归用例**仅凭其命令的退出状态判定(退出码 0 → 通过;其他任何
  情况 —— 超时、SSH 失败、非零退出 —— → 失败);其 `results` 文件
  只作展示存储,不会翻转判定。运行对所有用例汇总:任一用例失败 →
  格子显示 ✗(见[测试矩阵](#/docs/test-matrix))。
- 单元测试运行只带聚合计数(总数 / 通过 / 失败 / 跳过),跨所有配置
  的结果文件求和。逐测试明细由浏览器从存储的结果文件解析(见
  [测试矩阵](#/docs/test-matrix));运行的 `taskId` 链回阶段的任务日志
  (stdout)。
- 回归运行**增量聚合**:每个用例子任务 upsert 自己的行(同一用例重跑
  会替换旧行),运行的计数/状态/摘要基于已见的所有行重新计算("3/4
  cases passed; failed: heat")。重新派发会先重置运行再落入新的用例。
- 没有自动重试:重新推送提交或重新执行派发即可重试。

### Artifact 与回归扩展

结果文件统一存在 `test_artifacts` 表中,按运行关联 —— 一次运行(或单
个用例)可以有多条 —— `case_id` 列为 0 表示运行级文件。这也是回归测试
的扩展点:回归的逐用例日志和系列(画图)数据将作为 `log` / `series`
artifact 存在同一张表里,由浏览器的"分析"视图取回展示;而逐用例的完
成情况(状态、误差值、耗时)走常规的用例结果行。

## 前置条件

- **服务器** 需要对代码仓库有读权限(读 YAML 和克隆)—— 公开仓库开箱
  即用,私有仓库使用站点设置中配置的 Project Access Token(项目访问
  令牌,见[站点配置](#/docs/site-configuration))。克隆通过 go-git 在
  进程内完成:服务器**不需要**安装 `git`。
- 每个**远程环境**需要 `bash`、`tar`、`gzip` 和 `timeout`,以及构建与
  测试命令所需的工具链。它不需要 git,也不需要仓库访问权限。
