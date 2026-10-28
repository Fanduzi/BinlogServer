# 可观测性

本文档介绍如何监控 Binlog Server，包括指标、日志、事件。

## 1. 监控架构

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  Binlog Server  │────►│   Prometheus    │────►│    Grafana      │
│   /metrics      │     │   (采集)        │     │   (可视化)      │
└─────────────────┘     └─────────────────┘     └─────────────────┘
         │
         │ 日志
         ▼
┌─────────────────┐
│   Loki / ES     │
│   (日志聚合)    │
└─────────────────┘
```

## 2. Prometheus 指标

### 2.1 访问指标

```bash
curl http://localhost:8080/metrics
```

### 2.2 核心指标

**任务指标：**

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `binlog_server_task_state_count` | gauge | `state` | 各状态任务数 |
| `binlog_server_task_healthy` | gauge | `task_id`, `reason` | 本进程评估器对这个任务的 `HealthOf` 结论。1 只在 HEALTHY，`reason="ok"`。0 的 `reason` 是 `critical`、`warning`、`unknown`、`unevaluated` 或 `unread`。只输出本节点负责的任务。没有评估结果时只有一条 `task_id=""`、`reason="ok"`、值为 1 的哨兵，避免 `== 0` 在评估器关闭时误报 |
| `binlog_server_task_progress_age_seconds` | gauge | `task_id` | desired RUN 的任务，距最后一次源字节或本地推进的秒数。不是 desired RUN，或这一轮没有时钟，为 0 |
| `binlog_server_task_upload_lag_seconds` | gauge | `task_id` | 最老一条已封存但还不是 `UPLOADED` 且校验和 match 的段的年龄，秒。没开对象存储，或每条都已匹配，为 0 |
| `binlog_server_task_open_critical` | gauge | `task_id` | 产生 `task_healthy` 的那一轮里，仍开着的 CRITICAL finding 数 |
| `binlog_server_finding_open` | gauge | `task_id`, `kind`, `severity` | 当前开着的 finding。`severity` 是 `CRITICAL`、`WARNING` 或 `INFO` |
| `binlog_server_task_last_verified_timestamp` | gauge | `task_id` | 该任务最近一次巡检完成的 Unix 时间。没有巡检过是 0 |

**复制指标：**

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `binlog_server_replication_lag_seconds` | gauge | `task_id` | 复制延迟（秒） |
| `binlog_server_checkpoint_age_seconds` | gauge | `task_id` | Checkpoint 年龄（秒） |
| `binlog_server_source_switchovers` | gauge | `task_id`, `outcome` | 该任务的切换次数，同一次切换被重复检查（停下的任务每点一次 Start 都会再记一条同样的事件）只算一次。`outcome="continued"` 是地址换到另一台后任务仍在复制，回切 A→B→A 计 2。`outcome="stopped"` 是任务现在仍停在这次切换上（`FAILED`，`last_error` 是 `SOURCE_SWITCHOVER`）；任务回到运行后它回到 0，告警会恢复；之后普通 Stop 或进程重启也保持 0，因为判断依据是停止事件之后有没有 `TASK_RUNNING`。每次采集按已保存的事件重算。读到的任务两个序列都有，包含 0。没有任务时 `task_id=""` 的两个序列为 0 |

**Worker 指标：**

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `binlog_server_worker_online` | gauge | `worker_id` | Worker 在线状态（1=在线，0=离线） |

**上传指标：**

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `binlog_server_upload_failures_total` | gauge | - | 上传失败的文件记录数 |
| `binlog_server_upload_tls_failures_total` | counter | - | 上传时 TLS 握手失败的次数，含协议版本不匹配和 `remote error: tls:` 这类握手 alert。同一失败使 `/api/health` 为 503 `UPLOAD_TLS`。明文端点的错误提示 `upload.use_ssl: false`，主机名不匹配不提示 `ca_file`，未知 CA 提示 `upload.tls.ca_file`，协议版本和握手 alert 保留原文。校验和匹配的下一次成功上传或补传后回到 200；一次 `UPLOAD_RECORD_FAILED` 和非 TLS 的上传失败都不清这条 reason |
| `binlog_server_upload_retry_total` | counter | `result` | 重试上传 API 调用结果（success/failed/skipped） |
| `binlog_server_upload_retry_last_ts` | gauge | - | 最近一次重试上传 API 执行时间（Unix 时间戳） |

**Go 运行时指标：**

| 指标名 | 类型 | 说明 |
|--------|------|------|
| `go_goroutines` | gauge | goroutine 数量 |
| `go_memstats_alloc_bytes` | gauge | 内存分配 |
| `go_gc_duration_seconds` | summary | GC 耗时 |

### 2.3 Prometheus 配置

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'binlog-server'
    static_configs:
      - targets: ['localhost:8080']
    scrape_interval: 15s
```

## 3. Grafana 仪表板

### 3.1 关键面板

**概览面板：**
- 任务总数（按状态分组）
- 活跃 worker 数
- 总吞吐量（bytes/s）

**任务详情面板：**
- 各任务复制延迟
- 各任务连接状态
- 各任务错误率

**资源面板：**
- 内存使用
- Goroutine 数量
- GC 频率

### 3.2 示例查询

**任务状态分布：**

```promql
binlog_server_task_state_count
```

**复制延迟（按任务）：**

```promql
binlog_server_replication_lag_seconds
```

**Checkpoint 年龄：**

```promql
binlog_server_checkpoint_age_seconds
```

**源地址换到了另一台，任务仍在复制：**

```promql
binlog_server_source_switchovers{outcome="continued"} > 0
```

**源地址换到了另一台，任务已停止：**

```promql
binlog_server_source_switchovers{outcome="stopped"} > 0
```

**Worker 在线状态：**

```promql
binlog_server_worker_online
```

**上传重试成功率：**

```promql
sum(rate(binlog_server_upload_retry_total{result="success"}[5m]))
/
sum(rate(binlog_server_upload_retry_total[5m]))
```

## 4. 告警规则

推荐规则在 [`deploy/prometheus/binlog-server-alerts.yml`](../../../deploy/prometheus/binlog-server-alerts.yml)。`promtool test rules deploy/prometheus/binlog-server-alerts.test.yml` 在 CI 的 `promtool` job 里每个 PR 都跑。下面是同一份规则，改规则先改那个文件。

抓取每个节点。一个任务只由正在评估它的节点输出（lease 主人、委派的链主人，或持有 `_health` 的节点）。`binlog_server_task_healthy == 0` 不会因为哨兵序列（值为 1）触发。进程挂了用 `up == 0`。评估器始终在运行，每个被评估的任务都有序列。

| 告警 | 表达式 | for | 级别 |
|------|--------|-----|------|
| `BinlogTaskUnhealthy` | `binlog_server_task_healthy == 0` | 2m | critical |
| `BinlogTaskProgressAge` | `binlog_server_task_progress_age_seconds > 45` | 2m | warning |
| `BinlogTaskUploadLag` | `binlog_server_task_upload_lag_seconds > 900` | 5m | warning |
| `BinlogTaskOpenCritical` | `binlog_server_task_open_critical > 0` | 1m | critical |
| `BinlogFindingCritical` | `binlog_server_finding_open{severity="CRITICAL"} > 0` | 1m | critical |
| `BinlogVerifyStale` | 有过巡检（时间戳 > 0）且距今超过 48h | 1h | warning |
| `BinlogProcessDown` | `up{job="binlog-server"} == 0` | 1m | critical |

同一文件里还有复制延迟、源切换、checkpoint、FAILED、worker 和上传失败这几条，阈值与原先写在这里的草稿相同。`job="binlog-server"` 要和 Prometheus 的 `job_name` 一致。

### 4.1 通用 webhook

`notify.webhook_url` 非空时，评估器在健康状态变化，或 CRITICAL finding 打开、关闭、升级时 POST 一批 JSON。第一次观察到 HEALTHY 当作基线，不发。状态不变、也没有新的 CRITICAL 转移时不发。POST 失败留在本进程内存里，下一轮再送；进程重启可能再发一次。集群里只有评估该任务的节点发。

```json
{"version":1,"events":[{"type":"health","task_id":"9","status":"UNHEALTHY","previous":"HEALTHY","at":"2026-10-26T18:00:00Z"}]}
```

finding 事件的 `type` 是 `finding`，并带 `kind`、`severity`、`op`（`OPEN`、`CLOSE`、`ESCALATE`）、`subject`、`detail`（最长 1024 字符）。请求头：`Content-Type: application/json`；配了 `notify.webhook_token` 时有 `X-BinlogServer-Token`；`X-BinlogServer-Delivery` 是正文的 SHA-256，同一次重试不变。不跟随重定向。传输错误、5xx 和 429 按 `notify.backoff_ms` 指数退避，最多 `notify.attempts` 次；其它 4xx 不重试。

### 4.2 告警分级

| 级别 | 含义 | 响应时间 |
|------|------|----------|
| critical | 服务不可用 | 立即 |
| warning | 可能有问题 | 1 小时内 |
| info | 需要关注 | 1 天内 |

## 5. 日志

### 5.1 日志系统

日志使用 **zap + lumberjack**，支持：

- JSON / Console 两种输出格式
- 按大小轮转（单文件超过 max_size_mb）
- 按时间轮转（每隔 rotate_interval）
- 自动清理（max_backups + max_age_days）
- 可选压缩旧文件

### 5.2 日志格式

**JSON 格式（生产推荐）：**

```json
{
  "level": "info",
  "ts": "2024-01-01T10:00:00.000Z",
  "caller": "app/app.go:42",
  "msg": "task started",
  "task_id": "xxx",
  "source": "10.0.0.1:3306",
  "worker_id": "worker-1"
}
```

**Console 格式（开发调试）：**

```
2024-01-01T10:00:00.000Z    INFO    app/app.go:42    task started    {"task_id": "xxx", "source": "10.0.0.1:3306"}
```

### 5.3 日志级别

| 级别 | 用途 |
|------|------|
| debug | 详细调试信息（生产环境不推荐） |
| info | 正常操作日志 |
| warn | 警告，不影响服务 |
| error | 错误，需要关注 |

### 5.4 日志轮转

| 触发条件 | 行为 |
|----------|------|
| 文件大小 > max_size_mb | 立即轮转 |
| 距上次轮转 > rotate_interval | 定时轮转（即使文件很小） |
| 文件数量 > max_backups | 删除最旧文件 |
| 文件年龄 > max_age_days | 删除过期文件 |

**轮转后的文件命名：**

```
binlog-server.log          # 当前文件
binlog-server-2024-01-01T00-00-00.000.log   # 轮转后（带时间戳）
binlog-server-2024-01-02T00-00-00.000.log
```

### 5.5 关键日志模式

**搜索错误：**

```bash
# 所有错误日志
grep '"level":"error"' ./logs/binlog-server.log

# 特定任务的错误
grep '"level":"error".*"task_id":"xxx"' ./logs/binlog-server.log
```

**搜索租约问题：**

```bash
grep 'lease' ./logs/binlog-server.log
```

**搜索复制连接问题：**

```bash
grep -E '(connection refused|connect failed|replication error)' ./logs/binlog-server.log
```

### 5.6 Loki 查询示例

```logql
# 所有错误日志
{app="binlog-server"} |= `"level":"error"`

# 特定任务日志
{app="binlog-server"} | json | task_id="xxx"

# 租约相关日志
{app="binlog-server"} |= "lease"

# 过去 1 小时的错误
{app="binlog-server"} |= `"level":"error"` [1h]
```

## 6. 事件查询

### 6.1 通过 API 查询

```bash
# 最近事件
curl "http://localhost:8080/api/tasks/{task_id}/events?limit=20"

# 特定类型事件
curl "http://localhost:8080/api/tasks/{task_id}/events?event_type=TASK_ERROR"
```

### 6.2 事件类型

| 类型 | 说明 |
|------|------|
| `TASK_CREATED` | 任务创建 |
| `TASK_STARTED` | 任务启动 |
| `TASK_STOPPED` | 任务停止 |
| `DUMP_CLEANUP_PENDING` | Stop 没能 KILL 掉的源库 Binlog Dump。`message` 说明连接号还可能开着，源库可达后会 KILL |
| `DUMP_CLEANUP_CLEARED` | 该连接已 KILL，或源库 processlist 里已经没有这个号 |
| `TASK_ERROR` | 任务错误 |
| `TASK_LEASE_ACQUIRED` | 获取租约 |
| `TASK_LEASE_LOST` | 租约丢失 |
| `TASK_LEASE_DEGRADED` | 租约降级 |
| `TASK_FILE_ROTATED` | 文件切换 |
| `TASK_FILE_UPLOADED` | 文件上传成功 |
| `TASK_FILE_UPLOAD_FAILED` | 文件上传失败 |

### 6.3 通过数据库查询

```sql
-- 最近错误事件
SELECT task_id, event_type, message, created_at
FROM task_events
WHERE event_type = 'TASK_ERROR'
ORDER BY created_at DESC
LIMIT 20;

-- 特定任务的所有事件
SELECT event_type, message, detail, created_at
FROM task_events
WHERE task_id = 'xxx'
ORDER BY created_at DESC;
```

## 7. 健康检查

### 7.1 HTTP 健康检查

```bash
# 简单检查
curl http://localhost:8080/api/health
# {"status": "ok"}

# 详细检查（包含依赖）
curl http://localhost:8080/api/health?full=true
# {
#   "status": "ok",
#   "checks": {
#     "database": "ok",
#     "storage": "ok"
#   }
# }
```

### 7.2 Kubernetes 探针

```yaml
livenessProbe:
  httpGet:
    path: /api/health
    port: 8080
  initialDelaySeconds: 10
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /api/health
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 5
```

## 8. 监控最佳实践

### 8.1 监控清单

| 监控项 | 重要性 | 检查频率 |
|--------|--------|----------|
| 任务状态 | 高 | 实时 |
| 复制延迟 | 高 | 1 分钟 |
| 上传状态 | 中 | 5 分钟 |
| Worker 状态 | 高 | 1 分钟 |
| 内存使用 | 中 | 5 分钟 |
| 磁盘空间 | 高 | 5 分钟 |

### 8.2 告警收敛

- 相同告警 5 分钟内只发一次
- 相关告警聚合发送
- 设置合理的静默窗口

### 8.3 仪表板设计

- 概览仪表板：全局视图
- 任务仪表板：单任务详情
- 集群仪表板：Worker 状态
- 资源仪表板：系统资源
