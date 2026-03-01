# 配置参数详解

本文档详细说明所有配置参数及其作用。

## 1. 配置优先级

```
默认值 < 配置文件 < 环境变量
```

环境变量命名规则：`BINLOG_SERVER_<FIELD_NAME>`

例如：
- `listen_addr` → `BINLOG_SERVER_LISTEN_ADDR`
- `meta_dsn` → `BINLOG_SERVER_META_DSN`

### 1.1 敏感信息保护（重要）

**推荐使用环境变量占位符**，避免在配置文件中明文存储敏感信息：

```yaml
# 推荐：使用 ${ENV_VAR} 占位符
meta_dsn: "${BINLOG_SERVER_META_DSN}"
upload:
  access_key: "${BINLOG_SERVER_UPLOAD_ACCESS_KEY}"
  secret_key: "${BINLOG_SERVER_UPLOAD_SECRET_KEY}"

# 不推荐：明文存储敏感信息（启动时会警告）
meta_dsn: "root:password123@tcp(127.0.0.1:3306)/db"
```

**占位符语法：** `${ENV_VAR_NAME}`

启动时，系统会自动将 `${ENV_VAR}` 替换为对应的环境变量值。如果环境变量未设置，则保留原占位符。

**安全警告：** 如果配置文件中包含明文敏感信息（DSN、密码、密钥），启动时会打印警告：

```
config warning: key "meta_dsn" appears to contain plaintext sensitive value; prefer ${ENV_VAR} or environment injection
```

**敏感字段列表：**
- `meta_dsn` - 元数据库连接串
- `upload.access_key` - 对象存储 Access Key
- `upload.secret_key` - 对象存储 Secret Key

## 2. 完整配置示例

```yaml
# 服务监听地址
listen_addr: ":8080"

# 数据存储目录
data_dir: "./data"

# 元数据库连接串（推荐使用环境变量占位符）
meta_dsn: "${BINLOG_SERVER_META_DSN}"

# 运行模式：standalone 或 cluster
mode: "standalone"

# 集群配置（cluster 模式）
cluster:
  role: "all-in-one"           # control-plane / worker / all-in-one
  worker_id: ""                # 可选，不配置则自动生成并持久化
  worker_health_listen_addr: "" # 仅 role=worker 时生效
  lease_ttl_sec: 15            # 租约有效期（秒）
  lease_renew_interval_sec: 5  # 续租间隔（秒）
  lease_grace_sec: 30          # 宽限期（秒）
  failover_policy: "rebuild_current_file"

# 上传配置（可选）
upload:
  endpoint: ""
  bucket: ""
  access_key: "${BINLOG_SERVER_UPLOAD_ACCESS_KEY}"  # 推荐使用环境变量
  secret_key: "${BINLOG_SERVER_UPLOAD_SECRET_KEY}"  # 推荐使用环境变量
  region: ""
  prefix: ""
  use_ssl: false

# 日志配置
log:
  level: "info"                # debug / info / warn / error
  encoding: "json"             # json / console
  file: "./logs/binlog-server.log"
  max_size_mb: 100             # 单文件最大大小（MB）
  max_backups: 7               # 保留旧文件最大数量
  max_age_days: 30             # 保留旧文件最大天数
  compress: false              # 是否压缩旧文件
  rotate_interval: "24h"       # 定时轮转间隔
```

## 3. 参数详解

### 3.1 基础配置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `listen_addr` | string | `:8080` | HTTP API 监听地址 |
| `data_dir` | string | `./data` | binlog 文件存储目录 |
| `mode` | string | `standalone` | 运行模式 |

### 3.2 元数据库配置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `meta_dsn` | string | - | MySQL 连接字符串（cluster 模式必需） |

**DSN 格式：**

```
user:password@tcp(host:port)/database?parseTime=true
```

**推荐：使用环境变量占位符避免明文密码**

```yaml
meta_dsn: "${BINLOG_SERVER_META_DSN}"
```

```bash
# 设置环境变量
export BINLOG_SERVER_META_DSN="root:pass@tcp(127.0.0.1:3306)/binlog_server_meta?parseTime=true"
```

### 3.3 集群配置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `cluster.role` | string | all-in-one | 角色：control-plane / worker / all-in-one |
| `cluster.worker_id` | string | 自动生成 | Worker 唯一标识（可选，不配置则自动生成并持久化） |
| `cluster.worker_health_listen_addr` | string | - | Worker 健康检查地址（仅 role=worker 时生效） |
| `cluster.lease_ttl_sec` | int | 15 | 租约有效期（秒） |
| `cluster.lease_renew_interval_sec` | int | 5 | 续租间隔（秒） |
| `cluster.lease_grace_sec` | int | 30 | 宽限期（秒） |
| `cluster.failover_policy` | string | rebuild_current_file | 故障恢复策略 |

**角色说明：**

| 角色 | 职责 |
|------|------|
| `control-plane` | 提供 API、管理任务状态机、不执行任务 |
| `worker` | 接管并执行任务、不提供 API（仅健康检查） |
| `all-in-one` | 同时提供 API 和执行任务 |

**worker_id 自动生成：**

如果 `cluster.worker_id` 为空，系统会自动生成 UUID 并持久化到 `{data_dir}/.worker-id` 文件，重启后保持不变。

### 3.3.1 参数关系与推荐值（租约/认领/注册）

下表给出术语与真实配置项映射，避免把逻辑名和 YAML 名混淆：

| 逻辑名 | 真实配置项/实现 | 语义 |
|------|------|------|
| `lease.ttl` | `cluster.lease_ttl_sec` | 任务 lease 和 worker registration 的租期 |
| `lease.renew_interval` | `cluster.lease_renew_interval_sec` | 任务 lease 续租周期；worker registration 续租周期也复用该值 |
| `lease.grace_period` | `cluster.lease_grace_sec` | lease 续租报错后允许继续运行的宽限窗口 |
| `cluster.claim_interval` | 固定值 `2s`（`internal/app/app.go`） | worker 轮询认领 `STARTING` 任务的间隔（当前无配置项） |
| `worker registration ttl/renew` | 复用 `cluster.lease_ttl_sec`/`cluster.lease_renew_interval_sec` | 注册租期与续租周期，无独立 YAML 参数 |

推荐关系（至少满足）：

```text
lease_renew_interval_sec < lease_ttl_sec < lease_grace_sec
```

推荐起步值：

- `cluster.lease_ttl_sec = 15`
- `cluster.lease_renew_interval_sec = 5`
- `cluster.lease_grace_sec = 30`

说明：

- 以上也是当前默认值（见 `internal/config/config.go`）。
- worker registration 的 TTL/续租周期复用 cluster lease 参数（见 `internal/app/app.go` 的 `effectiveWorkerRegistrationTTL` 与 `effectiveWorkerRegistrationRenewInterval`）。
- 当 `lease_renew_interval_sec >= lease_ttl_sec` 时，worker registration 续租周期会被自动收敛到 `ttl/2`（`internal/app/app.go`），但任务 lease 仍建议显式配置成 `renew < ttl`。

哪些是“租期”，哪些是“单次调用超时”：

- 租期/周期参数：`cluster.lease_ttl_sec`、`cluster.lease_renew_interval_sec`、`cluster.lease_grace_sec`、claim interval(2s)。
- 单次调用超时（固定代码值，不是配置项）：worker registration `Renew` / `Release` 与 heartbeat `Upsert` 都使用 `context.WithTimeout(..., 2*time.Second)`（`internal/app/app.go`）。

反模式示例：

- `renew_interval` 过短（如 `1s`，且实例较多）会导致 MySQL 元数据写放大，出现续租抖动与日志风暴。
- `ttl` 接近 `renew_interval`（如 `ttl=6, renew=5`）在短时网络抖动下容易误判失租。
- `grace_period <= ttl` 会让续租瞬断时过快触发 `TASK_LEASE_GRACE_EXCEEDED`，增加非必要停机。

### 3.4 上传配置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `upload.endpoint` | string | - | S3 兼容端点 |
| `upload.bucket` | string | - | 存储桶名称 |
| `upload.access_key` | string | - | Access Key（推荐使用环境变量占位符） |
| `upload.secret_key` | string | - | Secret Key（推荐使用环境变量占位符） |
| `upload.region` | string | - | 区域 |
| `upload.prefix` | string | - | 对象前缀 |
| `upload.use_ssl` | bool | false | 是否使用 SSL |

**推荐配置：**

```yaml
upload:
  endpoint: "s3.amazonaws.com"
  bucket: "my-bucket"
  access_key: "${BINLOG_SERVER_UPLOAD_ACCESS_KEY}"
  secret_key: "${BINLOG_SERVER_UPLOAD_SECRET_KEY}"
  region: "us-east-1"
  prefix: "binlog-backup/"
  use_ssl: true
```

```bash
# 设置环境变量
export BINLOG_SERVER_UPLOAD_ACCESS_KEY="AKIAXXXX"
export BINLOG_SERVER_UPLOAD_SECRET_KEY="secret"
```

### 3.5 日志配置

日志使用 zap + lumberjack，支持**按大小 + 按时间**双重轮转。

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `log.level` | string | info | 日志级别：debug / info / warn / error |
| `log.encoding` | string | json | 输出格式：json / console |
| `log.file` | string | - | 日志文件路径（为空则输出到 stdout） |
| `log.max_size_mb` | int | 100 | 单文件最大大小（MB），超过则轮转 |
| `log.max_backups` | int | 7 | 保留旧文件的最大数量 |
| `log.max_age_days` | int | 30 | 保留旧文件的最大天数 |
| `log.compress` | bool | false | 是否压缩旧文件 |
| `log.rotate_interval` | duration | 24h | 定时轮转间隔（0 则禁用定时轮转） |

**配置示例：**

```yaml
log:
  level: "info"
  encoding: "json"
  file: "./logs/binlog-server.log"
  max_size_mb: 100      # 单文件超过 100MB 自动轮转
  max_backups: 7        # 最多保留 7 个旧文件
  max_age_days: 30      # 旧文件超过 30 天自动删除
  compress: false       # 不压缩旧文件
  rotate_interval: "24h" # 每 24 小时强制轮转一次
```

**轮转机制说明：**

| 触发条件 | 行为 |
|----------|------|
| 文件大小 > max_size_mb | 立即轮转（lumberjack 自动处理） |
| 距上次轮转 > rotate_interval | 定时器触发 Rotate()（即使文件很小） |
| 文件数量 > max_backups | 删除最旧文件 |
| 文件年龄 > max_age_days | 删除过期文件 |

**环境变量：**

```bash
export BINLOG_SERVER_LOG_LEVEL="debug"
export BINLOG_SERVER_LOG_ENCODING="console"
export BINLOG_SERVER_LOG_FILE="/var/log/binlog-server/app.log"
export BINLOG_SERVER_LOG_MAX_SIZE_MB="200"
export BINLOG_SERVER_LOG_MAX_BACKUPS="14"
export BINLOG_SERVER_LOG_MAX_AGE_DAYS="60"
export BINLOG_SERVER_LOG_COMPRESS="true"
export BINLOG_SERVER_LOG_ROTATE_INTERVAL="12h"
```

## 4. 任务配置参数

创建任务时通过 API 指定，不在配置文件中。

### 4.1 创建任务

**从最新位置开始（LATEST）：**

```bash
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "LATEST"
    },
    "storage": {
      "retention_days": 30
    }
  }'
```

**从指定文件位置开始（FILE_POS）：**

```bash
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "FILE_POS",
      "file": "mysql-bin.000010",
      "pos": 12345
    }
  }'
```

**从指定 GTID 开始（GTID）：**

```bash
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "backup-mysql-prod",
    "cluster_key": "prod-cluster",
    "source": {
      "host": "10.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret"
    },
    "start": {
      "mode": "GTID",
      "gtid": "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100"
    }
  }'
```

### 4.2 启动模式

| 模式 | 说明 |
|------|------|
| `LATEST` | 从最新位置开始（默认） |
| `FILE_POS` | 从指定文件和位置开始 |
| `GTID` | 从指定 GTID 开始 |

### 4.3 cluster_key 规则

- 必需字段
- 全局唯一
- 只允许 `[a-zA-Z0-9._-]`
- 不能包含 `/` 或 `..`

用途：
- 区分不同集群的备份
- 作为对象存储路径的一部分

### 4.4 常用操作 CURL 示例

**列出所有任务：**

```bash
curl http://localhost:8080/api/tasks
```

**获取单个任务：**

```bash
curl http://localhost:8080/api/tasks/{task_id}
```

**启动任务：**

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/start
```

**停止任务：**

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/stop
```

**删除任务：**

```bash
curl -X DELETE http://localhost:8080/api/tasks/{task_id}
```

**查看任务复制状态：**

```bash
curl http://localhost:8080/api/tasks/{task_id}/replication
```

**查看任务 Checkpoint：**

```bash
curl http://localhost:8080/api/tasks/{task_id}/checkpoint
```

**查看任务事件：**

```bash
curl "http://localhost:8080/api/tasks/{task_id}/events?limit=20"
```

**列出任务文件：**

```bash
curl http://localhost:8080/api/tasks/{task_id}/files
```

**重试上传失败的文件：**

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/files/retry-upload
```

**查看集群状态：**

```bash
curl http://localhost:8080/api/cluster/overview
```

**查看在线 Workers：**

```bash
curl http://localhost:8080/api/workers
```

**健康检查：**

```bash
curl http://localhost:8080/api/health
```

## 5. 环境变量示例

**推荐方式：配置文件 + 环境变量占位符**

```yaml
# config.yaml
meta_dsn: "${BINLOG_SERVER_META_DSN}"
upload:
  access_key: "${BINLOG_SERVER_UPLOAD_ACCESS_KEY}"
  secret_key: "${BINLOG_SERVER_UPLOAD_SECRET_KEY}"
```

```bash
# 敏感信息通过环境变量注入
export BINLOG_SERVER_META_DSN="root:pass@tcp(127.0.0.1:3306)/binlog_server_meta?parseTime=true"
export BINLOG_SERVER_UPLOAD_ACCESS_KEY="AKIAXXXX"
export BINLOG_SERVER_UPLOAD_SECRET_KEY="secret"

# 启动服务
./binlog-server --config config.yaml
```

**备选方式：完全使用环境变量**

```bash
# 基础配置
export BINLOG_SERVER_LISTEN_ADDR=":8080"
export BINLOG_SERVER_DATA_DIR="/data/binlog"
export BINLOG_SERVER_MODE="cluster"

# 元数据库（敏感）
export BINLOG_SERVER_META_DSN="root:pass@tcp(127.0.0.1:3306)/binlog_server_meta?parseTime=true"

# 集群配置
export BINLOG_SERVER_CLUSTER_ROLE="worker"
export BINLOG_SERVER_CLUSTER_WORKER_ID="worker-1"

# 上传配置（敏感）
export BINLOG_SERVER_UPLOAD_ACCESS_KEY="AKIAXXXX"
export BINLOG_SERVER_UPLOAD_SECRET_KEY="secret"
```

**Kubernetes Secret 示例：**

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: binlog-server-secrets
type: Opaque
stringData:
  META_DSN: "root:pass@tcp(mysql:3306)/binlog_server_meta?parseTime=true"
  UPLOAD_ACCESS_KEY: "AKIAXXXX"
  UPLOAD_SECRET_KEY: "secret"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: binlog-server-config
data:
  config.yaml: |
    listen_addr: ":8080"
    mode: "cluster"
    meta_dsn: "${BINLOG_SERVER_META_DSN}"
    upload:
      access_key: "${BINLOG_SERVER_UPLOAD_ACCESS_KEY}"
      secret_key: "${BINLOG_SERVER_UPLOAD_SECRET_KEY}"
```

## 6. 配置验证

启动时会自动验证配置，常见错误：

| 错误 | 原因 |
|------|------|
| `cluster.worker_id is required in cluster mode` | 集群模式未配置 worker_id |
| `meta.dsn is required in cluster mode` | 集群模式未配置元数据库 |
| `storage.retention_days must be 1-3650` | 保留天数超出范围 |
| `invalid cluster_key format` | cluster_key 包含非法字符 |

## 7. 下一步

- 阅读 [故障排查](./troubleshooting.md) 学习如何诊断配置问题
- 阅读 [可观测性](./observability.md) 了解如何监控服务状态
