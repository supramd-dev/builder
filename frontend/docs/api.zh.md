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
结果(名称、状态、误差值、简短备注)和 worker 报告的一段式摘要。没有
运行但存在活跃任务图的单元格显示 **queued** / **running…**(任务在报告
前失败则为 ✗)。每个 commit 行还带 **graph** 链接:该 commit 任务管线
(clone → build → 单元/回归)的依赖图,GitHub Actions 风格 —— 点击阶段
节点跳转到运行详情或实时任务日志(见 [Runner 与任务](#/docs/runner-strategy))。

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
`done` 之一)。`taskIds` 将环境映射到根任务 ID,用于图链接。

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
    {"name": "water-tip4p-npt", "status": "failed", "errorValue": 0.02,
     "message": "drift above threshold"}
  ]
}
```

- `commitId` 可以换成 `"commitSha"` + `"commitRepo"`。
- `startedAt` / `finishedAt` 可选(RFC 3339)。
- 提供 `cases` 时,运行状态与计数从用例推导。没有用例时,直接存储
  显式的 `"status"`(`passed` | `failed`)和 `"summary"` —— 这是简化
  报告路径(构建运行使用;它们没有逐用例结果)。
- 对同一(环境, 提交, 类别)重复报告会替换已存结果 —— API 是幂等的,
  不稳定的报告端可以安全重试。
- 删除环境会同时删除其测试运行。

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

## API 端点

除非另行说明,所有端点都需要会话(cookie)。用户通过 `adduser` CLI
创建(见[快速上手](#/docs/getting-started))。

| 方法   | 路径                            | 说明                                          |
|--------|---------------------------------|-----------------------------------------------|
| GET    | `/api/health`                   | 健康检查(无需会话)                          |
| POST   | `/api/login`                    | 认证,设置会话 cookie                         |
| POST   | `/api/logout`                   | 销毁当前会话                                  |
| GET    | `/api/me`                       | 当前用户                                      |
| GET    | `/api/environments`             | 列出用户的测试环境                            |
| POST   | `/api/environments`             | 创建测试环境                                  |
| GET    | `/api/environments/{id}`        | 获取单个环境                                  |
| PUT    | `/api/environments/{id}`        | 更新单个环境                                  |
| DELETE | `/api/environments/{id}`        | 删除单个环境(及其测试运行)                  |
| POST   | `/api/environments/{id}/test`   | SSH 连通性检查                                |
| PUT    | `/api/environments/{id}/enabled`| 启用/停用(`{"enabled": bool}`)               |
| POST   | `/api/environments/{id}/exec`   | 运行 shell 命令(`{"command": string}`)       |
| POST   | `/api/environments/{id}/script` | 运行脚本(`{"language", "script"}`)           |
| GET    | `/api/site-config`              | 站点仓库配置(`codeRepo`、`testInputRepo`、`testRepoRef`、凭据设置标志) |
| PUT    | `/api/site-config`              | 更新站点配置(deploy key/token:留空保留,`clearDeploy*` 删除) |
| GET    | `/api/dashboard/{kind}`         | 测试结果矩阵,`kind` = `regression` \| `unit` \| `build` |
| GET    | `/api/dashboard/full`           | 全量管线矩阵:每个 commit 与环境下的构建/单元/回归阶段,以及任务图链接 |
| POST   | `/api/test-runs`                | 报告测试运行结果                              |
| GET    | `/api/test-runs/{id}`           | 单次运行详情,含逐用例结果                    |
| POST   | `/api/jobs`                     | 手动重新派发某提交的任务图                    |
| GET    | `/api/jobs`                     | 最近的任务图(`?limit=`;监控;旧 job 形状)    |
| GET    | `/api/tasks/{id}`               | 单个任务;root 附带子任务列表与提交/环境上下文 |
| GET    | `/api/tasks/{id}/log?after=<seq>` | 给定序号之后的任务日志块(增量,实时跟随)   |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook 接收器(无需会话;见 [Webhooks](#/docs/webhooks)) |
