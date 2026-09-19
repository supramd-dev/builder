# 站点配置

打开 **Settings** 页面并填写:

- **代码仓库** —— 被测试的仓库。指向它的 webhook 推送会触发测试运行。
  md-builder.yaml 也从这个仓库读取。测试输入应内置于代码仓库本身
  (或由代码仓库自行获取) —— 没有单独的测试输入仓库。

仓库应托管在 **GitLab** 上(gitlab.com 或任意自建实例)。地址不做主机
校验,请使用你的实例对应的 URL 形式:

```
https://gitlab.example.com/group/code
git@gitlab.example.com:group/code.git
```

## 私有仓库的凭据

私有仓库需配置一个 **Project Access Token(项目访问令牌)** —— 在
GitLab 的 *Settings → Access Tokens* 下创建,勾选 `read_repository`
权限。令牌**仅在服务端使用**:读取代码仓库中的测试矩阵,以及在把源码
上传到测试环境之前克隆该仓库(测试环境本身不需要任何仓库访问权限
—— 见 [Runner 与任务](#/docs/runner-strategy))。以 SSH 形式给出的
仓库地址(ssh:// 或 git@host:group/repo)会转为 https 形式并携带令牌
克隆。公开仓库令牌留空即可。

令牌为只写:表单只显示是否已配置,绝不显示其值;留空表示保留已存的
令牌,勾选 *Remove* 复选框则删除。

## 命令用 Secret token

**Settings → Repository** 标签页还提供一个可选的 **secret token** ——
一个站点级密钥,以环境变量 `MD_SECRET_TOKEN` 导出到每个阶段命令
(md-builder.yaml 中的 `build.command`、`unit.command`、回归预设的
command)。它让这些命令能向内部服务认证 —— 软件源镜像、工件存储、
付费软件的 license 服务器 —— 而无需把凭证硬编码进代码仓库。

```yaml
build:
  command: "cmake -DFETCH_TOKEN=\"$MD_SECRET_TOKEN\" . && cmake --build ."
```

同样的只写约定:表单只报告是否已设置。若命令把值回显到输出中
(`env`、`set -x`、`curl -v`),runner 会在写入任务日志前把每一处出现
都替换为 `REDACTED`。用法见
[测试矩阵 → Secret token](#/docs/test-matrix)。

## 显示时区

**Settings → Display** 标签页设置所有时间戳(仪表板、任务与运行页面)
的显示时区:选择一个 IANA 时区如 `Asia/Shanghai`,或保持 *Browser
local* 让每个访问者按自己浏览器的时区查看。该设置仅影响显示 —— 存储
数据与日志保留原始时间戳;浏览器会在本地缓存该选择,刷新页面后时间立
即按所选时区渲染,无需等待请求。

## 用户账号

账号分为两类。**普通用户**登录后使用 md-builder;**管理员**除此之外还能
管理账号,入口是 **Settings → Users** 标签页:列出全部账号及其用户名、
邮箱、角色、状态和创建时间,每行有 **Edit**(改用户名、邮箱、密码)和
**Disable**/**Enable**。普通用户看到的是同一个标签页,名为 **Account**,
里面只有自己的资料。

管理员只能在服务端创建:

```sh
md-builder adduser -admin -username root -email root@example.com
```

网页端发出的任何请求都无法创建或提升管理员 —— 角色不属于任何账号表单的
字段 —— 所以这个面板不是获取管理员权限的途径。

**禁用**一个账号会立刻把它登出,并拒绝后续登录(提示 "this account has
been disabled")。账号本身和它配置的内容都保留,**Enable** 即可恢复访问。
你不能禁用自己的账号,也不能禁用其他管理员,因此站点不会落到无人能进的
地步。

修改密码会把这个账号在其他浏览器上的会话全部登出,只保留做出修改的那一
个 —— 这正是"重置"的意义。密码至少 8 位;用户名和邮箱都必须唯一。

## 对象存储

以上设置保存在数据库中,并在浏览器中编辑。有一项部署设置例外:存放
测试输出文件的**对象存储**。它在服务端主机上配置,写在
`md-builder-server.yaml` 中或通过 `MD_BUILDER_S3_*` 环境变量提供,且
服务端在它缺失时不会启动。见
[对象存储(MinIO)](#/docs/object-storage)。
