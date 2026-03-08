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

**复制指标：**

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `binlog_server_replication_lag_seconds` | gauge | `task_id` | 复制延迟（秒） |
| `binlog_server_checkpoint_age_seconds` | gauge | `task_id` | Checkpoint 年龄（秒） |

**Worker 指标：**

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `binlog_server_worker_online` | gauge | `worker_id` | Worker 在线状态（1=在线，0=离线） |

**上传指标：**

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `binlog_server_upload_failures_total` | gauge | - | 上传失败的文件记录数 |
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

### 4.1 Prometheus 告警规则

```yaml
# alerts.yml
groups:
  - name: binlog-server
    rules:
      # 任务告警
      - alert: TaskHighLag
        expr: binlog_server_replication_lag_seconds > 30
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Task {{ $labels.task_id }} has high replication lag"

      - alert: TaskCheckpointStale
        expr: binlog_server_checkpoint_age_seconds > 300
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Task {{ $labels.task_id }} checkpoint is stale"

      - alert: TaskInFailedState
        expr: binlog_server_task_state_count{state="FAILED"} > 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Task is in FAILED state"

      # 集群告警
      - alert: NoActiveWorkers
        expr: sum(binlog_server_worker_online) == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "No active workers in the cluster"

      - alert: WorkerOffline
        expr: binlog_server_worker_online == 0
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Worker {{ $labels.worker_id }} is offline"

      # 上传告警
      - alert: UploadFailuresExist
        expr: binlog_server_upload_failures_total > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Upload failures exist for tasks"

      - alert: UploadRetryFailing
        expr: rate(binlog_server_upload_retry_total{result="failed"}[5m]) > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Upload retry is failing"
```

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
