# binlog_server

Binlog Server MVP（进行中）。

当前已实现：

- 单进程 Go 服务启动骨架（Gin 路由）
- 任务状态机与内存调度器
- 管理 API（创建任务、列表、启动、停止）
- 健康检查 `/healthz`
- `fsync` 成功后才推进 checkpoint 的可靠性语义
- MySQL 复制协议拉流（`LATEST/FILE_POS/GTID` 起点）

学习路线文档：`docs/learning-guide.md`  
分节目录：`docs/learning/README.md`

## 运行

```bash
go run ./cmd/binlog-server
```

使用 YAML 配置文件：

```bash
go run ./cmd/binlog-server --config ./config.yaml
```

或先构建二进制：

```bash
go build -o binlog-server ./cmd/binlog-server
./binlog-server --config ./config.yaml
```

默认监听地址：`:8080`  
可通过环境变量覆盖：`BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080`

启动后打开管理台：`http://127.0.0.1:8080/ui/`
（`/ui/` 使用 `internal/ui/static` 下的前端构建产物）
  
默认数据目录：`./data`  
可通过环境变量覆盖：`BINLOG_SERVER_DATA_DIR=/path/to/data`

可选元数据 MySQL DSN：`BINLOG_SERVER_META_DSN=user:pass@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true`

可选上传（S3/OBS 兼容）：
- `BINLOG_SERVER_UPLOAD_ENDPOINT`
- `BINLOG_SERVER_UPLOAD_BUCKET`
- `BINLOG_SERVER_UPLOAD_ACCESS_KEY`
- `BINLOG_SERVER_UPLOAD_SECRET_KEY`
- `BINLOG_SERVER_UPLOAD_REGION`（可选）
- `BINLOG_SERVER_UPLOAD_PREFIX`（可选）
- `BINLOG_SERVER_UPLOAD_USE_SSL=true|false`

配置加载优先级：`默认值 < YAML 配置文件 < 环境变量`。  
未传 `--config` 时，会尝试读取当前目录 `./config.yaml`，不存在则忽略。

配置文件示例（YAML）：

```yaml
listen_addr: ":8080"
data_dir: "./data"
meta_dsn: ""
upload:
  endpoint: ""
  bucket: ""
  access_key: ""
  secret_key: ""
  region: ""
  prefix: ""
  use_ssl: false
```

仓库内也提供示例文件：`config.example.yaml`

## 前后端分离开发

后端：

```bash
go run ./cmd/binlog-server
```

前端（Vue3 + Element Plus）：

```bash
cd frontend
npm install
npm run dev
```

默认前端地址：`http://127.0.0.1:5173`  
`vite` 已代理 `/api` 到 `http://127.0.0.1:8080`。

可通过环境变量切换前端代理目标（例如后端跑在 `18080`）：

```bash
cd frontend
VITE_API_TARGET=http://127.0.0.1:18080 npm run dev
```

将前端打包并同步到后端内置 UI（`/ui/`）：

```bash
make ui-build
```

## Swagger API 文档

启动后端后可直接访问：

- `http://127.0.0.1:8080/swagger/index.html`

你可以在页面中：

- 浏览所有已文档化的 API endpoint
- 查看 request/response schema
- 直接在浏览器执行请求（Try it out）
- 调整参数并查看实时响应

详细用法与调试案例见：`docs/swagger-api-guide.md`

若你新增/修改了 handler 的 Swagger 注解，可重新生成文档：

```bash
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/binlog-server/main.go -o internal/swaggerdocs --parseInternal
```

## API

- `POST /api/tasks` body 示例：

```json
{
  "name": "cluster-a",
  "source": {
    "host": "127.0.0.1",
    "port": 3306,
    "user": "repl",
    "password": "secret",
    "flavor": "mysql",
    "server_id": 200001,
    "semi_sync": false
  },
  "start": {
    "mode": "LATEST"
  },
  "storage": {
    "retention_days": 7
  }
}
```

- `POST /api/tasks` 指定 FILE/POS 示例：

```json
{
  "name": "cluster-a",
  "source": {
    "host": "127.0.0.1",
    "port": 3306,
    "user": "repl",
    "password": "secret",
    "flavor": "mysql"
  },
  "start": {
    "mode": "FILE_POS",
    "file": "mysql-bin.000123",
    "pos": 4
  }
}
```

- `POST /api/tasks` 指定 GTID 示例：

```json
{
  "name": "cluster-a",
  "source": {
    "host": "127.0.0.1",
    "port": 3306,
    "user": "repl",
    "password": "secret",
    "flavor": "mysql"
  },
  "start": {
    "mode": "GTID",
    "gtid_set": "24BC785E-9A61-11E1-8A5D-080027635EF5:1-10"
  }
}
```

- `GET /api/tasks`
- `GET /api/tasks?host=127.0.0.1&port=3306`（按源库过滤）
- `GET /api/summary`
- `GET /api/dashboard`（大盘聚合，支持 `host/port` 过滤）
- `GET /api/sources/lookup?host=127.0.0.1&port=3306`（检查该源库是否有任务）
- `GET /api/tasks/{id}`
- `PUT /api/tasks/{id}`
- `DELETE /api/tasks/{id}`
- `POST /api/tasks/{id}/start`
- `POST /api/tasks/{id}/stop`
- `GET /api/tasks/{id}/checkpoint`
- `GET /api/tasks/{id}/replication`（复制延迟与最近位点）
- `GET /api/tasks/{id}/events`
- `GET /api/tasks/{id}/files`
- `GET /healthz`

说明：如果服务启用了 MySQL runner（当前默认启用），任务 `start` 前必须配置有效 `source`。
`storage.retention_days` 默认 7 天，runner 会在打开 binlog 文件时清理过期文件（跳过当前活动文件）。
`source.server_id` 支持自定义，推荐为每个任务显式设置唯一值，避免与其他 replication client/slave 冲突；不设置时系统会按 task ID 自动生成默认值。
`source.semi_sync` 默认 `false`；设置为 `true` 时会尝试半同步拉流，若主库未开启半同步会自动降级为异步继续运行。

如果配置了 `BINLOG_SERVER_META_DSN`，任务配置与状态会持久化到外部 MySQL，服务重启后会自动恢复任务元数据。
同时会持久化每个任务的最新 checkpoint（`file/pos`），重启后优先从 checkpoint 位点继续拉取。
任务事件（创建、启动、重试、错误等）也会持久化到 MySQL，可通过 `/api/tasks/{id}/events` 查询。
`/api/tasks/{id}/files` 会返回文件元数据与上传状态（`LOCAL_ONLY/UPLOADED/UPLOAD_FAILED`）。
开启上传后，binlog rotate 封口后会上传到对象存储，object key 规则：`<prefix>/<taskID>/<fileName>`（prefix 可空）。
当前上传策略是“最佳努力模式”：上传失败会记录为 `UPLOAD_FAILED`，但不会中断 binlog 拉取。

## Docker E2E

为了便于开源协作和本地联调，仓库提供了四源库并行的 e2e 环境：
详细脚本说明见：`scripts/e2e/README.md`

- `mysql:5.7`
- `mysql:8.0`
- `percona/percona-server:5.7`
- `percona/percona-server:8.0`

MySQL 参数使用 `my.cnf` 管理（公共模板 + 服务差异配置）：

- `deploy/e2e/my.cnf/base.cnf`
- `deploy/e2e/my.cnf/mysql57.cnf`
- `deploy/e2e/my.cnf/mysql80.cnf`
- `deploy/e2e/my.cnf/percona57.cnf`
- `deploy/e2e/my.cnf/percona80.cnf`

说明：

- `mysql80/percona80` 默认开启 `binlog_transaction_compression=ON`
- 在 ARM 主机上，`mysql57/percona57` 使用 `linux/amd64` 运行（Docker Desktop 会走 emulation）

文件位置：

- `deploy/e2e/docker-compose.yml`
- `deploy/e2e/orchestrator/orchestrator.conf.json`
- `deploy/e2e/init/00-init.sql`
- `deploy/e2e/config.yaml`
- `scripts/e2e/up.sh`
- `scripts/e2e/run-server.sh`
- `scripts/e2e/smoke.sh`
- `scripts/e2e/smoke-compression.sh`
- `scripts/e2e/smoke-orchestrator.sh`
- `scripts/e2e/smoke-semisync.sh`
- `scripts/e2e/run-suite.sh`
- `scripts/e2e/down.sh`

建议先装好 `jq`（脚本用它做 JSON 解析，避免难读的 `sed` 回退逻辑）。

日常（quick）回归：

```bash
# 默认 quick：smoke + compression
./scripts/e2e/run-suite.sh
# 或
make e2e-quick
```

全量（full）回归：

```bash
# full：smoke + compression + orchestrator + semisync
./scripts/e2e/run-suite.sh --profile full
# 或
make e2e-full
```

按场景自定义：

```bash
# 只跑指定场景（覆盖 profile）
./scripts/e2e/run-suite.sh --scenarios smoke,compression
./scripts/e2e/run-suite.sh --scenarios orchestrator,semisync
# 或
make e2e SCENARIOS=smoke,compression
```

`run-suite.sh` 默认会自动 `up -> 启动 binlog-server -> 跑场景 -> down`。  
排障时可保留环境：

```bash
./scripts/e2e/run-suite.sh --profile full --keep-env
```

你仍然可以手动跑单项脚本（兼容旧流程）：

```bash
./scripts/e2e/smoke.sh
./scripts/e2e/smoke-compression.sh
./scripts/e2e/smoke-orchestrator.sh
./scripts/e2e/smoke-semisync.sh
```

## 测试

```bash
go test ./...
```
