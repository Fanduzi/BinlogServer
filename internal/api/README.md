# internal/api Module

## Files
| File | Responsibility |
|------|---------------|
| `server.go` | HTTP server/router 组装、路由注册（含 `/healthz` 与 `/api/health`） |
| `auth.go` | 路由级鉴权配置与认证中间件、ServerOption 定义 |
| `rate_limiter.go` | 基于 IP 的令牌桶限流器 |
| `metrics_prometheus.go` | `/metrics` 采集与输出：一次 scrape 只读一份 `ListClusterObservation`，失败 5xx，不在 Collect 里再读一遍后记日志并吐空计数 |
| `tracing.go` | HTTP 入站 tracing middleware（OTel span） |
| `handlers_tasks.go` | 任务相关 API 处理（CRUD、批量创建、启动停止、checkpoint、source lookup、summary/dashboard 一次过滤读取后内存切页） |
| `handlers_cluster.go` | 集群观测：overview / workers 任务计数读 `ListClusterObservation`（有 store 时全库所有权抄本，不是任务页过滤，也不是启动内存名单） |
| `cluster_observation_test.go` | HTTP 缝测试：过滤后的 dashboard 汇总 ≠ 集群人数；store 主人/状态变化反映到 overview/workers/metrics；无 store 仍用内存名单；`/metrics` 一次 scrape 只读一份 store 抄本 |
| `swagger_docs_only.go` | swagger 注释占位 |

## Exports
- `NewServer(taskService, ...ServerOption) http.Handler` - 创建 API 服务器
- `WithAuth(AuthConfig) ServerOption` - 注入认证配置
- `WithTracing(TracingConfig) ServerOption` - 注入 tracing 配置
- `WithRateLimit(RateLimiterConfig) ServerOption` - 注入限流配置
- `GET /api/summary` - 返回兼容既有字段的任务计数；`starting` 单独统计 STARTING，`running` 仅统计 runner ready 后的 RUNNING。
- `GET /api/dashboard` - 返回同口径 summary、任务明细与 source 聚合；source 状态计数同时暴露 `starting` 与 `running`。
- `GET /api/cluster/overview` / `GET /api/workers` / `GET /metrics` 的任务与主人计数共用 `ListClusterObservation`：有 store 时读 `store.ListTasks` 全库抄本，store 错误返回 5xx，不退回启动时的内存名单；没有 store 时仍读同一份内存名单。`/metrics` 一次 scrape 只读一份抄本。任务页 dashboard 过滤汇总不是集群人数。
- `GET /api/tasks/{id}` - 按 id 读单个任务。有 store 时 store 未找到返回 404，其它 store 错误返回 5xx，不把内存里的旧主人/epoch 抄本当成 200；没有 store 时仍读内存名单。
- `GET /api/tasks` - 返回 `{items,total,limit,offset}` 任务页；页序为数字 id 升序；支持 host/port/state 过滤；cluster/mysql 走 `ListTasksPage`（COUNT + `ORDER BY CAST(id AS UNSIGNED), id LIMIT/OFFSET`），standalone 仍切内存快照。默认 limit=100，limit 必须为 1..500，超过 500 返回 400 `invalid limit`。
- `GET /api/dashboard` - 支持同一组过滤/分页参数；一次 `ListTasksPage`（Limit<=0 表示全部匹配）得到匹配集，再内存切页；`total` 与 `summary.total` 同一数字。
- `POST /api/tasks/batch` - 接收 `items` 数组（1..100 个现有创建请求），整包 envelope 错误返回 400 且不创建；合法 envelope 按顺序逐项调用 `CreateTaskFromSpec`，返回 200 的 `{index,cluster_key,task|error}` 结果数组。

## Dependencies
- Upstream: `internal/app` - 应用启动时注入
- Downstream: `internal/tasks` - 任务服务接口
- Metrics: `github.com/prometheus/client_golang`
- Tracing: `go.opentelemetry.io/otel`

## Features
- 认证：支持 Bearer Token 或 API Key；`/healthz` 默认匿名，`/metrics` 与 `/api/*` 可配置保护。`sanitizeTask` 在响应中清空 `source.password`（解密仅供内部使用）。`/ui` 与 `/swagger` 仍不走 API auth。
- 创建任务：`CreateTaskFromSpec` 整包校验通过后才落库；400 返回 JSON `{"error","code"}`。批量创建复用同一入口，单项错误不阻塞后续项，成功任务脱敏返回。
- source lookup：`GET /api/sources/lookup` 使用 tasks 共享 loopback 分类归并 localhost 与显式 loopback literal，端口仍严格匹配；非 loopback host 保持修剪后的原文精确匹配且不做 DNS 解析。
- 健康检查：`GET /healthz` 文本 `ok`；`GET /api/health` JSON `{"status":"ok"}`
- 文件观测：`GET /api/tasks/{id}/files` 返回当前 `OPEN` segment 与历史 `SEALED` 文件。
- 状态汇总：summary/dashboard 保留既有计数键，并新增 `starting`；STARTING 不混入 `running`。
- Source 聚合：dashboard source 项保留 `running`，并新增独立 `starting` 状态计数。
- 服务端分页：任务列表仍 SQL LIMIT/OFFSET；dashboard/summary 一次取出过滤匹配集再内存切页，total 与 summary.total 同一数字。非法 state/limit/offset/port 返回 400，limit 超过 500 返回 `invalid limit`。
- 限流：基于 IP 的令牌桶限流，默认 100 req/s，burst 200
- Tracing：OTel HTTP span（可选）

## Validation Pilot (P4)
- Gin binding + validator 校验：
  - `/api/sources/lookup`（`host`/`port` 必填 + 端口格式校验）
  - `/api/tasks/{id}/files/retry-upload`（`limit` 范围 1..1000，默认 100）
  - `/api/tasks/{id}/upload-failures/reasons`（`limit` 范围 1..200，默认 20）
- task、replication 与 dashboard 响应保留 `FAILED` 状态及稳定的源错误 `last_error`，供管理台直接展示。
- RUNNING 且 dump 已在源 tip（`ReplicationProgress.AtTip`）时，`delay_seconds` 为 0 / `NORMAL`，即使 `last_event_at` 仍是旧 event header；仍在追位点的 catch-up 继续按 `now - last_event_at` 计算 DELAYED。

## Update Rule
- 路由、请求/响应结构、认证/限流配置变化时，更新本文件。
