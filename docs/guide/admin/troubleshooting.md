# 故障排查

本文档介绍常见问题的诊断方法和解决方案。

## 1. 排查思路

```
1. 查日志 → 找错误信息
2. 查状态 → API /metrics /events
3. 查数据库 → 任务状态、租约、worker
4. 查网络 → 连接性、延迟
```

## 2. 日志分析

### 2.1 日志位置

- 标准输出（推荐用 systemd/journald 收集）
- 或配置日志文件输出

### 2.2 关键日志模式

**启动成功：**

```
{"level":"info","msg":"server started","listen_addr":":8080"}
{"level":"info","msg":"scheduler restored","tasks_count":5}
```

**任务启动：**

```
{"level":"info","msg":"task started","task_id":"xxx","source":"10.0.0.1:3306"}
```

**复制正常：**

```
{"level":"debug","msg":"binlog event received","task_id":"xxx","file":"mysql-bin.000010","pos":12345}
```

**错误日志：**

```
{"level":"error","msg":"replication failed","task_id":"xxx","error":"connection reset"}
```

### 2.3 常见错误及解决

| 错误信息 | 原因 | 解决方案 |
|----------|------|----------|
| `connection refused` | 无法连接源 MySQL | 检查网络、防火墙、MySQL 状态 |
| `access denied` | 认证失败 | 检查用户名密码、权限 |
| `server_uuid mismatch` | server_id 冲突 | 检查 server_id 配置 |
| `lease acquire failed` | 租约被其他 worker 持有 | 正常现象，或检查是否有重复 worker |
| `checkpoint save failed` | 无法保存位点 | 检查元数据库连接 |
| `api.auth.enabled=false cannot protect` | 鉴权未启用但尝试保护路由 | 设置 `api.auth.enabled=true` 或关闭保护 |
| `bearer_token is required when protection is enabled` | 启用保护但未配置凭证 | 配置 `bearer_token` 或 `api_key` |
| `http.*.read_timeout_sec must be > 0` | 超时参数配置非法 | 确保所有超时参数 > 0 |

### 2.4 API 鉴权错误

当启用 API 鉴权后，请求可能返回以下错误：

| HTTP 状态码 | 场景 | 原因 | 解决方案 |
|------------|------|------|----------|
| 401 Unauthorized | Bearer 模式 | 缺少 `Authorization` 头 | 添加 `Authorization: Bearer <token>` |
| 401 Unauthorized | API Key 模式 | 缺少 API Key 头 | 添加对应的请求头（如 `X-API-Key: <key>`） |
| 403 Forbidden | Bearer 模式 | Token 格式错误（无 `Bearer ` 前缀） | 确保格式为 `Bearer <token>` |
| 403 Forbidden | 任意模式 | 凭证不匹配 | 检查 Token/API Key 是否正确 |

**排查步骤：**

```bash
# 1. 确认鉴权配置
curl http://localhost:8080/healthz  # 健康检查始终不需要鉴权

# 2. 测试 Bearer Token
curl -H "Authorization: Bearer your-token" \
  http://localhost:8080/api/tasks

# 3. 测试 API Key
curl -H "X-API-Key: your-api-key" \
  http://localhost:8080/api/tasks

# 4. 查看服务日志确认鉴权配置生效
# 日志中会显示 api.auth.enabled=true
```

**常见配置错误：**

```yaml
# 错误：enabled=false 但 protect_api=true
api:
  auth:
    enabled: false
    protect_api: true  # ❌ 启动报错

# 正确：要么启用鉴权，要么关闭保护
api:
  auth:
    enabled: true
    protect_api: true  # ✅
```

### 2.5 HTTP 超时问题

当客户端遇到连接超时或断开时，可能是 HTTP 超时配置问题：

| 症状 | 可能原因 | 解决方案 |
|------|----------|----------|
| 大请求返回 408/超时 | `read_timeout_sec` 过小 | 增大 `http.control_plane.read_timeout_sec` |
| 大响应被截断 | `write_timeout_sec` 过小 | 增大 `http.control_plane.write_timeout_sec` |
| 连接频繁重建 | `idle_timeout_sec` 过小 | 增大 `idle_timeout_sec` |
| 慢客户端攻击 | 无 `read_header_timeout_sec` | 确保 > 0（默认 5 秒） |

**排查步骤：**

```bash
# 1. 检查当前超时配置
# 查看配置文件或环境变量

# 2. 测试慢请求
time curl -X POST http://localhost:8080/api/tasks \
  -H "Content-Type: application/json" \
  -d '{"name":"test","cluster_key":"test","source":{"host":"10.0.0.1","port":3306,"user":"repl","password":"secret"},"start":{"mode":"LATEST"}}'

# 3. 检查服务端日志是否有超时断开记录
```

**生产环境推荐值：**

```yaml
http:
  control_plane:
    read_header_timeout_sec: 5
    read_timeout_sec: 60       # 大任务创建可能需要更长时间
    write_timeout_sec: 60      # 大响应（如文件列表）可能需要更长时间
    idle_timeout_sec: 120
```

## 3. API 诊断

### 3.1 健康检查

```bash
curl http://localhost:8080/healthz
```

正常响应：
```text
ok
```

### 3.2 任务状态

```bash
# 列出所有任务
curl http://localhost:8080/api/tasks

# 查看单个任务
curl http://localhost:8080/api/tasks/{task_id}
```

关键字段：
- `state` - 任务状态
- `last_error` - 最后一次错误
- `owner_worker_id` - 当前执行的 worker

### 3.3 复制状态

```bash
curl http://localhost:8080/api/tasks/{task_id}/replication
```

```json
{
  "task_id": "1",
  "state": "RUNNING",
  "status": "NORMAL",
  "threshold_seconds": 30,
  "has_progress": true,
  "delay_seconds": 0,
  "last_event_at": "2024-01-01T10:00:00Z",
  "last_event_file": "mysql-bin.000010",
  "last_event_pos": 12345
}
```

关键字段：
- `status`: `NORMAL / DELAYED / ABNORMAL`
- `delay_seconds`: lag in seconds. At the source tip (LATEST start after StartSync, or idle dump wait) this is 0 even if `last_event_at` is an old event header. Catch-up that is still behind the source tip uses `now - last_event_at`.
- `has_progress=false`: 当前没有可用复制进度（需结合任务状态判断）

### 3.4 任务事件

```bash
curl http://localhost:8080/api/tasks/{task_id}/events?limit=50
```

查看任务的历史事件：
- `TASK_START_DISPATCHED` - 控制面已分发启动
- `TASK_STARTED` / `TASK_RUNNING` - 任务启动并进入运行
- `TASK_RUNNER_ERROR` / `TASK_RETRY_BACKOFF` - 执行错误与退避
- `TASK_LEASE_DEGRADED` / `TASK_LEASE_LOST` / `TASK_LEASE_GRACE_EXCEEDED` - 租约异常链路

### 3.5 集群状态

```bash
# 查看在线 workers
curl http://localhost:8080/api/workers

# 查看集群概览
curl http://localhost:8080/api/cluster/overview
```

## 4. 数据库诊断

### 4.1 检查任务状态

```sql
SELECT id, name, state, owner_worker_id, last_error, updated_at
FROM backup_tasks
ORDER BY updated_at DESC;
```

### 4.2 检查租约状态

```sql
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE lease_expire_at > NOW(6);
```

### 4.3 检查 Worker 注册

```sql
SELECT worker_id, session_id, lease_expire_at, renewed_at
FROM worker_registrations
WHERE lease_expire_at > NOW(6);
```

### 4.4 检查最近事件

```sql
SELECT task_id, event_type, message, event_time
FROM task_events
ORDER BY event_time DESC
LIMIT 20;
```

## 5. 常见问题

### 5.1 任务卡在 STARTING

**症状：**

- `GET /api/tasks/{id}` 长时间显示 `state=STARTING`
- `/api/tasks/{id}/events` 只有 `TASK_START_DISPATCHED`，迟迟没有 `TASK_RUNNING`
- 控制面启动成功，但业务不推进

**排查步骤（API / 日志 / SQL）：**

```bash
# API：检查 worker 在线与任务租约
curl http://localhost:8080/api/workers
curl http://localhost:8080/api/tasks/{task_id}/lease
curl http://localhost:8080/api/tasks/{task_id}/events?limit=50
```

```text
# 日志关键字（worker 节点）
worker claim starting tasks failed
worker claimed starting tasks count=
worker registration renew lost ownership
```

```sql
-- SQL：任务状态、租约、注册状态
SELECT id, state, owner_worker_id, epoch, updated_at, last_error
FROM backup_tasks
WHERE id = '{task_id}';

SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = '{task_id}';

SELECT worker_id, session_id, lease_expire_at, renewed_at
FROM worker_registrations
ORDER BY renewed_at DESC
LIMIT 20;
```

**修复动作：**

- 无 worker 在线：先恢复 worker 进程，再观察 claim 日志与 `/api/workers`。
- `worker_registrations` 持续失租：检查 worker 到元数据库网络与延迟。
- `STARTING` 为旧脏状态且无运行归属时，可执行一次 `POST /api/tasks/{id}/stop` 后再 `POST /api/tasks/{id}/start` 触发重新分发。

### 5.2 运行中 lease 或 registration 丢失

**症状：**

- 任务从 `RUNNING` 进入 `LEASE_DEGRADED` 或停止
- 事件出现 `TASK_LEASE_DEGRADED` / `TASK_LEASE_LOST` / `TASK_LEASE_GRACE_EXCEEDED`
- worker 日志出现 `worker registration renew lost ownership`

**排查步骤（API / 日志 / SQL）：**

```bash
# API：看任务状态和运行历史
curl http://localhost:8080/api/tasks/{task_id}
curl http://localhost:8080/api/tasks/{task_id}/lease
curl http://localhost:8080/api/tasks/{task_id}/events?limit=100
curl http://localhost:8080/api/tasks/{task_id}/runs?limit=20
```

```text
# 日志关键字
worker registration renew failed
worker registration renew lost ownership
lease renew loop panic
TASK_LEASE_GRACE_EXCEEDED
```

```sql
-- SQL：检查 lease 是否还有效
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = '{task_id}';

-- SQL：检查 worker 注册是否临近/已过期
SELECT worker_id, session_id, lease_expire_at, renewed_at
FROM worker_registrations
WHERE worker_id = '{owner_worker_id}';

-- SQL：检查最近任务事件
SELECT task_id, event_type, message, detail, event_time
FROM task_events
WHERE task_id = '{task_id}'
ORDER BY event_time DESC
LIMIT 20;
```

**修复动作：**

- 按推荐关系调整参数：`renew < ttl < grace`（见 `configuration.md`）。
- 排查元数据库瞬时超时/抖动，优先稳定 worker 到 DB 的网络链路。
- 出现注册失租后，先确保单实例持有同一 `worker_id`，避免重复进程竞争。

### 5.3 checkpoint 不推进

**症状：**

- `/api/tasks/{id}/checkpoint` 的 `pos` 长时间不变化
- `/api/tasks/{id}/replication` 显示任务运行但 `last_event_at` 或 `last_event_pos` 不前进
- 任务事件里反复出现 `TASK_RUNNER_ERROR`

**排查步骤（API / 日志 / SQL）：**

```bash
# API：对比 checkpoint 与复制进度
curl http://localhost:8080/api/tasks/{task_id}/checkpoint
curl http://localhost:8080/api/tasks/{task_id}/replication
curl http://localhost:8080/api/tasks/{task_id}/events?limit=100
curl http://localhost:8080/api/tasks/{task_id}/files?limit=50
```

```text
# 日志关键字
runner error
context deadline exceeded
connection reset
checkpoint
```

```sql
-- SQL：checkpoint 最新位点
SELECT task_id, file_name, pos, gtid_set, updated_at
FROM backup_checkpoints
WHERE task_id = '{task_id}';

-- SQL：最近错误事件（查看 detail）
SELECT task_id, event_type, message, detail, event_time
FROM task_events
WHERE task_id = '{task_id}'
ORDER BY event_time DESC
LIMIT 50;

-- SQL：确认任务是否处于反复重试
SELECT id, state, last_error, updated_at
FROM backup_tasks
WHERE id = '{task_id}';
```

**修复动作：**

- 优先处理 `TASK_RUNNER_ERROR` 对应根因（源库连通、权限、磁盘空间、元数据库可用性）。
- 若任务陷入错误重试且位点不前进，可执行 `stop` / `start` 触发新 run，并观察 `/runs` 与 `/events` 是否恢复推进。
- 若只见 `.open.e*` 文件且未 seal，先确认任务是否仍持有有效 lease，避免失租后继续误操作文件。

## 6. 性能问题

### 6.1 CPU 使用率高

检查：
- 是否有大量任务
- 复制速度是否过快
- 是否有频繁的 GC

```bash
# 查看 goroutine 数量
curl http://localhost:8080/metrics | grep goroutines

# 查看 GC 信息
curl http://localhost:8080/metrics | grep go_gc
```

### 6.2 内存使用增长

检查：
- 是否有内存泄漏
- 缓冲区是否过大

```bash
# 查看内存指标
curl http://localhost:8080/metrics | grep go_memstats
```

### 6.3 磁盘空间不足

```bash
# 查看数据目录大小
du -sh /data/binlog

# 检查保留策略是否生效
# retention_days 配置是否合理
```

## 7. 紧急操作

### 7.1 强制停止任务

```bash
curl -X POST http://localhost:8080/api/tasks/{task_id}/stop
```

### 7.2 强制释放租约

```sql
-- 谨慎操作！确保没有其他 worker 在执行
UPDATE task_leases
SET owner_worker_id = '', lease_expire_at = NOW(6), renewed_at = NOW(6)
WHERE task_id = 'xxx';
```

### 7.3 清理过期数据

```sql
-- 清理过期 worker 注册
DELETE FROM worker_registrations WHERE lease_expire_at < NOW(6);

-- 清理过期租约
DELETE FROM task_leases WHERE lease_expire_at < NOW(6);
```

## 8. 获取支持

收集以下信息：

1. 服务版本：`binlog-server --version`
2. 配置文件（脱敏）
3. 错误日志（最近 100 行）
4. 任务状态：`curl /api/tasks/{task_id}`
5. 复制状态：`curl /api/tasks/{task_id}/replication`
6. 最近事件：`curl /api/tasks/{task_id}/events`
7. Metrics：`curl /metrics`

---

**下一步**：阅读 [可观测性](./observability.md) 了解如何设置监控和告警。
