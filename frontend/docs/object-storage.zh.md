# 对象存储(MinIO)

平台产出的每一个测试输出文件 —— 构建的 `test_detail.xml`、单元测试的
日志、回归运行的结果序列 —— 都以 **MinIO 对象**的形式保存,数据库中只
保存对它的引用(对象 key 与大小)。数据库里不再保留大块内容。

任务日志是例外:它在阶段运行过程中逐行追加,浏览器需要实时跟随,因此
仍保存在数据库中。

## 配置服务端

服务端在启动时读取自己的配置文件 **`md-builder-server.yaml`**。这是
部署文件 —— 它位于服务端二进制旁边,而不是被测试的代码仓库中:

```yaml
objectStorage:
  endpoint: minio.example.com:9000
  accessKey: md-builder
  secretKey: "…"
  bucket: md-builder
  useSSL: true
  # prefix: md-builder          # 可选:桶内的命名空间
  # region: us-east-1           # 可选:适用于 AWS 风格端点
  autoCreateBucket: true        # 启动时若桶不存在则创建
  gc: true                      # 回收孤儿对象(见下文)
  # gcIntervalHours: 6
```

文件按以下顺序查找:

1. `-config` 命令行参数:
   `md-builder -config /etc/md-builder/server.yaml`;
2. `$MD_BUILDER_CONFIG`(若已设置,该文件必须存在);
3. `./md-builder-server.yaml`;
4. `./server/md-builder-server.yaml` —— 这样在项目根目录也能直接运行。

被显式指定的路径(命令行参数或环境变量)必须存在,绝不会静默地改用
另一个文件,因此拼写错误不会启动一个配置不同的部署。`md-builder -h`
会列出该参数;`seed` 子命令同样支持它(`md-builder seed -config …`)。

未知的键会导致启动失败,因此像 `endpont` 这样的拼写错误会被报出来,
而不会静默地留下空 endpoint。

### 通过环境变量配置

容器与 CI 部署通常注入凭据而不是挂载文件。每个配置项都有对应的环境
变量,且环境变量的值优先于文件:

| 环境变量 | 配置项 |
| --- | --- |
| `MD_BUILDER_CONFIG` | 配置文件路径(命令行 `-config` 优先于它) |
| `MD_BUILDER_S3_ENDPOINT` | `endpoint` |
| `MD_BUILDER_S3_ACCESS_KEY` | `accessKey` |
| `MD_BUILDER_S3_SECRET_KEY` | `secretKey` |
| `MD_BUILDER_S3_BUCKET` | `bucket` |
| `MD_BUILDER_S3_REGION` | `region` |
| `MD_BUILDER_S3_PREFIX` | `prefix` |
| `MD_BUILDER_S3_USE_SSL` | `useSSL`(`true`/`false`) |

只要设置了这四个必需的变量(`ENDPOINT`、`ACCESS_KEY`、`SECRET_KEY`、
`BUCKET`),就完全可以不使用配置文件运行。

## 对象存储是强制要求

当对象存储未配置或不可达时,服务端**拒绝启动**:它会先校验配置、建立
连接、确认桶存在,然后才打开数据库。不存在把工件悄悄写回数据库的兜底
路径 —— 一个看起来健康、实际会丢工件的部署,比一个在启动时就失败的
部署更糟。

运行期同样如此。若服务运行期间对象存储不可用,工件接口返回
**502 Bad Gateway**,而不会返回陈旧内容,同时健康看板会变红。

## Key 布局

对象以确定性的 key 写入:

```
[<prefix>/]runs/<运行 id>/<kind>/<文件名>
```

`<kind>` 是工件类型(`results`、`log`、`series`、`file`),`<文件名>` 是
上报者使用的名字,其中不适合出现在对象 key 中的字符会被压平为 `_` ——
所以 `build/test_detail.xml` 变成 `build_test_detail.xml`。超过 128 个
字符的名字会被截断,并追加完整名字的短哈希,从而保证共享前缀的两个长
名字仍然互相区分。同一次运行中同名的第二个工件会带上 `-2`、`-3` …
后缀。

由于 key 由运行与文件名推导而来,重复上报一次运行会**原地覆盖它的
工件**,而不是不断堆积副本。

## 下载

前端从不直接访问 MinIO,也不需要任何 MinIO 凭据。字节由服务端中转:

| 接口 | 返回 |
| --- | --- |
| `GET /api/test-artifacts/{id}` | JSON 形式的内容(`{id, runId, kind, name, content}`) |
| `GET /api/test-artifacts/{id}/download` | 原始文件,以附件形式下载 |
| `GET /api/test-runs/{id}/artifacts/zip` | 该运行的全部工件,打包为 zip |

因此 MinIO 只需对服务端主机可达即可 —— 它完全可以位于没有任何公网入口
的私有网络中。

## 回收孤儿对象

删除一次运行(或重复上报)会移除数据库中的行。它们对应的对象由后台
**清扫(sweep)**删除,而不是随删除操作一起删除:删除发生在事务中,若在
事务提交前就删除对象,一旦回滚就会丢数据。

清扫会列出桶中的 `runs/`,删除所有存在超过一小时、且没有任何
`test_artifacts` 行引用的对象。这一宽限期保护的是刚刚上传、对应行还在
插入过程中的对象。清扫按 `gcIntervalHours`(默认 6 小时)运行,可以用
`gc: false` 关闭 —— 如果桶与其他工具共享、并在别处清理,就应该关掉。

## 迁移已有部署

对象存储上线之前写入的行把内容内联保存在数据库中。启动时服务端会把
它们分批搬入桶中,并清空内联列。该迁移是幂等的,失败也不会导致启动
失败 —— 未能搬走的行仍可从数据库读取,下次重启会重试。

## 本地运行 MinIO

开发时,一个用完即弃的 Docker MinIO 就够了:

```sh
docker run --rm -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=md-builder \
  -e MINIO_ROOT_PASSWORD=md-builder-secret \
  minio/minio server /data --console-address ":9001"
```

配套的 `md-builder-server.yaml`:

```yaml
objectStorage:
  endpoint: 127.0.0.1:9000
  accessKey: md-builder
  secretKey: md-builder-secret
  bucket: md-builder
  useSSL: false
  autoCreateBucket: true
  gc: true
```

`useSSL: false` 仅适用于这种本地明文 HTTP 场景 —— 任何可从网络访问的
部署都应使用 TLS(`useSSL: true`)。

## 凭据

`accessKey` 与 `secretKey` 绝不会被记录到日志,也不会由任何接口返回:
启动日志只输出端点、桶与协议,错误信息只指出缺失的配置项而不带其值。
请把配置文件排除在版本控制之外(它已被 git 忽略;改为提交
[`md-builder-server.example.yaml`](https://github.com/genshen/md-builder/blob/main/md-builder-server.example.yaml)),
并优先使用仅限该桶权限的专用 MinIO 用户,而不是 root 凭据。

## 健康检查

**Settings → Health** 会在代码仓库之外一并探测对象存储:所用的端点与
桶,以及对其的请求是否成功。可达但缺少对应桶的存储会被判定为失败,
因此配置错误的部署会在这里暴露出来,而不是等到第一次写入工件时才
发现。见 [仪表板与 API](#/docs/dashboard)。
