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

DSN 取自服务端配置文件的 `database.dsn`;文件里没写时默认使用本地
SQLite 文件 `md-builder.db`。环境变量 `MD_BUILDER_DSN` 会覆盖两者。

```sh
export MD_BUILDER_DSN='postgres://user:pass@localhost:5432/mdbuilder?sslmode=disable'
```

## 启动服务

```sh
go run ./server                       # http://localhost:8080
go run ./server -config /etc/md-builder/server.yaml
```

`-config` 指定服务端配置文件,也是服务端唯一的命令行参数:监听地址、
数据库、worker 数量都来自这个文件。`-h` 会列出程序与各个子命令的参数。

```yaml
server:
  addr: 127.0.0.1   # 主机或 主机:端口;留空表示监听所有网卡
  port: 9000        # 0 表示取 addr 里的端口,都没有则为 8080
```

配置文件里还有服务端必需的对象存储配置(见
[对象存储(MinIO)](#/docs/object-storage))和 `worker` 执行池。可以复制
`md-builder-server.example.yaml` 作为起点。

每个配置项都有对应的环境变量,环境变量优先于文件,容器部署通常就用
这种方式:`MD_BUILDER_ADDR` 与 `MD_BUILDER_PORT` 对应监听地址,
`MD_BUILDER_DSN` 对应数据库,`MD_BUILDER_DIST` 对应前端构建产物,
`MD_BUILDER_WORKERS` 与 `MD_BUILDER_DISABLE_WORKER` 对应执行池,
`MD_BUILDER_S3_*` 对应对象存储。

## 用 Docker 或 Podman 部署

仓库自带 `Dockerfile` 与 `docker-compose.yml`,会把服务端和一份 MinIO
一起启动:

```sh
export MINIO_ROOT_PASSWORD='换成一个足够长的密码'
mkdir -p data/md-builder data/minio   # 第一次 up 之前先建好
docker compose up -d                  # 或者:podman compose up -d
```

整个过程不需要任何配置文件:compose 文件里除了 MinIO 密码之外每一项
都有默认值,密码从环境变量读取(没设置就拒绝启动)。其它默认值同样
可以用环境变量覆盖 —— 例如 `MD_BUILDER_PORT=9000 docker compose up -d`
会同时改掉监听端口和映射到宿主机的端口。

配置项比 compose 变量更多时,写成文件更方便:挂载到
`/app/md-builder-server.yaml`(镜像的工作目录就是 `/app`,服务端默认
会读这个路径),同时去掉对应的 `MD_BUILDER_*` 变量即可。环境变量优先
于文件,两者可以混用。

之后界面在 <http://localhost:8080>,MinIO 控制台在
<http://127.0.0.1:9001>。

部署写入的所有数据都留在这两个目录里 —— 数据库在 `data/md-builder`,
桶数据在 `data/minio` —— 因此备份就是把 `data/` 拷走。这两个目录需要
自己先建好:Docker 会自动创建不存在的挂载源目录,但所有者是 root,
而 root 属主的目录容器里的用户写不进去。

服务端以 uid 10001 运行。macOS 上这一点看不出来(文件共享会映射属主),
但在 Linux 上文件会属于一个宿主上并不存在的 uid —— 用 `id -u`/`id -g`
查出来,设成 `MD_BUILDER_UID`/`MD_BUILDER_GID`,文件就仍然归你所有。

因为没有注册界面,第一个账号仍然要用命令行创建。`cli` 服务与
服务端共用同一个数据库和对象存储:

```sh
docker compose --profile tools run --rm cli adduser -username alice -email alice@example.com
```

密码会以交互方式读取(也可以用 `-password` 传入,但那样密码会留在
shell 历史里)。需要演示数据时,`seed` 走同一条路。

不用 compose 也可以,直接构建镜像即可 —— 镜像里是一个静态二进制
文件加上构建好的前端,运行在 Alpine 上,全部配置也可以来自环境变量:

```sh
docker build -t md-builder:local .
mkdir -p data/md-builder
docker run -d --name md-builder -p 8080:8080 \
  --user "$(id -u):$(id -g)" \
  -v "$PWD/data/md-builder:/data" \
  -v "$PWD/md-builder-server.yaml:/app/md-builder-server.yaml:ro" \
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
