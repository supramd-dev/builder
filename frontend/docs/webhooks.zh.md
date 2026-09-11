# GitLab webhooks

在 GitLab 项目中(Settings → Webhooks)把 webhook 指向:

```
POST https://your-server/api/webhooks/gitlab
```

并勾选 **Push events** 触发器。该端点无需认证(由 GitLab 服务器调用);
一旦配置了密钥,请通过 `X-Gitlab-Token` 请求头校验。

## 一次推送会发生什么

每个 push 事件都会记录到 `commits` 表 —— 每个提交成为测试仪表板的一列
(按 repo + sha 去重)。其他事件类型(pipeline、tag push 等)以
`status: ignored` 确认。

当站点配置中的**代码仓库**已设置且推送指向该仓库(按路径匹配)时,
派发自动启动:

1. 服务器在**被推送的提交**上读取 `md-builder.yaml`。
2. 矩阵条目按标签匹配到**已启用**的环境;每个条目创建一条任务图
   (root + clone/build/测试阶段)(见
   [Runner 与任务](#/docs/runner-strategy)和
   [测试矩阵](#/docs/test-matrix))。
3. 响应携带 `jobsCreated` / `entriesSkipped`,以及 YAML 无法获取或解析
   时的 `dispatchError` —— 无论成败,提交都会被记录。

私有仓库通过站点设置中配置的 deploy key / deploy token 处理(见
[站点配置](#/docs/site-configuration))。

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
