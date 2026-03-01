# binlog_server

Binlog Server：MySQL binlog 拉取、落盘、位点持久化与集群租约调度服务。

当前已实现：

- 单进程 Go 服务启动骨架（Gin 路由）
- 任务状态机与内存调度器
- 管理 API（创建任务、列表、启动、停止）
- 健康检查 `/healthz`
- `fsync` 成功后才推进 checkpoint 的可靠性语义
- MySQL 复制协议拉流（`LATEST/FILE_POS/GTID` 起点）

学习与运维入口：`docs/guide/README.md`
开发 TODO：`docs/develop/TODO.md`
集群 HA 设计草案：`docs/develop/plans/2026-02-16-cluster-ha-design.md`
集群 HA 实施计划：`docs/develop/plans/2026-02-16-cluster-ha-implementation-plan.md`
Swagger 使用指南：`docs/swagger-api-guide.md`

## Architecture

Binlog Server 由命令入口层（`cmd`）、核心运行时（`internal`）、前端管理台（`frontend`）和工程脚本（`scripts`）组成。  
服务启动后，API 与 UI 统一由后端进程暴露；任务调度驱动复制与元数据持久化，前端通过 `/api/*` 进行控制与观测。

### Modules

| Module | Description | Doc |
|--------|-------------|-----|
| `cmd` | 服务与迁移命令入口 | [README](cmd/README.md) |
| `cmd/binlog-server` | 主服务启动命令 | [README](cmd/binlog-server/README.md) |
| `cmd/migrate` | 数据库迁移命令 | [README](cmd/migrate/README.md) |
| `internal` | 核心业务与基础设施模块 | [README](internal/README.md) |
| `internal/api` | HTTP API 与路由 | [README](internal/api/README.md) |
| `internal/tasks` | 任务状态机与调度核心 | [README](internal/tasks/README.md) |
| `internal/meta` | 元数据存储与 schema 校验 | [README](internal/meta/README.md) |
| `internal/replication` | MySQL 复制执行链路 | [README](internal/replication/README.md) |
| `frontend` | 管理台前端源码与构建 | [README](frontend/README.md) |
| `scripts` | 构建与 E2E 脚本入口 | [README](scripts/README.md) |
| `scripts/e2e` | E2E 套件与场景脚本 | [README](scripts/e2e/README.md) |

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

可选元数据 MySQL DSN：`BINLOG_SERVER_META_DSN=${BINLOG_META_DSN}`

使用元数据库时，请先执行数据库迁移（服务启动不会自动建表/改表）：

```bash
export BINLOG_META_PASS='replace_me'
export META_DSN="meta:${BINLOG_META_PASS}@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true"
make migrate-up META_DSN="$META_DSN"
```

可选上传（当前实现：S3 API 兼容对象存储）：
- `BINLOG_SERVER_UPLOAD_ENDPOINT`
- `BINLOG_SERVER_UPLOAD_BUCKET`
- `BINLOG_SERVER_UPLOAD_ACCESS_KEY`
- `BINLOG_SERVER_UPLOAD_SECRET_KEY`
- `BINLOG_SERVER_UPLOAD_REGION`（可选）
- `BINLOG_SERVER_UPLOAD_PREFIX`（可选）
- `BINLOG_SERVER_UPLOAD_USE_SSL=true|false`

API 鉴权（默认保护 `/api/*` 与 `/metrics`）：
- `BINLOG_SERVER_API_AUTH_ENABLED=true|false`
- `BINLOG_SERVER_API_AUTH_MODE=bearer|api_key`
- `BINLOG_SERVER_API_AUTH_BEARER_TOKEN`（`mode=bearer` 时必填）
- `BINLOG_SERVER_API_AUTH_API_KEY`（`mode=api_key` 时必填）
- `BINLOG_SERVER_API_AUTH_API_KEY_HEADER`（默认 `X-API-Key`）
- `BINLOG_SERVER_API_AUTH_PROTECT_API=true|false`
- `BINLOG_SERVER_API_AUTH_PROTECT_METRICS=true|false`

上传适配说明：
- 当前上传实现走 S3 API 兼容路径（MinIO SDK）。
- 其余对象存储厂商可通过兼容层接入，是否切换官方 SDK 以代码实现为准。

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
  "cluster_key": "cluster-a-prod",
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
  "cluster_key": "cluster-a-prod",
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
  "cluster_key": "cluster-a-prod",
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
- `POST /api/tasks/{id}/files/retry-upload`（手动补传 `UPLOAD_FAILED` 文件）
- `GET /api/tasks/{id}/upload-failures/reasons`（失败原因聚合）
- `GET /api/workers`（集群 worker 视图）
- `GET /api/cluster/overview`（集群汇总）
- `GET /api/tasks/{id}/lease`（任务 lease 归属）
- `GET /api/tasks/{id}/runs`（Run History，最近 N 条）
- `GET /metrics`（Prometheus 指标）
- `GET /healthz`

说明：`cluster_key` 为创建/更新任务必填字段，且全局唯一。
如果服务启用了 MySQL runner（当前默认启用），任务 `start` 前必须配置有效 `source`。
`storage.retention_days` 默认 7 天，runner 会在打开 binlog 文件时清理过期文件（跳过当前活动文件）。
`source.server_id` 支持自定义，推荐为每个任务显式设置唯一值，避免与其他 replication client/slave 冲突；不设置时系统会按 task ID 自动生成默认值。
`source.semi_sync` 默认 `false`；设置为 `true` 时会尝试半同步拉流，若主库未开启半同步会自动降级为异步继续运行。

输入校验规则（前端预校验 + 后端权威校验）：
- `name`：trim 后 1-255 字符。
- `cluster_key`：trim 后非空；仅允许 `[a-zA-Z0-9._-]`；禁止 `/`、`\`、`..`。
- `source.host`：trim 后非空、长度 <=255，且不允许空白字符。
- `source.port`：1-65535。
- `source.user`：trim 后非空、长度 <=128，且不允许空白字符。
- `source.flavor`：为空时默认 `mysql`；非空时仅允许 `[a-zA-Z0-9._-]` 且长度 <=32。
- `start.mode`：仅允许 `LATEST` / `FILE_POS` / `GTID`。
- `start.file`、`start.pos`：当 `FILE_POS` 时必填，且 `file` 长度 <=255、`pos` > 0。
- `start.gtid_set`：当 `GTID` 时必填且非空。
- `storage.retention_days`：1-3650。

如果配置了 `BINLOG_SERVER_META_DSN`，任务配置与状态会持久化到外部 MySQL，服务重启后会自动恢复任务元数据。
同时会持久化每个任务的最新 checkpoint（`file/pos`），重启后优先从 checkpoint 位点继续拉取。
任务事件（创建、启动、重试、错误等）也会持久化到 MySQL，可通过 `/api/tasks/{id}/events` 查询。
`/api/tasks/{id}/files` 会返回文件元数据与上传状态（`LOCAL_ONLY/UPLOADED/UPLOAD_FAILED`）。
开启上传后，只会在 binlog 文件 **seal（封口）** 后上传。  
这里 `seal` 指：当前 OPEN 文件在 rotate 或 stop 时被重命名为最终文件名（去掉 `.open.e<epoch>`），不再继续写入。
上传触发时机是“文件已封口且不再追加”，避免对象存储上出现同名对象多版本覆盖和低频层早删成本风险。
object key 规则（当前实现）：`<prefix>/<cluster_key>/<source_server_uuid>/<fileName>`（prefix 可空）。
当前上传策略是“最佳努力模式”：上传失败会记录为 `UPLOAD_FAILED`，但不会中断 binlog 拉取。
可通过 `POST /api/tasks/{id}/files/retry-upload?limit=100` 对历史 `UPLOAD_FAILED` 文件做手动补传。
可通过 `GET /api/tasks/{id}/upload-failures/reasons?limit=20` 聚合查看失败原因与频次。
默认不做远端对象回读校验（不额外 download 对象做 checksum 比对），由本地 seal/rotate 语义保证“单文件完整后再上传”。

当前实现基于 S3 API 兼容协议，常见可用后端包括：
- 华为云 OBS（S3 兼容）
- 腾讯云 COS（S3 兼容）
- 阿里云 OSS（需使用其 S3 兼容接入方式）

后续目标（已在 TODO 记录）：
- AWS S3 继续使用 MinIO SDK；
- 华为云 OBS / 腾讯云 COS / 阿里云 OSS 分别接入官方 SDK 实现。

## Cluster 部署与运行（速查）

### 1) standalone（默认）

```yaml
mode: standalone
listen_addr: ":8080"
data_dir: "./data"
meta_dsn: ""
```

适用场景：单机、快速上线、无集群抢占需求。

### 2) cluster + all-in-one（单节点集群形态）

```yaml
mode: cluster
cluster:
  role: all-in-one
  worker_id: "node-a"
  lease_ttl_sec: 15
  lease_renew_interval_sec: 5
  lease_grace_sec: 30
  failover_policy: rebuild_current_file
meta_dsn: "${BINLOG_SERVER_META_DSN}"
```

适用场景：小规模集群，先接入 lease/epoch 语义，再平滑扩容。

### 3) cluster + control-plane / worker（推荐生产形态）

control-plane 节点：

```yaml
mode: cluster
cluster:
  role: control-plane
meta_dsn: "${BINLOG_SERVER_META_DSN}"
listen_addr: ":8080"
```

worker 节点：

```yaml
mode: cluster
cluster:
  role: worker
  worker_id: "worker-bj-a"
  worker_health_listen_addr: ":18081"
  lease_ttl_sec: 15
  lease_renew_interval_sec: 5
  lease_grace_sec: 30
  failover_policy: rebuild_current_file
meta_dsn: "${BINLOG_SERVER_META_DSN}"
```

部署建议：
- control-plane 至少 1 台（按 API/UI SLA 可做多实例）。
- worker 至少 2 台，`worker_id` 全局唯一。
- 所有节点 `meta_dsn` 指向同一元数据库入口（VIP/ProxySQL）。
- 若是 worker-only 部署，建议配置 `cluster.worker_health_listen_addr` 暴露 `/healthz` 与 `/readyz` 供探针使用。

## Failover 期间预期行为与告警解释

在 cluster 模式、元数据库发生主从切换时，预期行为如下：

1. worker 可能短暂进入 lease 续约失败窗口（degraded）。
2. 若在 `lease_grace_sec` 内恢复，任务继续运行；checkpoint 持续推进。
3. 若超出 `lease_grace_sec` 仍无法续约，worker fail-safe 停止（避免错误 seal/upload）。
4. 重新抢占成功后按 `rebuild_current_file` 策略恢复，优先保证文件完整与字节一致。

常见事件/告警可从 `/api/tasks/{id}/events` 观察：
- `TASK_LEASE_DEGRADED`：续约失败但仍在 grace 窗口。
- `TASK_LEASE_LOST`：lease 丢失，触发保护停机。
- `TASK_LEASE_GRACE_EXCEEDED`：grace 超时，必须停止。

## 运维排障入口（关键命令）

健康与基础状态：

```bash
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/api/summary | jq
curl -s http://127.0.0.1:8080/api/cluster/overview | jq
curl -s http://127.0.0.1:8080/api/workers | jq
```

按任务排障：

```bash
TASK_ID=1
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/replication" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/lease" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/runs" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/events?limit=50" | jq
```

e2e 回归（failover 相关）：

```bash
./scripts/e2e/run-suite.sh --scenarios meta-failover
./scripts/e2e/run-suite.sh --scenarios meta-failover-override
./scripts/e2e/run-suite.sh --scenarios smoke-cluster-roles
./scripts/e2e/run-suite.sh --scenarios smoke-control-plane-failover
```

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
# full：smoke + compression + orchestrator + semisync + meta-failover
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

## Observability

- 指标入口：`GET /metrics`（Prometheus exposition 格式）
- 观测文档：`docs/observability.md`
