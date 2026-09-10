# 快速上手

## 账号

平台**没有注册界面**。用户通过服务器上的 `adduser` CLI 子命令创建;
Web 界面只负责登录。

```sh
# 交互式输入密码(隐藏,从终端读取):
go run ./server adduser -username alice -email alice@example.com

# 或者直接传入密码:
go run ./server adduser -username alice -email alice@example.com -password 's3cret!'

# 选择其他数据库:
go run ./server adduser -username alice -email alice@example.com -dsn 'postgres://...'
```

会话以 64 位随机十六进制 token 的形式存放在数据库中,7 天后过期。
密码使用 bcrypt(代价 12)哈希存储。

## 演示数据

在没有真实 git 服务或 SSH 节点时,可以先灌入演示数据,再浏览各个
仪表板(用户 `demo` / `demo-pass-123`,三个模拟环境、五次推送、
回归/单元/构建运行报告,以及两个带日志的已完成任务图):

```sh
make seed-demo          # 或:go run ./server seed
make seed-demo FORCE=1  # 重建演示任务图
```

然后(重新)启动服务器,用 `demo` 登录。

## 数据库选择

DSN 优先取自环境变量 `MD_BUILDER_DSN`,未设置时默认使用本地 SQLite
文件 `md-builder.db`。

```sh
export MD_BUILDER_DSN='postgres://user:pass@localhost:5432/mdbuilder?sslmode=disable'
```

## 首次运行清单

1. 用 `adduser` 创建用户并登录。
2. **Settings**:配置代码仓库、测试输入仓库以及用于测试的 ref(见
   [站点配置](#/docs/site-configuration));仓库为私有
   时,配置 deploy key 或 deploy token。
3. **User center**:注册至少一个测试环境并打上标签(见
   [测试环境](#/docs/environments))。
4. **代码仓库**:添加 md-builder.yaml 测试矩阵(见
   [测试矩阵](#/docs/test-matrix))。
5. **GitLab**:添加指向服务器的 push 事件 webhook(见
   [GitLab webhooks](#/docs/webhooks))。
6. 推送一个提交 —— 仪表板(见
   [仪表板与报告](#/docs/dashboard))会随着任务图的运行逐步填充
   (见 [Runner 与任务](#/docs/runner-strategy))。
