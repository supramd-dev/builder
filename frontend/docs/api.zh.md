# 仪表板、报告与 API

## 测试仪表板

仪表板(登录后的第一个页面)呈现 build.golang.org 风格的矩阵:**每个
最近的 git 推送一行**(默认 10 行,`?commits=` 上限 50,最新在前),
**每个测试环境一列**(站点级 —— 所有用户配置的环境;停用的置灰)。
顶部页签提供四种视图:

- **All(全量)** —— 完整管线矩阵(`GET /api/dashboard/full`):每个
  单元格按展示顺序显示该 (commit, 环境) 下的三个阶段(构建、单元
  测试、回归),各自带状态与详情链接。
- **Build(构建)** —— 代码在每个环境上能否编译(点击查看构建日志)。
- **Unit tests(单元)/ Regression tests(回归)** —— 逐用例矩阵。

有已记录运行的单元格显示通过/失败计数;点击打开运行详情,包含逐用例
回归用例的子运行列表(名称、状态、简短备注、耗时)和 worker 报告的
一段式摘要。没有
运行但存在活跃任务图的单元格显示 **queued** / **running…**(任务在报告
前失败则为 ✗);完全不属于图的阶段显示"—"(它从未被请求执行)。每个
commit 行还带 **graph** 链接:该 commit 任务管线
(clone → build → 单元/回归)的依赖图,GitHub Actions 风格 —— 点击阶段
节点跳转到运行详情或实时任务日志(见 [Runner 与任务](#/docs/runner-strategy))。

当某个 commit **完全没有图**、原因是派发失败时 —— `md-builder.yaml`
读不到、内容非法、没有任何条目匹配到环境、没有配置代码仓库 —— graph
列不再显示链接,而是一个警告三角:悬停显示原因,点击打开完整文本。
该消息存放在 commit 行上(`dispatchError`,派发时写入,之后某次成功的
派发会清空它),因此不会随 webhook 响应一起消失。

手动派发的图在单元格上带一个小的 **M** 徽标,在任务页面上显示
"manual" 标签。被重跑过的手动派发行(同一提交存在更新的尝试)会保留
但置灰并带 **superseded**(已过期)标签 —— 同一提交只有最新一次尝试
是生效的。

全量矩阵响应形状:

```json
{
  "environments": [{"id": 1, "name": "cpu-node-1", "...": "..."}],
  "rows": [
    {
      "commit": {"sha": "abc123", "...": "..."},
      "taskIds": {"1": 42},
      "stages": {
        "1": [
          {"kind": "build", "runId": 7, "status": "passed"},
          {"kind": "unit", "taskId": 42, "status": "running"}
        ]
      }
    }
  ]
}
```

每个阶段要么携带已记录的 `runId`(打开运行详情),要么在运行尚未落库时
携带任务图的实时 `taskId`(`status` 为 `pending`/`running`/`failed`/
`done` 之一)。`taskIds` 将环境映射到根任务 ID,用于图链接;
`commit.dispatchError` 在派发完全没有产出图时携带记录下来的原因。

## 报告结果

结果通过 `POST /api/test-runs` 报告:

```json
{
  "environmentId": 1,
  "commitId": 7,
  "kind": "regression",
  "summary": "max relative error 3e-7 within tolerance",
  "startedAt": "2026-09-08T03:00:00Z",
  "finishedAt": "2026-09-08T03:04:00Z",
  "cases": [
    {"name": "water-tip4p-npt", "status": "failed",
     "message": "drift above threshold", "durationMillis": 4200}
  ]
}
```

- `commitId` 可以换成 `"commitSha"` + `"commitRepo"`。
- `startedAt` / `finishedAt` 可选(RFC 3339)。
- 每个回归用例都会成为所报运行的一个**子测试运行**(child test run):
  运行详情里的 `cases` 是子运行摘要列表,用例行的 `id` 即子运行的 id,
  点击即打开该用例自己的运行详情页。用例状态为 `passed` / `failed` /
  `skipped`(skipped 表示因上游阶段失败、子任务从未执行的用例)。
- 提供 `cases` 时,运行状态与计数从用例推导。没有用例时,直接存储
  显式的 `"status"`(`passed` | `failed`)、`"summary"` 和可选的聚合计数
  (`"total"` / `"passed"` / `"failed"` / `"skipped"`)—— 这是简化报告
  路径(构建运行和单元测试运行使用;单元测试的逐用例明细存在结果文件
  artifact 里,不进数据库)。
- 对同一(环境, 提交, 类别)重复报告会替换已存结果 —— API 是幂等的,
  不稳定的报告端可以安全重试。
- 删除环境会同时删除其测试运行。

`GET /api/test-runs/{id}` 返回运行详情,含 `taskId`(产出该运行的阶段
子任务,其日志即阶段的 stdout;外部上报为 0)、`rootTaskId`(所在图的
root 任务 —— 返回流水线页面的链接;外部上报为 0)、`name`/`message`
(仅子运行:预置名与备注)、`parentRunId`/`parentName`(仅子运行 ——
面包屑返回父回归运行的链接)、`cases`(子运行摘要列表:`id` = 子运行
id,以及 `name`、`status`、`message`、`durationMillis`)以及
`artifacts` —— 存储文件的引用,例如 runner 取回的 googletest 结果文件
(一次运行可以产出多个):

```json
{
  "id": 12, "kind": "unit", "status": "failed", "taskId": 77,
  "rootTaskId": 70,
  "name": "", "message": "", "parentRunId": 0, "parentName": null,
  "total": 12, "passed": 9, "failed": 2, "skipped": 1,
  "artifacts": [
    {"id": 3, "kind": "results",
     "name": "build/test_detail.xml", "size": 15832},
    {"id": 4, "kind": "results",
     "name": "build/extra.json", "size": 2101}
  ]
}
```

Artifact 归属产出它的运行:单元测试运行的结果文件挂在单元测试运行上;
回归用例的 artifact 挂在该用例自己的子运行上(父运行只聚合计数)。

`GET /api/test-artifacts/{id}` 返回单个 artifact 的原始 `content` ——
浏览器端的结果解析和后续回归的"分析"视图都从这里取数。
`GET /api/test-artifacts/{id}/download` 以文件下载(Content-Disposition
附件)形式返回同样的字节;`GET /api/test-runs/{id}/artifacts/zip` 把该
运行的全部工件 —— 自身的加上所有子运行的(回运用例在
`cases/<名称>/` 下)—— 打包成一个 zip。

## 脚本执行(交互式)

除自动任务图外,环境还接受来自 **Run command** 页面的临时命令和脚本:

- `POST /api/environments/{id}/exec` 运行原始 shell 命令。
- `POST /api/environments/{id}/script` 接受 bash 或 Python 脚本
  (`"language": "bash"` 或 `"python"`),经 stdin 流式送给远程解释器。

解释器取自脚本首行注释,它优先于声明的语言:

- `#!/usr/bin/env bash`、`#!/bin/bash` 或 `# bash` → `bash -`
- `#!/usr/bin/env python3` 或 `# python3` → `python3 -`

仅接受 bash、sh、python 和 python3。命令 60 秒超时,脚本 10 分钟。
停用的环境拒绝两者。

## 手动测试派发

**Run command** 页面还可以把用户自定义的测试作为受调度的任务图派发
(*Manual test* 标签)—— 与 webhook 相同的机制,只是各阶段命令来自
表单而不是 `md-builder.yaml`(细节见 [Runner 与任务](#/docs/runner-strategy)):

```
POST /api/jobs/manual
{
  "repo": "https://gitlab.example.com/group/code",   // 可选:默认站点配置
  "ref": "master",                                    // 可选:HEAD
  "buildCommand": "cmake . && cmake --build . -j8",   // 可选;留空 = 无构建阶段
  "unitCommand": "ctest -L unit",                     // 可选:阶段跳过
  "unitArtifacts": "build/test_detail.xml",           // 可选:工件文件
  "regressionCommand": "python3 run.py",              // 可选:阶段跳过
  "regressionArtifacts": "reg/results.json",          // 可选:工件文件
  "environmentIds": [1, 2]
}
```

`unitArtifacts` / `regressionArtifacts` 接受单个路径或路径列表 —— 一次
运行可以产出多个工件文件。

- 至少需要一个阶段命令和一个环境。
- ref 会用站点的 Project Access Token 解析为具体提交(远程 ref 列举,
  等价于 `git ls-remote`);响应为
  `{"roots": [{"taskId": 42, "environmentId": 1}, …]}` —— 每个环境一个
  root 任务,顺序与请求一致。
- 图会标记 `trigger: 1`(手动);每次派发都记录一条新的 commit 行,
  因此重跑同一 ref 会新增矩阵行,旧行标记为 superseded。

### YAML 矩阵派发

`POST /api/jobs/manual-yaml` 按需执行**webhook 流程**:对站点配置的代码
仓库的一个 ref(Manual test 页签上的一个输入框 + 一个按钮):

```
POST /api/jobs/manual-yaml
{
  "ref": "main"   // 分支、tag、短/完整 SHA;留空 = HEAD
}
```

ref 被解析(与手动派发相同的 `git ls-remote`)后,提交行按 **webhook
同样的去重方式**记录,读取并解析该提交上的 md-builder.yaml,为每个匹配
的环境创建一个任务图:

```
{
  "commitId": 7, "commitSha": "abc123…", "commitCreated": true,
  "jobsCreated": 2, "entriesSkipped": 0
}
```

- 图标记 `trigger: 2`(手动 yaml)。重复触发同一 ref 会**复用同一批**
  图(按 commit+environment 定位)并以最新快照重建 —— yaml 或环境标签
  的修改会被采纳,矩阵不会多出新行。
- 出错(ref 无法解析、yaml 非法、无匹配环境)时返回 422,响应体带
  `dispatchError` 字段,与 webhook 响应一致;若提交已解析成功,该行
  仍会被记录。

## API 端点

除非另行说明,所有端点都需要会话(cookie)。用户通过 `adduser` CLI
创建(见[快速上手](#/docs/getting-started));管理员同样在那里创建,用
`adduser -admin`。

| 方法   | 路径                            | 说明                                          |
|--------|---------------------------------|-----------------------------------------------|
| GET    | `/api/health`                   | 健康检查(无需会话)                          |
| POST   | `/api/login`                    | 认证,设置会话 cookie                         |
| POST   | `/api/logout`                   | 销毁当前会话                                  |
| GET    | `/api/me`                       | 当前用户(`id`、`username`、`email`、`role`) |
| GET    | `/api/users`                    | 列出全部账号(仅管理员)                      |
| PUT    | `/api/users/{id}`               | 修改账号:自己的,或管理员修改任意账号      |
| GET    | `/api/environments`             | 列出用户的测试环境                            |
| POST   | `/api/environments`             | 创建测试环境                                  |
| GET    | `/api/environments/{id}`        | 获取单个环境                                  |
| PUT    | `/api/environments/{id}`        | 更新单个环境                                  |
| DELETE | `/api/environments/{id}`        | 删除单个环境(及其测试运行)                  |
| POST   | `/api/environments/{id}/test`   | SSH 连通性检查                                |

环境的创建/更新请求体包含 `name`、`host`、`username`、`privateKey`、
`tags`、`description`、`enabled` 和 `envScript`(在每个阶段之前被
source 的环境设置脚本 —— 见[测试环境](#/docs/environments))。与私钥
不同(更新时留空 = 保留原值),`envScript` 省略或留空即清除脚本。两个
字段的回显行为也不同:私钥永不回显,环境脚本会原样返回(它不是机密)。
| PUT    | `/api/environments/{id}/enabled`| 启用/停用(`{"enabled": bool}`)               |
| POST   | `/api/environments/{id}/exec`   | 运行 shell 命令(`{"command": string}`)       |
| POST   | `/api/environments/{id}/script` | 运行脚本(`{"language", "script"}`)           |
| GET    | `/api/site-config`              | 站点仓库配置(`codeRepo`、`accessTokenSet`、`timezone`、`webhookToken` 仅管理员) |
| PUT    | `/api/site-config`              | 更新站点配置(access token:留空保留,`clearAccessToken` 删除;`timezone`:IANA 名称,空 = 浏览器本地) |
| POST   | `/api/site-config/webhook-token`| 轮换 webhook 密钥并返回配置(仅管理员) |
| GET    | `/api/dashboard/{kind}`         | 测试结果矩阵,`kind` = `regression` \| `unit` \| `build` |
| GET    | `/api/dashboard/full`           | 全量管线矩阵:每个 commit 与环境下的构建/单元/回归阶段,以及任务图链接 |
| POST   | `/api/test-runs`                | 报告测试运行结果                              |
| GET    | `/api/test-runs/{id}`           | 单次运行详情:用例、计数、artifact            |
| GET    | `/api/test-artifacts/{id}`      | 单个存储 artifact 的原始内容                 |
| GET    | `/api/test-artifacts/{id}/download` | 单个 artifact 以文件下载               |
| GET    | `/api/test-runs/{id}/artifacts/zip` | 一个运行的全部工件(含子运行)打成 zip |
| POST   | `/api/jobs`                     | 手动重新派发某提交的任务图(webhook 方式,读取 YAML) |
| POST   | `/api/jobs/manual`              | 派发自定义测试(仓库、ref、阶段命令、环境;无需 YAML) |
| POST   | `/api/jobs/manual-yaml`         | 派发某 ref 上的 md-builder.yaml 矩阵(按需执行的 webhook 流程) |
| GET    | `/api/jobs`                     | 最近的任务图(`?limit=`;监控;旧 job 形状)    |
| GET    | `/api/tasks/{id}`               | 单个任务;root 附带子任务列表与提交/环境上下文 |
| GET    | `/api/tasks/{id}/log?after=<seq>` | 给定序号之后的任务日志块(增量,实时跟随)   |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook 接收器(无需会话:由 `X-Gitlab-Token` 请求头认证,见 [Webhooks](#/docs/webhooks)) |

站点配置里的几个 token 在“是否可读”上不同。`accessToken` 和
`secretToken` 是只写的:接口只报告 `accessTokenSet` / `secretTokenSet`,
永不返回值本身,更新时若不传值也不传对应的 `clear…` 标志就保留原值。
`webhookToken` 是唯一的例外,会完整返回 —— 因为它需要被手工复制进
GitLab —— 且只对管理员返回,其他人拿到的该字段为空。它随站点配置一起
生成,因此永远不会为空;不能清空,只能用
`POST /api/site-config/webhook-token` 轮换(仅管理员,返回完整配置)。
webhook 端点用常量时间比较 `X-Gitlab-Token`,不匹配时在解析请求体之前
就返回 401。见 [站点配置 → Webhook 密钥](#/docs/site-configuration)。

账号相关端点正是两种角色差别所在。`/api/users` 需要管理员身份;
`/api/users/{id}` 允许改自己的账号,管理员则可以改任意账号。请求体包含
`username`、`email`、`password`(为空表示保留原密码)和 `disabled` ——
最后一项仅管理员可用,且对管理员账号和自己都会被拒绝。`role` 不属于
请求体,传了也不会生效:只有 `adduser -admin` 能创建管理员。改密码会
登出该账号的其他会话,禁用则登出全部。见
[站点配置 → 用户账号](#/docs/site-configuration)。
