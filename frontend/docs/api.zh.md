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

每个单元格都是该 (commit, 环境)**任务图中的一个阶段节点**,而不是
结果表里的一行。它的状态、通过/失败计数和时间戳都取自该节点最新一次
尝试;点击打开这次尝试的运行详情:逐用例结果(名称、状态、错误值、
简短备注)、runner 写下的一段式摘要、日志与工件。状态词表整体沿用
任务词表 —— `pending`、`running`、`passed`、`failed`、`skipped` ——
因此上游失败(或自身被上报为 `skipped`)的阶段读作 `skipped`(原因在
摘要里,日志中还有一行 `skipped: <原因>`),尚未被 worker 领取的阶段
读作 `pending`(界面显示
为排队中)。单元格为 **null** 时,表示该 commit 在此环境上没有任务图,
或该图根本不包含这个阶段;界面对两者都显示"—",因为在矩阵看来,从未
被请求的阶段和从未被派发的 commit 是一回事。

**回归这一列是图中的虚拟容器**:它什么都不执行,因此没有自己的尝试
和运行 —— 单元格的 `runId` 为 0,计数是各用例的汇总,点击打开任务页,
在那里每个用例都是独立节点,有自己的状态、日志、尝试与工件。正是这一点
让用例的历史在一次重新派发后仍然保留,而不是被最后一个上报的用例改写。

每个 commit 行还带 **graph** 链接:该 commit 任务管线
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
      "triggers": {"1": 1},
      "stages": {
        "1": [
          {"kind": "build", "runId": 7, "taskId": 43, "status": "passed"},
          {"kind": "unit", "taskId": 44, "status": "running"},
          {"kind": "regression", "taskId": 45, "status": "pending",
           "summary": "0/4 cases passed; 4 queued"}
        ]
      }
    }
  ]
}
```

每个阶段总会携带它所展示节点的 `taskId` —— 这正是单元格可点击的原因
—— 并在该节点有自己的尝试时携带其 `runId`;虚拟的回归容器没有运行,
因此其 `runId` 为 0,`summary` 统计用例数。`taskIds` 将环境映射到根任务
ID,用于图链接;`triggers` 映射到该 root 的触发方式(0 = webhook,
1 = 手动,2 = 手动 yaml);`commit.dispatchError` 在派发完全没有产出图时
携带记录下来的原因。

## 报告结果

结果通过 `POST /api/test-runs` 报告,报告的对象是**该次尝试所属的任务**:

```json
{
  "taskId": 44,
  "status": "failed",
  "summary": "max relative error above tolerance on 2 of 12 cases",
  "total": 12, "passed": 9, "failed": 2, "skipped": 1,
  "durationMillis": 4200,
  "startedAt": "2026-09-08T03:00:00Z",
  "finishedAt": "2026-09-08T03:04:00Z"
}
```

- `taskId` 是唯一必填字段。环境、提交和运行类别都从任务读出,因此报告
  不可能落到别的测试上 —— 而且正因为任务就是调用方需要有权处置的东西,
  接口才能分辨一次报告是否名正言顺。
- 其余字段都可选。`status` 为空时从计数推导(有任何失败即 `failed`;
  总数非零且全部 skipped 即 `skipped`;否则 `passed`);`startedAt` 为空
  时由 `finishedAt` 和 `durationMillis` 推出。`status` 不在 `passed` /
  `failed` / `skipped` 之内返回 `400`。
- 报告必须来自**有权对该测试报告的人:任务所在环境的 owner,或管理员**。
  其他账号一律 `403` —— 否则任何已登录用户只要猜到任务 ID 就能覆盖任何
  测试的结果。任务不存在返回 `404`;**虚拟任务**返回 `400` —— root 和
  回归容器不记录运行,因此报告只能指向真正执行过的子节点("virtual
  tasks do not record runs; report against their children")。已经不存在
  "外部上报"这种行了:每个运行都归属于一个任务。
- 报告关闭的是存储器中**正在进行的**那次尝试。该尝试结束之后再报告不会
  覆盖它,而是开启同一任务的**下一次尝试**,这就是重试路径:重跑的两条
  记录都会保留,并且都还能查看。响应为 `201`,返回新运行,其 `attempt`
  即本次开启的尝试序号。
- 请求体里没有逐用例列表,也没有 artifact 字段。回归用例本身就是任务,
  所以某个用例的结果就是这个节点的报告;它的文件由 runner 取回,因为
  只有 runner 持有到那台机器的连接。
- runner 根本不走 HTTP:它运行在服务进程内,通过存储层
  (`FinishAttempt`)关闭自己的尝试,写入的结果值完全一致。本接口是给
  服务之外的测试端用的(见 [Runner 与任务](#/docs/runner-strategy))。
- 删除环境会同时删除它的任务、运行与工件。

## 运行、任务、日志与工件

`GET /api/test-runs/{id}` 是**某个任务某一次尝试**的详情视图:运行本身
—— `id`、`taskId`、`attempt`、`kind`(阶段类别,`build` / `unit` /
`regression`;用例的运行沿用阶段类别,这样所有用例才落在同一回归列里)、
`status`、`summary`、各项计数、`durationMillis`、`environmentId`、
`commitId`、`startedAt`、`finishedAt` —— 加上它所属任务的身份
(`taskName`、`taskDescription`、`taskKey`、`taskKind`、`rootTaskId`
—— 返回流水线页面的链接)、它的提交与环境上下文、同一任务的其他所有
尝试 `attempts`(最新在前,形状与运行相同)以及本次尝试的 `artifacts`:

```json
{
  "id": 12, "taskId": 44, "attempt": 2, "kind": "unit",
  "status": "failed", "summary": "9/12 passed; failed: models",
  "total": 12, "passed": 9, "failed": 2, "skipped": 1,
  "durationMillis": 4200, "environmentId": 1, "commitId": 7,
  "startedAt": "2026-09-08T03:00:00Z",
  "finishedAt": "2026-09-08T03:04:00Z",
  "taskName": "unit tests", "taskDescription": null,
  "taskKey": "unit", "taskKind": "unit", "rootTaskId": 42,
  "environmentName": "cpu-node-1", "commitSha": "abc123…",
  "commitShortSha": "abc123", "commitMessage": "fix …",
  "commitAuthor": "…", "commitRepo": "group/code",
  "commitRepoUrl": "https://gitlab.example.com/group/code",
  "attempts": [
    {"id": 12, "attempt": 2, "status": "failed", "...": "..."},
    {"id": 9,  "attempt": 1, "status": "passed", "...": "..."}
  ],
  "artifacts": [
    {"id": 3, "kind": "results",
     "name": "build/test_detail.xml", "size": 15832}
  ]
}
```

任务身份与提交/环境字段在所引用的行已被删除时返回 `null` —— 运行比它
所运行的环境活得久,因此旧的运行页面仍然打得开。读取权限与矩阵本身一致,
是站点级的:任何已登录用户都可以打开任意任务、运行、日志或工件。受限的
只有写操作 —— 向任务报告(环境 owner 或管理员)以及向环境派发。读取正是
所有权模型不适用的地方,这样同事想问"这个用例为什么失败"时页面才用得上。
Artifact 归属产出它的那次尝试:单元测试运行的结果文件挂在单元测试运行上,
回归用例的文件挂在该用例自己的运行上 —— 回归容器不产出任何东西,只汇总
子节点的计数。

`GET /api/tasks/{id}` 是节点的测试侧视图。它携带任务身份(`id`、
`rootId`、`parentId`、`kind`、`nodeKey`、`name`、`description`、
`virtual`、`retired`)、状态(`status`、`summary`、`error`、各项计数、
`attempts`、`startedAt`、`finishedAt`)、运行位置(`commitId`、
`environmentId`、`tags`、`trigger`,以及解析后的 `commit` 与
`environment`);对于**真实**任务还带 `runs` 里的每一次尝试(最新在前)。
`GET /api/tasks/{id}/runs` 单独返回该列表(`{"runs": […]}`)—— 日志查看
器的尝试切换器读的就是它 —— 对虚拟任务返回 `{"runs": []}`。

**root** 返回的是图而不是尝试:

- `subTasks` —— 当前图的节点:`id`、`parentId`(嵌套关系,即界面绘制的
  那棵树)、`kind`、`nodeKey`(重新派发时用于匹配的稳定身份)、`name`、
  `description`、`virtual`、`status`、`summary`、`error`、`dependsOn`
  (调度 DAG,永远是列表,不会是 null)、各项计数、`attempts`、`runId`
  (最新一次尝试的运行,即详情链接;容器没有)以及时间戳。
- `retiredTasks` —— 早期派发定义过、后来的派发丢弃掉的节点。它们被保留
  下来,不再调度,也不参与汇总:当某个用例从 `md-builder.yaml` 里消失,
  它的历史不会从页面上消失;再次加回该用例也不会复活旧节点。

日志按尝试存储为有序的块,并以增量方式读取:
`GET /api/tasks/{id}/log?after=<seq>` 返回
`{"attempt": 2, "chunks": [{"seq": 4, "content": "…"}], "lastSeq": 9}`
—— 即 `after` 之后写入的块,这正是跟随运行中任务的查看器所要的。两个
日志端点都接受 `?attempt=<n>`,默认当前尝试,因此重试之后早先那一次仍然
可读。`after` 为负或 `attempt` 非正返回 `400`,而不是悄悄取默认值。
单次读取最多返回 1000 个块:读取方从 `lastSeq` 继续,直到某一页不满为止
—— 日志查看器靠它显示很长的尝试,下载端点也靠它逐段遍历。
`GET /api/tasks/{id}/log/download` 把整次尝试作为一个 `text/plain` 文件
附件流出 —— 文件名为 `task-<id>.log`,非当前尝试时带 `-attempt-<n>` 后缀
—— 它按批次从存储的块拼出,因此很长的构建输出不必整体放进内存。

`GET /api/test-artifacts/{id}` 返回单个 artifact 的原始 `content`,以及
它的 `id`、`runId`、`kind` 和 `name` —— 浏览器端的结果解析和回归的
"分析"视图都从这里取数。`GET /api/test-artifacts/{id}/download` 以文件
下载(Content-Disposition 附件,文件名取自源路径的 basename)形式返回
同样的字节。字节存放在对象存储里:对象已不存在时返回 `404`,后端本身
出错时返回 `502` —— 因为数据库行还在,请求本身没有问题(见
[对象存储](#/docs/object-storage))。

有两个 zip 打包工件,而且都绝不会是空归档:

- `GET /api/test-runs/{id}/artifacts/zip` —— 该**次尝试自己的**文件,
  平铺,名为 `run-<id>-artifacts.zip`。
- `GET /api/tasks/{id}/artifacts/zip` —— 整棵**子树**最新尝试的工件:
  被指定任务的文件放在归档根目录,每个后代的文件放在以它命名的目录下,
  名为 `task-<id>-artifacts.zip`。因此一个回归阶段会作为一个包含其全部
  用例的包下载,每个用例位于按节点 key 生成、并把 `:` 换成 `-` 的目录里
  (`regression-heat/file.json`),这样目录结构在重新派发后依然稳定。
  两个文件若会落到同一路径,会加上 ` (2)` 后缀而不是互相覆盖;已退休的
  节点不会包含在内:它们的文件属于更早的图形态。
- 请求的包中完全没有文件时返回 `404`(`{"error":"no artifacts"}`)——
  空 zip 看起来会像一次成功的下载。

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

这两个端点都用环境存储的私钥登录,因此 —— 与
`POST /api/environments/{id}/test` 以及对环境的任何写操作一样 ——
仅限该环境的 owner 和管理员(否则 `403`)。读取列表则不受限制,见
[测试环境](#/docs/environments)。

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
- 环境是逐个派发的:列表中途失败(环境被禁用、任务图无法构建)时返回
  `422`,响应体仍带着此前已创建的 `roots` —— 这些任务已入队并在运行,
  Run 页面会把它们连同错误一起列出,而不是报成"什么都没发生"。
- 图会标记 `trigger: 1`(手动);每次派发都记录一条新的 commit 行,
  因此重跑同一 ref 会新增矩阵行,旧行标记为 superseded。
- `environmentIds` 是显式指定环境 —— 这里不做标签匹配 —— 且每个 id 都
  必须是调用方**有权管理**的环境:该环境的 owner,或管理员。阶段命令会
  用环境所有者的私钥通过 SSH 执行,因此允许任何人指向任何人的机器,
  就成了绕过 `/api/environments/{id}/exec`、`/script` 已有规则的缺口。
  别人的 id 返回 `403`(并指出是哪个环境),不存在的返回 `422`。Run
  页面的选择框给出的正是这个集合,所以这是接口补上了界面本就遵循的约束,
  而不是新增了限制。
- **webhook** 路径有意不加这层限制:一次推送会把 yaml 条目与站点上所有
  已启用环境匹配,不管环境是谁注册的(见
  [Runner 与任务](#/docs/runner-strategy))。

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
- 请求里没有环境列表:每个 yaml 条目按 tags 与**全站所有已启用环境**
  匹配,不管环境是哪个账号注册的 —— 与 webhook 推送的匹配完全一致,
  因此一次运行可能落在别的账号的机器上。
- 出错(ref 无法解析、yaml 非法、无匹配环境)时返回 422,响应体带
  `dispatchError` 字段,与 webhook 响应一致;若提交已解析成功,该行
  仍会被记录。

## 首次启动引导

一个账号都没有的数据库没有任何可登录的东西,因此接口返回的是首次启动
引导页(见[快速上手](#/docs/getting-started))。两个端点都是**未认证**的
—— 此时还没有会话可用,也没有管理员能创建会话 —— 且只要有账号存在就
什么都不做:

```
GET /api/setup
{"required": true}
```

```
POST /api/setup
{
  "codeRepo": "https://gitlab.example.com/group/code",
  "accessToken": "glpat-…",       // 可选:留空表示公开仓库
  "username": "root",             // 站点的第一个管理员
  "email": "root@example.com",
  "password": "…"                 // 至少 8 个字符
}
```

- 账号与仓库**在同一个事务中**写入:任一字段被拒绝(`400`,规则与 `adduser`
  及账号接口一致)则什么都不写;站点已初始化后再请求同样什么都不写
  (`409`)。
- 创建的账号是**管理员**,也是该端点唯一能创建的账号:角色在这里是固定的,
  这是唯一一处角色不由 CLI 决定的地方。
- 响应与 `/api/login` 同形,会话 cookie 也以同样方式设置 —— 引导页会直接
  把新管理员登录进去。
- `accessToken` 与其他地方一样是只写的:会存储、不回显、不写日志。

## API 端点

除非另行说明,所有端点都需要会话(cookie)。除上面引导页创建的第一个
管理员之外,用户通过 `adduser` CLI 创建(见[快速上手](#/docs/getting-started));
后续管理员同样在那里创建,用 `adduser -admin`。

| 方法   | 路径                            | 说明                                          |
|--------|---------------------------------|-----------------------------------------------|
| GET    | `/api/health`                   | 健康检查(无需会话)                          |
| GET    | `/api/setup`                    | 站点是否仍需要首次启动引导(无需会话)        |
| POST   | `/api/setup`                    | 创建第一个管理员并保存代码仓库,然后登录(无需会话;已初始化则 409) |
| POST   | `/api/login`                    | 认证,设置会话 cookie                         |
| POST   | `/api/logout`                   | 销毁当前会话                                  |
| GET    | `/api/me`                       | 当前用户(`id`、`username`、`email`、`role`) |
| GET    | `/api/auth/gitlab/enabled`      | 站点是否提供 GitLab 登录(无需会话)         |
| GET    | `/api/auth/gitlab/start`        | 发起 GitLab 登录(重定向到 GitLab)          |
| GET    | `/api/auth/gitlab/callback`     | 完成登录(GitLab 回调到这里)                |
| GET    | `/api/users`                    | 列出全部账号(仅管理员)                      |
| PUT    | `/api/users/{id}`               | 修改账号:自己的,或管理员修改任意账号      |
| GET    | `/api/environments`             | 列出站点上的全部环境,每行带 `owner` 与 `canEdit` |
| POST   | `/api/environments`             | 创建测试环境(创建者即 owner)               |
| GET    | `/api/environments/{id}`        | 获取单个环境(任意已登录用户)                |
| PUT    | `/api/environments/{id}`        | 更新单个环境(owner 或管理员)                |
| DELETE | `/api/environments/{id}`        | 删除单个环境(owner 或管理员;及其测试运行)  |
| POST   | `/api/environments/{id}/test`   | SSH 连通性检查(owner 或管理员)              |
| PUT    | `/api/environments/{id}/enabled`| 启用/停用(`{"enabled": bool}`)               |
| POST   | `/api/environments/{id}/exec`   | 运行 shell 命令(`{"command": string}`)       |
| POST   | `/api/environments/{id}/script` | 运行脚本(`{"language", "script"}`)           |
| GET    | `/api/site-config`              | 站点仓库配置(`codeRepo`、`accessTokenSet`、`timezone`、`webhookToken` 仅管理员) |
| PUT    | `/api/site-config`              | 更新站点配置(access token:留空保留,`clearAccessToken` 删除;`timezone`:IANA 名称,空 = 浏览器本地;`gitlab*` 字段仅管理员) |
| POST   | `/api/site-config/webhook-token`| 轮换 webhook 密钥并返回配置(仅管理员) |
| GET    | `/api/dashboard/{kind}`         | 测试结果矩阵,`kind` = `regression` \| `unit` \| `build` |
| GET    | `/api/dashboard/full`           | 全量管线矩阵:每个 commit 与环境下的构建/单元/回归阶段,以及任务图链接 |
| POST   | `/api/test-runs`                | 关闭任务的当前尝试:按 `taskId` 报告结果(任务所在环境的 owner 或管理员) |
| GET    | `/api/test-runs/{id}`           | 某个任务的某次尝试:计数、任务身份、全部尝试、artifact |
| GET    | `/api/test-runs/{id}/artifacts/zip` | 该次尝试自己的工件打成 zip(没有工件时 `404`) |
| GET    | `/api/test-artifacts/{id}`      | 单个存储 artifact 的原始内容                 |
| GET    | `/api/test-artifacts/{id}/download` | 单个 artifact 以文件下载               |
| POST   | `/api/jobs`                     | 手动重新派发某提交的任务图(webhook 方式,读取 YAML) |
| POST   | `/api/jobs/manual`              | 派发自定义测试(仓库、ref、阶段命令、环境;无需 YAML) |
| POST   | `/api/jobs/manual-yaml`         | 派发某 ref 上的 md-builder.yaml 矩阵(按需执行的 webhook 流程) |
| GET    | `/api/jobs`                     | 最近的任务图(`?limit=`;监控;旧 job 形状)    |
| GET    | `/api/tasks/{id}`               | 单个任务;root 附带节点列表(当前 + 已退休)与提交/环境上下文 |
| GET    | `/api/tasks/{id}/runs`          | 该任务的各次尝试,最新在前(一次尝试一个运行) |
| GET    | `/api/tasks/{id}/log?after=<seq>&attempt=<n>` | 给定序号之后的任务日志块(增量,实时跟随)   |
| GET    | `/api/tasks/{id}/log/download?attempt=<n>` | 该次尝试的完整日志,以 `text/plain` 文件附件返回(`Content-Disposition`) |
| GET    | `/api/tasks/{id}/artifacts/zip` | 子树的全部最新工件打成一个 zip,后代位于以自身命名的目录下(没有工件时 `404`) |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook 接收器(无需会话:由 `X-Gitlab-Token` 请求头认证,见 [Webhooks](#/docs/webhooks)) |

环境列表**不按 owner 过滤**:派发会把 yaml entry 与所有已启用环境逐一匹配,
因此资源池是全站的,每一行对所有人都可见。每行带 `owner`(管理该环境的账号
名)和 `canEdit`(owner 与管理员为 true),前端据此把别人的行渲染为只读。
修改不属于自己的行返回 `403`;不存在的 id 返回 `404`。见
[测试环境](#/docs/environments)。

环境的创建/更新请求体包含 `name`、`host`、`username`、`privateKey`、
`tags`、`description`、`enabled`、`envScript`(在每个阶段之前被 source
的环境设置脚本)和 `allowedEnvVars`(在这台机器上允许被 md-builder.yaml
的 `variables:` 值展开的主机环境变量名单 —— 见
[测试环境](#/docs/environments))。与私钥不同(更新时留空 = 保留原值),
`envScript` 省略或留空即清除脚本。两个字段的回显行为也不同:私钥永不
回显,环境脚本会原样返回(它不是机密)。

`allowedEnvVars` 是这里唯一既不是机密也不是布尔值的项:它以一段文本提交
(名字之间用逗号、空格或换行分隔),并且空字符串是一个明确的决定 —— 一个
都不允许,因此它是普通字符串而不是"可省略"的指针形状。不传则保留已存
名单,因此保存表单时没动这个输入框就不会改写它;创建时没有"已存名单"可
保留,不传即表示内置默认名单(响应中的 `allowedEnvVarsDefault` 会报告它,
新环境会把默认名单存下来,而不是留空)。名单是**每个环境**自己的,不是
站点级的:同一站点的两台主机可以公开不同的名字。名单只有名字没有取值,
因此对所有人可见,而该行自身的权限决定谁能改 —— 其所有者或管理员。名字在
保存时校验:不是 shell 标识符、或以 `MD_` 开头的名字会被拒绝,返回 `400`
并指出是哪个。

站点配置里的几个 token 在“是否可读”上不同。`accessToken` 和
`secretToken` 是只写的:接口只报告 `accessTokenSet` / `secretTokenSet`,
永不返回值本身,更新时若不传值也不传对应的 `clear…` 标志就保留原值。
`webhookToken` 是唯一的例外,会完整返回 —— 因为它需要被手工复制进
GitLab —— 且只对管理员返回,其他人拿到的该字段为空。它随站点配置一起
生成,因此永远不会为空;不能清空,只能用
`POST /api/site-config/webhook-token` 轮换(仅管理员,返回完整配置)。
webhook 端点用常量时间比较 `X-Gitlab-Token`,不匹配时在解析请求体之前
就返回 401。见 [站点配置 → Webhook 密钥](#/docs/site-configuration)。

GitLab 登录配置位于同一端点,但仅管理员可写:`gitlabUrl`、
`gitlabClientId`、`gitlabClientSecret` 和 `gitlabLoginEnabled`。其他账号
的请求只要带上其中任意一项就返回 `403`。`gitlabLoginEnabled` 与
`gitlabClientSecretSet` 对所有人可见(登录页需要前者),而实例地址、
Application ID 与 `gitlabRedirectUri` 只返回给管理员。client secret 与
上面两个 token 遵循同样的只写规则(`clearGitlabClientSecret` 可删除)。
`gitlabUrl` 与 `gitlabClientId` 的可选性略有不同:不传就保留已存的值,
所以只想开关集成的请求不会动到凭据;显式传 `""` 才清空,而清空后集成仍
处于开启状态则返回 `400`。三项配置不全时开启集成同样返回 `400`。
`gitlabUrl` 与 `server.publicURL` 都必须是带 `http://` 或 `https://` 的
完整 URL:只写主机名会被拒绝,因为它会拼出相对的回调地址,而 GitLab
拒绝时给出的报错完全看不出原因。见
[站点配置 → GitLab 登录](#/docs/site-configuration)。

三个 `/api/auth/gitlab/*` 端点都无需会话 —— 访客正是通过它们变成账号。
`start` 设置一个短时效的 `md_gitlab_state` cookie 并重定向到实例的授权页;
`callback` 用常量时间比较回传的 `state` 与该 cookie,随后清除它,并返回
一个重定向:成功时进入控制台,被拒绝时回到登录页并附带状态词
(`pending`、`disabled`、`email_taken`、`no_email`、`denied`、
`unavailable`、`error`)。任何结果都只是带固定状态词的重定向 —— 绝不含
token、授权码,或远端返回的任何文本。回调地址由配置项
`server.publicURL` 拼出,绝不取自请求的 `Host` 请求头。

账号相关端点正是两种角色差别所在。`/api/users` 需要管理员身份;
`/api/users/{id}` 允许改自己的账号,管理员则可以改任意账号。请求体包含
`username`、`email`、`password`(为空表示保留原密码)、`disabled` 和
`approved` —— 最后两项仅管理员可用,且对管理员账号和自己都会被拒绝。
`role` 与 `source` 不属于请求体,传了也不会生效:只有 `adduser -admin`
能创建管理员,而账号的来源是既成事实。账号行还会返回 `source`(`local`
或 `gitlab`)、`approved` 与 `gitlabId`(本地账号为 0)。改密码会登出该
账号的其他会话,禁用则登出全部,撤销审批同样登出全部 —— 账号在决定作出时
即失去访问权,而不是等会话自然过期。见
[站点配置 → 用户账号](#/docs/site-configuration)。
