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

**删除事件会被丢弃。** 分支或标签删除没有可测试的提交:GitLab 在
`after`(以及 merge request 的 `last_commit.id`)里传的是全零 SHA,
因此这类事件只以 `status: ignored` 确认,不入库 —— 否则矩阵上会永久
留下永远填不出来的格子。

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
   时的 `dispatchError` —— 无论成败,提交都会被记录。它还携带
   `graphsCancelled`:在 `fork_cancel` 模式下,本次事件丢弃了该 revision
   早先的多少张图(另外两种模式下为 0);已经跑完的、或更早的事件已经丢
   掉过的图不计入 —— 这个数字说的是本次事件做了什么。响应是给调用方
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

该提交下没有这个文件时 —— 最常见的情况,即 YAML 还没提交 —— 主机会
明说,派发以
`file not found — commit the md-builder.yaml to the repository root`
结束,不再做别的。这个失败最常见,所以最值得保持便宜。

其余在这条路由上定不下来的情况,会回退到完整克隆(更慢,且*可能*超过
十秒):

- 用配置的令牌读不到该项目;
- 仓库地址没有可用的 https 形式(比如裸写 `group/code`);
- 主机回的是一个网页而不是文件(比如重定向到登录页)—— 服务端不会把
  这种东西交给 YAML 解析器。

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

### 同一个 SHA,两个事件:先 push,后开 MR

有一种很常见的顺序会让同一个版本被触发两次:先推送分支(一个
**push** 事件),然后为它开一个 merge request。MR 的 `last_commit`
就是你刚推的那个提交,因此两个事件携带的是同一个 (repo, sha)。
实际发生的是:

- **两个事件都会派发。** push 派发一次,MR 的 `open` 再派发一次 ——
  `open`、`reopen`、`merge` 的含义是"有一个新的源状态需要测试",服务端
  不会去识别第二个事件是第一个事件的重复。两个事件,两次派发。
- **第二次派发对第一次的任务做什么,是一项设置。** **Settings → Dispatch**
  决定是重排那个任务(默认)、给新事件一个自己的任务,还是在此基础上再把
  较早的那个取消。三种模式的完整说明见
  [站点配置 → 重复 commit](#/docs/site-configuration);下面说明每种模式在
  仪表板上的表现。

**默认模式(requeue)下:**

- **仪表板上只有一列,不是两列。** `commits` 按 (repo, sha) 去重,因此
  MR 记录的是 push 创建的那一行:同一行、同一个 commit id,`event` 被
  刷新为 `merge_request`,`ref` 刷新为 MR 的源分支(消息刷新为 MR
  last-commit 的标题)。所以一个你明明用 push 推上去的提交,在矩阵上
  可能戴着 MR 徽标 —— 这是刷新 (restamp),不是多了一行。MR 并**不会**
  在推送行之外另建一行,push 原本的 `event` 也不会保留。
- **图还是同一张图。** 任务图以(提交, 环境)为键,因此第二次派发是
  重排第一次创建的图,而不是再建一张:一个 root、一列单元格,
  `attempts` 加一。
- **还在运行的任务会重新开始。** 重排会把每个节点重新武装成一个新的
  attempt —— 状态和计数全部回到 `pending` —— 并把被顶掉的那个在途
  attempt 的运行关闭为 `skipped`,摘要为
  `superseded by a new dispatch of this task`;否则没有任何东西会去关闭
  它。随后该阶段在新的 attempt 上从头再跑一遍,而被顶替的 attempt 连同
  它的日志和工件仍然留在运行页面上,作为历史可读。

所以"push 触发的任务被改标成 MR 并从头重跑"是设计行为,而不是结果丢失:
一行提交、一张图,新的 attempt 在跑,早先的 attempt 保留为历史。注意
刷新与派发是相互独立的 —— 不派发的 MR 动作(`update`、`close`、
`approved`)同样会把该行的 event 和 ref 刷新,因为无论哪种情况 SHA 都是
同一个版本。任何其他"再次记录同一 SHA"的情况也是如此:对已推送提交的
tag push,或对同一 ref 重跑手动 yaml 派发,都是这样刷新并重排。

**`fork` 模式下:** MR 会有自己的 commit 行和自己的任务图,而 push 的那些
原样不动 —— 仍在排队或仍在运行,不刷新任何字段,也不关闭任何运行。矩阵上
于是出现同一个 SHA 的两行,各有各的单元格;较早的那行标为 `older`,在它不再
变化之后置灰。两张图确实会在同一个环境上同时运行:请预期相应的负载,也预期
两份日志。

**`fork_cancel` 模式下:** 同上,并且在新的任务图存在之后,较早那些行尚未完成
的工作会被取消 —— 仍在环境上运行的阶段会被中断,其 attempt 以 `cancelled`
关闭,摘要为 `cancelled by a newer dispatch of this commit`,而已经完成的阶段
保留其结果与日志。此时响应会在 `jobsCreated` 之外携带 `graphsCancelled`:
本次事件丢弃了该 revision 早先的多少张图。什么都创建不出来的派发 —— YAML
读不到、没有条目匹配到环境 —— 不会取消任何东西:取消要等新的工作真的存在,
因此一次失败的读取永远不会杀掉正在运行的测试。
