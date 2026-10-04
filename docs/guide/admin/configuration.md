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
- `api.auth.bearer_token` - API Bearer Token
- `api.auth.api_key` - API Key

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

# API 鉴权配置
api:
  auth:
    enabled: false             # 是否启用鉴权（默认不启用）
    mode: "bearer"             # 鉴权模式：bearer | api_key
    bearer_token: "${BINLOG_SERVER_API_AUTH_BEARER_TOKEN}"
    api_key: ""
    api_key_header: "X-API-Key"
    protect_api: false         # 是否保护 /api/* 路由
    protect_metrics: false     # 是否保护 /metrics 路由

# HTTP 超时配置
http:
  control_plane:               # control-plane API 服务超时
    read_header_timeout_sec: 5
    read_timeout_sec: 30
    write_timeout_sec: 30
    idle_timeout_sec: 120
  worker_health:               # worker 健康检查服务超时
    read_header_timeout_sec: 3
    read_timeout_sec: 10
    write_timeout_sec: 10
    idle_timeout_sec: 30

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
| `meta_dsn` | string | - | MySQL 连接字符串。cluster 模式必需。standalone 为空时控制面只在内存，进程退出后任务元数据丢失；binlog 文件仍落在 `data_dir`。 |

没配 `meta_dsn`，或该任务的 `binlog_files` 一条目录都没有时，`GET /api/tasks/{id}/files` 扫描 `{data_dir}/{task_id}/`，列出封存文件和 `.open.e<epoch>` 分段。standalone 用同一个 `data_dir` 重启后，任务列表和 dashboard 会列出仍有这些分段的目录，id 就是目录名，files 用同一套扫描；没有源库账号，不能启动。配了 `meta_dsn` 时不从磁盘发现任务。`file_name` 是源文件名，`file_path` 是磁盘路径，open 分段的 `file_path` 保留 `.open.e<epoch>`。`start_pos` 和 `end_pos` 是 0。磁盘扫描不编造 checkpoint：没有位点行时 `GET /api/tasks/{id}/checkpoint` 仍是 `404`，正文是 `checkpoint not found`。该任务已有目录行时以目录为准，磁盘上多出来的文件不会列出。store 出错不会退回磁盘。用磁盘分段回放见 [部署指南「源库不可用时，用本地分段回放」](deployment.md#replay-local-segments)（English: [Replay local segments when the source is gone](deployment.md#replay-local-segments-en)）。

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

### 3.6 API 鉴权配置

**鉴权默认行为与安全约束：**
- **Loopback 监听：** 监听 `127.0.0.1` / `localhost` / `::1` 时，鉴权默认可关闭（`api.auth.enabled=false`），方便本地开发调试。
- **非 Loopback 监听：** 监听 `:8080`、`0.0.0.0:8080` 或任何内网/公网 IP 时，系统启动时严格检查：`api.auth.enabled` 必须为 `true`，且 `protect_api` 与 `protect_metrics` 必须同时为 `true`，否则严格退出。
- **保护标志默认值行为：** 当配置 `api.auth.enabled: true` 时，若未显式指定 `protect_api` 与 `protect_metrics`，配置加载器会自动将它们缺省视为 `true`（无需手动冗余指定）。若显式配置为 `false`，则非 Loopback 监听与 `PRODUCTION=true` 均会报错拦截。

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `api.auth.enabled` | bool | false | 是否启用鉴权（非 Loopback 监听必须为 true） |
| `api.auth.mode` | string | bearer | 鉴权模式：`bearer` 或 `api_key` |
| `api.auth.bearer_token` | string | - | Bearer Token（推荐使用环境变量占位符，保护开启时必填） |
| `api.auth.api_key` | string | - | API Key（推荐使用环境变量占位符，保护开启时必填） |
| `api.auth.api_key_header` | string | X-API-Key | API Key 所在的请求头名称 |
| `api.auth.protect_api` | bool | false (enabled 时缺省视为 true) | 是否保护 `/api/*` 路由 |
| `api.auth.protect_metrics` | bool | false (enabled 时缺省视为 true) | 是否保护 `/metrics` 路由 |

**鉴权模式说明：**

| 模式 | 客户端使用方式 | 适用场景 |
|------|--------------|----------|
| `bearer` | `Authorization: Bearer <token>` | 标准 Bearer 认证，适合微服务与 Web 客户端 |
| `api_key` | `X-API-Key: <key>`（可自定义头名） | 简单自动化脚本调用 |

**路由保护范围矩阵 (v0.5.6)：**

| 路由 | 未开启鉴权 (`enabled=false`, 仅限 Loopback) | 开启鉴权 (`enabled=true`) |
|------|---------|-------------------|
| `/healthz` | **始终开放 (无鉴权)** | **始终开放 (无鉴权)**，供 LB/K8s 探活 |
| `/metrics` | 开放 | **受保护** (要求 Token / Key) |
| `/api/*` | 开放 | **受保护** (要求 Token / Key) |
| `/swagger/*` | 开放 | **受保护** (跟从 `api.auth.enabled` 拦截，不跟 `protect_api`) |
| `/ui/*` | 开放 | **开放**（页面和静态资源不鉴权；页面里的 `/api/*` 仍要 Token） |

> 浏览器打开 `/ui/` 不需要事先带 `Authorization`。`/api/*` 返回 401 时，控制台弹出设置框，填入 bearer token。Swagger 没有这个输入框，`/swagger/*` 仍用 curl 或反向代理注入请求头。`/healthz` 始终开放。设置框发送的是 `Authorization: Bearer`，不是 API Key 头。

**开发环境配置（默认，无鉴权）：**

```yaml
api:
  auth:
    enabled: false
    protect_api: false
    protect_metrics: false
```

**生产环境配置（启用鉴权）：**

```yaml
api:
  auth:
    enabled: true
    mode: "bearer"
    bearer_token: "${BINLOG_SERVER_API_AUTH_BEARER_TOKEN}"
    protect_api: true
    protect_metrics: true
```

```bash
# 设置环境变量
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="your-secure-token-here"
```

**使用 Bearer Token 调用 API：**

```bash
curl -H "Authorization: Bearer your-secure-token-here" \
  http://localhost:8080/api/tasks
```

浏览器打开 `http://<host>:<port>/ui/`，在设置里粘贴同一个 bearer token。不需要在地址栏或 curl 之外先准备 `Authorization` 头。



**使用 API Key 调用 API：**

```yaml
# 配置
api:
  auth:
    enabled: true
    mode: "api_key"
    api_key: "${BINLOG_SERVER_API_AUTH_API_KEY}"
    api_key_header: "X-API-Key"
    protect_api: true
```

```bash
# 调用
curl -H "X-API-Key: your-api-key-here" \
  http://localhost:8080/api/tasks
```

**配置验证规则：**

- `enabled=false` 时，`protect_api` 和 `protect_metrics` 必须为 `false`（否则启动报错）
- `enabled=true` 且启用保护时，必须配置对应的凭证（`bearer_token` 或 `api_key`）
- `mode` 只能是 `bearer` 或 `api_key`

**生产环境认证警告：**

当 `api.auth.enabled=false` 时，启动时会显示黄色警告：

```
⚠️  SECURITY WARNING: API authentication is DISABLED
   For production, set api.auth.enabled=true and configure your auth method.
   See docs/security.md for details.
```

如果设置环境变量 `PRODUCTION=true`，则服务拒绝在未启用认证时启动：

```bash
export PRODUCTION=true
./binlog-server --config config.yaml
# Error: api.auth.enabled must be true in PRODUCTION mode
```

### 3.7 API 限流配置

为防止 API 滥用，Binlog Server 内置了基于 IP 的令牌桶限流器：

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `api.rate_limit.enabled` | bool | true | 是否启用限流 |
| `api.rate_limit.requests_per_second` | float | 100 | 每秒请求数上限 |
| `api.rate_limit.burst` | int | 200 | 突发容量 |

**限流机制说明：**

- 基于**客户端 IP** 维度限流（支持 `X-Forwarded-For` 和 `X-Real-IP` 头）
- 使用令牌桶算法，允许短时突发流量（burst）
- 超过限制返回 HTTP 429：`{"error":"rate limit exceeded","code":429}`

**配置示例：**

```yaml
api:
  rate_limit:
    enabled: true
    requests_per_second: 100  # 100 req/s
    burst: 200                # 允许短时突发到 200
```

**环境变量：**

```bash
export BINLOG_SERVER_API_RATE_LIMIT_ENABLED=true
export BINLOG_SERVER_API_RATE_LIMIT_REQUESTS_PER_SECOND=100
export BINLOG_SERVER_API_RATE_LIMIT_BURST=200
```

### 3.8 配置值加密与 `--encryption-key` 生产强校验

BinlogServer 支持使用 AES-256-GCM 对敏感配置值进行加解密，同时也使用该密钥对元数据库中的源库连接密码进行落盘加密：

**核心安全规则：**
1. **`PRODUCTION=true` 必须提供密钥：** 当设置环境变量 `PRODUCTION=true` 时，启动时必须通过命令行参数 `--encryption-key` 提供 32 字节的 AES 密钥。如果 key 为空，服务将拒绝启动，甚至不会打开网络监听端口。
2. **源库密码透明落盘加密：** 只要提供了 `--encryption-key`，所有通过 API 创建或更新的任务，其源库密码在存入 MySQL `backup_tasks.source_json` 字段时，均会自动加密为 `enc:aes256:<base64-encoded-ciphertext>` 密文。非生产环境若未提供密钥，则保持旧版本兼容的明文 JSON 存储。
3. **配置文件字段解密：** 配置文件中任何标有 `enc:aes256:...` 的值，在加载时会自动使用传入的密钥解密。

**加密密钥生成与启动示例：**

1. **生成 32 字节强随机密钥（AES-256 需要）：**
   ```bash
   # 生成 32 字节十六进制字符密钥
   export BINLOG_SERVER_ENCRYPTION_KEY="$(openssl rand -hex 16)"
   ```

2. **在生产环境启动服务：**
   ```bash
   export PRODUCTION=true
   export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="$(openssl rand -hex 32)"
   
   ./binlog-server --config config.production.example.yaml --encryption-key "$BINLOG_SERVER_ENCRYPTION_KEY"
   ```

**安全建议：**
- 密钥请通过受控的注入渠道（如 Kubernetes Secrets、HashiCorp Vault、AWS Secrets Manager）直接作为环境变量或命令行参数传递。
- 绝不要将密钥写入 git 版本控制或硬编码在镜像中。

### 3.9 HTTP 超时配置

HTTP 超时配置用于防止慢连接拖垮服务，**两个独立的 HTTP 服务**各有独立配置：

| 服务 | 配置块 | 监听地址 | 用途 |
|------|--------|---------|------|
| Control Plane | `http.control_plane` | `listen_addr` | API/UI/Swagger/Metrics |
| Worker Health | `http.worker_health` | `cluster.worker_health_listen_addr` | 健康检查（仅 worker 角色） |

**超时参数说明：**

| 参数 | 默认值（Control Plane） | 默认值（Worker Health） | 说明 |
|------|----------------------|----------------------|------|
| `read_header_timeout_sec` | 5 | 3 | 读取请求头的超时（秒） |
| `read_timeout_sec` | 30 | 10 | 读取整个请求体的超时（秒） |
| `write_timeout_sec` | 30 | 10 | 写入响应的超时（秒） |
| `idle_timeout_sec` | 120 | 30 | Keep-Alive 连接空闲超时（秒） |

**超时参数的意义：**

```
┌─────────────────────────────────────────────────────────────────┐
│                      HTTP 请求生命周期                           │
│                                                                 │
│  客户端 ──────► 建立连接 ──────► 发送请求头 ──────► 发送请求体    │
│                    │                 │                  │       │
│                    │                 │                  │       │
│                    │                 ▼                  ▼       │
│                    │         ReadHeaderTimeout    ReadTimeout   │
│                    │                                        │    │
│                    ▼                                        ▼    │
│               IdleTimeout                           WriteTimeout │
│              (Keep-Alive)                              (响应)    │
└─────────────────────────────────────────────────────────────────┘
```

**推荐配置：**

```yaml
http:
  control_plane:
    read_header_timeout_sec: 5    # 防止慢速攻击
    read_timeout_sec: 30          # 允许较大请求体
    write_timeout_sec: 30         # 允许较大响应
    idle_timeout_sec: 120         # 支持连接复用
  worker_health:
    read_header_timeout_sec: 3    # 健康检查要求快速响应
    read_timeout_sec: 10
    write_timeout_sec: 10
    idle_timeout_sec: 30          # 健康检查不需要长连接复用
```

**环境变量：**

```bash
export BINLOG_SERVER_HTTP_CONTROL_PLANE_READ_TIMEOUT_SEC="60"
export BINLOG_SERVER_HTTP_CONTROL_PLANE_WRITE_TIMEOUT_SEC="60"
export BINLOG_SERVER_HTTP_WORKER_HEALTH_READ_TIMEOUT_SEC="10"
```

**为什么需要超时配置？**

| 问题 | 没有超时 | 有超时 |
|------|---------|--------|
| 慢速攻击（Slowloris） | 连接堆积，耗尽资源 | `ReadHeaderTimeout` 快速断开 |
| 大文件上传阻塞 | 长时间占用连接 | `ReadTimeout` 限制时间 |
| 慢客户端阻塞响应 | 其他请求排队 | `WriteTimeout` 释放连接 |
| 空闲连接占用 | 连接池耗尽 | `IdleTimeout` 回收连接 |

## 4. 任务配置参数

创建任务时通过 API 指定，不在配置文件中。

### 4.1 创建任务

创建任务通过 `POST /api/tasks` 提交，其中存储与保留策略参数由 `storage` 对象控制：
- `storage.retention_days`: 本地 binlog 分段文件保留天数（有效范围 `1` 到 `3650` 天，默认 `7` 天）。过期且已封存的分段由后台定期安全清理，正在写入的 `OPEN` 分段绝不会被清理。

**从最新位置开始（LATEST）：**

```bash
curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${TOKEN}" \
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
      "gtid_set": "3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100"
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

**重试上传失败的文件 (解耦归档与手动补传)：**

S3 / 对象存储上传是完全异步解耦的，上传网络波动不会中断复制。上传成功并且核对完成之后，任务文件清单的 `checksum` 为 `match` 或 `mismatch`。`mismatch` 表示字节不同，不是已校验，也不会把拉流停掉。对象 HEAD 失败时 `checksum` 为空，这一行仍是 `UPLOADED`，不算已校验。若遇到失败文件，可通过 API 或 Web 控制台手动触发重传：

```bash
# 重试当前任务失败的文件（支持 limit 参数，默认或最大按批次触发）
curl -X POST "http://localhost:8080/api/tasks/{task_id}/files/retry-upload?limit=100" \
  -H "Authorization: Bearer ${BINLOG_SERVER_API_AUTH_BEARER_TOKEN}"
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
curl http://localhost:8080/healthz
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
| `worker_id is already in use: <worker-id>` | 同一 `worker_id` 被其他活跃实例占用 |
| `storage.retention_days must be 1-3650` | 保留天数超出范围 |
| `invalid cluster_key format` | cluster_key 包含非法字符 |
| `api.auth.enabled=false cannot protect api or metrics routes` | `enabled=false` 但 `protect_api` 或 `protect_metrics` 为 `true` |
| `api.auth.bearer_token is required when protection is enabled` | 启用保护但未配置 `bearer_token` |
| `api.auth.api_key is required when protection is enabled` | 启用保护但未配置 `api_key` |
| `api.auth.mode must be bearer or api_key` | `mode` 值非法 |
| `http.control_plane.read_timeout_sec must be > 0` | 超时参数必须大于 0 |
| `http.worker_health.read_header_timeout_sec must be > 0` | 超时参数必须大于 0 |

## 7. 下一步

- 阅读 [故障排查](./troubleshooting.md) 学习如何诊断配置问题
- 阅读 [可观测性](./observability.md) 了解如何监控服务状态
