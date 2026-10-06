# internal/meta Module

## Files
- `mysql_store.go`: 元数据持久化实现与 schema 校验（含 binlog file OPEN/SEALED 状态）；`GetTask` 按主键读取；`ListTasks` 仍为 Restore 全量快照（`ORDER BY CAST(id AS UNSIGNED), id`）；`ListTasksPage` 使用同一排序加 `LIMIT/OFFSET` 与 COUNT，host/port 过滤经 `JSON_EXTRACT(source_json)`，host 回环 SQL 与 `SameSourceHost` 同规则（拒绝 `127.000.0.1` / `[127.0.0.1]`，接受展开的 IPv6 `::1`）；`CountTaskStates` / `CountTasksBySource` 用同一 WHERE 做 `GROUP BY`（忽略 limit/offset，不读整行）；`ListRunningTaskRefs` 只取 RUNNING 的 id/host/port；`ListStartingUnownedTasks` 只查 STARTING 且 owner 为空；`ListTasksWithExpiredLease` 以 INNER JOIN `task_leases` 列出租约已过期的 RUNNING/LEASE_DEGRADED/RETRY_BACKOFF，以及同样过期的 STOPPING。`ListEvents` 用 `ORDER BY id DESC LIMIT` 取最新若干条，再反转成旧的在前。`ListBinlogFiles` 读出该任务全部 `binlog_files` 行，按源序号升序（同序号再按 epoch 升序），`limit` 保留序号最大的窗口，与磁盘扫描一致，并带回 `checksum`（`match` / `mismatch`，空值保持空）和 `epoch`。`ListBinlogFilesPage` 按唯一键 `(file_name, epoch)` 做键集分页，带 `LIMIT`，单页最多 10000 行，供保留和 rotate 分段读取；`ListBinlogFiles` 的回放窗口查询不变。`UpsertBinlogFile` 按 `(task_id, source_file, epoch)` 写入一段，`file_name` 与 `source_file` 都是源文件名，所以 `uk_task_source_epoch` 和 `uk_task_file_epoch` 命中同一行，后一次打开不会改写更早 epoch 的行。Go 里的位点 0 是未知：绑定 NULL。调用方不知道 `end_pos` 时，更新语句不赋值 `end_pos`，已有终点保持不动。知道终点时才写 `end_pos = VALUES(end_pos)`。失败补传列表用 `state = 'SEALED'`，不再用文件名 `NOT LIKE`。已有行的 `epoch` 默认为 0，仍然有效。`000002` 在替换唯一键之前，把 `file_path` 结尾是 `.open.eN` 或 `.sealed.eN` 的行的 `epoch` 改成 N；普通封存名看不出 epoch，保持 0，对象键也不改。`DeleteBinlogFile` 按 `task_id`、`file_name` 和 `epoch` 删除一行。`backup_tasks` 的读写带上 `desired_run`、`spec_revision`、`applied_spec_revision`、`failed_spec_revision`、`retry_attempt`、`consecutive_source_failures`。`ReconcileLegacyDesiredRun` 只更新 `spec_revision=0` 且 `applied_spec_revision=0`、并且 `desired_run` 与状态不一致的行，不增加修订号，重复执行不再改行。`pending_dump_cleanup` 只在跑过 `000004` 时读写，空字符串表示没有待 KILL 的连接；这一份代码的 `minRequiredSchemaVersion` 是 5，schema 5 已经包含这一列，缺列的进程内标记只在版本检查通过之后才会走到。任务 upsert 在库里的 `spec_revision` 更新时，整行保持不动，避免 worker 的旧重试快照盖掉控制面刚写的 Stop 或新密码。启动要求 `schema_migrations` 版本至少为 5，低于 5 时拒绝启动并提示执行 `./migrate up`，并且存在索引 `uk_task_file_epoch` 和 `uk_task_source_epoch`。配置了 encryption key 时只加密 `source_json` 的 `password` 字段（`enc:aes256:`），无 key 时保持明文以兼容现有部署。
- `lease_store.go`: lease 读写（Acquire/Renew/Release/Verify，与封文件前验租同一扇门）。同一 `worker_id` 收回租约（含已过期）不增加 epoch；别的 worker 接管过期租约才 `epoch+1`。`Release` 把 `owner_worker_id` 写成空，下一次 Acquire 视为换主并增加 epoch。
- `retry.go`: 重试策略适配层与执行器封装（基于 backoff v4，屏蔽第三方类型）。`IsTransientMySQLError` 与任务重试白名单共用同一组文本（deadlock、lock wait timeout、connection reset、connection refused、broken pipe、server has gone away、invalid connection、bad connection、read-only、read only、timeout、eof）。
- `tracing.go`: metadata store tracing 开关与 span helper（默认关闭）。
- `sql/*.sql`: sqlc 查询定义（当前试点覆盖 lease、task_runs、worker_heartbeats）。
- `sqlcgen/*`: sqlc 生成代码（禁止手改，使用 `make sqlc-generate` 更新）。`stamp_sqlc_headers.sh` 在生成后补上 L3 文件头。

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
