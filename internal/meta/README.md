# internal/meta Module

## Files
- `mysql_store.go`: 元数据持久化实现与 schema 校验（含 binlog file OPEN/SEALED 状态）；`GetTask` 按主键读取；`ListTasks` 仍为 Restore 全量快照（`ORDER BY CAST(id AS UNSIGNED), id`）；`ListTasksPage` 使用同一排序加 `LIMIT/OFFSET` 与 COUNT，host/port 过滤经 `JSON_EXTRACT(source_json)`，host 回环 SQL 与 `SameSourceHost` 同规则（拒绝 `127.000.0.1` / `[127.0.0.1]`，接受展开的 IPv6 `::1`）；`CountTaskStates` / `CountTasksBySource` 用同一 WHERE 做 `GROUP BY`（忽略 limit/offset，不读整行）；`ListRunningTaskRefs` 只取 RUNNING 的 id/host/port；`ListStartingUnownedTasks` 只查 STARTING 且 owner 为空；`ListTasksWithExpiredLease` 以 INNER JOIN `task_leases` 列出租约已过期的 RUNNING/LEASE_DEGRADED/RETRY_BACKOFF，以及同样过期的 STOPPING。`ListEvents` 用 `ORDER BY id DESC LIMIT` 取最新若干条，再反转成旧的在前。`ListSourceSwitchEvents` 只取 `event_type = 'SOURCE_SWITCHOVER'`，`ORDER BY id ASC LIMIT 1000`，旧的在前。没有新表。`ListBinlogFiles` 读出该任务全部 `binlog_files` 行，按源序号升序（同序号再按 epoch 升序），`limit` 保留序号最大的窗口，与磁盘扫描一致，并带回 `checksum`（`match` / `mismatch`，空值保持空）和 `epoch`。`ListBinlogFilesPage` 按唯一键 `(file_name, epoch)` 做键集分页，带 `LIMIT`，单页最多 10000 行，供保留和 rotate 分段读取；`ListBinlogFiles` 的回放窗口查询不变。`UpsertBinlogFile` 按 `(task_id, source_file, epoch)` 写入一段，`file_name` 与 `source_file` 都是源文件名，所以 `uk_task_source_epoch` 命中这一行。`000006` 丢掉 `uk_task_file_epoch`，`file_name` 列保留。后一次打开不会改写更早 epoch 的行。Go 里的位点 0 是未知：绑定 NULL。调用方不知道 `end_pos` 时，更新语句不赋值 `end_pos`，已有终点保持不动。知道终点时才写 `end_pos = VALUES(end_pos)`。失败补传列表用 `state = 'SEALED'`，不再用文件名 `NOT LIKE`。已有行的 `epoch` 默认为 0，仍然有效。`000002` 在替换唯一键之前，把 `file_path` 结尾是 `.open.eN` 或 `.sealed.eN` 的行的 `epoch` 改成 N；普通封存名看不出 epoch，保持 0，对象键也不改。`DeleteBinlogFile` 按 `task_id`、`file_name` 和 `epoch` 删除一行。`backup_tasks` 的读写带上 `desired_run`、`spec_revision`、`applied_spec_revision`、`failed_spec_revision`、`retry_attempt`、`consecutive_source_failures`。`ReconcileLegacyDesiredRun` 只更新 `spec_revision=0` 且 `applied_spec_revision=0`、并且 `desired_run` 与状态不一致的行，不增加修订号，重复执行不再改行。`pending_dump_cleanup` 只在跑过 `000004` 时读写，空字符串表示没有待 KILL 的连接；这一份代码的 `minRequiredSchemaVersion` 是 6，schema 6 已经包含这一列，缺列的进程内标记只在版本检查通过之后才会走到。任务 upsert 在库里的 `spec_revision` 更新时，整行保持不动，避免 worker 的旧重试快照盖掉控制面刚写的 Stop 或新密码。启动要求 `schema_migrations` 版本至少为 6，低于 6 时拒绝启动并提示执行 `./migrate up`。`binlog_files` 要求索引 `uk_task_source_epoch`，不再要求 `uk_task_file_epoch`。`file_name` 列保留。仍把 `uk_task_file_epoch` 列入必选索引的旧二进制，在 schema 6 上以 `missing index binlog_files.uk_task_file_epoch` 拒绝启动。配置了 encryption key 时只加密 `source_json` 的 `password` 字段（`enc:aes256:`），无 key 时保持明文以兼容现有部署。
- `findings_store.go`: `task_findings`（迁移 000007）的写入与查询。主键 `(task_id, kind, subject)`，行未解决时 `first_seen` 取较早值，已解决的行再次写入时是新的一次、`first_seen` 与 `count` 取新值（§25.8 R4），`resolved_at` 为 NULL 表示未解决。表不存在（schema 6）时返回 `tasks.ErrFindingStoreAbsent`，结论汇改为只在内存与 `/metrics` 中保留。
- `health_store.go`: 迁移 000009 之上的 `healthstore.Store`（`task_findings` 新增列、`task_health`、`finding_transitions`）。一个事务内：cluster / never-leased 先 `SELECT … FROM task_leases … FOR UPDATE`，再锁 `task_health` 行（行在事务外 `INSERT IGNORE` 预建，避免并发插入死锁），`healthstore.CheckFence` 判定后，每条新 transition 分配 `task_health.generation+1`，`transition_digest` 已存在的视为重放（generation 0）；死锁 / 锁等待（1213/1205）整体重试最多 50 次。finding upsert 在 MySQL 8.0.20+ 用 `AS new`，5.7 与 MariaDB 用 `VALUES()`（`SELECT VERSION()` 一次后缓存）。缺表或缺列（1146/1054）返回 `healthstore.ErrSchemaAbsent`，不记忆，下次调用重新尝试，所以 `./migrate up` 后无需重启。`ListOpenFindingsForTask` 按 `(kind, subject)` 键集分页、每页 1000 行、不设上限。v0.5.66 的 `UpsertFinding` / `ListFindings` 在 schema 7/8/9 上都照旧可用。下限仍是 6。`LoadRehashCursor` / `SaveRehashCursor` 读写单行 `rehash_cursor`（P2-3 复核器的游标）。 §25.8 R3：`task_findings`、`finding_transitions`、`task_health` 的时间一律 `UTC_TIMESTAMP(6)`（Go 传入的时间转 UTC），OPEN 的 `first_seen` 与 CLOSE 的 `resolved_at` 取转移的 `FactAt`；lease 表仍用 `NOW(6)`（只与 `NOW(6)` 比较）。
- `health_store_utc_test.go`: sqlmock 断言健康存储写入不用会话时区的 `NOW(6)`，OPEN 的 `first_seen`、CLOSE 的 `resolved_at` 是转移的 `FactAt`（§25.8 R3）。
- `health_store_test.go`: sqlmock 覆盖 ErrSchemaAbsent 不记忆与 upsert 方言；设置 `BINLOG_TEST_MYSQL_ROOT_DSNS`（逗号分隔、以 `/` 结尾的 root DSN）时，在真实 MySQL 8.0 / 5.7 / MariaDB 上跑 healthstore contract 与 6/7/8 → 9 迁移（含 down 再 up）；未设置时跳过。
- `reach.go`: #427（设计 0009 §25）。`PingMeta`（连接池上一次受 ctx 限时的 ping）与 `MetaUnreachable` / `IsUnreachable`：超时、`driver.ErrBadConn`、`ErrInvalidConn`、EOF、`net.Error`、客户端错误 2002/2003/2006/2013 算「meta 不可达」；服务端返回的 SQL 错误（只读 1290、权限、缺表）不算，它们各有 kind。实现 `tasks.MetaProbe`。
- `seal_facts_store.go`: `binlog_files` 的封存事实（迁移 000008：`first_event_at`、`last_event_at`、`sha256`）。`UpsertBinlogFile` 写完行之后，对带事实的 SEALED 行另发一条 `UPDATE`（主行 SQL 不变）；库里没有这些列（MySQL 1054，schema 6/7）时跳过并在进程内记住，不再尝试。下限仍是 6。 `ListSealedChecksums` 读回某任务封存行的 sha256 与大小，给 P2-3 复核器当预期校验和；缺列时返回空并同样记住。
- `rehash_source_test.go`: `ListSealedChecksums` 的 sqlmock（含 1054 记忆）；设了 `BINLOG_TEST_MYSQL_ROOT_DSNS` 时在真实目录上：复核器从目录行 sha256 发现原地翻转并读完全部封存字节，手工加的 `.sealed.e3` 副本开 SEGMENT_OVERLAP。
- `lease_store.go`: lease 读写（Acquire/Renew/Release/Verify，与封文件前验租同一扇门）。同一 `worker_id` 收回租约（含已过期）不增加 epoch；别的 worker 接管过期租约才 `epoch+1`。`Release` 把 `owner_worker_id` 写成空，下一次 Acquire 视为换主并增加 epoch。
- `write_fence.go`: ADR 0009 P2-6b / #417。`execUnderFence`：写的 context 带本任务的 `tasks.FenceIdentity` 时，`UpsertCheckpoint`、`UpsertBinlogFile`、`DeleteBinlogFile` 在同一事务里先 `SELECT owner_worker_id, epoch FROM task_leases … FOR UPDATE`，owner 与 epoch 都相同才写，否则（含 lease 已释放、没有行）返回 `tasks.ErrWriteFenced`，不写；重试包住整个事务。没有令牌（standalone、后台补传）时仍是原来的单条语句。
- `write_fence_test.go`: sqlmock：新 epoch / 已释放 / 无行被拒且不执行 INSERT，匹配时锁与写同一事务，无令牌时单条语句。
- `fenced_exec.go`: P2-6a / #360（P2-6c / #421：规则改为 `tasks.CheckTaskRowFence`；已释放于更大 epoch 拒绝，无 lease 行时锁 `backup_tasks` 行判断）。`FencedExec` / `UpsertTaskFenced` 在同一事务里 `SELECT … FROM task_leases … FOR UPDATE`，仅当 (owner, epoch) 仍匹配（或 lease 已释放 / 无行）时写任务行，否则返回 `healthstore.ErrFenced`，拒绝陈旧 STOPPING/STOPPED 覆盖新 holder。
- `fenced_exec_test.go`: sqlmock：陈旧 owner 被拒、匹配/已释放/无行可写、fenced 时不跑回调。
- `retry.go`: 重试策略适配层与执行器封装（基于 backoff v4，屏蔽第三方类型）。`IsTransientMySQLError` 与任务重试白名单共用同一组文本（deadlock、lock wait timeout、connection reset、connection refused、broken pipe、server has gone away、invalid connection、bad connection、read-only、read only、timeout、eof）。
- `tracing.go`: metadata store tracing 开关与 span helper（默认关闭）。
- `sql/*.sql`: sqlc 查询定义（当前试点覆盖 lease、task_runs、worker_heartbeats）。
- `sqlcgen/*`: sqlc 生成代码（禁止手改，使用 `make sqlc-generate` 更新）。`stamp_sqlc_headers.sh` 在生成后补上 L3 文件头。

## Exports
- Task/Checkpoint/Event/File（含 OPEN/SEALED）/Lease/Run/Worker metadata 存储接口（含 `GetTask`、`ListTasksPage`、`ListStartingUnownedTasks`、`CountTaskStates`、`CountTasksBySource`、`ListRunningTaskRefs`）。
- `ListTasksWithExpiredLease`：cluster worker 查询，返回 `lease_expire_at <= NOW(6)` 的 RUNNING/LEASE_DEGRADED/RETRY_BACKOFF，以及同样过期的 STOPPING。STOPPING 由 worker 收成 STOPPED，不接管执行。
- `ListTaskIDsWithExpiredOwnerLease`（#424 r3，设计 §24.13）：健康评估器代评用，只返回 id：owner 非空、epoch > 0、desired RUN、状态不是 FAILED/STOPPED/CREATED、`lease_expire_at <= NOW(6)` 的任务。实现 `tasks.DeadOwnerLister`。
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
