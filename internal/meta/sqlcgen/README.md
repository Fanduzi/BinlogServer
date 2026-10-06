# internal/meta/sqlcgen

sqlc 生成包。不要手改这里的 Go 文件。改 `internal/meta/sql/*.sql` 后执行 `make sqlc-generate`。生成器不写 L3 文件头，`internal/meta/stamp_sqlc_headers.sh` 会补上。

## Files

- `db.go`: `Queries` 与 `New`。
- `models.go`: 表行结构。
- `querier.go`: `Querier` 接口。
- `lease.sql.go`: 租约 Acquire/Renew/Release/Get。同一 worker 收回过期租约不增加 epoch。
- `task_runs_worker_heartbeats.sql.go`: task run 与 worker heartbeat 查询。

## Interface

`Querier` 是这些查询的接口。业务代码用 `sqlcgen.New(db)`。

## Dependencies

上游是 `internal/meta/sql` 和 `migrations/`。下游是 `internal/meta` 的 store。
