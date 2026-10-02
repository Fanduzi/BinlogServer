# 部署与运维指南

本文档面向负责生产运维的 DBA 及基础设施工程师，详细介绍 BinlogServer 的环境准备、版本下载、三种部署拓扑（单机模式、控制面+Worker 集群、一体化节点）、安全加固以及日常运维规范。

---

## 1. 架构拓扑概览

根据业务高可用级别与备份规模，BinlogServer 支持三种标准部署形态：

| 拓扑模式 | 适用场景 | 核心组件规划 | 高可用与容灾能力 |
|---|---|---|---|
| **1. 单机独立模式 (Standalone)** | 单机轻量备份、测试验证环境 | 单进程（包含 API、Web 控制台与复制引擎）。配置 `meta_dsn` 后元数据持久化，重启自动续传。 | 单机故障时停机，依赖宿主机守护进程重启。 |
| **2. 控制面 + Worker 集群 (推荐)** | 中大规模生产集群、多源实例集中备份 | 独立部署 2+ Control Plane 节点专职 API/调度，部署 2+ Headless Worker 节点执行拉取，后端接独立元数据 MySQL。 | Worker 崩溃后健康节点租约超时自动接管，API 节点无状态水平扩展。 |
| **3. 一体化集群节点 (All-in-one)** | 资源紧凑的生产高可用环境（2~3 台服务器） | 每台机器运行一个进程，同时承担 API 与 Worker 功能，共同接入后端元数据 MySQL。 | 任何单机故障均不影响其余节点的任务认领与控制面访问。 |

---

## 2. 环境前置条件

### 2.1 依赖与操作系统要求

- **运行平台：** Linux (x86_64, arm64) 或 macOS (darwin-amd64, darwin-arm64)。
- **CGO 规范：** Linux 生产环境直接使用官方发布的静态构建二进制（`CGO_ENABLED=0`，兼容 `glibc 2.17+`）。
- **源 MySQL 要求：** MySQL 5.7+ / 8.0+ 或 MariaDB 10.3+，开启 `log_bin=ON` 且为 `ROW` 模式。
- **元数据库 (可选/集群必需)：** 独立部署的高可用 MySQL 5.7+ / 8.0+。

### 2.2 源 MySQL 配置与复制账号授权

确认源 MySQL 开启 Binlog 及 ROW 格式：

```sql
SHOW VARIABLES LIKE 'log_bin';       -- 期望: ON
SHOW VARIABLES LIKE 'binlog_format';  -- 期望: ROW
SHOW VARIABLES LIKE 'binlog_row_image'; -- 推荐: FULL
```

在源库创建专职复制账号（密码请按企业规范生成强随机密码）：

```sql
CREATE USER 'repl_binlog'@'%' IDENTIFIED BY 'YourStrongReplPassword';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl_binlog'@'%';
FLUSH PRIVILEGES;
```

### 2.3 元数据库隔离红线 (Mandatory Isolation)

> ⚠️ **DBA 铁律：** `meta_dsn` 连接的 MySQL 实例必须专职专用于存放控制面元数据，**严禁**将元数据库所在实例自身纳管为 binlog 备份目标库。
> BinlogServer 在启动和创建任务时会强行校验端点，拒绝完全相同的 TCP `host:port` 以及各类 Loopback 别名（`localhost`、`127/8`、`::1`）。

---

## 3. 安装与产物准备 (v0.5.6)

生产部署无需安装 Go 编译器，直接下载带有校验签名的官方 Release 归档：

```bash
VER=0.5.6
OS=linux          # linux 或 darwin
ARCH=amd64        # amd64 或 arm64

curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing

tar -xzf "binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
cd "binlog-server_${VER}_${OS}_${ARCH}"
```

解压后的标准目录结构如下：
```text
binlog-server_0.5.6_linux_amd64/
├── binlog-server                  # 服务核心二进制（已内嵌 Web 控制台）
├── migrate                        # 数据库 Schema 迁移工具
├── migrations/                    # SQL 迁移脚本目录 (000001_init_schema)
├── config.example.yaml            # 完整参数参考配置
├── config.production.example.yaml # 生产安全基线模板
├── README.md                      # 英文说明
├── README_ZH.md                   # 中文说明
└── CHANGELOG.md                   # 版本记录
```

---

## 4. 拓扑部署实施步骤

### 4.1 拓扑一：单机模式 (Standalone)

适合单机评估或节点资源独立的独立备份场景。

#### 4.1.1 配置文件规划 (`config.standalone.yaml`)

```yaml
listen_addr: ":8080"
data_dir: "/data/binlog-server/data"
mode: "standalone"

# 持久化元数据库（推荐配置，重启可自动恢复任务）
meta_dsn: "${BINLOG_SERVER_META_DSN}"

api:
  auth:
    enabled: true
    mode: "bearer"
    bearer_token: "${BINLOG_SERVER_API_AUTH_BEARER_TOKEN}"
    protect_api: true
    protect_metrics: true

http:
  control_plane:
    read_header_timeout_sec: 5
    read_timeout_sec: 30
    write_timeout_sec: 30
    idle_timeout_sec: 120

log:
  level: "info"
  encoding: "json"
  file: "/data/binlog-server/logs/app.log"
  max_size_mb: 100
  max_backups: 7
  max_age_days: 30
  compress: true
```

#### 4.1.2 初始化元数据库与启动

```bash
# 1. 执行 Schema 迁移
export META_DSN="binlog_meta:YourPassword@tcp(10.0.0.20:3306)/binlog_meta?parseTime=true"
./migrate up --dsn "$META_DSN" --path ./migrations

# 2. 注入敏感密钥并启动服务
export BINLOG_SERVER_META_DSN="$META_DSN"
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="$(openssl rand -hex 32)"
export BINLOG_SERVER_ENCRYPTION_KEY="$(openssl rand -hex 16)" # 32 bytes

./binlog-server --config config.standalone.yaml --encryption-key "$BINLOG_SERVER_ENCRYPTION_KEY"
```

---

### 4.2 拓扑二：控制面 + Worker 集群架构 (推荐生产方案)

将流量入口与后台拉流执行彻底解耦，提供高可用的容灾能力。

#### 4.2.1 初始化元数据库

在独立的元数据库中创建数据库并执行迁移（只需执行一次）：

```sql
CREATE DATABASE binlog_server_meta DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
```

```bash
export META_DSN="binlog_meta:YourPassword@tcp(10.0.0.20:3306)/binlog_server_meta?parseTime=true"
./migrate up --dsn "$META_DSN" --path ./migrations
```

#### 4.2.2 Control Plane 节点配置 (`control-plane.yaml`)

Control Plane 专职提供 API、Swagger 与 Web 控制台，不拉取复制流：

```yaml
listen_addr: ":8080"
data_dir: "/data/binlog-server/data"
mode: "cluster"

cluster:
  role: "control-plane"

meta_dsn: "${BINLOG_SERVER_META_DSN}"

api:
  auth:
    enabled: true
    mode: "bearer"
    bearer_token: "${BINLOG_SERVER_API_AUTH_BEARER_TOKEN}"
    protect_api: true
    protect_metrics: true

http:
  control_plane:
    read_header_timeout_sec: 5
    read_timeout_sec: 30
    write_timeout_sec: 30
    idle_timeout_sec: 120

log:
  level: "info"
  encoding: "json"
  file: "/data/binlog-server/logs/control-plane.log"
```

启动 Control Plane 节点：
```bash
export PRODUCTION=true
export BINLOG_SERVER_META_DSN="binlog_meta:YourPassword@tcp(10.0.0.20:3306)/binlog_server_meta?parseTime=true"
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="your-shared-bearer-token"
export BINLOG_SERVER_ENCRYPTION_KEY="0123456789abcdef0123456789abcdef" # 32 bytes

./binlog-server --config control-plane.yaml --encryption-key "$BINLOG_SERVER_ENCRYPTION_KEY"
```

#### 4.2.3 Worker 节点配置 (`worker.yaml`)

Worker 节点专注于拉流，不开放外部控制面 API（仅暴露本地 `/healthz` 供心跳探活）：

```yaml
data_dir: "/data/binlog-server/data"
mode: "cluster"

cluster:
  role: "worker"
  # 推荐显式指定 worker_id 便于识别；如留空则系统自动生成 wk-<host>-<ip>-<random> 并持久化至 .worker-id
  worker_id: "${BINLOG_SERVER_CLUSTER_WORKER_ID}"
  worker_health_listen_addr: "127.0.0.1:8081"
  lease_ttl_sec: 15
  lease_renew_interval_sec: 5
  lease_grace_sec: 30
  failover_policy: "rebuild_current_file"

meta_dsn: "${BINLOG_SERVER_META_DSN}"

# 可选：S3 / MinIO 远端对象存储上传
upload:
  endpoint: "s3.us-east-1.amazonaws.com"
  bucket: "prod-mysql-binlogs"
  access_key: "${BINLOG_SERVER_UPLOAD_ACCESS_KEY}"
  secret_key: "${BINLOG_SERVER_UPLOAD_SECRET_KEY}"
  use_ssl: true

http:
  worker_health:
    read_header_timeout_sec: 3
    read_timeout_sec: 10
    write_timeout_sec: 10
    idle_timeout_sec: 30

log:
  level: "info"
  encoding: "json"
  file: "/data/binlog-server/logs/worker.log"
```

启动 Worker 节点（可启动多个 Worker 实例）：
```bash
export PRODUCTION=true
export BINLOG_SERVER_CLUSTER_WORKER_ID="worker-node-01"
export BINLOG_SERVER_META_DSN="binlog_meta:YourPassword@tcp(10.0.0.20:3306)/binlog_server_meta?parseTime=true"
export BINLOG_SERVER_ENCRYPTION_KEY="0123456789abcdef0123456789abcdef"
export BINLOG_SERVER_UPLOAD_ACCESS_KEY="AKIA..."
export BINLOG_SERVER_UPLOAD_SECRET_KEY="..."

./binlog-server --config worker.yaml --encryption-key "$BINLOG_SERVER_ENCRYPTION_KEY"
```

---

### 4.3 拓扑三：一体化集群节点 (All-in-one)

单进程同时提供 API 控制面与本地 Worker 执行引擎，两台或多台机器接入同一个元数据库即可构成高可用对。

配置要点：
```yaml
listen_addr: ":8080"
data_dir: "/data/binlog-server/data"
mode: "cluster"

cluster:
  role: "all-in-one"
  worker_id: "${BINLOG_SERVER_CLUSTER_WORKER_ID}"
  lease_ttl_sec: 15
  lease_renew_interval_sec: 5
  lease_grace_sec: 30

meta_dsn: "${BINLOG_SERVER_META_DSN}"

api:
  auth:
    enabled: true
    mode: "bearer"
    bearer_token: "${BINLOG_SERVER_API_AUTH_BEARER_TOKEN}"
    protect_api: true
    protect_metrics: true
```

启动方式同 Control Plane，各节点需分配唯一的 `BINLOG_SERVER_CLUSTER_WORKER_ID`。

---

## 5. 生产安全与配置加固规范

### 5.1 Fail-Closed 启动安全门禁

1. **非 Loopback 端口强鉴权：**
   若配置的 `listen_addr` 为 `:8080`、`0.0.0.0:8080` 或任何公网/内网 IP，启动时必须满足：
   - `api.auth.enabled: true`
   - `protect_api: true`
   - `protect_metrics: true`
   - 凭证已正确配置（非空且已解析环境变量占位符）
2. **`PRODUCTION=true` 模式强制校验：**
   若设置了环境变量 `PRODUCTION=true`，服务在未传入 `--encryption-key` 启动参数时直接报错并退出，坚决不进行网络监听。
3. **敏感凭证防明文落盘：**
   启动时若检测到配置文件中有明文密码或 Token，服务会在日志中打印警示信息。所有凭证应使用 `${ENV_VAR}` 占位符通过环境变量或 Secret 管理工具注入。

### 5.2 Systemd 守护进程托管示例

在 Linux 生产宿主机上推荐通过 systemd 进行常驻与自启动管理：

#### 1. 建立环境文件 `/etc/binlog-server/binlog.env` (权限设为 600)
```bash
sudo mkdir -p /etc/binlog-server
sudo tee /etc/binlog-server/binlog.env << 'EOF'
PRODUCTION=true
BINLOG_SERVER_META_DSN=binlog_meta:SecretPassword@tcp(10.0.0.20:3306)/binlog_server_meta?parseTime=true
BINLOG_SERVER_API_AUTH_BEARER_TOKEN=YourVerySecureRandomToken32BytesLong
BINLOG_SERVER_ENCRYPTION_KEY=0123456789abcdef0123456789abcdef
BINLOG_SERVER_UPLOAD_ACCESS_KEY=AKIAXXXXXXXXXXXX
BINLOG_SERVER_UPLOAD_SECRET_KEY=XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
EOF
sudo chmod 600 /etc/binlog-server/binlog.env
sudo chown -R binlog:binlog /etc/binlog-server
```

#### 2. 创建 Unit 文件 `/etc/systemd/system/binlog-server.service`
```ini
[Unit]
Description=BinlogServer Centralized MySQL Backup Control Plane
After=network.target

[Service]
Type=simple
User=binlog
Group=binlog
WorkingDirectory=/data/binlog-server
EnvironmentFile=/etc/binlog-server/binlog.env
ExecStart=/usr/local/bin/binlog-server --config /data/binlog-server/config.production.yaml --encryption-key ${BINLOG_SERVER_ENCRYPTION_KEY}
Restart=always
RestartSec=5s
LimitNOFILE=65536
LimitNPROC=32768

[Install]
WantedBy=multi-user.target
```

#### 3. 启用并启动
```bash
sudo systemctl daemon-reload
sudo systemctl enable binlog-server
sudo systemctl start binlog-server
sudo systemctl status binlog-server
```

---

## 6. 日常运维、探活与监控

### 6.1 健康检查与状态排查端点

| 端点 | 鉴权要求 | 适用对象 | 预期响应与说明 |
|---|---|---|---|
| `GET /healthz` | **无鉴权** | 负载均衡探活、K8s Liveness/Readiness 探针 | `200 OK`，响应文本 `ok`。服务只要正常监听即返回。 |
| `GET /api/health` | 受保护 (需 Bearer Token) | 内部组件集成、自动化流水线 | `200 OK`，JSON `{"status":"ok"}`。 |
| `GET /api/summary` | 受保护 (需 Bearer Token) | 监控巡检大盘、仪表盘全局状态 | 统计所有任务各状态数量、Worker 分布与复制延迟聚合。 |
| `GET /api/cluster/overview` | 受保护 (需 Bearer Token) | 集群健康巡检 | 查看当前在线 Worker 节点列表、持有租约分布及心跳时间戳。 |
| `GET /metrics` | 受保护 (需 Bearer Token) | Prometheus 采集器 | 标准 OpenMetrics 指标（延迟、任务状态分布、上传重试统计等）。 |

### 6.2 Prometheus 核心告警指标

- `binlog_server_replication_lag_seconds{task_id="..."}`: 复制延迟（秒）。若该值超过阈值（如 300 秒），表明拉流严重落后主库。
- `binlog_server_task_state_count{state="FAILED"}`: 失败任务计数。大于 0 应立即触发 P1/P2 告警介入排查。
- `binlog_server_worker_online{worker_id="..."}`: Worker 在线状态（1 在线，0 离线）。
- `binlog_server_upload_failures_total`: 上传失败待补传的文件累计数。

### 6.3 常见运维故障自愈与处理

#### 1. 复制任务因源库账号问题进入 `FAILED`
- **现象：** 任务状态变为 `FAILED`，查看任务详情 `last_error` 包含 `SOURCE_ACCESS_DENIED`。
- **处理：** 修复源库授权后，调用启动接口 `POST /api/tasks/{id}/start` 即可重新进入同步。

#### 2. Worker 异常退出与自动接管
- **现象：** 运行任务的 Worker 所在主机故障下线。
- **自愈机制：** 经过 `lease_ttl_sec`（默认 15s）后，该 Worker 登记的租约失效。集群中其余健康 Worker 的认领循环（`ClaimRunnableTasks`）将自动获取排他锁并接管复制，从上次 `fsync` 的 Checkpoint 处继续拉取。

#### 3. 远端对象存储抖动导致上传堆积
- **现象：** `binlog_server_upload_failures_total` 指标上涨。
- **说明：** 本地 binlog 复制流**不受任何影响**，仍在持续写入本地磁盘。
- **处理：** 待 S3 服务恢复后，调用补传接口重试：
  ```bash
  curl -X POST http://localhost:8080/api/tasks/{task_id}/files/retry-upload?limit=100 \
    -H "Authorization: Bearer ${TOKEN}"
  ```
