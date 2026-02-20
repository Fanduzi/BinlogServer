# Observability（Prometheus）

本文说明 `/metrics` 指标采集、Prometheus rule 示例与基础 runbook。  
说明：本项目只提供指标暴露与规则示例，不内置告警执行引擎。

## 1. 指标端点

- 地址：`GET /metrics`
- 格式：Prometheus exposition text（`text/plain; version=0.0.4`）
- 采集失败策略：best-effort（单项采集失败不阻断主流程与接口返回）

当前核心指标：

- `binlog_server_task_state_count{state="<STATE>"}`
- `binlog_server_replication_lag_seconds{task_id="<TASK_ID>"}`
- `binlog_server_checkpoint_age_seconds{task_id="<TASK_ID>"}`
- `binlog_server_worker_online{worker_id="<WORKER_ID>"}`
- `binlog_server_upload_failures_total`（当前元数据中 `UPLOAD_FAILED` 记录总数）

## 2. Prometheus 抓取配置示例

```yaml
scrape_configs:
  - job_name: "binlog-server"
    scrape_interval: 15s
    scrape_timeout: 5s
    static_configs:
      - targets:
          - "127.0.0.1:8080"
    metrics_path: /metrics
```

## 3. Prometheus Rule 示例（仅示例）

```yaml
groups:
  - name: binlog-server-alerts
    rules:
      - alert: BinlogServerWorkerOffline
        expr: binlog_server_worker_online == 0
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Worker offline"
          description: "worker {{ $labels.worker_id }} is offline for more than 2m"

      - alert: BinlogServerReplicationLagHigh
        expr: binlog_server_replication_lag_seconds > 120
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Replication lag high"
          description: "task {{ $labels.task_id }} lag > 120s for 5m"

      - alert: BinlogServerCheckpointStale
        expr: binlog_server_checkpoint_age_seconds > 300
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Checkpoint stale"
          description: "task {{ $labels.task_id }} checkpoint age > 300s"

      - alert: BinlogServerUploadFailuresDetected
        expr: binlog_server_upload_failures_total > 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Upload failures detected"
          description: "upload failed file records exist"
```

## 4. Runbook（告警排查步骤）

### 4.1 Worker Offline

1. 检查 worker 进程与主机存活（systemd/k8s pod 状态）。
2. 查询 `/api/workers` 确认 `last_seen_at` 与 `online` 状态。
3. 查看 worker 日志，重点排查心跳写入错误（meta DB 连接、认证、超时）。
4. 检查元数据库可达性与延迟（网络/连接池/ProxySQL）。
5. 恢复后确认 `binlog_server_worker_online` 回到 `1`。

### 4.2 Replication Lag High

1. 查询 `/api/tasks/{id}/replication` 确认 lag 与最近位点。
2. 检查源库写入峰值与 binlog 生成速率是否突增。
3. 检查 worker 负载（CPU/IO/网络）与磁盘吞吐瓶颈。
4. 检查任务事件 `/api/tasks/{id}/events` 是否有重试/错误。
5. 滞后恢复后确认 `binlog_server_replication_lag_seconds` 回落。

### 4.3 Checkpoint Stale

1. 查询 `/api/tasks/{id}/checkpoint` 与 `/api/tasks/{id}` 状态是否一致。
2. 检查任务是否进入 `LEASE_DEGRADED/RETRY_BACKOFF/FAILED`。
3. 检查元数据库写入是否异常（checkpoint 更新失败）。
4. 检查 lease 续约事件（`TASK_LEASE_*`）定位是否为控制面/元库窗口问题。
5. 恢复后确认 `binlog_server_checkpoint_age_seconds` 回落到阈值内。

### 4.4 Upload Failures Detected

1. 查询 `/api/tasks/{id}/files` 筛查 `UPLOAD_FAILED` 记录。
2. 检查对象存储凭证、网络、bucket 权限与 endpoint 可达性。
3. 检查上传失败错误文案（`upload_error`）定位具体原因。
4. 修复外部依赖后手动触发补传：`POST /api/tasks/{id}/files/retry-upload?limit=100`。
5. 再次查询 `/api/tasks/{id}/files`，确认历史失败文件开始转为 `UPLOADED`。
