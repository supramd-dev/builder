# 站点配置

在一个还没有任何账号的站点上,代码仓库是引导页的第一个板块 —— 引导页会
取代登录表单出现(见[快速上手](#/docs/getting-started)):字段相同,存放位置
也相同。下面说的是之后修改它们的 **Settings** 页面。

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
权限。这一个权限就覆盖了服务端使用令牌的两件事:读取代码仓库中的
测试矩阵(见 [GitLab webhooks](#/docs/webhooks)),以及在把源码上传
到测试环境之前克隆该仓库(测试环境本身不需要任何仓库访问权限 —— 见
[Runner 与任务](#/docs/runner-strategy))。以 SSH 形式给出的仓库地址
(ssh:// 或 git@host:group/repo)会转为 https 形式并携带令牌读取与
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

## Webhook 密钥

**Settings → Webhook** 标签页显示 webhook 端点用于校验的共享密钥。
它在站点配置第一次被读取时自动生成 —— 无需任何手工步骤 —— GitLab
投递的每个事件都必须通过 `X-Gitlab-Token` 请求头把它带回。缺失或
错误的请求会在解析请求体之前被拒绝(401),因此伪造的 push 既不能
写入 commit,也不能触发构建。

把该值粘贴到 GitLab 侧 webhook 的 **Secret token** 字段。**Regenerate**
会用新的随机值替换它;在新值保存到 GitLab 之前,webhook 会一直失败,
所以当旧值可能泄露时(截图、共享终端、GitLab 导出)才需要轮换。

这个密钥与上面的 secret token 是两回事,方向正好相反:

| | 方向 | 用途 | 界面可见性 |
|---|---|---|---|
| **Webhook 密钥**(本标签页) | 入站 | 认证 GitLab 对 md-builder 的调用 | 可见,仅管理员 |
| **Secret token**(Repository 标签页) | 出站 | 以 `MD_SECRET_TOKEN` 认证构建命令访问内部服务 | 永不可见 |

读取或轮换 webhook 密钥都需要管理员账号;普通用户打开该标签页只能
看到 webhook URL 和一句提示(请管理员提供密钥)。两种角色见
[用户账号](#/docs/site-configuration)。

## 显示时区

**Settings → Display** 标签页设置所有时间戳(仪表板、任务与运行页面)
的显示时区:选择一个 IANA 时区如 `Asia/Shanghai`,或保持 *Browser
local* 让每个访问者按自己浏览器的时区查看。该设置仅影响显示 —— 存储
数据与日志保留原始时间戳;浏览器会在本地缓存该选择,刷新页面后时间立
即按所选时区渲染,无需等待请求。

## GitLab 登录

**Settings → GitLab** 标签页(仅管理员可见)让用户用 GitLab 账号登录,
而不必使用本地密码。它与上面的仓库配置相互独立:那个 token 读取待测
**代码**,而这个用来识别**人** —— 一个站点完全可以从一个 GitLab 实例
拉取代码,却用另一个实例做身份认证。

先在 GitLab 实例上创建一个带 **`read_user`** scope 的 application
(gitlab.com 在 *Preferences → Applications*,自建实例在
*Admin Area → Applications*),并把标签页上显示的 **Redirect URI** 填进去。
然后在标签页中填写:

| 字段 | 含义 |
| --- | --- |
| GitLab site address | 用户登录所针对的实例,如 `https://gitlab.com`。必须是带 scheme 的完整 URL,只写主机名会被拒绝。 |
| Redirect URI | 只读。由服务端根据 `server.publicURL` 拼出 —— 原样复制到 application 里即可。 |
| Application ID | application 的 *Application ID*。 |
| Application secret | application 的 *Secret*。与仓库 token 一样只写:存在服务端,之后不再显示。 |
| Offer sign-in with GitLab | 总开关。关闭时只有本地账号能登录。 |

Redirect URI 来自服务端配置,而非请求本身:

```yaml
server:
  # 用户访问本站所用的地址。GitLab 登录必须配置它:回调地址由它拼出,
  # 且必须与 GitLab application 上登记的 redirect URI 完全一致。
  publicURL: https://md.example.com
```

它必须是一项配置值,这是有意的:若从请求的 `Host` 请求头推导,调用方就
可以把回调指向自己的服务器;何况这个值本来就必须与 GitLab 上登记的一致。
与 GitLab 地址一样,它必须是带 scheme 的完整 URL:否则回调地址会是相对
地址,而 GitLab 拒绝时给出的报错完全看不出原因。

配置项不全时不允许开启集成 —— 否则站点会显示一个根本无法工作的按钮。
之后保存时留空并不等于清空:留空表示"保留已存的值",所以只切换开关不会
动到凭据。清除 application secret 有单独的勾选项;而在集成开启时清空地址
或 Application ID 同样会被拒绝,理由与上面一致。

### 首次登录会发生什么

GitLab 登录只是**注册**账号,并不会放行。新账号以普通用户身份创建
(无论 GitLab 那边怎么说,绝不会是管理员),没有密码,并在
**Settings → Users** 标签页中标记为 **Pending approval**。在管理员点击
**Approve** 之前,它既不能用 GitLab 登录,也不能用密码登录 —— 登录页会
提示该账号正在等待审批。只要有账号在等待,账号表顶部就会显示一条提示;
**Source** 列显示 **GitLab** 或 **Local**,一眼即可看出哪些是自助注册的。

账号的身份是它的 GitLab user id,因此后续登录会落到同一个账号,不会再
创建第二个。GitLab 用户名只是个标签:若与已有账号重名,新账号会带上数字
后缀(`alice`、`alice-2`……)。

有两种情况会被直接拒绝,而不会自动合并:

- **邮箱已属于本站的某个账号。** GitLab 对该邮箱归属的判断,本站无法
  核实;一旦合并,就等于把已有账号交给在 GitLab 实例上控制该邮箱的人。
  登录会被拒绝并给出提示,已有账号不受影响。
- **GitLab 账号没有可见邮箱。** 没有可用于建账号的信息,因此无法使用
  GitLab 登录。

被禁用的已批准账号同样无法走 GitLab 登录,提示与密码登录一致;把已批准的
账号**撤销审批**也是一样 —— 它会回到等待审批的状态。两种决定都会立即结束
该账号的活动会话:访问权在决定作出时即失效,而不是等会话自然过期。

## 用户账号

账号分为两类。**普通用户**登录后使用 md-builder;**管理员**除此之外还能
管理账号,入口是 **Settings → Users** 标签页:列出全部账号及其用户名、
邮箱、来源、角色、状态和创建时间,每行有 **Edit**(改用户名、邮箱、密码)
和 **Disable**/**Enable**。普通用户看到的是同一个标签页,名为
**Account**,里面只有自己的资料。

账号的**来源**是 `Local`(在本站创建:管理员创建或首次启动引导创建)或
`GitLab`(通过 GitLab 登录注册,见上文 [GitLab 登录](#gitlab-登录))。
它是账号的既成事实而非设置项,任何表单都改不了。

管理员只能在服务端创建:

```sh
md-builder adduser -admin -username root -email root@example.com
```

网页端发出的任何请求都无法创建或提升管理员 —— 角色不属于任何账号表单的
字段 —— 所以这个面板不是获取管理员权限的途径。

**禁用**一个账号会立刻把它登出,并拒绝后续登录(提示 "this account has
been disabled")。账号本身和它配置的内容都保留,**Enable** 即可恢复访问。
**批准**(Approve)用于放行通过 GitLab 自助注册的账号,**撤销审批**则把
它退回等待审批的状态;两者都会立刻登出该账号,理由与禁用相同 —— 见
[GitLab 登录](#gitlab-登录)。你不能禁用或批准自己的账号,也不能对其他
管理员做这两件事,因此站点不会落到无人能进的地步。

修改密码会把这个账号在其他浏览器上的会话全部登出,只保留做出修改的那一
个 —— 这正是"重置"的意义。密码至少 8 位;用户名和邮箱都必须唯一。

## 对象存储

以上设置保存在数据库中,并在浏览器中编辑。有一项部署设置例外:存放
测试输出文件的**对象存储**。它在服务端主机上配置,写在
`md-builder-server.yaml` 中或通过 `MD_BUILDER_S3_*` 环境变量提供,且
服务端在它缺失时不会启动。见
[对象存储(MinIO)](#/docs/object-storage)。
