# internal/api Module

## Files
| File | Responsibility |
|------|---------------|
| `server.go` | HTTP server/router 组装、路由注册（含 `/healthz` 与 `/api/health`） |
| `auth.go` | 路由级鉴权配置与认证中间件、ServerOption 定义 |
| `rate_limiter.go` | 基于 IP 的令牌桶限流器 |
| `metrics_prometheus.go` | `/metrics` 采集与输出（基于 `prometheus/client_golang`） |
| `tracing.go` | HTTP 入站 tracing middleware（OTel span） |
| `handlers_tasks.go` | 任务相关 API 处理（CRUD、启动停止、checkpoint、loopback-aware source lookup、summary/dashboard 聚合，以及任务 state/host/port 服务端分页） |
| `handlers_cluster.go` | 集群观测与控制相关 API（workers、overview） |
| `swagger_docs_only.go` | swagger 注释占位 |

## Exports
- `NewServer(taskService, ...ServerOption) http.Handler` - 创建 API 服务器
- `WithAuth(AuthConfig) ServerOption` - 注入认证配置
- `WithTracing(TracingConfig) ServerOption` - 注入 tracing 配置
- `WithRateLimit(RateLimiterConfig) ServerOption` - 注入限流配置
- `GET /api/summary` - 返回兼容既有字段的任务计数；`starting` 单独统计 STARTING，`running` 仅统计 runner ready 后的 RUNNING。
- `GET /api/dashboard` - 返回同口径 summary、任务明细与 source 聚合；source 状态计数同时暴露 `starting` 与 `running`。
- `GET /api/tasks` - 返回 `{items,total,limit,offset}` 任务页；支持 host/port/state 过滤，默认 limit=100，最大有效 limit=500。
- `GET /api/dashboard` - 支持同一组过滤/分页参数；`summary`/`sources` 按全量过滤结果聚合，仅 `tasks` 明细切页，并返回 `total/limit/offset`。

## Dependencies
- Upstream: `internal/app` - 应用启动时注入
- Downstream: `internal/tasks` - 任务服务接口
- Metrics: `github.com/prometheus/client_golang`
- Tracing: `go.opentelemetry.io/otel`

## Features
- 认证：支持 Bearer Token 或 API Key；`/healthz` 默认匿名，`/metrics` 与 `/api/*` 可配置保护
- 创建任务：`CreateTaskFromSpec` 整包校验通过后才落库；400 返回 JSON `{"error","code"}`
- source lookup：`GET /api/sources/lookup` 使用 tasks 共享 loopback 分类归并 localhost 与显式 loopback literal，端口仍严格匹配；非 loopback host 保持修剪后的原文精确匹配且不做 DNS 解析。
- 健康检查：`GET /healthz` 文本 `ok`；`GET /api/health` JSON `{"status":"ok"}`
- 文件观测：`GET /api/tasks/{id}/files` 返回当前 `OPEN` segment 与历史 `SEALED` 文件。
- 状态汇总：summary/dashboard 保留既有计数键，并新增 `starting`；STARTING 不混入 `running`。
- Source 聚合：dashboard source 项保留 `running`，并新增独立 `starting` 状态计数。
- 服务端分页：任务与 dashboard 使用稳定 task ID 顺序在全量快照上过滤、计数和切片；非法 state/limit/offset/port 返回 400，超出 limit 上限时有效值为 500。
- 限流：基于 IP 的令牌桶限流，默认 100 req/s，burst 200
- Tracing：OTel HTTP span（可选）

## Validation Pilot (P4)
- Gin binding + validator 校验：
  - `/api/sources/lookup`（`host`/`port` 必填 + 端口格式校验）
  - `/api/tasks/{id}/files/retry-upload`（`limit` 范围 1..1000，默认 100）
  - `/api/tasks/{id}/upload-failures/reasons`（`limit` 范围 1..200，默认 20）
- task、replication 与 dashboard 响应保留 `FAILED` 状态及稳定的源错误 `last_error`，供管理台直接展示。

## Update Rule
- 路由、请求/响应结构、认证/限流配置变化时，更新本文件。
