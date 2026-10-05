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

## 3. 安装与产物准备 (v0.5.36)

生产部署无需安装 Go 编译器，直接下载带有校验签名的官方 Release 归档。

> ⚠️ **从 v0.5.35 滚动升级即可。** 这一版没有 schema migration。元数据 schema 仍是版本 2。`schema_migrations` 已经是版本 2 时，不用执行 `./migrate up`。还在 schema 1 上的库，先按 [v0.5.34 中文发布说明](../../releases/v0.5.34.zh-CN.md) 停掉每一台进程再 `./migrate up`，然后只启动 v0.5.34 或更新的二进制。以 GTID 启动的任务，落盘后 `gtid_set` 留下，并在事务提交时往前扩。源文件被清理后，流上的 MySQL 1236 会再打开 `StartSyncGTID`，任务进入 `RUNNING`（#175、#197、#198）。已经是空的 `gtid_set` 不会从本地文件重建。细节见 [v0.5.36 中文发布说明](../../releases/v0.5.36.zh-CN.md)。

```bash
VER=0.5.36
OS=linux          # linux 或 darwin
ARCH=amd64        # amd64 或 arm64

curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing

tar -xzf "binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
cd "binlog-server_${VER}_${OS}_${ARCH}"
```

已发布的 `v0.5.36` `checksums.txt`：

```text
39a894d91d955507be54a9af1895173459cfa8adf974bb45655c4bc05fe830e6  binlog-server_0.5.36_darwin_amd64.tar.gz
35d0b9e9436cc2f823e57d8f0eb1a6db5dca801d8f25e1da1e610926261c0565  binlog-server_0.5.36_darwin_arm64.tar.gz
1e168be5714f9d7fd687c7fb3c177953e77bdfdb890801ca956e38ae5f0f0947  binlog-server_0.5.36_linux_amd64.tar.gz
4c7918cfc922bdee54c209a3158b4b58f23ada3109a323455c972462380d2638  binlog-server_0.5.36_linux_arm64.tar.gz
```

解压后的标准目录结构如下：
```text
binlog-server_0.5.36_linux_amd64/
├── binlog-server                  # 服务核心二进制（已内嵌 Web 控制台）
├── migrate                        # 数据库 Schema 迁移工具
├── migrations/                    # SQL 迁移脚本目录 (000001_init_schema, 000002_binlog_file_epoch_key)
├── config.example.yaml            # 完整参数参考配置
├── config.production.example.yaml # 生产安全基线模板
├── README.md                      # 英文说明
├── README_ZH.md                   # 中文说明
├── CHANGELOG.md                   # 版本记录
└── docs/guide/                    # 运维指南，含本地分段回放
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
- **现象：** 运行任务的 Worker 进程消失，租约到期后另一台 Worker 认领该任务。
- **数据在哪：** 租约会转到活着的 worker。分段目录不会跟着租约走。目录记在 `binlog_files.file_path` 里，就是死掉的 worker 当时写入的路径。没有新的配置项，也不需要改表。`cluster.failover_policy` 仍接受 `rebuild_current_file`，接管不靠这个键切换。
- **磁盘能读到：** 接管从那个目录里最后一个完整事件接着写。同一个源文件名只保留一段，不另起 `{data_dir}/{task_id}/` 从位置 4 重拉，也不把已经封存的文件再封一次。
- **磁盘读不到：** open 分段读不到，或者还有未上传（不是 `UPLOADED`）的封存分段读不到，而且 checkpoint 还没有落在已上传的封存对象里：任务变成 `FAILED`。`last_error` 以 `SEGMENT_NOT_ON_WORKER` 开头，并写出缺失的分段路径。不会在新 worker 上新建目录继续拉。把该路径挂到这台 worker 上，或把分段拷过来，再 `POST /api/tasks/{id}/start`。
- **对象里已经有这段：** checkpoint 已经落在 `UPLOADED` 的封存对象里时，可以从对象接着拉。本地没有那个文件时，正在跑的进程用已经配好的上传客户端把对象读回来（和删除对象是同一个客户端），再从对象里的最后一个完整事件继续，不从位置 4 重拉这一份，也不因为本地文件不在就变成 `FAILED`。checkpoint 已经在下一个文件的位置 4，且已封存的行都是 `UPLOADED`，仍从下一个文件的位置 4 开始。
- **同一台机器：** `kill -9` 之后 adopt 再 start，仍用原来的 `{data_dir}/{task_id}/`，行为不变。保留策略不变：先删对象，再删目录行，再删本地文件。对象删除失败时 `last_error` 以 `OBJECT_PURGE_FAILED` 开头，本地文件留下；下次打开文件从下一个 binlog 文件继续，不会停在 `sealed file already exists`。

English: The lease moves to a live worker. The segment directory stays where the dead worker wrote it. That path is `binlog_files.file_path`. There is no new config key and no schema change. If this worker can read that directory, it continues from the last complete event there, keeps one file per source name, and does not open a fresh directory or seal the same file again. If the open segment, or a sealed segment that is not `UPLOADED`, is not readable and the checkpoint is not already inside a sealed `UPLOADED` object, the task stays `FAILED`. `last_error` starts with `SEGMENT_NOT_ON_WORKER` and names the missing path. Mount or copy that path here, then `POST /api/tasks/{id}/start`. A checkpoint already covered by a sealed `UPLOADED` object is read back by the running process, using the upload client already configured for deletes, and continued from its last complete event. The task does not fail only because the local file is gone. Same-host kill -9, adopt, then start still uses this worker's own directory. Retention is unchanged.

#### 3. 远端对象存储抖动导致上传堆积
- **现象：** `binlog_server_upload_failures_total` 指标上涨。
- **说明：** 本地 binlog 复制流**不受任何影响**，仍在持续写入本地磁盘。
- **处理：** 待 S3 服务恢复后，调用补传接口重试：
  ```bash
  curl -X POST http://localhost:8080/api/tasks/{task_id}/files/retry-upload?limit=100 \
    -H "Authorization: Bearer ${TOKEN}"
  ```

---

## 7. 源库不可用时，用本地分段回放

<a id="replay-local-segments"></a>

English: [Replay local segments when the source is gone](#replay-local-segments-en).

源库已经不可用、手里只剩落盘文件时，用这一节按序号回放。回放 MySQL 源必须用 MySQL 自带的 `mysqlbinlog`，不要用 MariaDB 的。回放 MariaDB 源必须用 `mariadb-binlog`。7.3 在管道之前用 `mysqlbinlog --version` 确认客户端。下面写的是当前代码的行为。

### 7.1 文件在哪

分段在 `{data_dir}/{task_id}/`。`data_dir` 默认是进程工作目录下的 `./data`，对应环境变量 `BINLOG_SERVER_DATA_DIR`。`task_id` 是创建任务返回的 id，Quick Start 的第一个任务是 `1`。

单机模式下目录就在这个进程所在的机器上。集群模式下目录在执行该任务的 worker 本机，不在 control plane 上。

Day-1 已经拉到数据并在源上 `FLUSH LOGS` 之后，常见内容是：

```text
./data/1/mysql-bin.000003
./data/1/mysql-bin.000004.open.e1
```

`mysql-bin.NNNNNN` 是源库自己的 binlog 文件名。源库的 `log_bin` 前缀不同时，这里的前缀跟着变。

### 7.2 封存名和 `.open.e<epoch>`

不带 `.open.e<epoch>` 的文件已经封存。源库发出真实 rotate（例如 `FLUSH LOGS`）后，服务把当前文件 `rename` 成源文件名，字节不改。

`mysql-bin.000004.open.e1` 是还没封存的分段。`e` 后面的数字是这次运行的租约 epoch，单机第一次 `start` 是 `1`。后缀只在文件名上。文件从 4 字节 magic header `fe 62 69 6e`（`0xfe` + `bin`）开始，后面是已经 `fsync` 的事件原文。封存前后是同一串字节。

`mysqlbinlog` / `mariadb-binlog` 认这 4 个字节，不认文件名。把 `.open.e1` 的路径原样放进命令。`stop` 只关闭文件，不会做这次 `rename`。文件若只有这 4 个字节，说明还没有事件落盘，命令能打开它，输出里没有业务事件。

从源文件中部开始时（常见的 `LATEST`），源库会先下发 format description，它的 `end_log_pos` 常常是 126，小于当时的位点。第一个业务事件落盘时，这条 description 写在它前面，字节是源库下发的原文，所以 `mysqlbinlog --verify-binlog-checksum` 能解析后面的行事件。它不推进 checkpoint，也不拿它的时间算延迟。还没有业务事件时，文件仍只有这 4 个字节，停止后再启动不会退回 126。已经有事件的分段，下次 start 不会再写第二条。

每个序号只传一个文件，按序号从小到大：

- 这个序号只有封存名：传封存名。
- 这个序号只有 `.open.e<epoch>`：原样传这个路径。同一序号有多个 epoch 时，传数字最大的那个。
- 同一序号封存名和 `.open.e*` 都在：只传 epoch 最大的 `.open.e*`。正常 rotate 之后旧名字不会留下；两个都在，说明后一次写入没有覆盖已经封存的文件。

`GET /api/tasks/{id}/replay` 按这三条选出回放集。`paths` 每个序号一条，值就是 files API 的 `file_path`，open 分段仍带 `.open.e*`，原样交给 `mysqlbinlog` / `mariadb-binlog`。`limit` 与 files API 是同一窗口（默认 200，超出时保留序号最大的那段），窗口里仍是每个序号一条。`source.flavor` 为 `mysql` 时 `client` 是 `mysqlbinlog`，`client_hint` 是 `MySQL mysqlbinlog`；为 `mariadb` 时 `client` 和 `client_hint` 都是 `mariadb-binlog`。目录里没有分段时 `paths` 是 `[]`。files API 仍列出封存名和每个 epoch。Console 任务详情显示这条命令，点一次即可复制。

### 7.3 回放命令

先停止任务，让当前分段不再追加。监听地址按实际替换。开启了 API 鉴权时加上 `Authorization: Bearer`。进程已经退出时，跳过 `stop`，直接读磁盘。任务已经是 `FAILED` 或 `STOPPED` 时，这个 `stop` 返回 HTTP 400，正文 `cannot stop from state ...`，文件当时没有被打开，直接回放。

```bash
curl -fsS -X POST http://127.0.0.1:8080/api/tasks/1/stop
```

回放 MySQL 源必须用 MySQL 自带的 `mysqlbinlog`，不要用 MariaDB 的。回放 MariaDB 源必须用 `mariadb-binlog`。名叫 `mysqlbinlog` 的程序经常就是 MariaDB。Debian 上 `/usr/bin/mysqlbinlog` 与 `mariadb-binlog` 是同一个文件。管道之前先确认厂商：

```bash
mysqlbinlog --version
```

输出印着 MariaDB（例如 `mysqlbinlog from 11.8.6-MariaDB`）时，不要拿它回放 MySQL。这个客户端退出码是 0，并注入 binlog 文件里没有的 `SET @@session.check_constraint_checks=1`。MySQL 8.0 回答 `ERROR 1193 (HY000) Unknown system variable 'check_constraint_checks'`，管道在 `check_constraint_checks` 这里失败，一条数据都不进。文件没有坏。换成 `--version` 印着 MySQL 的官方 `mysqlbinlog`（例如 `Ver 8.0.46`）再执行下面的命令。

然后按源文件名序号从小到大回放。下面就是上一节那两个文件：

```bash
mysqlbinlog \
  ./data/1/mysql-bin.000003 \
  ./data/1/mysql-bin.000004.open.e1 \
  | mysql -h 127.0.0.1 -u root -p
```

MariaDB 源把 `mysqlbinlog` 换成 `mariadb-binlog`，把 `mysql` 换成 `mariadb`，文件顺序不变。`mariadb-binlog --version` 应印着 MariaDB。

管道右侧是恢复库。生产环境把 `./data` 换成 `data_dir`（部署示例里是 `/data/binlog-server/data`）。文件里已有的 GTID 事件会跟着这段输出执行。整段回放按上面的文件顺序即可。

`LATEST` 从订阅成功之后的事件开始写。本地第一段不包含订阅之前已经写在源库该文件里的事件。

### 7.4 源已经没了，信磁盘、files API，还是对象存储

回放信磁盘上的字节。`{data_dir}/{task_id}/` 里仍保留的封存文件，加上还没封存的 `.open.e<epoch>`，就是已经 `fsync` 的全部分段。

standalone 没配 `meta_dsn` 时，任务和位点仍只在内存，分段在磁盘上：

- 进程还在、任务还在：`GET /api/tasks/{id}/files` 扫描 `{data_dir}/{task_id}/`。返回封存文件和 `.open.e<epoch>`。`file_name` 是源文件名，`file_path` 是磁盘路径，open 分段的 `file_path` 带 `.open.e<epoch>`，可直接交给 `mysqlbinlog` / `mariadb-binlog`。顺序按源文件序号升序；同一序号先封存名，再按 epoch 从小到大。`limit` 默认 200，超出时保留序号最大的那段，窗口内仍是升序。目录里没有分段时正文是 `[]`。磁盘扫描不知道事件位点，`start_pos` 和 `end_pos` 是 0，不要当成 checkpoint。Console 任务文件表的「文件」列是磁盘文件名，「磁盘路径」列是这些 `file_path`。
- 没有位点行时，`GET /api/tasks/{id}/checkpoint` 仍是 `404`，正文 `checkpoint not found`。磁盘扫描不编造 checkpoint。
- 进程退出后，用同一个 `data_dir` 再启动：`GET /api/tasks` 和 dashboard 列出仍有封存或 `.open.e<epoch>` 分段的 `{data_dir}/<task_id>/`，id 就是目录名。`GET /api/tasks/{id}/files` 与上面同一份磁盘扫描。没有位点行时 checkpoint 仍是 `404 checkpoint not found`。这一行没有源库账号。先 adopt，再 start。adopt 之前，`PUT /api/tasks/{id}` 和 `POST /api/tasks/{id}/start` 返回 `400`，正文是 `on-disk backup has no task metadata`，不会开始复制。
- `POST /api/tasks/{id}/adopt` 把 `cluster_key` 和 source 接到这个已经列出的遗留目录 id。请求体需要 `cluster_key` 和 `source`（`host`、`port`、`user`、`password`、`flavor`）。`name`、`start`、`storage` 可选。成功是 `200`，状态是 `STOPPED`，响应不返回密码。adopt 不会开始复制。没传 `start`，或 `start.mode` 为空时，保存的位点是 `FILE_POS`：`file` 是最高封存分段或 `.open.e*` 分段的源文件名，`pos` 是该分段的字节大小。显式 `start.mode` 会覆盖这个默认值。然后 `POST /api/tasks/{id}/start` 返回 `204`，新分段写在同一目录。原来的封存文件和 open 分段还在。配了 `meta_dsn` 时不会从磁盘发现这些目录，adopt 也不会从目录创建任务。对不是目录任务的目录做 adopt，返回 `404`，正文是 `task not found`。Console 任务详情对这条已列出的遗留目录显示「认领」，提交的就是这次 POST。界面保持 `STOPPED`，不回显密码。认领不会开始复制。启动仍是单独的启动操作。目录任务的编辑仍走 `PUT`。

`checkpoint not found` 表示没有位点行。分段仍在磁盘上。回放命令仍按 7.2：每个序号只传一个文件。files API 把同一序号的封存名和各个 epoch 都列出来，方便核对；不要把同一序号的每一行都塞进命令。要直接拿这组路径，用 `GET /api/tasks/{id}/replay`，或在 Console 任务详情复制回放命令。

配了 `meta_dsn` 时，files API 仍读 `binlog_files`。该任务有目录行时，顺序与上面的磁盘扫描相同：按源文件序号升序；同一序号先封存名，再按 epoch 从小到大。默认最多 200 条，超出时保留序号最大的那段，窗口内仍是升序。Console 任务文件表从上到下就是这个顺序。`OPEN` 行的 `file_name` 是源文件名，`file_path` 才是带 `.open.e<epoch>` 的磁盘路径。该任务一条目录都没有时，改用上面的磁盘扫描。checkpoint 的 `file` 和 `pos` 仍是最后一次 `fsync` 的源文件名和位点；没有位点行时仍是 404。选进回放命令的文件仍按 7.2。`GET /api/tasks/{id}/replay` 用的就是这份目录窗口。

对象存储只保存已经封存并且上传成功的文件。对象键是 `{upload.prefix/}{cluster_key}/{source_identity}/{封存文件名}`，没有 `.open.e`。`source_identity` 在 MySQL 上是 `server_uuid`，在 MariaDB 上是 `mariadb:<server_id>:<gtid_domain_id>`。正在写的分段不会出现在桶里。`UPLOAD_FAILED` 的封存文件仍在磁盘上。没配上传时桶是空的。

封存分段上传之后，`GET /api/tasks/{id}/files` 和 Console 任务文件表的「校验」列给出 `checksum`。`match` 表示桶里该对象和这份封存文件是同一串字节：对象 HEAD 的 ETag 与本地文件一致（不超过 16MiB 时是整文件 MD5，更大时是与这次上传相同的 16MiB 分片 ETag）。`mismatch` 表示这次核对已经完成且字节不同。`checksum` 写成 `match` 只发生在这次 ETag 核对通过之后。对象 HEAD 失败时 `checksum` 留空，这一行仍是 `UPLOADED`，空值不是已校验。`mismatch` 或 HEAD 失败时拉流继续。

复制循环下次打开文件时，会删掉修改时间早于 `storage.retention_days` 的其它已封存本地文件。当前这个 `.open.e<epoch>` 不删。保留清理也不删其它 open 分段，也不碰它们的对象。已经上传的封存分段会在同一次清理里从桶中删除，并删掉目录行。还在保留期内的对象不删。对象删除失败时本地文件留下，任务 `last_error` 以 `OBJECT_PURGE_FAILED` 开头，下次打开文件会再试。这次删除后来成功时，复制从下一个 binlog 文件继续，不会因为刚封存的文件已在磁盘上而停在 `sealed file already exists`。

配了对象存储、并且任务有目录（`meta_dsn`）时，过期的封存文件如果目录行是 `UPLOAD_FAILED` 或 `LOCAL_ONLY`，本地文件和目录行都留下。这是唯一一份拷贝，要等它上传成功。复制继续跑。这次跳过不写 `last_error`，也不进入 `RETRY_BACKOFF`。每个留下的文件追加一条任务事件 `RETENTION_SKIPPED_NOT_UPLOADED`。`message` 里有文件名和 `upload_state`，`GET /api/tasks/{id}/events` 能看到。本进程还在跑时，之后再打开别的 binlog 文件，不会为同一个文件再追加这条事件。跑保留清理的进程上，`GET /metrics` 的 `binlog_server_retention_blocked_files{task_id}` 是最近一次成功清理仍留下的这类过期文件数。下次清理把它们删掉后，这个数变为 0。进程重启后，在下一次打开文件并跑完保留清理之前，这个数是 0。后台补传或手动补传把行变成 `UPLOADED` 之后，下一次打开文件仍按原来的顺序删：先删对象，再删目录行，再删本地文件。已经是 `UPLOADED` 的行，以及 `OBJECT_PURGE_FAILED`，行为不变。没配上传时，本地清理也不变。

没配 `meta_dsn` 的单机没有目录行，保留清理看不出这个封存文件从没上传成功。按年龄清理仍会删掉本地封存文件，包括没进桶的那一份。对象存储中断超过本地保留天数时，不要把这种单机当成唯一副本。只跑控制面的进程不做保留清理，那边的这个 gauge 保持 0；任务事件仍在 `GET /api/tasks/{id}/events`。

`storage.local_retention_days` 和 `storage.bucket_retention_days` 可以分开配。省略或 `0` 等于 `storage.retention_days`，这时上面的单截止时间不变。桶保留短于本地保留时，创建或更新任务返回 HTTP 400。配了上传和目录，且桶保留更长时：修改时间早于本地保留、但仍在桶保留之内的 `UPLOADED` 封存文件只删本地。对象和目录行留下。文件列表的 `location` 是 `bucket`。`file_path` 仍是目录里的路径，磁盘上已经没有这个文件。分段下载、`replay`（`limit` 和 `stop_datetime`）和 `replay/archive` 仍从对象读。`locations` 里的 `bucket` 表示不要把该路径直接交给 `mysqlbinlog`，先下载。本地文件删掉之后，桶年龄用 `sealed_at`，没有则用 `uploaded_at`；都空则留下。超过桶保留后，对象、目录行和还在的本地文件一起删。未上传的 `UPLOAD_FAILED` / `LOCAL_ONLY` 仍按本地保留留下。没有目录时，更长的桶保留不生效。Console 任务详情里的本地保留和桶保留是这两个有效天数；文件表的位置列是 `location`。

源库以后又恢复、任务再次 `start` 并且连上源时，新的 epoch 会删掉其他 epoch 的 `.open.e*`。封存文件还在。要留住当前这段未封存的尾部，先把 `{data_dir}/{task_id}/` 复制出来。源已经连不上时，`start` 在打开本地文件之前失败，不会走到删除这一步。

### 7.5 恢复到一个时间点

全量备份由你自己恢复。Binlog Server 只给出从那个备份追到某个时间点所需的分段，以及一条 `mysqlbinlog` / `mariadb-binlog` 命令。

时间是 UTC。`stop_datetime` 必填，`start_datetime` 可选。`YYYY-MM-DD HH:MM:SS` 按 UTC 理解。每个序号仍只传一个文件，规则与 7.2 相同。选中的分段是事件头时间落在这个窗口里的那些：停止时间是开区间右端，与 `--stop-datetime` 相同；给了开始时间时，左端含这个时刻。这条查询看整份清单，不用回放接口的 `limit`。本地文件优先；本地没有时，只读封存且 `UPLOADED`、`object_key` 非空的对象。某个已选序号打不开时，响应是错误。窗口里没有分段时 `paths` 是 `[]`，`command` 是空字符串。

```bash
curl -G -sS "http://127.0.0.1:8080/api/tasks/1/replay" \
  --data-urlencode "stop_datetime=2024-01-01 01:30:00"
```

响应里的 `command` 已经带 `TZ=UTC` 和 `--stop-datetime`。给了开始时间时还有 `--start-datetime`。先恢复你自己的全量备份，再执行这条命令，把输出管道到恢复库。Console 任务详情可以填写这两个时间并复制同一条命令，也可以下载这个窗口的 tar：`GET /api/tasks/1/replay/archive` 使用同一对参数。参数和错误句子见 [API 参考「按时间点选取分段」](../reference/api.md#pitr-replay)。

---

## 8. Replay local segments when the source is gone

<a id="replay-local-segments-en"></a>

中文：[源库不可用时，用本地分段回放](#replay-local-segments).

Use this section when the source is gone and the only copy is the files on disk. Replaying a MySQL source requires MySQL's own `mysqlbinlog`, not MariaDB's. Replaying a MariaDB source requires `mariadb-binlog`. Section 8.3 checks the client with `mysqlbinlog --version` before the pipe. The behavior below is what the current code does.

### 8.1 Where the files are

Segments live in `{data_dir}/{task_id}/`. `data_dir` defaults to `./data` under the process working directory (`BINLOG_SERVER_DATA_DIR`). `task_id` is the id returned when the task was created. The first Quick Start task is `1`.

On a standalone process the directory is on that machine. In cluster mode it is on the worker that is running the task, not on the control plane.

After a Day-1 pull and a source `FLUSH LOGS`, the directory usually looks like this:

```text
./data/1/mysql-bin.000003
./data/1/mysql-bin.000004.open.e1
```

`mysql-bin.NNNNNN` is the source's own binlog file name. A different `log_bin` prefix shows up here unchanged.

### 8.2 Sealed names and `.open.e<epoch>`

A file with no `.open.e<epoch>` suffix is sealed. After a real rotate from the source (for example `FLUSH LOGS`), the server `rename`s the current file to the source file name. The bytes stay the same.

`mysql-bin.000004.open.e1` is the segment that is still open. The number after `e` is the lease epoch of this run. The first `start` in a standalone process is `1`. The suffix is only on the name. The file starts with the 4-byte magic header `fe 62 69 6e` (`0xfe` + `bin`) and then the raw events that have been `fsync`ed. Sealing does not rewrite them.

`mysqlbinlog` and `mariadb-binlog` accept that header. Pass the `.open.e1` path as it is. `stop` closes the file and leaves the name in place. A file that is only those 4 bytes has no events yet. The tool opens it and prints no row or statement events.

A start in the middle of a source file, which is what `LATEST` does, receives a format description whose `end_log_pos` is often 126 and behind the dump cursor. That description is written in front of the first copied event, using the bytes MySQL sent, so `mysqlbinlog --verify-binlog-checksum` can decode the row events that follow. It does not move the checkpoint, and its timestamp is not lag. Until a later event arrives the file stays those 4 bytes, and stop then start does not rewind to 126. A segment that already has events does not gain a second description on the next start.

Pass one file per index, in ascending index order:

- Index has only the sealed name: pass that name.
- Index has only `.open.e<epoch>`: pass that path unchanged. If several epochs exist for one index, pass the highest epoch.
- Index has both a sealed name and `.open.e*`: pass only the highest-epoch `.open.e*` file. A successful rotate does not leave the old name behind. Both names mean a later write did not replace the sealed file.

`GET /api/tasks/{id}/replay` applies these three rules. `paths` is one entry per index. Each value is the files API `file_path`. An open segment still ends in `.open.e*` and is passed to `mysqlbinlog` or `mariadb-binlog` unchanged. `limit` is the same window as the files API (default 200; past that, the highest indexes). Inside the window there is still one path per index. When `source.flavor` is `mysql`, `client` is `mysqlbinlog` and `client_hint` is `MySQL mysqlbinlog`. When it is `mariadb`, `client` and `client_hint` are both `mariadb-binlog`. An empty directory returns `paths: []`. The files API still lists the sealed name and every epoch. The Console task detail shows this command and copies it in one click.

### 8.3 Replay command

Stop the task first so the current segment stops growing. Change the listen address to match the process. Add `Authorization: Bearer` when API auth is on. If the process has already exited, skip `stop` and read the files. When the task is already `FAILED` or `STOPPED`, this `stop` returns HTTP 400 with body `cannot stop from state ...`. The file is not open then. Replay it directly.

```bash
curl -fsS -X POST http://127.0.0.1:8080/api/tasks/1/stop
```

Replaying a MySQL source requires MySQL's own `mysqlbinlog`, not MariaDB's. Replaying a MariaDB source requires `mariadb-binlog`. The binary named `mysqlbinlog` is often MariaDB. On Debian, `/usr/bin/mysqlbinlog` is the same file as `mariadb-binlog`. Check the client before the pipe:

```bash
mysqlbinlog --version
```

If that prints MariaDB (for example `mysqlbinlog from 11.8.6-MariaDB`), do not use it against MySQL. The client exits 0 and injects `SET @@session.check_constraint_checks=1`, which is not in the binlog file. MySQL 8.0 answers `ERROR 1193 (HY000) Unknown system variable 'check_constraint_checks'`. The pipe fails at `check_constraint_checks` with 1193, and no rows land. The files are not corrupt. Use the official MySQL `mysqlbinlog` (`--version` prints MySQL, for example `Ver 8.0.46`), then run the command below.

Replay in source-file index order. These are the two files from the layout above:

```bash
mysqlbinlog \
  ./data/1/mysql-bin.000003 \
  ./data/1/mysql-bin.000004.open.e1 \
  | mysql -h 127.0.0.1 -u root -p
```

On a MariaDB source, use `mariadb-binlog` and `mariadb` with the same files in the same order. `mariadb-binlog --version` should print MariaDB.

The right-hand side is the restore server. In production, replace `./data` with `data_dir` (the deployment samples use `/data/binlog-server/data`). GTID events already stored in the files are part of this output. Replay the files in this order.

A `LATEST` task writes events from the moment the subscription is up. The first local segment does not contain events that were already in that source file before the subscription.

### 8.4 Disk, the files API, or object storage

Replay the bytes on disk. Sealed files still under `{data_dir}/{task_id}/`, plus the `.open.e<epoch>` segment that has not been sealed, are the segments that have been `fsync`ed.

Standalone with no `meta_dsn` still keeps tasks and checkpoints in memory. The segments are on disk:

- While the process is up and the task still exists, `GET /api/tasks/{id}/files` scans `{data_dir}/{task_id}/`. It returns sealed files and `.open.e<epoch>` segments. `file_name` is the source file name. `file_path` is the on-disk path. An open segment's `file_path` includes `.open.e<epoch>` and is the path to pass to `mysqlbinlog` or `mariadb-binlog`. Order is ascending source index. The same index lists the sealed name first, then open epochs from low to high. `limit` defaults to 200 and, past that, keeps the highest indexes, still ascending inside the window. An empty directory returns `[]`. The disk scan does not know event offsets, so `start_pos` and `end_pos` are 0. Do not treat them as a checkpoint. The Console task files table shows the on-disk name in the file column and these `file_path` values in the on-disk path column.
- With no checkpoint row, `GET /api/tasks/{id}/checkpoint` stays `404` with body `checkpoint not found`. The disk scan does not invent a checkpoint.
- After the process exits and a new one starts with the same `data_dir`, `GET /api/tasks` and the dashboard list `{data_dir}/<task_id>/` directories that still contain sealed or `.open.e<epoch>` segments. The id is the directory name. `GET /api/tasks/{id}/files` uses the same disk scan. With no checkpoint row, checkpoint stays `404 checkpoint not found`. The row has no source credentials. `PUT /api/tasks/{id}` and `POST /api/tasks/{id}/start` return `400` with body `on-disk backup has no task metadata` and do not start replication.
- `POST /api/tasks/{id}/adopt` attaches source identity to that same id. The body requires `cluster_key` and `source` (`host`, `port`, `user`, `password`, `flavor`). `name`, `start`, and `storage` are optional. Success is `200`. The response shows that id, the source fields with `password` omitted, and state `STOPPED`. Adopt does not start replication. When `start` is omitted, or `start.mode` is empty, the saved start is `FILE_POS`: `file` is the source name of the highest sealed or `.open.e*` segment under `{data_dir}/{id}/`, and `pos` is that file's size in bytes. An explicit `start.mode` of `LATEST`, `FILE_POS`, or `GTID` overrides that. Then `POST /api/tasks/{id}/start` returns `204`. The task enters `STARTING` or `RUNNING`, and new bytes are written in the same directory as the next `.open.e<epoch>`. The sealed files and the open segments that were already there stay. A configured `meta_dsn` does not discover these directories, and adopt does not create a task from a directory in that mode. Adopting an id that is not a catalog task returns `404` with body `task not found`. The Console task detail shows Adopt for that listed leftover row and submits this same POST. The task stays `STOPPED`, the password is not shown, and replication does not start. Start is still the separate start action. Edit on a catalog task still uses `PUT`.

`checkpoint not found` means there is no checkpoint row. The segments are still on disk. The replay command still follows section 8.2: one file per index. The files API lists every sealed name and every epoch for that index so you can see them. Do not pass every row for one index. For the selected paths, use `GET /api/tasks/{id}/replay`, or copy the replay command from the Console task detail.

With `meta_dsn`, the files API still reads `binlog_files` when that task has catalog rows. Order matches the disk scan above: ascending source index, and for one index the sealed name before open epochs. It returns at most 200 rows by default and, past that, keeps the highest indexes, still ascending inside the window. An `OPEN` row's `file_name` is the source file name. `file_path` is the on-disk path that includes `.open.e<epoch>`. When the catalog has no rows for that task, the API uses the disk scan above. Checkpoint `file` and `pos` are still the source file name and position of the last `fsync`. A missing checkpoint row is still 404. Choose replay arguments with section 8.2. `GET /api/tasks/{id}/replay` uses this same catalog window. The Console files table lists these rows from top to bottom in this order.

Object storage receives a file only after it is sealed and the upload succeeds. The object key is `{upload.prefix/}{cluster_key}/{source_identity}/{sealed file name}`, with no `.open.e`. `source_identity` is the MySQL `server_uuid`, or `mariadb:<server_id>:<gtid_domain_id>` for MariaDB. The segment still being written is not in the bucket. A sealed file in `UPLOAD_FAILED` is still on disk. With upload unconfigured, the bucket is empty.

After that upload, `GET /api/tasks/{id}/files` and the Checksum column in the Console task files table show `checksum`. `match` means the object in the bucket is the same bytes as that sealed file: the object HEAD ETag equals the local file (the whole-file MD5 at or under 16MiB, and the same 16MiB multipart ETag as the upload when the file is larger). `mismatch` means that comparison finished and the bytes differ. `checksum` is `match` only after the ETag comparison holds. When the object HEAD fails, `checksum` is empty, the row stays `UPLOADED`, and that empty value is not verified. Replication keeps pulling for `mismatch` and for a failed HEAD.

The next time the replication loop opens a file, it deletes other sealed local files whose modification time is older than `storage.retention_days`. The current `.open.e<epoch>` file stays. Retention does not delete other open segments, and it does not delete an object for an open segment. A sealed segment that was uploaded is deleted from the bucket in that same purge, and its catalog row is removed. A segment still inside retention stays in the bucket. If the object delete fails, the local file stays, `last_error` begins with `OBJECT_PURGE_FAILED`, and the next file open tries the delete again. When that delete succeeds, replication continues on the next binlog file. It does not stay in retry with `sealed file already exists` for the file just sealed.

When object storage is configured and the task has a catalog (`meta_dsn`), an expired sealed file whose row is `UPLOAD_FAILED` or `LOCAL_ONLY` stays on disk with its catalog row. That file is the only copy until a later upload succeeds. Replication keeps running. The skip does not set `last_error` and does not enter `RETRY_BACKOFF`. Each kept file appends one task event `RETENTION_SKIPPED_NOT_UPLOADED`. The message names the file and its `upload_state`. `GET /api/tasks/{id}/events` returns it. While this process keeps running, opening another binlog file does not append that event again for the same file. On the process that runs retention, `GET /metrics` exposes `binlog_server_retention_blocked_files{task_id}` as how many of those expired files the latest successful pass is still keeping. The gauge drops when a later pass deletes them. After a restart it stays 0 until the next file open runs retention again. After background retry or a manual retry makes the row `UPLOADED`, the next file open purges that segment as before: the object, then the catalog row, then the local file. Already-`UPLOADED` rows and `OBJECT_PURGE_FAILED` are unchanged. With upload not configured, local retention is unchanged.

Standalone without `meta_dsn` has no catalog row, so retention cannot tell that the sealed file never uploaded. Age retention still deletes the local sealed file, including one that never reached the bucket. Do not use that mode as the only copy of binlogs you still need when object storage is down for longer than the local retention. A control-plane-only process does not run retention, so this gauge stays 0 there. The task event is still on `GET /api/tasks/{id}/events`.

`storage.local_retention_days` and `storage.bucket_retention_days` can be set apart. Omitted or `0` means `storage.retention_days`, and the single cutoff above is unchanged. Create or update rejects a bucket retention shorter than the local retention with HTTP 400. With upload and a catalog, and a longer bucket retention, a sealed `UPLOADED` file whose modification time is past the local retention and still inside the bucket retention is removed from local disk only. The object and the catalog row stay. The files list `location` is `bucket`. `file_path` remains the catalog path and the file is not on disk. Segment download, `replay` (limit and `stop_datetime`), and `replay/archive` still read the object. A `bucket` entry in `locations` means do not pass that path to `mysqlbinlog` until you download it. After the local file is gone, bucket age is `sealed_at`, or `uploaded_at` when `sealed_at` is empty. A row with neither timestamp stays. Past the bucket retention, the object, the catalog row, and any remaining local file are purged together. An unuploaded `UPLOAD_FAILED` or `LOCAL_ONLY` file still stays, aged by the local retention. Without a catalog, a longer bucket retention is not applied. The Console task detail shows both effective retentions. The files table location column is `location`.

If the source comes back and a later `start` connects for a task that already has metadata, the new epoch deletes `.open.e*` files from other epochs. Sealed files stay. Copy `{data_dir}/{task_id}/` first when you still need the unsealed tail. A task adopted from a leftover directory does not do that delete: start opens the next `.open.e<epoch>` in the same directory and leaves the existing sealed and open segments in place. When the source is unreachable, `start` fails before it opens a local file, so it does not reach that delete. Files older than `storage.retention_days` can still be removed when a file is opened, except an unuploaded sealed file that a catalog is keeping as described above.

### 8.5 Restore to a datetime

You restore the full backup yourself. Binlog Server returns the segments that carry that backup forward to a datetime, and one `mysqlbinlog` or `mariadb-binlog` command.

Times are UTC. `stop_datetime` is required. `start_datetime` is optional. `YYYY-MM-DD HH:MM:SS` is read as UTC. One file per index still follows section 8.2. A segment is included when its event-header times meet the window: the stop time is the exclusive end, the same rule as `--stop-datetime`. When a start time is set, that instant is included, the same rule as `--start-datetime`. This query reads the whole inventory. It does not apply the replay `limit`. A local file wins. A missing local sealed file is read from object storage only when that row is `UPLOADED` and `object_key` is non-empty. If one selected segment cannot be opened, the response is an error. An empty window returns `paths: []` and an empty `command`.

```bash
curl -G -sS "http://127.0.0.1:8080/api/tasks/1/replay" \
  --data-urlencode "stop_datetime=2024-01-01 01:30:00"
```

`command` already includes `TZ=UTC` and `--stop-datetime`. A start time adds `--start-datetime`. Restore your own full backup, then run that command and pipe it into the restore server. The Console task detail accepts both times, copies the same command, and can download that window: `GET /api/tasks/1/replay/archive` takes the same parameters. The parameter list and error sentences are in [API reference section 5.7](../reference/api.md#pitr-replay).
