# md-builder 架构说明

本文面向要读代码、改代码的人，描述 md-builder **当前**的架构：进程与分层、
数据模型、HTTP API、主要工作流程，以及开发与测试约定。文中的 `path:line`
指向仓库内位置；用户使用层面的说明在
[frontend/docs/](../frontend/docs/)（中英成对，能编译进 Web UI 的文档页），
两者视角不同：那边讲"怎么用"，这边讲"怎么长的"。

> 约定：本文描述的是**已实现**的行为。若与代码不一致，以代码为准，并顺手
> 修这里。

---

## 1. 概览

md-builder 是一个面向科学计算软件（分子动力学之类）的测试平台，灵感来自
[build.golang.org](https://build.golang.org)：**每次 push 都在一组远程机器上
跑构建与测试，把结果摊成一张 pass/fail 矩阵**（一行一次提交，一列一个环境）。

它解决的核心问题是"测试机是共享的、昂贵的外部资源"：

- 机器由**环境（environment）**描述，每台带一组 tag，测试矩阵按 tag 匹配，
  同一份 yaml 可以在不同机器上跑不同命令；
- 执行不在服务端本地，而是**通过 SSH 到目标机器**上跑一段生成的 bash 脚本；
- 结果不是只有"通过/失败"：单元测试解析 gtest 风格的用例计数，性能测试
  原样收走数据文件，回归测试是容器节点套若干用例节点。

### 1.1 技术栈

| 层 | 选型 | 说明 |
|---|---|---|
| 后端 | Go 1.26，标准库 `net/http` | 单一二进制，既是 HTTP 服务端也是 CLI |
| ORM / 数据库 | GORM v1.31；SQLite（默认，纯 Go 驱动，无 CGO）或 PostgreSQL | `github.com/glebarez/sqlite` / `gorm.io/driver/postgres` |
| 代码检出 | `go-git`（纯 Go） | 不依赖系统 git |
| SSH | `golang.org/x/crypto/ssh` | 不依赖系统 ssh 客户端 |
| 对象存储 | MinIO / S3 兼容（`minio-go`） | 工件与日志的字节都放这里，库只存索引 |
| 前端 | Vite 8 + React 19 + TypeScript 6 | hash 路由，页面按需加载 |
| 样式 | 手写 CSS（`frontend/src/index.css`） | sourcehut 风格：近单色、细边框、无阴影、无圆角 |
| 图表 | plotly.js（懒加载） | 仅在需要画图的页面下载 |
| 前端 lint / 测试 | oxlint；`node --test` | 测试跑在纯逻辑 `.ts` 模块上，不引入测试框架 |

一个刻意的限制：**整个服务端不依赖 CGO，也不依赖系统 git/ssh**。所以镜像只
需要一个 alpine 基础层，交叉编译和容器化都很轻。

### 1.2 进程模型

一个二进制、两条路：

- `md-builder`（无子命令）—— 起 HTTP 服务，同时内部起 runner 工作池；
- `md-builder adduser | seed | …` —— CLI 子命令，用完即退。

两条路共用同一套配置加载和同一个 store（见 §3.1）。HTTP 服务和 runner 池在
同一进程里，但**调度状态不放在内存里**：谁可以跑、跑到哪一步，都以数据库里的
任务表为准，进程重启后从表里恢复（`ResetStaleRunning`）。进程内确实有
内存状态——正在写日志的缓冲、正在跑的任务的取消登记表——但它们是缓存，
不是事实来源，丢了只是退化（跟随日志变成读已上传的分片），不会算错结果。

---

## 2. 仓库结构、构建与部署

### 2.1 目录

```
md-builder/
├── server/               Go 后端（一个二进制：服务端 + CLI）
│   ├── main.go           入口：子命令分发、配置加载、启动 HTTP 与 runner
│   ├── adduser.go        CLI: 建账号（唯一的建管理员途径）
│   ├── seed.go           CLI: 灌演示数据
│   ├── terminal.go       从 TTY 读密码（不回显）
│   ├── config/           服务端配置（文件 + MD_BUILDER_* 环境变量）
│   ├── store/            GORM 模型与查询：账号、会话、环境、站点配置、
│   │                     提交、任务图、运行、工件、对象存储索引、GC
│   ├── auth/             bcrypt 哈希 + 会话令牌
│   ├── api/              HTTP 处理器（按域分文件）
│   ├── runner/           调度与执行：yaml 解析、任务图构建、认领、SSH 执行、
│   │                     结果/日志/工件回报、取消、策略
│   ├── performance/      性能历史模块（自足）：报告 schema、解析、聚合、路由
│   └── storage/          对象存储抽象（MinIO 实现 + 测试用内存实现）
├── frontend/             Vite + React + TS 前端
│   ├── docs/             内嵌进应用的用户文档（中英成对，`?raw` 引入）
│   └── src/              页面、组件、纯逻辑模块与其单测
├── scripts/              端到端 API 冒烟测试、演示数据脚本
├── docs/                 本文档（开发者视角）
├── md-builder.example.yaml         测试矩阵 yaml 的完整示例（schema v3）
├── md-builder-server.example.yaml  服务端配置的完整示例
├── Dockerfile / docker-compose.yml 镜像与最小部署
└── Makefile              开发、构建、测试入口
```

### 2.2 构建与运行

```sh
make dev-frontend   # Vite 开发服务器 :5173，/api 代理到 :8080
make dev-backend    # 直接 go run，:8080
make serve          # 先建前端，再由 Go 在 :8080 提供 dist/
make build-server   # 带上 git describe 的版本号编译二进制
make test           # Go 测试 + 前端测试
make smoke-test     # 对运行中的服务跑一遍 API 冒烟（可当 CI 门禁）
make seed-demo      # 灌演示数据（提交、回归、单元测试、日志）
```

`make serve` 后前端由 Go 进程托管：`resolveDistDir` 相对工作目录找
`frontend/dist`，找不到就是纯 API 服务。**对象存储是强制的**：没有可达的
对象存储，服务端拒绝启动（`server/main.go:170`），因为工件和日志的字节只
存在那里。DSN 默认是本地 SQLite 文件 `md-builder.db`；`MD_BUILDER_DSN`
可切到 PostgreSQL。

### 2.3 容器化

`Dockerfile` 三段式：node 阶段 `npm ci && npm run build`；golang 阶段
`CGO_ENABLED=0` 静态编译，并把版本号用 `-ldflags -X main.version` 打进二进制；
运行阶段基于 alpine，装 `ca-certificates` + `tzdata`，建 uid 10001 的账号，
把前端 `dist` 拷到 `/app/frontend/dist`。镜像自带 `HEALTHCHECK`，探的是不需要
登录的 `/api/health`（依赖探针在 `/api/health/deep`，需要会话，放在容器探针
里会因为对象存储抖动而反复重启）。

`docker-compose.yml` 只跑镜像（从不构建），把服务端和 MinIO 放一起，配置全部
走 `MD_BUILDER_*` 环境变量（同一个 `x-app-env` 锚点也被 `cli` service 复用，
保证子命令打开的是同一个库和同一个桶）。host 上的 `/data/md-builder` 是 bind
mount，Linux 下需要 `chown 10001:10001`，否则 SQLite 文件建不出来，服务端
以 `open store: open db: unable to open database file` 退出——这是唯一一个
"不做也不报错、只是起不来"的坑，README 和 compose 注释里都写了。

---

## 3. 服务端运行时架构

### 3.1 启动顺序与配置

入口 `server/main.go`：`os.Args[1]` 是子命令，`adduser` / `seed` 各有自己的
实现，**其余一律当作"起服务"**。服务路径的启动顺序是有讲究的
（`server/main.go:161`）：

1. `config.Load` —— 定位并解析配置（文件 + 环境变量 + 命令行标志）；
2. `resolveListenAddr` —— 端口优先于地址里带的端口，`server.port` 缺省 8080；
3. `openObjectStorage` —— 建 S3/MinIO 客户端并确认桶存在，**失败即退出**：
   工件只存对象存储，与其在第一个工件写入时失败，不如在开始服务前就死掉；
4. `store.Open(dsn, store.WithObjects(objs))` —— 开库、迁移、注入对象存储；
5. `MigrateInlineArtifacts` —— 10 分钟上限，把老库里内联的工件搬到对象存储；
6. `StartArtifactGC` —— 仅在配了 `objectStorage.gc` 时启动；
7. `runner.NewService(store)` → 设 `Workers`/`LogLimits` → `worker.enabled` 为真
   时 `Start`；**worker 可以关掉**，那时服务端只记录任务不执行（适合 API 与
   worker 分进程部署的形态）；
8. `api.New(store)` → `Register(mux)` → `SetRunner` → `SetPublicURL`
   （publicURL 用于拼 GitLab 登录回调地址，**绝不能从请求里推导**：Host 头是
   攻击者可控的，会让回调指向别人的服务器）；
9. `performance.New(store).Register(mux, apiServer.AuthMiddleware)` —— 挂上
   性能历史这一个路由（§7.7），**只传 store 和鉴权包装器**，不传 `api` 包本身；
10. 静态资源 + SPA 回退（`resolveDistDir` 相对工作目录找 `frontend/dist`）；
11. `http.ListenAndServe`。

`import _ "time/tzdata"`（`server/main.go:24`）让时区数据库进二进制，这样
调度时间段在 scratch 容器里也能按 IANA 时区解读。

配置加载（`server/config/config.go`）：

- 文件查找顺序（`config.go:262`）：`-config` 标志 → `$MD_BUILDER_CONFIG` →
  `./md-builder-server.yaml` → `./server/md-builder-server.yaml`；
- **严格解析**：`KnownFields(true)`（`config.go:237`），写错键名直接报错而不是
  被忽略——配置文件和测试矩阵 yaml 都是这个规矩；
- 环境变量 `MD_BUILDER_*` **覆盖**文件值；值解析失败是硬错误，不是回退默认值；
- 关键键：`server.addr`/`server.port`/`server.publicURL`、`database.dsn`
  （默认 `md-builder.db`）、`dist`、`worker.enabled`/`worker.count`（默认
  启用 / 2）、`logs.partBytes`（默认 8 MiB）、`objectStorage.*`
  （`gc` 默认开，`gcIntervalHours` 默认 6）。

### 3.2 包职责与分层规则

| 包 | 负责 | 不负责（代码里守着的边界） |
|---|---|---|
| `server/config` | 把文件 + 环境变量 + 标志收敛成一个 `Config` 值，管默认值 | 不碰数据库、HTTP、对象存储；只产出一个值 |
| `server/store` | **唯一的数据访问层**：GORM 模型、全部查询与写入、调度算法 `ClaimReadyTask`、任务图落库与汇总、取消、对象存储引用与 GC。事务和不变量都在这里 | 不做 HTTP、不 SSH、不渲染脚本；不直连 GitLab，只通过注入的 `Objects()` 碰对象存储 |
| `server/auth` | 纯凭据原语：bcrypt 哈希与校验（cost 12）、随机会话令牌、7 天 TTL、输入校验 | 不知道数据库和 HTTP；持久化与 Cookie 由调用方负责 |
| `server/storage` | `Store` 接口 + MinIO 实现，以及 key/前缀的纯函数（`ArtifactKey`、`LogPrefix`、`LogKey`…） | 不认识任务模型；只搬字节、拼 key |
| `server/runner` | 编排引擎：webhook → 建图派发、yaml 抓取与解析、SSH 执行、脚本渲染、日志、结果回报 | 不提供 HTTP（由 api 调进来）；不内嵌 SQL |
| `server/api` | HTTP 传输层：路由、会话 Cookie、鉴权（`requireAuth`/`requireAdmin`）、JSON 塑形、SPA 托管 | **不持有业务不变量**：校验请求后交给 `store` 或 `runner` |
| `server/performance` | 性能历史的**自足模块**（当成插件看）：报告 JSON 的 schema 与解析、跨提交的统计聚合、`GET /api/performance` 这一个路由，全在包内 | 只出站依赖 `store` 的窄接口（`Loader`，三个方法）；鉴权由宿主以 `func(http.HandlerFunc) http.HandlerFunc` 传进来，**不 import `api`**；不认识调度、不写库、不碰对象存储客户端；删掉这个包 + `main.go` 里两行即可摘除功能 |

一条推论值得记住：**api 层不做调度逻辑**。所以当 `Runner` 为 nil（worker 被
关掉、或这是另一个进程）时，取消仍然生效——只是走 `Store.CancelSubtree`，
关不掉本地并不存在的 SSH 会话。

### 3.3 调度循环与 worker

`runner.Service.Start`（`server/runner/service.go:145`）先把上次进程留下的
`running` 任务复位，再起 N 个 `loop` 协程；每个 `loop` 是一个 2 秒的 ticker
（`pollInterval`），每轮做一件事：认领、执行。worker 数量默认 2。

认领在 store 里（`ClaimReadyTask`）：按批（`claimBatch = 64`）扫出 pending 的
真实节点，在 Go 里判依赖就绪与时间段开闭，然后条件 UPDATE 抢下来；抢到的那
个协程拿到一个**自己的子 context**，登记进 `Service.cancels`（按任务 id），
执行完注销。所以"取消一个正在跑的阶段 = 关掉它那条 SSH 会话"是精确实现的：
`Service.CancelTask` 取消该任务的 context，SSH 连接随之关闭，而**状态由 store
在同一个事务里写成 `cancelled`**——写状态是 store 的事，关会话不是。

### 3.4 超时与重试

| 场合 | 值 | 位置 |
|---|---|---|
| 阶段命令超时 | 默认 3600s，最大 14400s（4h） | `runner/config.go:25`,`:28` |
| SSH 会话相对阶段超时的富余 | +5 分钟 | `runner/execute.go:22` |
| 命令超时退出码 | 124 | `runner/execute.go:623` |
| 抓 md-builder.yaml | 60s（单文件请求 20s / 4 MiB） | `runner/service.go:31`、`runner/codehost.go:148` |
| SSH 拨号 / 单条命令 / 脚本整体 | 8s / 60s / 10m | `runner/ssh.go:25`,`:28`,`:31` |
| 上报结果写入重试 | 4 次，100ms 退避 | `runner/execute.go:555` |
| 单个工件大小上限 | 8 MiB | `runner/execute.go:30` |
| 日志分片上传 | 5 分钟 | `runner/logs.go:51` |
| 工件对象上传 | 60s | `store/objectstore.go:28` |

### 3.5 后台维护

- **工件 GC**：`StartArtifactGC` 先扫一次然后每 6 小时（可配），见 §4.6；
- **内联工件迁移**：只在启动时跑一次；
- **健康探针**：`/api/health` 是**无需登录**的存活探针（容器 healthcheck 用
  它）；`/api/health/deep` 需要在登录后访问，并发跑两个探针（`git ls-remote`
  与对象存储 `Ping`），各 10 秒上限——放在容器探针里会因为对象存储偶尔变慢
  而反复重启容器。
- `CleanExpiredSessions` 已实现但**没有生产调用点**：过期会话靠查询时过滤
  `expires_at`，不靠定期清理。

## 4. 数据模型

### 4.1 存储的打开与迁移

`store.Open(dsn, opts…)`（`server/store/store.go:150`）先 `openDB` 再 `migrate`，
迁移失败就关掉句柄返回错误。唯一的生产期选项是 `WithObjects`
（`server/store/objectstore.go:36`），把对象存储挂进 `Store.objects`。

驱动按 DSN 前缀选（`server/store/store.go:169`）：`postgres://` / `postgresql://`
走 PostgreSQL，**其余一律当作 SQLite 文件路径**（纯 Go 驱动，无 CGO）。SQLite
会追加两个 pragma（`server/store/store.go:188`）：

```
_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)
```

原因是 runner 的工作协程和 API 在并发写：没有 WAL + busy timeout，并发写会
直接 `SQLITE_BUSY`。连接池没有显式设置，用 database/sql 的默认值。

`migrate()`（`server/store/store.go:197`）在 `AutoMigrate` **之前**手写了两条
按名字删旧索引的动作——AutoMigrate 只会建不会删，而这两个旧唯一索引会挡住
新行为：

| 删掉的索引 | 为什么 |
|---|---|
| `idx_commits_repo_sha` | `(repo, sha)` 从唯一改为普通索引：手动触发可能为同一个 SHA 记多条提交 |
| `idx_test_runs_env_commit_kind_name` | 运行从 `(环境, 提交, 类型, 父, 名字)` 标识改为 `(task, attempt)` 标识 |

随后迁移的八张表（`server/store/store.go:218`）：`User`、`Session`、
`TestEnvironment`、`SiteConfig`、`Commit`、`TestRun`、`TestArtifact`、`Task`。
**旧的 `jobs` 表有意不再迁移**，老库里的残留行无害。

另有一处手写回填：`MigrateInlineArtifacts`（`server/store/artifactmigrate.go:27`，
启动时由 `server/main.go:187` 调用）把还内联在 `test_artifacts.content` 里的
工件搬到对象存储（`object_key = '' AND content <> ''`，按 id 升序，每批 100），
上传后写 `object_key`/`size` 并清空 `content`。幂等，单行失败会聚合返回但不
致命。`nextArtifactKey`（`artifactmigrate.go:75`）负责同一运行内重名工件的
去重。

### 4.2 关系是"逻辑上的"

所有 GORM 标签都没有声明 `foreignKey` / `constraint` / `onDelete`，**数据库层
没有外键约束**。级联删除全部在应用层的事务里手写（见 §4.6）。所以下面表格中
的"删除时"描述的是应用行为，不是 DDL。

### 4.3 表

#### `users` — `User`（`server/store/store.go:45`）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | int64 PK | |
| `username` | string | 唯一索引，非空 |
| `email` | string | 唯一索引，非空 |
| `password_hash` | string | 非空；bcrypt 哈希，永不明文 |
| `role` | string | 非空，默认 `user`；`admin` / `user` |
| `disabled` | bool | 非空，默认 false；禁用后不能登录，已存在的会话同时失效 |
| `source` | string | 非空，默认 `local`；`local` / `gitlab` |
| `gitlab_id` | *int64 | 唯一索引；**可空指针**，这样多个本地账号可以共存（唯一索引里 NULL 互不相等），而一个 GitLab id 只对应一个账号 |
| `approved` | bool | 非空，默认 true；false = 等管理员批准。默认值让 AutoMigrate 能给已有数据的表加这一列 |
| `created_at` | time.Time | |

两条守卫：`role` 只能由 CLI 设置（`store.go:26`），`UserUpdate`
（`store.go:118`）刻意不含 `Role`/`Source`——**任何 API 请求都改不了角色**。
`CreatePendingUser`（`store.go:92`）先插入再在事务里把 `approved` 清掉，因为
GORM 会跳过带默认值的零值字段。

#### `sessions` — `Session`（`server/store/store.go:128`）

| 列 | 类型 | 说明 |
|---|---|---|
| `token` | string PK | 随机令牌 |
| `user_id` | int64 | 索引，非空 |
| `created_at` | time.Time | |
| `expires_at` | time.Time | 非空；查询时过滤 `expires_at > now`（`store.go:357`） |

`DeleteSessionsForUser`（`store.go:376`）在禁用账号和改密码时删掉除当前会话
外的所有会话。`CleanExpiredSessions`（`store.go:383`）**已实现但没有生产调用
点**，只在测试里用——过期会话靠查询过滤，不靠清理。

#### `test_environments` — `TestEnvironment`（`server/store/environment.go:16`）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | int64 PK | |
| `owner_id` | int64 | 索引，非空；`BeforeSave` 拒绝 0 |
| `name` | string | 非空，如 `cpu-node-1` |
| `host` | string | 非空；主机名/IP，可带 `host:port` |
| `username` | string | 非空；SSH 登录用户 |
| `private_key` | string | 非空；PEM 私钥，**只写不读** |
| `tags` | string | 非空，默认空串；逗号分隔、小写化，`BeforeSave` 里经 `NormalizeTags` 规范化 |
| `description` | string | 非空，默认空串 |
| `enabled` | bool | 非空，默认 true；派发只考虑启用的环境 |
| `env_script` | text | 非空，默认空串；每个阶段命令前 source 的 bash 片段，不算机密 |
| `allowed_env_vars` | *string | text，**可空指针、刻意不设默认值**：nil（从未配置 → 用内置默认列表）与 `''`（明确什么都不允许）是两种含义 |
| `created_at` / `updated_at` | time.Time | |

删除（`environment.go:184`）在一个事务里、按块先删该环境的工件行、运行行、
任务行，最后删环境行。

#### `site_configs` — `SiteConfig`（`server/store/siteconfig.go:19`）

**单行表**：永远只有 `id = 1` 一行（`siteconfig.go:15`），靠代码保证，不靠约束。
首次访问时创建默认行并生成 webhook token（`siteconfig.go:163`）。

| 列 | 说明 |
|---|---|
| `id` | 恒为 1 |
| `code_repo` | 被测代码仓库地址，如 `https://gitlab.com/group/code` |
| `access_token` | 读仓库用的 GitLab PAT；空 = 公开仓库。**只写**，返回时脱敏 |
| `timezone` | IANA 时区，用于时间显示**和调度时间段的解读**；空 = 浏览器/服务器本地 |
| `secret_token` | 作为 `MD_SECRET_TOKEN` 传给阶段脚本。**只写**，日志里 `Redact` |
| `webhook_token` | GitLab `X-Gitlab-Token` 共享密钥；管理员可读，自动生成 |
| `gitlab_url` | OAuth 用的 GitLab 实例地址 |
| `gitlab_client_id` | OAuth App id；管理员可读 |
| `gitlab_client_secret` | OAuth App secret；**只写**，脱敏 |
| `gitlab_login_enabled` | 管理员开关；关掉后登录路由直接不可用 |
| `duplicate_commit_policy` | `requeue` \| `fork` \| `fork_cancel`；空或无法识别 → `requeue`（**未知策略绝不能悄悄取消别人的工作**，`siteconfig.go:127`） |
| `updated_at` | |

除 `id` 外都是无 GORM 标签的 string/bool，即**可空**。写入路径分三种：
`ensureWebhookToken` 只写单列（避免覆盖并发写）、`UpdateSiteConfig`
（`siteconfig.go:219`）只写名字里点到的列、`SaveSiteConfig` 整行 upsert。

#### `commits` — `Commit`（`server/store/commit.go:33`）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | int64 PK | |
| `repo` | string | 复合索引 `idx_commits_repo_sha`，非空；项目路径 |
| `sha` | string | 同上 |
| `ref` | string | 非空，默认空串；分支名 |
| `author` | string | 非空，默认空串 |
| `message` | string | 非空，默认空串；头部提交标题 |
| `event` | string | 非空，默认空串；`push` / `tag_push` / `merge_request` / `manual` / `manual_yaml` |
| `pushed_at` | time.Time | 非空 |
| `dispatch_error` | string | 非空，默认空串；为什么没产出任务图，截断到 2000 字符 |
| `created_at` | time.Time | |

`(repo, sha)` **不是唯一**。webhook 路径在应用层去重（`GetOrCreateCommit`
`commit.go:101`，命中就原地改写 `event`/`ref`/`message`）；fork 类策略则每次
插入新行（`RecordCommit` `commit.go:73`）。`PriorCommits`（`commit.go:87`）
用来找同一 `(repo, sha)` 的更早几行，供 fork_cancel 用。

#### `tasks` — `Task`（`server/store/task.go:89`）

任务图。**两张独立的结构叠在一张表上**：

- `depends_on`（JSON 数组）是**调度 DAG**——决定谁可以开始；
- `parent_id` 是**汇总树**——决定结果往哪儿滚。

父子边**不代表先后顺序**：虚拟容器不是门禁（`task.go:80`）。

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | int64 PK | |
| `root_id` | int64 | 索引；图 id。部分唯一索引 `idx_tasks_node (root_id, node_key) WHERE kind <> 'root'` |
| `node_key` | string | 非空，默认空串；节点在图内的稳定标识，重新派发时按它匹配 |
| `parent_id` | int64 | 索引，非空，默认 0；汇总树的父节点，根为 0 |
| `kind` | string | 索引，非空；`root` / `clone` / `build` / `unit` / `performance` / `regression` / `regression_case` |
| `virtual` | bool | 非空，默认 false；`root` 与 `regression` 是虚拟节点，永不执行 |
| `retired` | bool | 索引，非空，默认 false；当前 yaml 已不再定义此节点，只作历史保留 |
| `name` | string | 非空 |
| `description` | string | 非空，默认空串；每次派发都刷新 |
| `commit_id` | int64 | 索引，非空；部分唯一索引 `idx_tasks_root (commit_id, environment_id) WHERE kind = 'root'` |
| `environment_id` | int64 | 同上 |
| `tags` | string | 非空，默认空串 |
| `trigger` | int | 非空，默认 0；0 = webhook，1 = 手动，2 = 手动 yaml |
| `config` | text | 节点配置快照；虚拟节点为 `{}` |
| `window` | string | 非空，默认空串；调度时间段 `"22:00-06:00"`，空 = 任意时间 |
| `depends_on` | text | JSON 数组的任务 id，如 `[3,4]` |
| `status` | string | 索引，非空，默认 `pending` |
| `summary` | string | 非空，默认空串 |
| `error` | string | 非空，默认空串 |
| `total` / `passed` / `failed` / `skipped` | int | 非空，默认 0；汇总用例计数 |
| `attempts` | int | 非空，默认 0；派发/尝试计数，真实节点的每次尝试对应一条 `test_runs` |
| `started_at` / `finished_at` | *time.Time | 可空指针 |
| `created_at` / `updated_at` | time.Time | |

`UpsertTaskGraph`（`task.go:259`）按 `node_key` 匹配既有节点；**退休的节点
永不删除**（`task.go:420`），这样历史结果还能看。

#### `test_runs` — `TestRun`（`server/store/testrun.go:93`）

一次尝试 = 一行。虚拟节点没有运行。

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | int64 PK | |
| `task_id` | int64 | 唯一索引 `idx_test_runs_task_attempt`，非空 |
| `attempt` | int | 同上；`(task_id, attempt)` 是运行的标识 |
| `kind` | string | 索引，非空；运行类型 |
| `environment_id` | int64 | 索引，非空；从任务反范式化 |
| `commit_id` | int64 | 索引，非空；同上 |
| `status` | string | 索引，非空，默认 `pending` |
| `summary` | string | 非空，默认空串 |
| `duration_ms` | float64 | 列名 `duration_ms`，非空，默认 0（Go 字段名是 `Duration`） |
| `total` / `passed` / `failed` / `skipped` | int | 非空，默认 0 |
| `log_object_key` | string | **Go 字段名是 `LogPrefix`**；非空，默认空串；该运行日志在对象存储里的*前缀* `<prefix>/runs/<id>/log/` |
| `log_bytes` | int64 | 非空，默认 0 |
| `started_at` / `finished_at` | time.Time | 可空 |
| `created_at` / `updated_at` | time.Time | |

日志放在运行（= 一次尝试）而不是任务上：重试是新尝试、新日志，上一次尝试
保留自己的日志。列名沿用旧名 `log_object_key`，因为单对象时代的一个 key
本身就是一个合法的单对象前缀，老数据仍能被列出来。

#### `test_artifacts` — `TestArtifact`（`server/store/testartifact.go:38`）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | int64 PK | |
| `run_id` | int64 | 复合索引 `idx_test_artifacts_run_kind (run_id, kind)`，非空；精确归属（哪一次尝试） |
| `task_id` | int64 | 索引，非空，默认 0；反范式化，便于列出整个任务的产物而不用连表 |
| `kind` | string | 同上复合索引，非空；工件类型 |
| `name` | string | 非空，默认空串；来源路径/标签 |
| `object_key` | string | 非空，默认空串；对象存储 key |
| `size` | int64 | 非空，默认 0 |
| `content` | string | 非空，默认空串；**遗留的内联内容**（对象存储之前），只在迁移完成前有值 |
| `created_at` | time.Time | 非空，默认 `CURRENT_TIMESTAMP` |

**字节在对象存储，行里只有引用**（`testartifact.go:32`）。

**性能数据没有表。** 性能报告就是 `kind = 'series'` 的工件，历史图在请求时
从这些工件读出来再聚合（§7.7）；没有单独的性能表，也就没有"回填"和"两张表
对不上"的问题。

### 4.4 枚举与不变量

| 枚举 | 取值 | 位置 |
|---|---|---|
| 任务类型 | `root`(虚拟) / `clone` / `build` / `unit` / `performance` / `regression`(虚拟) / `regression_case` | `task.go:29` |
| 任务与运行状态 | `pending` / `running` / `passed` / `failed` / `timeout` / `skipped` / `cancelled` | `task.go:42` |
| 终态 | `TaskStatusTerminal` = passed/failed/timeout/skipped/cancelled | `task.go:178` |
| 触发来源 | 0 webhook / 1 manual / 2 manual_yaml | `task.go:70` |
| 运行类型 | `clone` / `build` / `unit` / `performance` / `regression` | `testrun.go:14` |
| 工件类型 | `results`(gtest XML/JSON) / `log`(保留) / `series`(保留) / `file`(原样收走，不解析) | `testartifact.go:15` |
| 提交事件 | `push` / `tag_push` / `merge_request` / `manual` / `manual_yaml` | `commit.go:20` |
| 重复提交策略 | `requeue` / `fork` / `fork_cancel` | `siteconfig.go:105` |

`RunKindForTask`（`testrun.go:32`）把任务类型映射成运行类型，其中
**`regression_case` → `regression`**：一个用例的运行类型是它所属阶段，这样
矩阵里一列就能覆盖整个回归阶段。

`AttemptStatusGatesDependents`（`testrun.go:57`）= failed/timeout/skipped：
**只有通过的尝试才放行下游**。`cancelled` 永远只由 store 写，运行端不会上报。

`server/store/graph_invariants_test.go` 用随机游走把不变量钉住：

1. 容器的状态/计数等于子节点按优先级 **failed > running > pending >
   cancelled > skipped > passed** 汇总的结果（实现里另有一条特例：若容器的
   失败全部是 timeout，容器取 `timeout`，`task.go:1233`）；
2. 真实节点的状态/计数等于其**当前尝试**对应运行的状态/计数，且该运行必须存在；
3. 不存在被取代（attempt 小于当前）却仍非终态的尝试；
4. `root.total` 等于其下活跃的非虚拟叶子数。

### 4.5 并发与一致性

**事务包住的**（都用 `s.DB.Transaction`）：`UpsertTaskGraph`（根节点 upsert +
节点 upsert + 依赖/树解析 + 退休 + `beginAttemptsTx` + `rollupTx`）、
`BeginAttempt`、`FinishAttempt`（写运行 + 任务缓存 + 替换工件 + 级联 + 汇总）、
`SkipTask`/`SkipDependents`、`CancelGraph`/`CancelSubtree`、`RollupTaskTree`、
`ResetStaleRunning`、`DeleteTasksForEnvironment`、`DeleteEnvironment`、
`CompleteSetup`、`CreatePendingUser`。`putArtifacts` 把对象上传放在**调用者的
事务里**，保证库不会引用一个没存上的对象；回滚可能留下没人引用的对象，交给
清扫回收。

**让派发幂等的唯一约束**：

| 约束 | 作用 |
|---|---|
| `idx_tasks_root (commit_id, environment_id) WHERE kind='root'` | 一对 (提交, 环境) 只有一张图；requeue 路径靠插入失败发现"并发的另一个派发先到了" |
| `idx_tasks_node (root_id, node_key) WHERE kind<>'root'` | 一个 key 一个节点 |
| `idx_test_runs_task_attempt (task_id, attempt)` | 一次尝试一行；并发插入用 `OnConflict{DoNothing}`，输的一方认领既有行 |
| `GetOrCreateCommit` | 在应用层按 `(repo, sha)` 去重，带并发插入兜底 |

**乐观认领**：`ClaimReadyTask` 按批（64 个）扫 pending 的真实节点，在 Go 里
判就绪，然后用 `status='pending' AND attempts=?` 作条件 UPDATE 认领；
`RowsAffected == 0` 表示抢输了或快照过期。同一个事务里把该节点的运行置为
`running`，再汇总。

**状态汇总在 Go 里算，不在 SQL 里**：`rollupTx` 自底向上深度优先遍历汇总树。
就绪判断也在 Go 里，因为 `depends_on` 是 JSON，不可移植到 SQL。汇总在每次
认领/上报/跳过/取消/派发的事务末尾执行。

**终态不可改写**：`cancelled` 是吸收态——对已取消的任务 `BeginAttempt` 返回
`ErrTaskCancelled`，`FinishAttempt` 拒绝为已取消的图回报。取消和取代只动
pending/running，已经跑完的节点保留状态、计数、日志与工件。

**崩溃恢复**：`ResetStaleRunning`（`task.go:1381`）把 `running` 的任务改回
`pending`（`error="reset after restart"`）、对应运行改回 `pending`，关掉已退休
节点悬着的尝试，并重算受影响的图。runner 池启动时调用
（`server/runner/service.go:146`）。

### 4.6 对象存储

**职责划分**：数据库只存引用——`test_artifacts.object_key`/`size`、
`test_runs.log_object_key`/`log_bytes`；字节全在对象存储。`test_artifacts.content`
是遗留内联路径，只为让迁移前的行仍可读。

**后端**：接口 `storage.Store`（`Put`/`Get`/`Open`/`Delete`/`List`/`Ping`/
`Describe`/`KeyPrefix`），生产实现是 `storage.MinIO`，启动时 `EnsureBucket`
确认/创建桶。`storage.Memory` **只是测试替身，不是运行期兜底**。

**key 布局**（`server/storage/storage.go`）：

```
<prefix>/runs/<runID>/<artifactKind>/<name>     工件；同名重复时追加 -N
<prefix>/runs/<runID>/log/part-<12 位零填充偏移> 日志分片，按偏移即顺序
```

工件 key 是确定性的，重报会原地覆盖。各段经 `keySegment` 清洗（路径分隔符和
控制字符换成 `_`，超过 128 字符截断并附哈希后缀）。

**生命周期与 GC —— 靠清扫，不靠删除路径**：行删除发生在事务里，在提交前删
对象会丢数据，所以删行不动对象，数据库始终是索引。`SweepOrphanObjects`
列出 `RunsPrefix`，用 `test_artifacts.object_key` 和 `test_runs.log_object_key`
（按目录）算出存活集合，删掉既没被引用又超过 1 小时的对象。`StartArtifactGC`
先扫一次然后每 6 小时（`GCInterval`），**没配对象存储时是 no-op**。

**没有对象存储时的兜底**：没有。`Store.objects == nil`（`adduser` 这类 CLI
路径）时任何工件写入都返回 `storage.ErrNotConfigured`；服务端没有可达对象
存储直接拒绝启动。

### 4.7 关系一览

```
users 1—* sessions              (sessions.user_id)
users 1—* test_environments     (owner_id)
commits 1—* tasks               每个 (commit, environment) 一张图（root 节点）
test_environments 1—* tasks
tasks 1—* tasks                 自引用：root_id 分组，parent_id 是汇总树，
                                depends_on 是调度 DAG（SQL 不解析）
tasks 1—* test_runs             (task_id, attempt) 唯一；虚拟节点没有运行
test_runs 1—* test_artifacts    (run_id)；task_id 是反范式化
test_runs.log_object_key / test_artifacts.object_key → 对象存储 key（无库级关系）
site_configs                    独立单行
```

删除只经应用层事务级联，数据库层没有 `ON DELETE`。

## 5. HTTP API

### 5.1 通用约定

- 全部挂在 `/api/` 下，JSON 进 JSON 出；出错一律是
  `{"error": "<给人看的一句话>"}` 加一个恰当的状态码。
- **鉴权用会话 Cookie**：`md_session`，7 天有效。三个包装器把权限表达清楚
  ——`requireAuth`（登录即可）、`requireAdmin`（管理员），以及"先认证、在处理
  函数里再按目标判断"的第三种（账号编辑、环境管理都是这种：一般用户能改自己
  的，管理员能改所有人的）。
- **webhook 不走会话**：它由 GitLab 调用，用 `X-Gitlab-Token` 里的共享密钥
  做常数时间比较。
- **首次安装的端点是无认证的**，因为它跑在"还没有账号"的时候；库里一有账号
  它就自己失效。所以**全新的空库不要暴露在公网**：第一个访问安装页的人就
  拿走了管理员账号。
- **只写字段**：`accessToken`（读仓库的 token）、`secretToken`、`privateKey`
  （环境 SSH 私钥）、`gitlabClientSecret` 永远不会被读回；响应里只给
  `accessTokenSet` / `secretTokenSet` / `gitlabClientSecretSet` 这类布尔量
  表示"已配置"。清空要显式请求（`clearAccessToken` 之类），因为空串的语义
  是"保持原值"，不是"删掉"。
- **不返回的字段**：`passwordHash` 永远不出现在任何响应里。`role` 会被**返回**
  （前端要据此决定是否显示管理员入口），但**任何请求都设置不了它**——建管理员
  只能走 `adduser -admin`。
- 日志里出现的凭据一律经 `Redact` 处理。

### 5.2 路由总表

#### 登录与会话

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/api/login` | 无 | 用户名 + 密码换会话 Cookie |
| POST | `/api/logout` | 无 | 删除当前会话 |
| GET | `/api/me` | 登录 | 当前账号信息 |
| GET | `/api/auth/gitlab/enabled` | 无 | 登录页探针：站点是否开了 GitLab 登录 |
| GET | `/api/auth/gitlab/start` | 无 | OAuth 第一段，带 state Cookie 重定向 |
| GET | `/api/auth/gitlab/callback` | 无 | OAuth 第二段，校验 state 后建会话 |

#### 首次安装、健康检查

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/setup` | 无 | `{"required": bool}`：库里还没有任何账号 |
| POST | `/api/setup` | 无 | 一个事务里写仓库配置 + 第一个账号（强制管理员） |
| GET | `/api/health` | 无 | 存活探针，返回 `{"status":"ok","version":...}` |
| GET | `/api/health/deep` | 登录 | 并发探 `git ls-remote` 与对象存储，各 10s 上限 |

#### 站点配置

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/site-config` | 登录 | 读配置；webhook token、GitLab URL/clientId 只给管理员 |
| PUT | `/api/site-config` | 登录 | 改配置；未出现的字段保持原值，指针字段区分"不改"与"改成零值" |
| POST | `/api/site-config/webhook-token` | 管理员 | 轮换 webhook 密钥，返回整份配置 |

#### 账号

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/users` | 管理员 | 账号列表 |
| PUT | `/api/users/{id}` | 本人或管理员 | 改用户名/邮箱/密码；禁用与批准仅管理员、仅对普通用户、不能对自己；**不能改角色** |

#### 环境

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/environments` | 登录 | 列出全部环境（共享资源池，别人的也看得见），每条带 `canEdit` |
| POST | `/api/environments` | 登录 | 新建 |
| GET | `/api/environments/{id}` | 登录 | 详情 |
| PUT/PATCH | `/api/environments/{id}` | 属主或管理员 | 修改 |
| DELETE | `/api/environments/{id}` | 属主或管理员 | 删除，连带该环境的任务、运行与工件 |
| POST | `/api/environments/{id}/test` | 属主或管理员 | SSH 连通性检查 |
| PUT/PATCH | `/api/environments/{id}/enabled` | 属主或管理员 | 启用/禁用 |
| POST | `/api/environments/{id}/exec` | 属主或管理员 | 在目标机上跑一条命令（页面的 exec 标签页） |
| POST | `/api/environments/{id}/script` | 属主或管理员 | 跑一段 bash/python 脚本（页面上的 script 标签页） |

#### Webhook

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| POST | `/api/webhooks/gitlab` | `X-Gitlab-Token` | 唯一的入口，见 §7.1 |

#### 矩阵与运行

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/dashboard/{kind}` | 登录 | `kind` ∈ `regression`/`unit`/`build`；分页：`?page=`（从 1 开始）+ `?commits=`（每页条数，1–1000，默认 200），响应带 `page`/`pageSize`/`total` |
| GET | `/api/dashboard/full` | 登录 | 每个提交行、每个环境带 build/unit/performance/regression 四个阶段与任务图链接；分页参数相同 |
| POST | `/api/test-runs` | 环境属主或管理员 | **外部上报结果**：内置 runner 之外的程序也可以回报（虚拟任务拒绝） |
| GET | `/api/test-runs/{id}` | 登录 | 一次尝试的详情（用例列表、解析出的计数、工件） |
| GET | `/api/test-runs/{id}/artifacts/zip` | 登录 | 这次尝试的全部工件打包 |

#### 工件

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/test-artifacts/{id}` | 登录 | 元数据（名字、类型、大小、所属运行） |
| GET | `/api/test-artifacts/{id}/download` | 登录 | 下载（`Content-Disposition: attachment`） |
| GET | `/api/test-artifacts/{id}/raw` | 登录 | 内联返回，供前端预览（HTML/Markdown 预览走这里） |

#### 性能历史

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/performance?days=<1..3650>&environment=<id>` | 登录 | 窗口内（按 `commits.pushed_at`，默认 180 天）全部 performance 运行的 `series` 工件，逐个按报告 schema 解析、再按（case, 指标, 环境, 提交）取中位数聚合成点；由 `server/performance` 包实现，见 §7.7 |

响应里带 `environments`（出现过的机器）、`cases`（每个 case 有哪些指标与单位）、
`points`、`ignored`（读不了或不合规的工件及原因）、`truncated`（命中数超过
2000 时的截断标记）。

#### 任务图

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/tasks/{id}` | 登录 | 节点详情（含 `window`、依赖、每次尝试） |
| GET | `/api/tasks/{id}/runs` | 登录 | 该节点的历次尝试 |
| GET | `/api/tasks/{id}/log` | 登录 | 按字节游标翻页读日志（`after` / `tail`），响应给 `lastSeq` 作为下一次的游标 |
| GET | `/api/tasks/{id}/log/download` | 登录 | 整份尝试日志，带确定的 `Content-Length` |
| GET | `/api/tasks/{id}/artifacts/zip` | 登录 | 整棵子树的工件打包 |
| POST | `/api/tasks/{id}/cancel` | 登录 | 取消该节点及其未完成的后代；返回 `{taskId, cancelled, aborted, summary}`；没有可停的东西时 409 |

#### 派发

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/jobs?limit=1..100` | 登录 | 最近的根任务列表 |
| POST | `/api/jobs` | 登录 | 按提交重派（要求完整 SHA） |
| POST | `/api/jobs/manual` | 按环境逐个授权 | 手动命令派发：`repo`/`ref`/`buildCommand`/`unitCommand`/`unitArtifacts`/`performanceCommand`/`performanceArtifacts`/`regressionCommand`/`regressionArtifacts`/`environmentIds` |
| POST | `/api/jobs/manual-yaml` | 登录 | 按 ref 跑该 ref 的 yaml 矩阵（`{"ref": ...}`） |

后两个的响应形状一致：`commitId`、`commitSha`、`commitCreated`、`jobsCreated`、
`entriesSkipped`，以及只在 fork-cancel 策略下非零的 `graphsCancelled`。

#### 静态资源

其余路径交给前端：`resolveDistDir` 找到 `frontend/dist` 就提供静态文件，
未知路径回退到 `index.html`（hash 路由，所以服务端不需要知道任何前端路由）。

### 5.3 API 层做什么、不做什么

`server/api` **不持有业务不变量**：它解析请求、判权限、塑形 JSON，然后把决定
交给 `store`（读、取消）或 `runner`（派发、日志源）。两个直接后果：

- `Runner` 为 nil 时（worker 关掉、或 API 与 worker 分进程），取消仍然工作，
  只是走 store、不关本地 SSH 会话；派发类接口则明确返回 503
  `task dispatch is not configured`。
- 上报结果的权限是"这台环境的属主或管理员"——因为上报会写任务结果，
  否则任何登录用户都能覆盖任何测试的结论。

用户视角的 API 文档（含示例与 curl）在
[frontend/docs/api.md](../frontend/docs/api.md)（中文：[api.zh.md](../frontend/docs/api.zh.md)）；
本节是开发者视角的全量清单，两者冲突时以代码为准。

## 6. 前端架构

### 6.1 构建与工具链

- Vite 8 + React 19 + TypeScript 6，插件只有 `@vitejs/plugin-react`；开发时
  `/api` 代理到 `:8080`（`frontend/vite.config.ts`）。
- **类型检查的真实入口是 `npx tsc -b`**（或 `npm run build`，它先 `tsc -b`
  再 `vite build`）：根 `frontend/tsconfig.json` 是 solution-style，只有
  `references` 指向 `tsconfig.app.json` / `tsconfig.node.json` /
  `tsconfig.test.json`，`files` 为空。所以 **`npx tsc --noEmit` 在这里什么都不
  检查**——它看到零个文件。改完东西要验证类型，用 `-b`。
- lint 用 oxlint（`npm run lint`）。
- 测试是 `node --test`（`npm test`），**没有测试框架**：node 自己剥掉类型直接
  加载 `.ts` 模块。因此可测的东西必须待在 `.ts` 里——**`.tsx` 组件跑不了**，
  这是"把纯逻辑从组件里抽出来"的主要动机（`windows.ts`、`status.ts`、
  `artifacts.ts`、`overlap.ts`、`plot/figure.ts`、`gtest.ts`、`performance.ts`、
  `pagination.ts` 都是这么来的）。
- 构建产物由 Go 进程托管（§2.2），前端不需要知道自己的部署路径。

### 6.2 应用外壳与路由

`HashRouter`（`src/App.tsx:121`），所有页面 `lazy()` 按需加载，外面套
`Suspense`。路由表：

| 路径 | 页面 | 说明 |
|---|---|---|
| `/` | `DashboardPage` | 测试矩阵 |
| `/environments` | `UserCenter` | 环境管理 |
| `/run` | `RunPage` | 手动命令执行 + 手动派发 |
| `/performance` | `PerformancePage` | 性能随提交的历史演化（每个 case 一张图） |
| `/settings` | `SettingsPage` | 站点配置（多标签页） |
| `/docs`、`/docs/:section` | `DocsPage` | 内嵌用户文档 |
| `/health` | `HealthPage` | 站点状态板 |
| `/tasks/:taskId` | `TaskPipelinePage` | 任务流水线图 + 日志 |
| `/runs/:runId` | `TestRunDetailPage` | 一次尝试的详情 |
| `*` | 重定向到 `/` | |

外壳负责：`/api/me` 决定显示登录页还是应用；库里没有账号时显示安装页；
拉一次 `/api/health` 取版本号放页脚；订阅时区变化并 bump 一个 tick 让整棵树
重渲染（`subscribeTimezone`，`App.tsx:103`）——站点时区改了以后，页面上所有
时间显示立刻跟着变，不需要刷新。

因为路由是 hash 的，**服务端不需要知道任何前端路由**，SPA 回退只需把
`index.html` 送出去（§2.2）。

### 6.3 页面

| 页面 | 主要接口 | 刷新方式 |
|---|---|---|
| `DashboardPage` | `GET /api/dashboard/{kind}`、`/full`（带 `page`/`commits`） | 按需重取（切换 kind / 翻页 / 改每页条数后） |
| `TaskPipelinePage` | `GET /api/tasks/{id}`、`/runs` | 轮询：图还活着 3s，否则 15s |
| `TaskLogView`（组件） | `GET /api/tasks/{id}/log`（字节游标） | 2s 轮询，`lastSeq` 作游标 |
| `TestRunDetailPage` | `GET /api/test-runs/{id}`、工件预览 | 3s |
| `RunPage` | `GET /api/environments`、`POST /api/environments/{id}/exec`、`/script`、`POST /api/jobs/manual`、`/manual-yaml` | 手动 |
| `UserCenter` | `/api/environments*` | 手动 |
| `SettingsPage` | `/api/site-config`、`/api/users`、`/api/health/deep` | 手动 |
| `HealthPage` | `GET /api/health/deep` | 手动 |
| `PerformancePage` | `GET /api/performance` | 手动（窗口/机器切换时重取） |
| `DocsPage` | 无 | 静态 |

任务流水线页是交互最重的地方：把任务图按依赖关系布局成图
（`graphLayout.ts`），可以点任一节点看它的尝试、日志和工件，可以取消。

### 6.4 共享模块

| 模块 | 职责 |
|---|---|
| `src/api.ts` | **所有** 请求的唯一出口：`api<T>(path, init)` 包 `fetch`，非 2xx 抛 `ApiError`（带状态码与服务端给的 `error` 文案）；所有请求/响应类型都定义在这里 |
| `src/status.ts` | 状态到展示（文案、颜色）的映射，让矩阵、图、运行页对同一个状态说法一致 |
| `src/timezone.ts` | 站点时区：从 `site_configs.timezone` 来，缓存在 localStorage，提供 `siteTimezone()` / `applySiteTimezone()` / `subscribeTimezone()` |
| `src/windows.ts` | 调度时间段的前端镜像：解析、判断开闭、生成 `22:00–06:00` 这样的标签与说明；时钟按站点时区算 |
| `src/overlap.ts` | 重复提交策略的文案与选项 |
| `src/gtest.ts` | gtest 风格输出的前端解析（与后端的 `runner/gtest.go` 对应） |
| `src/pagination.ts` | 分页算术：把请求的页码夹到 `[1, 页数]`、算出当前页的"第几条到第几条 / 共几条"。服务端也会夹，两边保持一致，所以"第 9 / 7 页"这种自相矛盾的显示不会出现 |
| `src/artifacts.ts` | 工件类型的判断与预览能力（能不能预览、按什么方式预览） |
| `src/markdown.tsx` | Markdown 渲染（文档页与工件预览共用） |
| `src/performance.ts` | 性能页的**纯**图模型：从 `points` 归纳出指标下拉项（带单位与"越高越好/越低越好"）、按 case + 指标 + 机器切成 trace、算坐标轴标题与悬停文案、格式化数值。组件里只剩渲染 |
| `src/plot/` | plotly 封装：`PlotSection.tsx`、`PerformanceChart.tsx` 各自**懒加载** `react-plotly.js`（包很大，只有真要画图时才下载），`figure.ts` 放纯逻辑——从 `*.plot.json` / `*.plotly.json` 工件还原 figure 配置、算高度，因而可被 `node --test` 覆盖 |

`PlotSection` 目前只有 `TestRunDetailPage` 用：如果一次运行带了
`.plot.json` / `.plotly.json` 工件，就以内嵌交互图呈现。`PerformanceChart`
只有 `PerformancePage` 用，一个 trace 一台机器；traces 为空时它连 plotly 都
不去加载，直接显示一句提示。

**样式是手写的**（`src/index.css`，文件头写着 "No CSS framework is used"）：
sourcehut 风格，系统字体、零圆角、细边框、无阴影，配一套 `prefers-color-scheme`
的暗色主题。（README 里"Tailwind CSS v4"那句是过时的说法，代码里没有 Tailwind。）

### 6.5 两条数据流

**打开矩阵页看到一行在跑**：`DashboardPage` 挂载 → `api('/api/dashboard/full')`
→ 每个提交行、每个环境拿到四个阶段格子的状态；正在跑的行显示实时任务状态，
点进去跳到 `/tasks/:taskId`。

**打开一个任务跟着日志**：`TaskPipelinePage` → `api('/api/tasks/{id}')` 拿到图
与各节点状态（3s 或 15s 轮询）→ 选中一个节点打开 `TaskLogView` → 它按字节
游标每 2s 拉 `/api/tasks/{id}/log?after=<lastSeq>`，把新增的部分追加到视图；
日志读的是服务端的内存缓冲（§7.5），所以还在跑的阶段是近实时的。

### 6.6 加东西时的约定

- **加一个页面**：`src/<Name>Page.tsx` → 在 `App.tsx` 里 `lazy()` 引入 →
  加 `<Route>` → 需要的话在 `NavBar` 上加 `navLink`。可测的逻辑抽到 `.ts`
  模块里，别留在 `.tsx`。
- **加一个接口调用**：类型定义和 `api<T>()` 调用都写在 `src/api.ts`，组件里
  不直接 `fetch`。
- **加一篇用户文档**：`frontend/docs/<name>.md` + `<name>.zh.md` 成对，
  在 `DocsPage.tsx` 顶部 `?raw` 引入并登记到 `STRINGS`/列表里，页内链接用
  `#/docs/<name>` 这种 hash 形式。
- **加一个前端单测**：`src/<module>.test.ts`，用 `node:test` + `node:assert`，
  跑 `npm test`；不需要 npm install，也不需要框架。

## 7. 主要工作流程

### 7.1 一次 push 怎么变成矩阵上的一行

```
GitLab ──POST /api/webhooks/gitlab──▶ 校验 X-Gitlab-Token
                                        │
                         push / tag_push / merge_request
                                        │
                            记录提交行（去重或新行）
                                        │
                    仓库匹配？ ── 否 ──▶ 记 dispatch_error，结束
                                        │ 是
                    抓取该 commit 的 md-builder.yaml
                                        │
                        解析（严格，schema v3）
                                        │
              按 tag 给每个矩阵条目挑一台环境 ──▶ 每对 (提交, 环境) 建图
                                        │
                          任务图落库（幂等，可重派）
                                        │
                             调度池认领 → SSH 执行
```

- **入口**（`server/api/webhook.go:88`）：只接受 POST，用常数时间比较
  `X-Gitlab-Token`，请求体上限 1 MiB；`push` / `tag_push` / `merge_request`
  三类事件处理，`merge_request` 只在 open/reopen/merge 动作时派发；分支删除
  （全零 SHA）直接丢弃。
- **提交行**（`server/store/commit.go:73`）：`(repo, sha)` **不是唯一约束**，
  去重发生在应用层，且由站点策略决定（§4.3 的 `duplicate_commit_policy`）：
  `requeue` 命中已有行并原地改写 `event`/`ref`/`message`；`fork` /
  `fork_cancel` 每次都插新行——"同一个 revision 被推了两次"在现实里很常见
  （先推分支再开 MR、给老 commit 打 tag、webhook 重投）。
- **是否派发**：把事件里的 `path_with_namespace` 与站点配置的代码仓库比对，
  不匹配就只记一行 `dispatch_error`，不建图。
- **抓 yaml**（`server/runner/tasks.go:111`）：优先用 GitLab 的文件 API 取单个
  文件，失败或非 GitLab 时回退到 go-git 全量 clone（`runner/codehost.go:158`）。
  解析是严格的：schema 版本 3，未知键报错，窗口等字段在解析期校验。
- **选环境**（`runner/config.go:748`）：在启用的环境里挑**标签集最小且能覆盖**
  该条目标签的那台，同名按名字排序定胜负；没有匹配的环境，这个条目就没有图。
- **建图**（`runner/graph.go:78`）：`clone` 恒为 0 号节点；`build` 可选（命令为
  空则不建）；`unit` 与 `performance` 是**兄弟节点**，都依赖 build（没有 build
  时依赖 clone）；`regression` 是虚拟容器，其下每个用例一个节点。依赖用
  `TaskSubPlaceholderBase + i` 这种占位 id 表达，落库时再解析成真实任务 id。
- **落库**（`store.UpsertTaskGraph`）：按 `node_key` 匹配既有节点，因此**同一个
  图可以重派**：老节点被复用、尝试号 +1、当前 yaml 里已经没有的节点标记为
  `retired` 而**永不删除**（历史结果仍可查）。

### 7.2 手动触发

两条路，都在 `POST /api/jobs` 下：

- **手动命令**（`/api/jobs/manual`）：按环境逐个授权（`canManageEnvironment`），
  交给 `DispatchManual`。它**每次记一条新的提交行、不做去重**——手动触发就是
  想立刻再跑一遍。命令来自请求体，构造成与 yaml 一致的结构后再走同一条建图
  路径。
- **手动 yaml 触发**（`/api/jobs/manual-yaml`）：给一个 ref，走
  `DispatchForRef`，等于"假装收到了这个 ref 的 push"，同样遵守重复提交策略。
- 重派（`/api/jobs` 上的重新触发）要求**完整 SHA**（`IsFullSHA`），避免把
  分支名当作不可变的提交标识。

手动派发**不豁免任何规则**：时间段照样生效，依赖关系照样生效。

### 7.3 一个节点的一生

1. **认领**：调度池每 2 秒扫一批 pending 的真实节点，判依赖是否都已通过、
   时间段是否开着，然后乐观 UPDATE 抢下（`status='pending' AND attempts=?`），
   同一个事务里把该次尝试的运行置为 `running` 并重算汇总。
2. **执行**：`ExecuteTask` 按类型分派（`runner/execute.go:39`）。
   `clone` 用 go-git 在服务端检出、打 tar 流、经 SSH 解到目标机
   `$HOME/.md-builder/tasks/<sha12>`；其余阶段是在目标机上渲染并执行一段 bash：
   共享导言 + 站点级环境脚本 + 内置变量与 yaml 变量 + `cd` 到工作目录 +
   逐条命令（每条各自套一层 `timeout`，用 `&&` 串起来，任何一条失败即中断）。
3. **收工件**（`runner/execute.go:411`）：`results` 类走 gtest 解析出用例计数；
   `series` / `file` 类**原样收走不解析**；单个文件上限 8 MiB。
4. **回报**（`store.FinishAttempt`）：一个事务里写运行行、刷新任务上的计数缓存、
   上传并记录工件、按"只有通过才放行下游"的规则把下游标记为 `skipped` 或放行、
   最后重算整棵汇总树。
5. **汇总**：容器（回归阶段、根）的状态与计数由子节点按优先级汇总而来；根节点
   的 `total` 等于其下活跃的非虚拟叶子数。于是"矩阵上的一行"= 根节点，
   "一列"= 一个环境。

一次尝试的失败不是终点：重派会给同一个节点开下一次尝试（`attempts` +1），
上一次的日志与工件留在那次运行上。

### 7.4 取消与终止

`POST /api/tasks/{id}/cancel`（`server/api/tasks.go:152`）：任何登录用户都可以
停掉任何未完成的节点。语义是"停掉这个节点及其之下的一切"：

- 只动未完成的部分——已经跑完的节点保留状态、计数、日志和工件，**停止的动作
  不改写已经发生的事实**；
- 有本地 runner 时，store 先写状态，再逐节点 `CancelTask` 关掉进程内那条 SSH
  会话；没有 runner 时只写状态（分布部署下，跑它的那个进程自己会感知）；
- 停掉一个上游会让下游永远等不到，所以同一事务里顺依赖反边把后继标成
  `skipped`，理由写明上游是谁——这条级联与失败路径共用；
- 什么都没停到时返回 409。

另一条终止路径来自站点策略：`fork_cancel` 在新事件到达时取消同一
`(repo, sha)` 更早的图（`runner/policy.go:34`），并且是在**新图建好之后**才
取消：一次失败的读取不该成为正在跑的测试消失的理由。

### 7.5 日志

日志存在**运行**上（一次尝试一份），不写在数据库里：

- 写：`LogWriter` 先在内存里缓冲，攒够 `logs.partBytes`（默认 8 MiB）就作为一个
  分片上传，key 是 `.../log/part-<12 位零填充字节偏移>`——**偏移就是顺序**，列
  目录即可还原整份日志；同时把前缀记到运行行上，供 GC 判断存活。
- 读：`OpenLogSource` 优先读**活着的内存缓冲**（这样跟随是实时的），进程里
  没有这个任务的写入者时回退到已存的分片。因此 API 与 worker 分进程部署时，
  跟随退化为"读已上传的分片"。
- HTTP：`GET /api/tasks/{id}/log` 支持按字节游标翻页（`after`/`tail`），返回
  `lastSeq` 作为下一次的游标；`GET /api/tasks/{id}/log/download` 取整份尝试的
  日志。

### 7.6 崩溃恢复

进程被杀掉后，会有任务的 `running` 状态悬在数据库里。调度池启动时
`ResetStaleRunning`（`store/task.go:1381`）把它们改回 `pending`（写一句
`reset after restart` 作为理由）、把对应运行改回 `pending`、关掉已退休节点悬着
的尝试，并重算受影响的图。日志侧 `LogWriter.resume` 会接着已存分片往下写，
并在日志里留一句"服务重启过"，而不是从零覆盖。

### 7.7 性能历史的读取与聚合路径

性能数据**没有自己的表**：真相就是那些 `series` 工件本身，历史图是读出来的
视图。

写侧与别的阶段一样（§7.3）：`performance` 阶段的命令跑完，按
`performance.artifacts` 把文件作为 `series` 工件上传，**runner 不解析它们**。
所以换一套报告格式不需要动 runner，只需要动下面第 3 步。

读侧三步全在 `server/performance` 包内（§3.2）：

1. **选**——`GET /api/performance?days=<n>&environment=<id>` 触发一条 SQL
   （落在 `store`：`ListPerformanceObservations`）：`test_artifacts`
   ⋈ `test_runs` ⋈ `commits` ⋈ `test_environments`，条件是
   `a.kind = 'series'` 且 `r.kind = 'performance'` 且提交时间在窗口内；
   顺带取出提交（sha / 时间 / 信息）与环境（id / 名字）。最近 2000 条为限，
   超出则把 `truncated` 置真，页面显示一句提示。
2. **读**——并发 8 路去对象存储取内容。**先看大小**（> 1 MiB 不读，直接进
   `ignored`），读失败或解析失败的也进 `ignored` 并带上原因，页面把它们列在
   一个可展开的小节里。反过来，**没有 `schema` 键的 JSON 静默跳过**（普通日志、
   别的 plotly 工件都会命中这条）——否则一个不相关的工件就能把整页变成一片
   警告。
3. **算**——按（case, 指标, 环境, 提交）分组：同一组内的多次测量（重跑、同一
   提交的多个任务）取**中位数**，并记下样本数。点的 x 取报告里的 `date`，y 取
   值，悬停文案写明"N 次测量的中位数"。`better` 只用来挑坐标轴标题里的
   "越高越好 / 越低越好"，**不翻转发散点**：原始数值保持原样，解读留给看的人。

报告 schema 本身（`schema: 1` 是"这是报告"的标记、`date` 是 x 轴、每个指标都是
带 `value`/`unit`/`better` 的对象、上限与忽略规则）写在
[frontend/docs/test-matrix.md](../frontend/docs/test-matrix.md) 的
"Performance artifacts" 一节，那里是给写用例的人看的。

摘掉这个包，性能阶段照样跑、工件照样存、下载照样能下，只是历史图没了。

## 8. 开发、测试与约定

### 8.1 改完代码要做的事

```sh
cd server && gofmt -l . && go vet ./... && go test ./... -count=1
cd frontend && npx tsc -b --force && npm test && npm run build && npm run lint
```

几个容易踩的点：

- `npx tsc --noEmit` **等于没跑**（见 §6.1），用 `tsc -b`；
- `gofmt -l` 里 `server/runner/stages.go` 是历史遗留的未格式化文件，与你的改动
  无关时不必管，但别把它算作"我改干净了"；
- 前端的 `node --test` 只覆盖 `.ts`，`.tsx` 里的东西改了只能靠 `tsc -b` 和
  手动验证。

除此之外还有两个更重的验证手段：

- `make smoke-test`（`scripts/api-smoke.sh`）：对着运行中的服务把整个 API 面
  过一遍——鉴权门禁、首次安装、登录登出、环境 CRUD（含标签）、连通性测试、
  命令与脚本执行、启停门禁、webhook 记录、派发接口、结果上报、矩阵与运行
  详情、删除（含运行清理）。**幂等**，可以反复跑，失败即非零退出，可当 CI
  门禁。
- `make seed-demo`：造演示数据（提交、构建/单元/回归运行、日志），用来在
  没有真 GitLab 和真测试机的情况下看页面。

### 8.2 测试的写法

- Go 测试与代码同目录、同包（`*_test.go`），表驱动为主。
- 外部依赖在接口处被替身换掉：对象存储有 `storage.Memory`，SSH 有
  `runner.Execer` 接口（测试注入假实现），yaml 抓取有 `YAMLFetcher` 接口。
  因此绝大多数测试不碰网络、不碰真实机器。
- `store/graph_invariants_test.go` 是随机游走式的性质测试：随机地认领、上报、
  重试、级联、跳过、汇总、重派、取消，**每一步之后都重新检查 §4.4 的四条
  不变量**。改调度或汇总逻辑时，这是最有价值的一道网。
- 前端测试只测纯逻辑模块（`node:test` + `node:assert`）。

### 8.3 立的规矩

这些是代码里反复出现、且被注释明确写下来的取舍，改代码时应沿用而不是绕过：

1. **写状态是 store 的事，执行是 runner 的事。** 结束一次尝试要从一个入口进去
   （`FinishAttempt`），它在一个事务里把运行、任务缓存、工件、下游放行与汇总
   一起落定。绕过它单独改状态会破坏不变量。
2. **只写字段永远只写。** `accessToken`、`secretToken`、`privateKey`、
   `gitlabClientSecret` 不返回，日志里经 `Redact`。清空要显式请求。
3. **解析严格。** 服务端配置和测试矩阵 yaml 都用 `KnownFields(true)`：写错键名
   报错，而不是被默默忽略。调度时间段在解析期校验，理由写在 §7 里——一个
   静默失效的窗口，要等一夜才知道。
4. **不删历史。** 任务节点退休而不删除，重派开新尝试而不覆盖旧尝试，已经跑完
   的结果不因取消而改写。"停止"只作用于尚未完成的部分。
5. **幂等与并发靠数据库约束表达**：部分唯一索引 + `OnConflict DoNothing` +
   条件 UPDATE，而不是靠进程内的锁（同一进程内当然也有锁，但正确性不依赖它）。
6. **迁移分三类**：`AutoMigrate` 加表加列；手写按名字删旧索引（AutoMigrate
   只建不删）；一次性幂等回填（内联工件搬到对象存储）。数据不删。
7. **提交信息用英文长正文**，讲清楚"为什么"，而不只是"改了什么"；用户文档
   （`frontend/docs/`）中英成对，改一篇要改两篇。
8. **默认不信任 Host**：需要绝对 URL 的地方（GitLab 回调）用配置里的
   `publicURL`，不从请求头推导。
9. **能自成体系的功能做成自足模块**：`server/performance` 是范本——对外只声明
   它需要的窄接口（`Loader`）与一个鉴权包装器类型，自己不 import `api`、不
   碰调度，宿主在 `main.go` 里一行 `Register`。这样"删掉这个功能"是一次删包，
   而不是在 `api` 里翻找散落的代码。

---

## 附录 A：术语

| 词 | 在代码里的名字 | 含义 |
|---|---|---|
| 站点 | `SiteConfig` | 整个部署的配置，单行 |
| 环境 | `TestEnvironment` | 一台跑测试的远程机器，带 tag、SSH 凭据、env 脚本 |
| 提交行 | `Commit` | 一次 push/tag/MR/手动触发在矩阵上的一行 |
| 任务图 | `tasks` 表里的 `root` 子树 | 一次 (提交, 环境) 的完整流水线 |
| 节点 | `Task` | 图里的一个格子：clone/build/unit/performance/regression_case |
| 虚拟节点 | `Task.Virtual` | `root` 与 `regression` 容器，不执行、只汇总 |
| 依赖 | `Task.DependsOn` | 调度 DAG：谁必须通过谁才能开始 |
| 汇总树 | `Task.ParentID` | 结果往上滚的树；父子不代表先后 |
| 尝试 | `Task.Attempts` | 同一个节点第几次被跑（重派会 +1） |
| 运行 | `TestRun` | 一次尝试的执行记录，日志挂在它身上 |
| 工件 | `TestArtifact` | 运行产出的文件：`results` / `log` / `series` / `file` |
| 调度时间段 | `Task.Window` | 只允许在此区间内开始执行；空 = 任意时间 |
| 退休 | `Task.Retired` | 当前 yaml 已不定义该节点，只留历史 |
| 重复提交策略 | `duplicate_commit_policy` | 同一个 revision 再次到达时：requeue / fork / fork_cancel |
