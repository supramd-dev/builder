# md-builder

一个用于科学计算软件（如分子动力学）测试运行与结果展示的平台，风格参考 [build.golang.org](https://build.golang.org)。

## 技术栈

- **后端**：Go（`net/http` 原生，零外部依赖）
- **前端**：Vite + React 19 + TypeScript
- **样式**：Tailwind CSS v4，自定义 sourcehut 风格极简主题（近单色、细边框、无阴影、无圆角）

## 目录结构

```
md-builder/
├── server/            # Go 后端
│   ├── main.go
│   └── go.mod
└── frontend/          # 前端
    ├── src/
    ├── index.html
    ├── package.json
    └── vite.config.ts
```

## 开发

前端开发服务器（热更新）：

```sh
make dev-frontend
```

后端开发服务器（单独运行，端口 8080）：

```sh
make dev-backend
```

## 构建与运行

```sh
make serve   # 构建前端并由 Go 托管
```

访问 http://localhost:8080
