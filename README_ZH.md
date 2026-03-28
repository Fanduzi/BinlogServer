<div align="center">
  <div style="display:inline-block; text-align:left;">
    <pre>
 ____                  ___                       ____
/\  _`\    __         /\_ \                     /\  _`\
\ \ \L\ \ /\_\    ___ \//\ \     ___      __    \ \,\L\_\     __   _ __   __  __     __   _ __
 \ \  _ <'\/\ \ /' _ `\ \ \ \   / __`\  /'_ `\   \/_\__ \   /'__`\/\`'__\/\ \/\ \  /'__`\/\`'__\
  \ \ \L\ \\ \ \/\ \/\ \ \_\ \_/\ \L\ \/\ \L\ \    /\ \L\ \/\  __/\ \ \/ \ \ \_/ |/\  __/\ \ \/
   \ \____/ \ \_\ \_\ \_\/\____\ \____/\ \____ \   \ `\____\ \____\\ \_\  \ \___/ \ \____\\ \_\
    \/___/   \/_/\/_/\/_/\/____/\/___/  \/___L\ \   \/_____/\/____/ \/_/   \/__/   \/____/ \/_/
                                          /\____/
                                          \_/__/
    </pre>
  </div>
</div>

<div align="center">
# BinlogServer

[![English](https://img.shields.io/badge/doc-English-inactive.svg)](README.md)
[![中文](https://img.shields.io/badge/doc-中文-blue.svg)](README_ZH.md)
[![更新日志](https://img.shields.io/badge/docs-更新日志-informational.svg)](CHANGELOG.md)
[![安全策略](https://img.shields.io/badge/docs-安全策略-critical.svg)](SECURITY.md)
</div>

Binlog Server 是一个面向 MySQL binlog 备份与拉流场景的服务：负责从源库持续读取 binlog、落盘本地文件、持久化 checkpoint，并提供 API 控制、UI、S3-compatible upload 与集群调度能力。

如果你第一次打开这个仓库，先看 `Quick Start` 跑通服务；如果你在判断“这个项目适不适合我”，先看下面的定位说明。

## 这个项目解决什么问题

它把“拉 MySQL binlog、落盘、记 checkpoint、管理任务状态”收敛成一个独立服务，而不是让你自己拼脚本、cron 和零散元数据。

适合：

- 想把 binlog 拉取、落盘、状态管理做成一个可运维的服务
- 需要从 `LATEST`、`FILE_POS`、`GTID` 启动任务
- 需要本地持久化，并且可能接 S3-compatible upload
- 需要 API / UI / observability，而不是一次性脚本

不太适合：

- 只做一次性导出，不做持续复制
- 不需要 task orchestration，只想快速写个单机脚本
- 希望它直接替代完整 CDC 平台

## 为什么用它

- 明确的 checkpoint 语义：只有 `fsync` 成功后才推进 checkpoint
- 支持 `LATEST` / `FILE_POS` / `GTID` 三种起点
- 内建 API、UI、Swagger、metrics 与可选 tracing
- 支持 metadata 存储、lease 调度与 S3-compatible upload
- 仓库内带有 E2E 场景，方便验证回归

## 安装 / 下载

- 对于带 tag 的公开版本，优先从 GitHub Releases 下载与你平台匹配的压缩包：
  - `binlog-server_<version>_darwin_amd64.tar.gz`
  - `binlog-server_<version>_darwin_arm64.tar.gz`
  - `binlog-server_<version>_linux_amd64.tar.gz`
  - `binlog-server_<version>_linux_arm64.tar.gz`
- 同一页会提供 `checksums.txt`，下载后先校验再解压。
- `/ui/` 所需前端静态资源已经内嵌在二进制里，不需要额外下载前端包。

源码构建作为 fallback：

```bash
make ui-build
go build -o binlog-server ./cmd/binlog-server
```

如果你要在本地准备一组 release 产物：

```bash
make release-assets VERSION=v0.1.0
```

## Quick Start

这部分只保留“第一次跑起来”所需的最短路径。

### 前置条件

- Go `1.26.1+`
- 一个可访问的 MySQL 实例，并且已经开启 binlog
- 如需跑 E2E：Docker 可用

### 1. 启动服务

```bash
go run ./cmd/binlog-server
```

默认监听地址是 `:8080`。

如果你想显式指定监听地址：

```bash
BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080 go run ./cmd/binlog-server
```

### 2. 验证 `/healthz`

```bash
curl -fsS http://127.0.0.1:8080/healthz
```

期望返回：

```text
ok
```

### 3. 创建第一个任务

把下面示例里的 MySQL 连接信息替换成你自己的实例：

```bash
curl -fsS -X POST http://127.0.0.1:8080/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "quickstart-task",
    "cluster_key": "quickstart-task",
    "source": {
      "host": "127.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret",
      "flavor": "mysql"
    },
    "start": {
      "mode": "LATEST"
    },
    "storage": {
      "retention_days": 7
    }
  }'
```

### 4. 启动任务

把 `<task-id>` 替换成上一步返回的 `id`：

```bash
curl -i -X POST http://127.0.0.1:8080/api/tasks/<task-id>/start
```

### 5. 查看任务状态

```bash
curl -fsS http://127.0.0.1:8080/api/tasks
```

### 6. 打开 UI 或 Swagger

- UI: `http://127.0.0.1:8080/ui/`
- Swagger: `http://127.0.0.1:8080/swagger/index.html`

如果你要看更完整的运维与使用说明，从 [docs/guide/README.md](docs/guide/README.md) 进入。

### 运行后你会看到什么

- `/healthz` 返回 `ok`
- `/api/tasks` 能看到你刚创建的任务
- `/ui/` 可以打开管理界面
- `/swagger/index.html` 可以直接查看和调试 API
- 任务启动后，checkpoint 与 metrics 会逐步反映运行状态

> ⚠️ **Security Warning**
>
> 默认关闭 API authentication，是为了降低开发环境启动门槛。
>
> **生产环境必须：**
> 1. 设置 `api.auth.enabled: true`（或 `BINLOG_SERVER_API_AUTH_ENABLED=true`）
> 2. 配置认证方式（Bearer Token 或 API Key）
> 3. 保护 `/api/*` 与 `/metrics`
>
> 具体安全建议见 [SECURITY.md](SECURITY.md) 与 [docs/security.md](docs/security.md)。

## 最小生产配置提示

开发环境默认值偏宽松，生产环境不要直接照搬。

- Auth：默认关闭，生产环境至少应保护 `/api/*` 与 `/metrics`
- Meta DB：如果配置了 `meta_dsn`，必须先执行 migration，服务不会自动建表或自动升级 schema
- Upload：S3-compatible upload 是可选能力，但一旦启用，必填项必须完整
- Tracing：默认关闭，启用前先确认 exporter 配置和采样策略

生产环境最小建议：

```bash
export BINLOG_SERVER_API_AUTH_ENABLED=true
export BINLOG_SERVER_API_AUTH_MODE=bearer
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="$(openssl rand -hex 32)"
export BINLOG_SERVER_API_AUTH_PROTECT_API=true
export BINLOG_SERVER_API_AUTH_PROTECT_METRICS=true
```

如果使用 metadata database：

```bash
export META_DSN='meta:replace_me@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true'
make migrate-up META_DSN="$META_DSN"
```

如需启用 upload，至少提供这些配置：

- `BINLOG_SERVER_UPLOAD_ENDPOINT`
- `BINLOG_SERVER_UPLOAD_BUCKET`
- `BINLOG_SERVER_UPLOAD_ACCESS_KEY`
- `BINLOG_SERVER_UPLOAD_SECRET_KEY`

## FAQ / Common Pitfalls

### `make e2e-quick` 本地失败

- 先确认 Docker Desktop 或其他 Docker daemon 已启动。
- E2E 会拉起 MySQL / Percona 容器，并依赖本地 Docker 环境。
- 更详细的 E2E 说明见 [scripts/e2e/README.md](scripts/e2e/README.md)。

### 配了 `meta_dsn` 但任务跑不起来

- 常见原因是 metadata schema 还没 migrate。
- 先执行 `make migrate-up META_DSN=...`，再启动服务。
- 迁移命令说明见 [cmd/migrate/README.md](cmd/migrate/README.md)。

### 生产环境忘了开 auth

- 这是当前最需要显式覆盖的开发默认值。
- 开发环境默认关闭 auth，生产环境不应该这样部署。
- 具体安全配置建议见 [SECURITY.md](SECURITY.md) 和 [docs/security.md](docs/security.md)。

### upload 配置了但上传不工作

- `endpoint`、`bucket`、`access_key`、`secret_key` 必须完整出现。
- `region` 和 `prefix` 是可选项，不属于初始化必填。
- 当前 upload 实现面向 S3-compatible API。

### `/metrics` 或 tracing 看起来“没数据”

- `/metrics` 在任务还没运行时也会暴露基础指标；部分值可能只是 placeholder。
- tracing 默认关闭，所以没有 span 通常是预期行为，不一定是故障。

## Upgrade / Release 入口

升级前先看 [CHANGELOG.md](CHANGELOG.md)。

升级时优先关注这几类变化：

- schema / migration 变更
- config key 新增、废弃或默认值变化
- `sqlc` 相关工作流变化
- observability 合约变化，例如 metrics / tracing 对 dashboard 或告警的影响

这个仓库不会自动帮你 apply migration，也不会自动迁移配置，因此升级应当按运维变更来处理，而不是只替换二进制。

## 仓库入口导航

如果你已经跑通 `Quick Start`，下一步从这里进入：

| 主题 | 入口 |
| --- | --- |
| 使用与运维 guide | [docs/guide/README.md](docs/guide/README.md) |
| 安全策略 | [SECURITY.md](SECURITY.md) |
| 版本变化 | [CHANGELOG.md](CHANGELOG.md) |
| 服务启动命令 | [cmd/binlog-server/README.md](cmd/binlog-server/README.md) |
| 数据库迁移 | [cmd/migrate/README.md](cmd/migrate/README.md) |
| API 模块 | [internal/api/README.md](internal/api/README.md) |
| 复制执行链路 | [internal/replication/README.md](internal/replication/README.md) |
| Upload 模块 | [internal/upload/README.md](internal/upload/README.md) |
| E2E 测试套件 | [scripts/e2e/README.md](scripts/e2e/README.md) |

## 开发验证入口

常用验证命令：

```bash
go test ./...
go vet ./...
make e2e-quick
```

如果你要以前后端分离方式开发前端：

```bash
cd frontend
npm install
npm run dev
```
