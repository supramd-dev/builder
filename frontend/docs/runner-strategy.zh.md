# Runner 与任务

服务器内嵌 runner 组件:任务图模型、调度器以及通往测试环境的 SSH 传输。
原先的 sshcheck 包(SSH 传输)已并入其中。

## 派发

webhook 推送(或通过 `POST /api/jobs` 手动重跑,见
[Webhooks](#/docs/webhooks))会读取被推送提交上的矩阵,按标签把条目
匹配到已启用的环境,并为每个条目创建一条**任务图**。

一条任务图 = 一个 root 任务加一小组子任务 DAG:

```
root (test <sha> on <environment>)
 └─ clone repositories          # 服务器克隆,经 SSH 上传 tar
     └─ build                   # cmake 或自定义脚本,在代码目录中
         ├─ unit tests          # 每阶段命令,独立超时
         └─ regression tests    # 每阶段命令,独立超时
```

- 每个节点都是 `tasks` 表中的一行;`kind` 列区分 root / clone / build /
  unit / regression(列表是开放的 —— 未来的类别,例如性能测试,只需
  增加一个常量和一个执行器)。依赖关系以 JSON 任务 ID 存储。
- 每个节点保存其配置的**快照**,因此后续修改 YAML 不影响已派发的图。
- 图以(提交, 环境)为键:重复推送同一提交会重排其图(删除旧子任务与
  日志、刷新快照、attempts 计数加一)而不是重复创建。
- 子任务失败时,所有(传递地)依赖它的任务被标记为 **skipped**;仪表板
  仍会为被跳过的测试阶段显示 ✗ 单元格。

## 执行池

- 默认 2 个 worker goroutine 原子地认领就绪的子任务并运行,每 2 秒轮询
  一次。子任务在其全部依赖完成后即为就绪。可用 `MD_BUILDER_WORKERS`
  环境变量调整;`MD_BUILDER_DISABLE_WORKER=1` 完全关闭执行池(派发仍会
  记录任务)。
- 服务器崩溃后遗留的 `running` 任务在启动时被重置为 pending 并重新
  执行。

## 每个子任务

- **clone**:*服务器* 克隆被推送提交上的代码仓库和测试输入仓库,打包为
  tarball 并经 SSH 流式传到环境(解压为 `~/.md-builder/tasks/<sha12>`)。
  远程主机**不需要 git,也不需要任何仓库访问权限**。
- **build**:根据配置快照生成的 bash 脚本经 SSH 流式送到环境
  (`bash -s`),在代码目录中运行 cmake(或自定义构建命令),由远程
  `timeout` 命令按配置的超时约束(细节见
  [测试矩阵](#/docs/test-matrix))。构建结果会记录为一条 "build" 测试
  运行,显示在仪表板的构建矩阵上。
- **unit / regression**:同样方式在代码目录中运行阶段命令。阶段的完整
  输出流入任务日志;阶段可以打印一行 `MD-BUILDER-SUMMARY: <text>` 来
  自行声明一段式结论,否则存储退出码加日志尾部。结果被记为一条测试
  运行(见[仪表板与报告](#/docs/dashboard))。

脚本会导出 `MD_COMMIT`、`MD_ENV_NAME`、`MD_ENV_TAGS`、`MD_CODE_DIR`
(`…/tasks/<sha12>/code`)、`MD_TEST_INPUT_DIR`(`…/tasks/<sha12>/tests`)
以及条目的 `env` 映射。

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

每个测试阶段都会为仪表板记录一条运行(状态 + 摘要 + 阶段命令报告的
逐用例结果)。图的所有子任务都完成时 root 为 done,否则为 failed。

- SSH 连接失败、构建失败或阶段命令以非零退出时,子任务进入
  **failed**(错误记录在任务及其详情页上)—— *测试本身* 的通过/失败
  体现在仪表板上,而不是任务状态里。
- 没有自动重试:重新推送提交或重新执行派发即可重试。

## 前置条件

- **服务器** 的 PATH 上需要有 `git`,且对两个仓库有读权限(读 YAML 和
  克隆)—— 公开仓库开箱即用,私有仓库使用站点设置中配置的
  deploy key / deploy token(见[站点配置](#/docs/site-configuration))。
- 每个**远程环境**需要 `bash`、`tar`、`gzip` 和 `timeout`,以及构建与
  测试命令所需的工具链。它不需要 git,也不需要仓库访问权限。
