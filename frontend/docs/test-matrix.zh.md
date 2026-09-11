# 测试矩阵(md-builder.yaml)

将 md-builder.yaml 放在**代码仓库根目录** —— 它随代码一起演进,改动它
的推送会改变派发行为。每次推送时,服务器都会在被推送的提交上读取它
(git show <sha>:md-builder.yaml),因此矩阵的改动从引入它的那个提交
开始生效。

## 完整示例

```yaml
version: 1

# 可选的默认值,合并进每个矩阵条目(map 按键合并,标量按条目覆盖)。
defaults:
  timeout: 3600                 # 单命令超时秒数(硬上限 4 小时)
  env:
    OMP_NUM_THREADS: "4"
  build:
    generator: cmake            # cmake(默认)| script
    cmake_flags: "-DCMAKE_BUILD_TYPE=Release"
    threads: 8                  # cmake --build -j

# 必填:矩阵。每个条目声明环境必须携带的标签;派发时为每个条目挑选
# 一个匹配且已启用的环境。
matrix:
  - tags: [cpu]
    description: Generic CPU build
    env:
      CC: gcc
      CXX: g++
    build:
      cmake_flags: "-DENABLE_MPI=OFF"
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      timeout: 600
    regression:
      command: "python3 run_regression.py --suite full"
      timeout: 1800

  - tags: [gpu, cuda]
    env:
      CC: clang
    build:
      generator: script         # 非 cmake 项目
      command: "./build.sh --cuda"
    unit:
      command: "ctest --test-dir build -L unit"
```

## 字段参考

| 字段                         | 必填     | 说明                                                               |
|------------------------------|----------|--------------------------------------------------------------------|
| version                      | 是       | 必须为 1。                                                          |
| defaults                     | 否       | 条目级默认值:timeout、env、build、unit、regression。                |
| matrix                       | 是       | 一或多个条目;每个条目需要 tags 和至少一个阶段。                     |
| matrix[].tags                | 是       | 选择环境的标签(见[测试环境](#/docs/environments))。每个条目内必须唯一。 |
| matrix[].description         | 否       | 人类可读的标签。                                                    |
| matrix[].timeout             | 否       | 默认阶段超时秒数(默认 3600,上限 14400)。                          |
| matrix[].env                 | 否       | 为所有阶段导出的额外环境变量。                                      |
| matrix[].build               | 否       | 构建阶段(见下)。                                                   |
| matrix[].unit                | 否       | 单元测试阶段:至少有 command;可选 timeout。                         |
| matrix[].regression          | 否       | 回归测试阶段:至少有 command;可选 timeout。                         |
| unit.command / regression.command | 是(每阶段) | 在代码目录中运行的 shell 命令。                                 |

构建阶段有两种形式:

| 字段                | 说明                                                                        |
|---------------------|-----------------------------------------------------------------------------|
| build.generator     | cmake(默认)或 script。                                                    |
| build.cmake_flags   | 传给 cmake 的参数(仅 cmake 生成器)。                                       |
| build.threads       | 并行构建任务数(默认 8)。                                                   |
| build.command       | shell 命令(仅 script 生成器)。                                             |

每个超时通过远程的 `timeout` 命令约束对应阶段;整个 SSH 会话的上限是各
阶段超时之和再加 15 分钟余量。

## 校验规则

- `version` 必须为 1;`matrix` 不能为空。
- 每个条目需要非空的 `tags` 以及 `unit` / `regression` 中的至少一个,
  且各自带有 `command`。
- 条目之间不允许重复的标签组合。
- `build.generator` 必须是 `cmake` 或 `script`;`script` 必须提供
  `build.command`。

非法的 YAML 会使派发失败:推送仍被记录,`dispatchError` 出现在 webhook
响应中(见 [Webhooks](#/docs/webhooks)),但不会创建任何任务。

## Runner 做了什么

每个匹配的条目会成为一条任务图(见
[Runner 与任务](#/docs/runner-strategy)),各阶段按序运行:

1. **clone**:*服务器* 克隆被推送提交上的代码仓库,把工作树打包为
   tarball 并解压到环境上的 ~/.md-builder/tasks/<sha12>/code。
2. **build**:生成的脚本导出 MD_COMMIT、MD_ENV_NAME、MD_ENV_TAGS、
   MD_CODE_DIR(`…/code`)以及 yaml 的 env 变量,然后在代码目录中运行
   构建阶段。
3. 若构建(或克隆)失败,依赖它的测试阶段会被标记为 skipped,仪表板
   显示 ✗。
4. **unit / regression**:阶段命令在代码目录中运行,各自受超时约束;完整
   输出流入任务日志,结果被存为一条测试运行。

## 自定义摘要

阶段命令可以在 stdout 打印一行摘要:

```
MD-BUILDER-SUMMARY: all 8 tests passed, max rel err 3.2e-7
```

前缀之后的文本会成为仪表板上显示的运行摘要。没有这行时,摘要为退出码
加阶段日志的最后几行(截断到 500 字符)。
