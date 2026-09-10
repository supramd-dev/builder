# 站点配置

打开 **Settings** 页面并填写:

- **代码仓库** —— 被测试的仓库。指向它的 webhook 推送会触发测试运行。
  md-builder.yaml 也从这个仓库读取。
- **测试输入仓库** —— 存放测试输入(测试用例)的仓库,代码将与这些
  输入一起运行。
- **测试输入分支或提交 id** —— 运行时克隆测试输入仓库所用的 ref。

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
- **Deploy key** —— PEM 编码的 SSH 私钥,其公钥在 GitLab 注册为对两个
  仓库均有读权限的 deploy key。https 地址会自动转换为 ssh:// 形式。

两者**仅在服务端使用**:读取代码仓库中的测试矩阵,以及在把源码上传到
测试环境之前克隆两个仓库(测试环境本身不需要任何仓库访问权限 —— 见
[Runner 与任务](#/docs/runner-strategy))。密钥为只写:表单只显示是否
已配置,绝不显示其值;留空表示保留已存的密钥,勾选 *Remove* 复选框则
删除。
