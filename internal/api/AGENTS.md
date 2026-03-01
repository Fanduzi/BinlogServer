# internal/api AGENTS

## Members
- `server.go`: HTTP server/router 组装。
- `handlers_tasks.go`: 任务相关 API 处理。
- `handlers_cluster.go`: 集群观测与控制相关 API。
- `swagger_docs_only.go`: swagger 注释占位。

## Interfaces
- `/api/tasks*`: 任务创建、启动、停止、查询、重传等。
- `/api/cluster/*`, `/api/workers`: 集群与 worker 状态接口。
- `/healthz`, `/readyz`: 健康检查接口。

## Dependencies
- Upstream: HTTP client/UI。
- Downstream: `internal/tasks` service 接口。

## Update Rule
- 路由、请求/响应结构、错误语义变化时，更新本文件。
