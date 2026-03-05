# internal/api Module

## Files
- `server.go`: HTTP server/router 组装。
- `metrics_prometheus.go`: `/metrics` 采集与输出（基于 `prometheus/client_golang`）。
- `auth.go`: 路由级鉴权配置与认证中间件。
- `handlers_tasks.go`: 任务相关 API 处理。
- `handlers_cluster.go`: 集群观测与控制相关 API。
- `swagger_docs_only.go`: swagger 注释占位。
- `server_test.go`: API 行为测试（含 Bearer/API Key 鉴权失败/成功路径）。

## Exports
- `/api/tasks*`: 任务创建、启动、停止、查询、重传等。
- `/api/cluster/*`, `/api/workers`: 集群与 worker 状态接口。
- `/healthz`, `/readyz`: 健康检查接口。
- 可配置鉴权：支持 `Bearer Token` 或 `API Key`；`/healthz` 默认匿名，`/metrics` 与 `/api/*` 可按配置开启保护。

## Dependencies
- Upstream: HTTP client/UI。
- Downstream: `internal/tasks` service 接口。
- Metrics: `github.com/prometheus/client_golang`（兼容现有 `binlog_server_*` 指标契约）。

## Update Rule
- 路由、请求/响应结构、错误语义变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
