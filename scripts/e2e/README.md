# E2E 脚本说明

本目录用于本项目的端到端测试，覆盖：

- 基础拉流与 checkpoint 推进
- MySQL 8.0 压缩事务与 binlog 文件一致性校验
- orchestrator 拓扑发现行为
- semi-sync ACK/阻塞语义
- metadata MySQL failover（Percona57 主从 + ProxySQL + orchestrator）
- cluster 角色分离（control-plane + worker）与 worker heartbeat 在线/离线恢复

## 依赖

运行前请确保本机有：

- `docker`（含 `docker compose`）
- `curl`
- `jq`
- `go`

可选：

- `make`（用于快捷命令）

## 推荐入口

优先使用统一入口脚本 `run-suite.sh`：

```bash
# 日常回归（默认）：smoke + compression
./scripts/e2e/run-suite.sh

# 全量回归：smoke + compression + orchestrator + semisync + meta-failover
./scripts/e2e/run-suite.sh --profile full

# 自定义场景
./scripts/e2e/run-suite.sh --scenarios smoke,compression
./scripts/e2e/run-suite.sh --scenarios orchestrator,semisync
./scripts/e2e/run-suite.sh --scenarios meta-failover
./scripts/e2e/run-suite.sh --scenarios meta-failover-override
./scripts/e2e/run-suite.sh --scenarios smoke-cluster-roles
./scripts/e2e/run-suite.sh --scenarios smoke-control-plane-failover
```

也可用 `Makefile`：

```bash
make e2e-quick
make e2e-full
make e2e SCENARIOS=smoke,compression
```

## 脚本列表

- `up.sh`: 启动 4 个 source MySQL（`mysql57/mysql80/percona57/percona80`）并等待可用。
- `run-server.sh`: 用 e2e 配置启动 `binlog-server`。
- `down.sh`: 清理 e2e 容器与 volume。
- `smoke.sh`: 创建并启动 4 个基础任务，写入数据并查看 checkpoint。
- `smoke-compression.sh`: 验证压缩事务场景，主动 rotate 并对比源端与备份 binlog 的 md5。
- `smoke-orchestrator.sh`: 验证 orchestrator 拓扑里是否误纳入 binlog 拉流客户端。
- `smoke-semisync.sh`: 验证 `semi_sync=true` 时的 client 挂载与停任务后主库提交阻塞到 timeout。
- `setup-meta-replication.sh`: 初始化 meta-primary/meta-replica GTID 主从复制与 ProxySQL 监控账号。
- `smoke-meta-failover.sh`: 触发 orchestrator 切主，验证元数据库 failover 后 checkpoint 继续推进。
- `smoke-meta-failover-override.sh`: 用非默认 `E2E_API/E2E_ORC_API` 地址（localhost）覆盖并执行 failover 场景。
- `smoke-cluster-roles.sh`: 启动 control-plane + worker 双进程，验证任务执行、worker 离线检测与恢复链路。
- `smoke-control-plane-failover.sh`: 验证 control-plane 崩溃/重启期间 worker 持续拉流，checkpoint 不中断推进。
- `run-suite.sh`: 统一编排入口（自动 `up -> 启动服务 -> 跑场景 -> down`）。

## 常用环境变量

- `E2E_DATA_DIR`: e2e 数据目录（默认 `./tmp/e2e/data-suite-<timestamp>`）。
- `E2E_SERVER_LOG`: `run-suite.sh` 启动后端时的日志路径（默认 `/tmp/binlog-server-e2e-suite.log`）。
- `SEMISYNC_TIMEOUT_MS`: `smoke-semisync.sh` 使用的半同步 timeout（默认 `7000`）。
- `E2E_WORKER_ID`: `smoke-cluster-roles.sh` 中 worker 进程上报的 worker_id（默认 `e2e-worker-1`）。
- `E2E_WORKER_OFFLINE_WAIT_SEC`: `smoke-cluster-roles.sh` 停 worker 后等待离线判定的秒数（默认 `20`）。
- `E2E_WORKER_HEALTH_ADDR`: `smoke-cluster-roles.sh` 中 worker health probe 地址（默认 `127.0.0.1:18081`）。

## smoke-cluster-roles 场景说明

该场景用于真实多进程验证 cluster 角色分离：

1. 启动 control-plane（仅 API/UI）与 worker（仅执行）两个独立进程。
2. 通过 control-plane API 创建并启动任务，确认任务进入 `RUNNING`。
3. 写入 source 数据并确认 checkpoint 推进，证明由 worker 执行。
4. 查询 `/api/workers`，确认 worker `online=true` 且存在 `last_seen_at`；并校验 worker health probe (`/healthz`,`/readyz`) 可用且不暴露 `/api/*`。
5. 停止 worker，等待超过在线阈值后确认 `online=false`。
6. 重启 worker，确认 `online=true` 恢复且任务继续推进。

## smoke-control-plane-failover 场景说明

该场景用于验证 control-plane 故障不影响 worker 数据面：

1. 启动 control-plane + worker，创建并启动任务，确认 `RUNNING`。
2. 记录 checkpoint A，写入数据并推进到 checkpoint B（B > A）。
3. 停止 control-plane（worker 保持运行），再次写入数据。
4. 重启 control-plane，查询 checkpoint C（C > B），证明控面停机窗口内 worker 仍持续复制。
5. 验证 control-plane 恢复后 `/healthz` 与任务 API 可访问，且 `/api/workers` 仍展示 worker 在线状态。

## meta-failover 场景说明

`meta-failover` 场景会额外拉起：
- `meta-primary` / `meta-replica`（Percona57）
- `meta-proxysql`（meta DSN 固定入口）
- `orchestrator`（触发切主）

运行该场景时，`run-suite.sh` 会自动：
1. 执行 `setup-meta-replication.sh` 建立主从复制
2. 将 `BINLOG_SERVER_META_DSN` 覆盖到 `127.0.0.1:16036`（ProxySQL）
3. 执行 `smoke-meta-failover.sh` 或 `smoke-meta-failover-override.sh` 验证 failover 后恢复
4. 可通过 `E2E_API` / `E2E_ORC_API` 覆盖 binlog-server 与 orchestrator 地址。

## 排障建议

- 保留现场环境：

```bash
./scripts/e2e/run-suite.sh --profile full --keep-env
```

- 查看后端日志：

```bash
cat /tmp/binlog-server-e2e-suite.log
```

- 查看容器状态与日志：

```bash
docker compose -f deploy/e2e/docker-compose.yml ps
docker compose -f deploy/e2e/docker-compose.yml logs
```

- 排障后清理：

```bash
./scripts/e2e/down.sh
```
