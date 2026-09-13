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

私有仓库需配置**以下之一**:

- **Deploy token** —— 具有 read_repository 权限的 GitLab deploy token
  或个人/组访问令牌,以及 GitLab 中显示在其旁边的用户名(例如
  gitlab+deploy-token-42;用户名留空则使用默认的 oauth2)。适用于
  https 仓库地址。
- **Deploy key** —— PEM 编码的 SSH 私钥,其公钥在 GitLab 注册为对该
  仓库有读权限的 deploy key。https 地址会自动转换为 ssh:// 形式。

两者**仅在服务端使用**:读取代码仓库中的测试矩阵,以及在把源码上传到
测试环境之前克隆该仓库(测试环境本身不需要任何仓库访问权限 —— 见
[Runner 与任务](#/docs/runner-strategy))。密钥为只写:表单只显示是否
已配置,绝不显示其值;留空表示保留已存的密钥,勾选 *Remove* 复选框则
删除。

## 显示时区

**Settings → Display** 标签页设置所有时间戳(仪表板、任务与运行页面)
的显示时区:选择一个 IANA 时区如 `Asia/Shanghai`,或保持 *Browser
local* 让每个访问者按自己浏览器的时区查看。该设置仅影响显示 —— 存储
数据与日志保留原始时间戳;浏览器会在本地缓存该选择,刷新页面后时间立
即按所选时区渲染,无需等待请求。
