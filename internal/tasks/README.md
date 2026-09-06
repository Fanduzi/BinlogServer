# internal/tasks Module

## Files
- `scheduler.go`: 调度器核心类型、选项注入、`TaskStore`（含 GetTask/ListTasksPage/ListStartingUnownedTasks）与通用辅助函数；`ExpiredLeaseTaskLister` 供 cluster 过期租约查询（缺实现返回 `ErrExpiredLeaseLookupNotAvailable`）。
- `memory_lease.go`: 进程内 `LeaseManager`，单机与测试走同一扇任务所有权门；`Release` 立刻腾出租约。
- `scheduler_task_ops.go`: 任务 CRUD 与配置更新（含整包 CreateTaskFromSpec）；`GetTask` 按主键刷新；`ListTasksPage` 在有 store 时走 SQL 分页。
- `scheduler_lifecycle.go`: 启停、运行协程、重试退避、`ClaimStartingTasks` 只认领 STARTING 空 owner、`ClaimExpiredTasks` 过期租约接管（缺查询报错，不走 StopTask），以及连续 10 次源不可达后的 FAILED（runner ready 后重新计数）。
- `task_list.go`: 数字 id 排序、host/port/state 过滤、内存分页，以及 `FailedUploadFiles` / `StartingUnownedTasks`，供 standalone 与测试 fake 复用。
- `scheduler_transitions.go`: 私有生命周期转换规则（状态、事件、错误、ownership 与持久化）。
- `errors.go`: 稳定操作员错误类型（永久的 1045 / log_bin off / 身份不可用，以及可重试的 `SOURCE_UNREACHABLE`）。
- `scheduler_cluster_lease.go`: cluster lease 续租与降级/失租处理。
- `scheduler_observability.go`: 复制进度（含 at-tip）、checkpoint、事件/文件/运行历史查询。
- `scheduler_retry_upload.go`: 上传失败补偿重试（只走失败文件查询，缺查询报错）与失败原因聚合。
- `sealed_upload.go`: 尽力上传的唯一调用方（首次封文件后与重试共用）。
- `model.go`: 任务领域模型与状态定义（含复制进度 `AtTip`）。
- 各 `*_test.go`: 状态机、租约、上传重试、事件等测试。
- `source_guard_test.go`: metadata/source 同端点拒绝策略的公开任务接口回归测试，覆盖 localhost、127/8、::1 与 IPv6 括号表示。
- `event_store_test.go` 中 fake store 为并发安全实现，用于 `-race` 校验稳定性。

## Exports
- 任务 CRUD、启动停止、状态推进。
- `GetTask` 走 store 主键查询；`ListTasksPage` 返回 `{page, total}`；`ClaimStartingTasks` 不扫描整表。
- `ClaimRunnableTasks`：开机和平时同一条「把该我跑的跑起来」（无主 STARTING + 过期租约 + 自己名下空闲 active）。`ClaimStartingTasks` / `ClaimExpiredTasks` 仍可单独调用。cluster 下 store 必须实现 `ExpiredLeaseTaskLister`，否则返回 `ErrExpiredLeaseLookupNotAvailable`。
- `RetryFailedUploads` 只走失败文件查询（`ListFailedUploadBinlogFiles`）。file store 未实现该查询时返回 `ErrFailedUploadLookupNotAvailable`，不得用限量 `ListBinlogFiles` 冒充没有失败文件。内存 fake 用 `FailedUploadFiles` 做等价实现。
- `StartTask` 允许在 Acquire 成功后接管过期的 RUNNING/LEASE_DEGRADED；仍拒绝抢占未过期租约或本机仍在跑的任务。
- `Restore` 仍通过 `ListTasks()` 加载启动全量快照。
- `CreateTaskFromSpec`：整包校验后才 persist。
- `FAILED` 立刻 `Release` 租约，其他 Worker 不必等 TTL；`RETRY_BACKOFF` 继续占着。可再次 `StartTask`（改完源库配置后）。
- `NewMemoryLease`：无租约表时的进程内所有权门；`LeaseManager.Verify` 供封文件前验租。
- 仅 `SOURCE_UNREACHABLE` 连续失败最多重试 10 次；runner ready 会清零进程内连续失败计数，服务重启后重新计数，其他 retryable source code 不共享此封顶。
- 事件记录、文件元信息、上传补偿。
- `BinlogFile.State` 暴露 `OPEN/SEALED` 生命周期，运行中 `/files` 可见当前 segment。
- `WithInternalCallTimeouts`：注入内部调用超时（read/write/lease/upload），用于 store/lease/uploader 依赖边界治理。
- `IsLoopbackHost`：只用字面规则识别 localhost、显式 loopback literal（127/8、::1）及有效 IPv6 括号表示，不做 DNS 解析，供 metadata guard 与 source lookup 共享。
- `WithMetadataSourceEndpoint`：注入 metadata TCP 端点，并在任务 create/update/configure/start 时拒绝同端点 source。
- Stop 路径 lease release 使用独立超时上下文（不复用已取消 runner ctx）。
- cluster fail-safe stop 一旦进入 `STOPPING/STOPPED`，会拒绝后续正常复制进度上报，避免失租后继续暴露健康运行态进度。
- runner/lease 的自动转换保持 best-effort 持久化语义，并统一记录持久化失败日志。

## Dependencies
- Upstream: `internal/api`, `internal/app`。
- Downstream: `internal/replication` runner, `internal/meta` stores。

## Update Rule
- 状态机规则、调度策略、外部接口变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
