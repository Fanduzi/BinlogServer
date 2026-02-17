# 生产部署模式

本文说明 `binlog_server` 的两种生产部署方式：

- 一体化部署（Go 同时提供 API + UI）
- 前后端分离部署（UI 由 Nginx/CDN 托管，Go 只提供 API）

---

## 1. 一体化部署（推荐先用）

### 架构

- Go 服务提供 API（`/api/*`）与 UI（`/ui/*`）
- UI 静态资源来自 `internal/ui/static/*`
- 通过 `go:embed` 打进二进制，部署只需一个进程

### 适用场景

- 内网运维平台
- 小中规模团队
- 希望部署简单、依赖少

### 优点

- 部署最简单：一个二进制即可
- 版本一致性好：前后端天然同版本
- 不需要额外 Nginx/CDN（可选）

### 缺点

- 每次前端改动都要重新打包后端
- 静态资源分发能力不如 CDN

### 部署步骤

1. 构建前端并同步到后端静态目录

```bash
make ui-build
```

2. 构建服务

```bash
go build -o binlog-server ./cmd/binlog-server
```

3. 启动服务

```bash
./binlog-server --config ./config.yaml
```

4. 访问

- UI：`http://<host>:<port>/ui/`
- API：`http://<host>:<port>/api/...`

---

## 2. 前后端分离部署

### 架构

- 前端：`frontend/dist` 部署到 Nginx/CDN
- 后端：Go 服务仅提供 API（`/api/*`）
- 网关或 Nginx 反向代理把 `/api` 转发到 Go

### 适用场景

- 需要更高并发静态资源分发
- 需要 CDN/缓存策略
- 前端发布节奏与后端发布节奏不同

### 优点

- 前端发布更灵活（无需重启 Go 服务）
- 可利用 CDN 降低后端静态流量
- 更符合大型团队协作模式

### 缺点

- 部署链路更复杂（前端制品 + API 服务 + 代理配置）
- 需要处理跨域或同域反代策略

### 部署步骤

1. 构建前端

```bash
cd frontend
npm ci
npm run build
```

2. 部署 `frontend/dist` 到 Nginx 静态目录（示例：`/var/www/binlog-ui`）

3. 启动 Go API 服务

```bash
./binlog-server --config ./config.yaml
```

4. 配置 Nginx（同域反代，推荐）

```nginx
server {
  listen 80;
  server_name binlog.example.com;

  root /var/www/binlog-ui;
  index index.html;

  location / {
    try_files $uri $uri/ /index.html;
  }

  location /api/ {
    proxy_pass http://127.0.0.1:8080/api/;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
  }

  location /healthz {
    proxy_pass http://127.0.0.1:8080/healthz;
  }
}
```

---

## 3. 如何选择

- 优先一体化：如果你现在更关注功能迭代和运维简单性
- 选择分离：如果你需要 CDN、前后端独立发布、高并发静态资源分发

当前项目阶段建议：

- 开发与早期生产：先用一体化
- 平台规模扩大后：切到前后端分离

---

## 4. 常见误区

### 为什么 `5173` 和 `8080/18080` 都能看页面？

- `5173` 是 Vite 开发服务器，只用于开发调试（热更新）
- `8080/18080` 是 Go 服务端口，`/ui/` 是打包后的生产形态页面

### 生产是否需要 `npm run dev`？

- 不需要。`npm run dev` 仅用于本地开发

### 一体化模式忘记 `make ui-build` 会怎样？

- Go 服务会继续提供旧的前端资源，页面看起来“代码没更新”
