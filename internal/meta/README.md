# internal/meta Module

## Files
- `mysql_store.go`: 元数据持久化实现与 schema 校验。
- `lease_store.go`: lease 读写逻辑。
- `retry.go`: 重试策略适配层与执行器封装（基于 backoff v4，屏蔽第三方类型）。
- `tracing.go`: metadata store tracing 开关与 span helper（默认关闭）。
- `sql/*.sql`: sqlc 查询定义（当前试点覆盖 lease、task_runs、worker_heartbeats）。
- `sqlcgen/*`: sqlc 生成代码（禁止手改，使用 `make sqlc-generate` 更新）。

## Exports
- Task/Checkpoint/Event/File/Lease/Run/Worker metadata 存储接口。
- 启动期 schema 版本与结构校验（支持 schema 校验超时配置）。

## Dependencies
- Upstream: `internal/tasks`, `internal/app`, `internal/replication`。
- Downstream: MySQL (`database/sql`)。
- Retry adaptor: `github.com/cenkalti/backoff/v4`（仅 `retry.go` 内部使用）。
- Tracing: `go.opentelemetry.io/otel`（仅在 app 启用 tracing 时生效）。
- SQL codegen: `github.com/sqlc-dev/sqlc`（通过 Makefile 统一生成/校验）。

## SQLC Pilot Boundary
- 已试点迁移：`lease_store`，`task_runs`，`worker_heartbeats`。
- 生成包路径：`internal/meta/sqlcgen`。
- 调用侧适配：业务层继续暴露原有 store 接口，内部通过 `sqlcgen.New(db|tx)` 调用。

## Update Rule
- 表结构契约、查询语义、重试策略变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
