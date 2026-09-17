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
