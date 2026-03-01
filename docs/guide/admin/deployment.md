# 部署指南

本章介绍如何部署 Binlog Server，包括单机模式和集群模式。

## 1. 前置条件

### 1.1 软件要求

| 组件 | 版本要求 |
|------|----------|
| Go | 1.24+ |
| MySQL | 5.7+ / 8.0+（作为元数据存储） |
| 源 MySQL | 5.7+ / 8.0+（需要开启 binlog） |

### 1.2 源 MySQL 配置

确保源 MySQL 开启了 binlog：

```ini
# /etc/my.cnf
[mysqld]
server-id = 1
log-bin = mysql-bin
binlog_format = ROW
binlog_row_image = FULL
```

验证：

```sql
SHOW VARIABLES LIKE 'log_bin';
-- 应该是 ON

SHOW VARIABLES LIKE 'binlog_format';
-- 应该是 ROW
```

### 1.3 创建复制账号

```sql
CREATE USER 'repl'@'%' IDENTIFIED BY 'your_password';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%';
FLUSH PRIVILEGES;
```

## 2. 单机模式部署

### 2.1 下载或编译

```bash
# 从源码编译
git clone https://github.com/your-org/binlog-server.git
cd binlog-server
go build -o binlog-server ./cmd/binlog-server

# 或下载预编译版本
# wget https://github.com/your-org/binlog-server/releases/download/v0.1.0/binlog-server-linux-amd64
```

### 2.2 创建配置文件

```yaml
# config.yaml
listen_addr: ":8080"
data_dir: "/data/binlog"

# 单机模式，不需要元数据库
mode: "standalone"

# 源 MySQL 连接示例（在创建任务时指定，这里只是模板）
# source:
#   host: "127.0.0.1"
#   port: 3306
#   user: "repl"
#   password: "your_password"

# 日志配置
log:
  level: "info"
  format: "json"
```

### 2.3 启动服务

```bash
./binlog-server --config config.yaml
```

### 2.4 验证

```bash
# 检查服务状态
curl http://localhost:8080/api/summary

# 访问 Swagger 文档
open http://localhost:8080/swagger/index.html
```

## 3. 集群模式部署

### 3.1 架构规划

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  Control Plane  │     │    Worker 1     │     │    Worker 2     │
│  (API + 调度)   │     │  (执行任务)      │     │  (执行任务)      │
└────────┬────────┘     └────────┬────────┘     └────────┬────────┘
         │                       │                       │
         └───────────────────────┼───────────────────────┘
                                 │
                                 ▼
                       ┌─────────────────┐
                       │  MySQL 元数据库  │
                       │  (高可用部署)    │
                       └─────────────────┘
```

### 3.2 创建元数据库

```sql
CREATE DATABASE binlog_server_meta;

-- 表结构请通过 migrations 执行；服务启动只做版本/结构校验，不会自动建表
```

```bash
# 执行迁移（使用内置迁移工具）
export META_DSN='user:password@tcp(127.0.0.1:3306)/binlog_server_meta?parseTime=true'
go run ./cmd/migrate up
```

### 3.3 Control Plane 配置

```yaml
# control-plane.yaml
listen_addr: ":8080"
data_dir: "/data/binlog"

mode: "cluster"
cluster:
  role: "control-plane"
  worker_id: "control-plane-1"

# 元数据库连接（推荐使用环境变量占位符）
meta_dsn: "${BINLOG_SERVER_META_DSN}"

# 租约配置
cluster:
  lease_ttl_sec: 30
  lease_renew_interval_sec: 10
  lease_grace_sec: 60

log:
  level: "info"
  encoding: "json"
```

### 3.4 Worker 配置

```yaml
# worker.yaml
listen_addr: ":8081"  # Worker 也需要 API 用于健康检查
data_dir: "/data/binlog"

mode: "cluster"
cluster:
  role: "worker"
  # worker_id: "worker-1"  # 可选，不配置则自动生成并持久化
  worker_health_listen_addr: ":8081"
  lease_ttl_sec: 30
  lease_renew_interval_sec: 10
  lease_grace_sec: 60

# 元数据库连接（推荐使用环境变量占位符）
meta_dsn: "${BINLOG_SERVER_META_DSN}"

log:
  level: "info"
  encoding: "json"
```

**worker_id 说明：**

| 配置方式 | 行为 |
|----------|------|
| 显式配置 | 使用配置值 |
| 不配置，首次启动 | 自动生成 `wk-<host>-<ip>-<random>`，持久化到 `{data_dir}/.worker-id` |
| 不配置，非首次启动 | 从 `.worker-id` 文件读取 |

**建议：** 生产环境可显式配置 `worker_id` 便于识别。不配置时系统也能正常工作，重启后保持相同身份。

### 3.5 启动服务

```bash
# 设置环境变量（敏感信息）
export BINLOG_SERVER_META_DSN="user:password@tcp(127.0.0.1:3306)/binlog_server_meta?parseTime=true"

# 启动 Control Plane
./binlog-server --config control-plane.yaml

# 启动 Worker（可以启动多个，每个使用不同的 data_dir）
export BINLOG_SERVER_CLUSTER_WORKER_ID="worker-1"
./binlog-server --config worker.yaml
```

**安全提示：**
- 避免在配置文件中明文存储密码
- 使用环境变量占位符 `${ENV_VAR}` 或直接通过环境变量注入
- 生产环境使用 Secret 管理工具（如 Kubernetes Secrets、Vault）

## 4. 使用 Systemd 管理服务

### 4.1 创建 Service 文件

```ini
# /etc/systemd/system/binlog-server.service
[Unit]
Description=Binlog Server
After=network.target mysql.service

[Service]
Type=simple
User=binlog
Group=binlog
# 敏感信息通过 EnvironmentFile 注入
EnvironmentFile=/etc/binlog-server/env
ExecStart=/usr/local/bin/binlog-server --config /etc/binlog-server/config.yaml
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

**创建环境变量文件：**

```bash
# /etc/binlog-server/env（权限设为 600）
BINLOG_SERVER_META_DSN=user:password@tcp(127.0.0.1:3306)/binlog_server_meta?parseTime=true
BINLOG_SERVER_UPLOAD_ACCESS_KEY=your_access_key
BINLOG_SERVER_UPLOAD_SECRET_KEY=your_secret_key
```

```bash
sudo chmod 600 /etc/binlog-server/env
sudo chown binlog:binlog /etc/binlog-server/env
```

### 4.2 启用并启动

```bash
sudo systemctl daemon-reload
sudo systemctl enable binlog-server
sudo systemctl start binlog-server
sudo systemctl status binlog-server
```

## 5. 使用 Docker 部署

### 5.1 Dockerfile

```dockerfile
FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o binlog-server ./cmd/binlog-server

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /app/binlog-server /usr/local/bin/
ENTRYPOINT ["binlog-server"]
CMD ["--config", "/etc/binlog-server/config.yaml"]
```

### 5.2 Docker Compose

```yaml
version: "3.8"

services:
  mysql-meta:
    image: mysql:8.0
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD}
      MYSQL_DATABASE: binlog_server_meta
    volumes:
      - mysql-data:/var/lib/mysql

  control-plane:
    build: .
    ports:
      - "8080:8080"
    environment:
      - BINLOG_SERVER_META_DSN=root:${MYSQL_ROOT_PASSWORD}@tcp(mysql-meta:3306)/binlog_server_meta?parseTime=true
    volumes:
      - ./config/control-plane.yaml:/etc/binlog-server/config.yaml
      - binlog-data:/data/binlog
    depends_on:
      - mysql-meta

  worker-1:
    build: .
    ports:
      - "8081:8081"
    environment:
      - BINLOG_SERVER_META_DSN=root:${MYSQL_ROOT_PASSWORD}@tcp(mysql-meta:3306)/binlog_server_meta?parseTime=true
      - BINLOG_SERVER_CLUSTER_WORKER_ID=worker-1
    volumes:
      - ./config/worker.yaml:/etc/binlog-server/config.yaml
      - binlog-data-worker-1:/data/binlog
    depends_on:
      - mysql-meta

volumes:
  mysql-data:
  binlog-data:
  binlog-data-worker-1:
```

**使用前创建 `.env` 文件：**

```bash
# .env（不要提交到版本控制）
MYSQL_ROOT_PASSWORD=your_secure_password
```

## 6. 健康检查

### 6.1 API 端点

```bash
# 服务健康检查
curl http://localhost:8080/api/health

# 查看集群状态
curl http://localhost:8080/api/cluster/overview

# 查看在线 workers
curl http://localhost:8080/api/workers
```

### 6.2 Prometheus 指标

```bash
curl http://localhost:8080/metrics
```

关键指标：
- `binlog_server_tasks_total` - 任务总数
- `binlog_server_tasks_running` - 运行中任务数
- `binlog_server_replication_lag_seconds` - 复制延迟

## 7. 常见问题

### 7.1 连接源 MySQL 失败

```
Error: connection refused
```

检查：
1. 源 MySQL 是否运行
2. 网络是否可达
3. 账号权限是否正确
4. 防火墙是否开放端口

### 7.2 元数据库连接失败

```
Error: Error 1045: Access denied
```

检查：
1. meta.dsn 配置是否正确
2. MySQL 用户权限
3. 数据库是否已创建

### 7.3 Worker 无法注册

```
Error: worker registration failed
```

检查：
1. worker_id 是否唯一
2. 元数据库是否可连接
3. 是否有其他 session 占用该 worker_id

## 8. 下一步

- 阅读 [配置参数详解](./configuration.md) 了解所有配置选项
- 阅读 [故障排查](./troubleshooting.md) 学习如何诊断问题
- 阅读 [可观测性](./observability.md) 了解监控和告警
