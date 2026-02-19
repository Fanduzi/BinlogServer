# 生产部署模式

本文统一说明 `standalone` / `cluster` 的部署方式与运维入口。

---

## 1. 模式与角色

### standalone

- 单节点本地调度与执行
- 不依赖 cluster lease 抢占
- 适合单机、快速交付

示例配置：

```yaml
mode: standalone
listen_addr: ":8080"
data_dir: "./data"
meta_dsn: ""
```

### cluster

cluster 模式通过 `cluster.role` 区分职责：

- `control-plane`
  - 提供 UI/API 与管理能力
  - 不执行 binlog 拉流
- `worker`
  - 执行任务、持有 lease、写 checkpoint/文件元数据
  - 默认不暴露管理面
- `all-in-one`
  - 同时提供 control-plane + worker
  - 适合小规模与过渡期

示例（all-in-one）：

```yaml
mode: cluster
cluster:
  role: all-in-one
  worker_id: "node-a"
  lease_ttl_sec: 15
  lease_renew_interval_sec: 5
  lease_grace_sec: 30
  failover_policy: rebuild_current_file
meta_dsn: "user:pass@tcp(meta-vip:3306)/binlog_meta?parseTime=true"
```

示例（control-plane）：

```yaml
mode: cluster
cluster:
  role: control-plane
meta_dsn: "user:pass@tcp(meta-vip:3306)/binlog_meta?parseTime=true"
listen_addr: ":8080"
```

示例（worker）：

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
meta_dsn: "user:pass@tcp(meta-vip:3306)/binlog_meta?parseTime=true"
```

---

## 2. 部署建议

### 最小生产建议

- control-plane: 1 台起步（可多实例扩容）
- worker: 2 台起步（跨可用区更稳）
- `worker_id` 必须全局唯一
- 所有 cluster 节点指向统一元数据库入口（VIP/ProxySQL）

### 前端交付方式

- 一体化：`make ui-build` 后随 Go 二进制发布（`/ui/`）
- 前后端分离：`frontend/dist` 走 Nginx/CDN，Go 仅提供 `/api/*`

---

## 3. Failover 预期行为与告警解释

当元数据库发生 failover（例如 orchestrator 切主）时：

1. worker 可能先进入 lease 续约失败窗口（degraded）。
2. 若在 `lease_grace_sec` 内恢复，任务继续运行。
3. 超过 grace 仍不可续约，worker fail-safe 停止，避免错误 seal/upload。
4. 重新抢占后按 `rebuild_current_file` 恢复，优先保证 strict 文件语义。

排障时重点看任务事件（`/api/tasks/{id}/events`）：

- `TASK_LEASE_DEGRADED`
- `TASK_LEASE_LOST`
- `TASK_LEASE_GRACE_EXCEEDED`

---

## 4. 运维排障入口（关键命令）

```bash
# 基础健康
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/api/summary | jq
# worker-only 探针（若配置了 worker_health_listen_addr）
curl -s http://127.0.0.1:18081/healthz
curl -s http://127.0.0.1:18081/readyz

# 集群总览
curl -s http://127.0.0.1:8080/api/cluster/overview | jq
curl -s http://127.0.0.1:8080/api/workers | jq

# 单任务排障
TASK_ID=1
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/replication" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/lease" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/runs" | jq
curl -s "http://127.0.0.1:8080/api/tasks/${TASK_ID}/events?limit=50" | jq
```

## 5. 指标与告警（示例）

- 指标端点：`GET /metrics`
- 规则与 runbook 示例：`docs/observability.md`
- 本项目不内置告警引擎，仅提供 Prometheus rule 示例，运行侧由外部 Prometheus/Alertmanager 承担。

e2e 验证入口：

```bash
./scripts/e2e/run-suite.sh --scenarios smoke,compression
./scripts/e2e/run-suite.sh --scenarios meta-failover
./scripts/e2e/run-suite.sh --scenarios meta-failover-override
./scripts/e2e/run-suite.sh --scenarios smoke-cluster-roles
./scripts/e2e/run-suite.sh --scenarios smoke-control-plane-failover
```

---

## 6. 常见误区

- `5173` 是 Vite 开发端口，仅用于本地开发热更新。
- 生产不需要 `npm run dev`。
- 一体化部署若忘记 `make ui-build`，`/ui/` 会继续提供旧静态资源。
