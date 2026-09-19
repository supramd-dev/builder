# GitLab webhooks

在 GitLab 项目中(Settings → Webhooks)把 webhook 指向:

```
POST https://your-server/api/webhooks/gitlab
```

并勾选 **Push events**、**Tag push events** 和 **Merge request events**
触发器。

该端点无法使用会话 cookie(调用方是 GitLab 服务器),因此用站点的
webhook 密钥认证:把 Settings → Webhook 里 **Secret token** 的值复制到
GitLab webhook 自己的 **Secret token** 字段。之后 GitLab 会在每个事件
里通过 `X-Gitlab-Token` 请求头带回该值,缺失或错误的请求会在解析
请求体之前就被拒绝(401)。这个密钥随站点配置一起生成,全新安装时
就已经存在;万一泄露,可在同一个标签页里重新生成。见
[站点配置 → Webhook 密钥](#/docs/site-configuration)。

## 一个事件会发生什么

Push、tag push 和 merge request 事件都会记录到 `commits` 表 ——
每个提交成为测试仪表板的一列(按 repo + sha 去重;同一 SHA 再次被
记录时会刷新该行的事件标记)。其他事件类型(pipeline、issue 等)以
`status: ignored` 确认。

| 事件 | 被测试的版本 | 矩阵行 |
|---|---|---|
| **push** | 推送列表的 head 提交 | 事件 `push`,ref = 分支名 |
| **tag push** | 被打的 SHA(`after`) | 事件 `tag_push`,ref = 标签名(如 `v1.0`) |
| **merge request** | MR 源分支上的 `last_commit` | 事件 `merge_request`,ref = 源分支 |

Merge request 事件在 `open`、`reopen` 和 `merge` 动作时派发
(新的源状态或合并结果)。其他动作(`update`、`close`、`approved`
等)只记录不派发 —— 被测试的 SHA 并未变化。

每个提交行都记录了创建它的事件(`event` 列:`push`、`tag_push`、
`merge_request`、`manual`、`manual_yaml`);仪表板在提交旁显示一个小
徽标(tag / MR / M),手动与 webhook 触发的行一眼可辨。

当站点配置中的**代码仓库**已设置且事件指向该仓库(按路径匹配)时,
派发自动启动:

1. 服务器在**该事件的提交**上读取 `md-builder.yaml`。
2. 矩阵条目按标签匹配到**已启用**的环境;每个条目创建一条任务图
   (root + clone/build/测试阶段)(见
   [Runner 与任务](#/docs/runner-strategy)和
   [测试矩阵](#/docs/test-matrix))。
3. 响应携带 `jobsCreated` / `entriesSkipped`,以及 YAML 无法获取或解析
   时的 `dispatchError` —— 无论成败,提交都会被记录。响应是给调用方
   (GitLab 的 webhook 日志)看的;同一条消息还会存到 commit 行上,
   因此响应早已消失之后,仪表板仍然能解释该 commit 的空列 —— 见下。

**GitLab 只给 webhook 十秒的响应时间**,所以第 1 步的读取不能随仓库
大小增长。它确实不会:服务器通过代码主机的 API 索取这一个文件 ——
与 `curl` 发出的请求完全相同:

```
GET /api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=<sha>
PRIVATE-TOKEN: <Project Access Token>
```

一个小请求,十次提交还是一百万次提交,代价都一样。

这条路取不到文件时,服务端会记录原因并回退到完整克隆(更慢,且*可能*
超过十秒):

- 该提交下没有这个文件 —— 最常见的情况,即 YAML 还没提交;
- 用配置的令牌读不到该项目;
- 仓库地址没有可用的 https 形式(比如裸写 `group/code`)。

两种情况都不会丢东西:提交在开始读取之前就已入库,所以仪表板列会立刻
出现,单元格在派发完成时填入。读取本身有上限(默认 60 秒),因此仓库
服务器不可达时,结果是一条记录下来的 `dispatchError`,而不是一个永远
不返回的请求。

注意:GitLab 的 *web* 文件地址 —— 浏览器打开的那个
`/-/raw/<ref>/<path>` —— 在这里用不了:它只认浏览器会话,会忽略令牌,
直接重定向到登录页。能用令牌的是上面那条 API 路由。

中间环节不会静默丢失:从收到事件到生成任务图,每一步都会留下记录。
派发失败的推送(读不到或内容非法的 `md-builder.yaml`、没有任何条目
匹配到已启用环境)会在全量仪表板的 **graph** 列显示一个警告三角 ——
悬停看原因,点击看完整文本。如果站点没有配置代码仓库、推送根本没有
被派发,它的行上同样会写明。派发*之后*的失败 —— clone、构建、测试
—— 记录在任务和运行本身,以常规的阶段状态呈现。

私有仓库通过站点设置中配置的 Project Access Token(项目访问令牌)处理
(见[站点配置](#/docs/site-configuration))。

## 重新派发

任务图以(提交, 环境)为键:重复推送同一提交会重排其图而不是重复
创建。要在没有推送的情况下重新派发 —— 比如修改了环境标签或 YAML ——
携带会话调用 jobs API:

```
POST /api/jobs {"commitId": 7}
POST /api/jobs {"commitSha": "abc123", "commitRepo": "group/code"}
```

这会重新读取该提交上的 `md-builder.yaml`;图与原始推送一样标记为
webhook 触发。如果想用**自己指定的阶段命令**运行测试(不需要 YAML,
任意仓库、任意 ref),请使用 **Run command** 页面的手动派发或
`POST /api/jobs/manual` —— 每次手动派发都有自己的矩阵行(见
[Runner 与任务](#/docs/runner-strategy))。

`GET /api/jobs?limit=20` 列出最近的图用于监控(状态、attempts、错误)。
