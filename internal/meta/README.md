# internal/meta Module

## Files
- `mysql_store.go`: 元数据持久化实现与 schema 校验（含 binlog file OPEN/SEALED 状态）；`GetTask` 按主键读取；`ListTasks` 仍为 Restore 全量快照（`ORDER BY CAST(id AS UNSIGNED), id`）；`ListTasksPage` 使用同一排序加 `LIMIT/OFFSET` 与 COUNT，host/port 过滤经 `JSON_EXTRACT(source_json)`，host 回环 SQL 与 `SameSourceHost` 同规则（拒绝 `127.000.0.1` / `[127.0.0.1]`，接受展开的 IPv6 `::1`）；`CountTaskStates` / `CountTasksBySource` 用同一 WHERE 做 `GROUP BY`（忽略 limit/offset，不读整行）；`ListRunningTaskRefs` 只取 RUNNING 的 id/host/port；`ListStartingUnownedTasks` 只查 STARTING 且 owner 为空；`ListTasksWithExpiredLease` 以 INNER JOIN `task_leases` 列出租约已过期的 RUNNING/LEASE_DEGRADED/RETRY_BACKOFF，以及同样过期的 STOPPING。`ListEvents` 用 `ORDER BY id DESC LIMIT` 取最新若干条，再反转成旧的在前。`ListBinlogFiles` 读出该任务全部 `binlog_files` 行，按源序号升序（同序号再按 epoch 升序），`limit` 保留序号最大的窗口，与磁盘扫描一致，并带回 `checksum`（`match` / `mismatch`，空值保持空）和 `epoch`。`ListBinlogFilesPage` 按唯一键 `(file_name, epoch)` 做键集分页，带 `LIMIT`，单页最多 10000 行，供保留和 rotate 分段读取；`ListBinlogFiles` 的回放窗口查询不变。`UpsertBinlogFile` 写入 `source_file`、`epoch` 和同一组列；唯一键是 `(task_id, file_name, epoch)`，所以后一次打开不会改写更早 epoch 的行。已有行的 `epoch` 默认为 0，仍然有效。`000002` 在替换唯一键之前，把 `file_path` 结尾是 `.open.eN` 或 `.sealed.eN` 的行的 `epoch` 改成 N；普通封存名看不出 epoch，保持 0，对象键也不改。`DeleteBinlogFile` 按 `task_id`、`file_name` 和 `epoch` 删除一行。启动要求 `schema_migrations` 版本至少为 2，并且存在索引 `uk_task_file_epoch`。配置了 encryption key 时只加密 `source_json` 的 `password` 字段（`enc:aes256:`），无 key 时保持明文以兼容现有部署。
- `lease_store.go`: lease 读写（Acquire/Renew/Release/Verify，与封文件前验租同一扇门）。
- `retry.go`: 重试策略适配层与执行器封装（基于 backoff v4，屏蔽第三方类型）。
- `tracing.go`: metadata store tracing 开关与 span helper（默认关闭）。
- `sql/*.sql`: sqlc 查询定义（当前试点覆盖 lease、task_runs、worker_heartbeats）。
- `sqlcgen/*`: sqlc 生成代码（禁止手改，使用 `make sqlc-generate` 更新）。

## Exports
- Task/Checkpoint/Event/File（含 OPEN/SEALED）/Lease/Run/Worker metadata 存储接口（含 `GetTask`、`ListTasksPage`、`ListStartingUnownedTasks`、`CountTaskStates`、`CountTasksBySource`、`ListRunningTaskRefs`）。
- `ListTasksWithExpiredLease`：cluster worker 查询，返回 `lease_expire_at <= NOW(6)` 的 RUNNING/LEASE_DEGRADED/RETRY_BACKOFF，以及同样过期的 STOPPING。STOPPING 由 worker 收成 STOPPED，不接管执行。
- `NewMySQLTaskStoreWithSchemaTimeout(dsn, timeout, encryptionKey)`：可选 AES-256 key，用于 source 密码加解密。
- 启动期 schema 版本与结构校验（支持 schema 校验超时配置）。

## Dependencies
- Upstream: `internal/tasks`, `internal/app`, `internal/replication`。
- Downstream: MySQL (`database/sql`)；源库密码加解密复用 `internal/config` 的 AES-256-GCM helpers。
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
