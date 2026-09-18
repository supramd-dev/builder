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

## 启动服务

```sh
go run ./server                       # http://localhost:8080
go run ./server -port 9000            # 换一个端口
go run ./server -addr 127.0.0.1:9000  # 指定监听的主机与端口
go run ./server -config /etc/md-builder/server.yaml
```

`-addr` 接受主机或 主机:端口 的形式,`-port` 会覆盖其中的端口 ——
因此 `-addr 127.0.0.1 -port 9000` 监听 127.0.0.1:9000。服务端还需要
对象存储(见 [对象存储(MinIO)](#/docs/object-storage));`-config`
用于指定该配置文件,`-h` 会列出全部参数。

容器里改用环境变量配置:`MD_BUILDER_ADDR` 与 `MD_BUILDER_PORT` 提供和
命令行参数相同的默认值,`MD_BUILDER_DSN` 指定数据库,`MD_BUILDER_S3_*`
配置对象存储。参数仍然优先于对应的环境变量。

## 用 Docker 或 Podman 部署

仓库自带 `Dockerfile` 与 `docker-compose.yml`,会把服务端和一份 MinIO
一起启动:

```sh
cp .env.example .env    # 然后修改 MINIO_ROOT_PASSWORD
docker compose up -d    # 或者:podman compose up -d
```

之后界面在 <http://localhost:8080>(改 `.env` 里的 `MD_BUILDER_PORT`
即可换端口,映射到宿主机的端口会一起变)。数据库是 `md-builder-data`
卷,MinIO 的数据是 `minio-data` 卷,其控制台在
<http://127.0.0.1:9001>。

因为没有注册界面,第一个账号仍然要用命令行创建。`cli` 服务与
服务端共用同一个数据库和对象存储:

```sh
docker compose --profile tools run --rm cli adduser -username alice -email alice@example.com
```

密码会以交互方式读取(也可以用 `-password` 传入,但那样密码会留在
shell 历史里)。需要演示数据时,`seed` 走同一条路。

不用 compose 也可以,直接构建镜像即可 —— 镜像里是一个静态二进制
文件加上构建好的前端,运行在 Alpine 上,全部配置都来自环境变量:

```sh
docker build -t md-builder:local .
docker run -d --name md-builder -p 8080:8080 \
  -v md-builder-data:/data \
  -e MD_BUILDER_S3_ENDPOINT=minio.example.com:9000 \
  -e MD_BUILDER_S3_ACCESS_KEY=... -e MD_BUILDER_S3_SECRET_KEY=... \
  -e MD_BUILDER_S3_BUCKET=md-builder \
  md-builder:local
```

无论是容器还是直接跑在宿主机上,服务端都需要对象存储才能启动:启动
时存储不可达就会**退出**(见
[对象存储(MinIO)](#/docs/object-storage))。compose 里的重启策略正是
用来等 MinIO 就绪的;如果它一直重启不停,说明地址或凭据配错了,
`docker compose logs md-builder` 会说明是哪一种。

## 首次运行清单

1. 用 `adduser` 创建用户并登录。
2. **Settings**:配置被测试的代码仓库(见
   [站点配置](#/docs/site-configuration));仓库为私有
   时,配置 Project Access Token(项目访问令牌)。
3. **运行环境**(Runner Envs):注册至少一个测试环境并打上标签(见
   [测试环境](#/docs/environments))。
4. **代码仓库**:添加 md-builder.yaml 测试矩阵(见
   [测试矩阵](#/docs/test-matrix))。
5. **GitLab**:添加指向服务器的 push 事件 webhook(见
   [GitLab webhooks](#/docs/webhooks))。
6. 推送一个提交 —— 仪表板(见
   [仪表板与报告](#/docs/dashboard))会随着任务图的运行逐步填充
   (见 [Runner 与任务](#/docs/runner-strategy))。
