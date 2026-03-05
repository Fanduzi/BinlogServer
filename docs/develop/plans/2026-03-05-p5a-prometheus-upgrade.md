# P5a Prometheus 指标升级报告

## Scope
- 阶段：P5a（Prometheus 指标升级）
- 仅涉及 `internal/api` 的 `/metrics` 输出实现与兼容性验证。
- 不改变 API 状态码、任务状态机语义。

## Implementation
1. 将 `/metrics` 从手写文本拼接切换为 `prometheus/client_golang`。
2. 引入自定义 Collector（`internal/api/metrics_prometheus.go`），按 scrape 实时采集任务/复制/checkpoint/worker/上传重试指标。
3. `server.go` 的 `handleMetrics` 改为通过 `promhttp.HandlerFor(registry)` 输出。
4. 保留现有指标名、HELP、TYPE 与关键标签。

## Compatibility List

### 保留指标
- `binlog_server_task_state_count{state=...}`
- `binlog_server_replication_lag_seconds{task_id=...}`
- `binlog_server_checkpoint_age_seconds{task_id=...}`
- `binlog_server_worker_online{worker_id=...}`
- `binlog_server_upload_failures_total`
- `binlog_server_upload_retry_total{result=success|failed|skipped}`
- `binlog_server_upload_retry_last_ts`

### 变更指标
- 无（名称、关键标签、类型保持兼容）。

### 新增指标
- 无（本阶段仅实现升级，不扩充指标面）。

## Compatibility Tests
- 既有指标存在性断言继续保留。
- 新增/加强关键断言：
  - `task_state_count` 包含 `state` 标签。
  - `replication_lag_seconds` 与 `checkpoint_age_seconds` 包含 `task_id` 标签。
  - `worker_online` 包含 `worker_id` 标签。
  - `upload_retry_total` 的 TYPE 仍为 `counter`。

## Overhead Rough Comparison (before vs after)

### Method
- 命令：`/usr/bin/time -l go test ./internal/api -run TestAPI_MetricsEndpointContainsCoreMetrics -count=30`
- 对比对象：
  - 改前：`main`（手写 metrics 输出）
  - 改后：`hardening/p5a-prometheus-upgrade`（client_golang）

### Observations
- 延迟（real）：稳定在约 `1.48s~1.49s`（两侧同量级，无明显退化）。
- CPU（user+sys）：改后轻微波动上升，仍在同量级。
- 内存（maximum resident set size）：改后约 `+10%~12%` 粗增幅（符合引入 client_golang 依赖后的预期范围）。

> 说明：该对比为阶段内粗测，包含测试框架与重复运行开销，不等同于线上 QPS 压测结果。

## Template Reference
- 阶段验收模板：`docs/develop/plans/2026-03-02-phase-acceptance-template.md`
