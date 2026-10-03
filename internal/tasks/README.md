# internal/tasks Module

## Files
- `scheduler.go`: 调度器核心类型、选项注入、`TaskStore`（含 GetTask/ListTasksPage/ListStartingUnownedTasks）与通用辅助函数；`ExpiredLeaseTaskLister` 供 cluster 过期租约查询（缺实现返回 `ErrExpiredLeaseLookupNotAvailable`）。
- `memory_lease.go`: 进程内 `LeaseManager`，单机与测试走同一扇任务所有权门；`Release` 立刻腾出租约。
- `scheduler_task_ops.go`: 任务 CRUD 与配置更新（含整包 CreateTaskFromSpec、`AdoptDiskBackup`）；`GetTask` 按主键刷新，store 失败不退回内存旧抄本；没有 store 时，内存未命中再认 `data_dir` 里仍有分段的目录。`ListClusterObservation` 有 store 时读 `store.ListTasks` 全库所有权抄本；`ListTasksPage` 在有 store 时走 SQL 分页；没有 store 时名单含这些磁盘目录。`DashboardCounters` 在 store 实现 `TaskDashboardRollup` 时走 SQL 计数，否则一次过滤读取。启停不在本文件。
- `disk_files.go`: `{data_dir}/{task_id}` 的封存名与 `.open.e<epoch>` 扫描，standalone 无 task store 时的剩余目录发现，以及 adopt 默认 `FILE_POS`（最高分段的源文件名和字节大小）和下一个 open epoch。
- `scheduler_lifecycle.go`: 启停、`ClaimRunnableTasks`（无主 STARTING + 过期租约 + 自己名下空闲）、重试退避、FAILED 立刻放租约。只存在于磁盘上的目录拒绝 start/stop。已 adopt 的目录在 start 时把 epoch 抬到现有 `.open.e*` 之上。
- `task_list.go`: 数字 id 排序、host/port/state 过滤（host 走 `SameSourceHost`）、内存分页、`SummarizeTaskStates` / `SummarizeTasksBySource` / `RunningRefs`，以及 `FailedUploadFiles` / `StartingUnownedTasks`，供 standalone 与测试 fake 复用。
- `scheduler_transitions.go`: 私有生命周期转换规则（状态、事件、错误、ownership 与持久化）。
- `errors.go`: 稳定操作员错误类型（永久的 1045 / log_bin off / 身份不可用，以及可重试的 `SOURCE_UNREACHABLE`）。
- `scheduler_cluster_lease.go`: cluster lease 续租与降级/失租处理。
- `scheduler_observability.go`: 复制进度（含 at-tip）、checkpoint、事件/文件/运行历史查询。无 task store 时，剩余目录的 files 走磁盘扫描，checkpoint 不编造。
- `scheduler_retry_upload.go`: 上传失败补偿重试（只走失败文件查询，缺查询报错）与失败原因聚合。
- `sealed_upload.go`: 尽力上传的唯一调用方（`ApplySealedUpload`、`ObjectKey`）；首次封文件后与重试共用。
- `model.go`: 任务领域模型与状态定义（含复制进度 `AtTip`，以及不进 JSON 的 `KeepLocalSegments`）。
- 各 `*_test.go`: 状态机、租约、上传重试、事件等测试。
- `source_guard_test.go`: metadata/source 同端点拒绝策略的公开任务接口回归测试，覆盖 localhost、127/8、::1 与 IPv6 括号表示。
- `event_store_test.go` 中 fake store 为并发安全实现，用于 `-race` 校验稳定性。

## Exports
- 任务 CRUD、启动停止、状态推进。
- `GetTask` 走 store 主键查询：store 说没有就是没有，其它错误原样失败，不退回内存里的旧主人/epoch 抄本，也不改扫磁盘。没有 store 时先读内存名单；没有该 id 时，若 `{data_dir}/{id}` 仍有封存或 `.open.e<epoch>` 分段，返回只读身份（`STOPPED`，无 source）。`ListTasks` / `ListTasksPage` / dashboard 在没有 store 时把这些目录并进名单，Console 任务表因此能打开它们的文件。有 store 时不发现磁盘目录。`ListClusterObservation` 返回全库所有权抄本（有 store 读 `store.ListTasks`，失败原样返回；没有 store 时用含磁盘目录的同一份名单），不走任务观测过滤。`ListTasksPage` 返回 `{page, total}`；`DashboardCounters` 忽略 limit/offset，rollup store 用 `GROUP BY`，其它 store 一次过滤读取。`ClaimStartingTasks` 不扫描整表。新建 standalone 任务的数字 id 会跳过已有分段目录，避免复用该目录。
- `ClaimRunnableTasks`：开机和平时同一条「把该我跑的跑起来」（无主 STARTING + 过期租约 + 自己名下空闲 active）。`ClaimStartingTasks` / `ClaimExpiredTasks` 仍可单独调用。cluster 下 store 必须实现 `ExpiredLeaseTaskLister`，否则返回 `ErrExpiredLeaseLookupNotAvailable`。
- `RetryFailedUploads` 只走失败文件查询（`ListFailedUploadBinlogFiles`）。file store 未实现该查询时返回 `ErrFailedUploadLookupNotAvailable`，不得用限量 `ListBinlogFiles` 冒充没有失败文件。内存 fake 用 `FailedUploadFiles` 做等价实现。
- `StartTask` 允许在 Acquire 成功后接管过期的 RUNNING/LEASE_DEGRADED；仍拒绝抢占未过期租约或本机仍在跑的任务。磁盘剩余目录返回 `ErrDiskBackupReadOnly`，不占租约。`AdoptDiskBackup` 把 source 和 `cluster_key` 接到同一 id 上，状态保持 `STOPPED`，不自动启动。没传 `start.mode` 时起点是最高分段末尾的 `FILE_POS`。显式 `start.mode` 覆盖它。有 task store 时不从磁盘 adopt。adopt 之后的 start 在单机 `MemoryLease` 上把 epoch 抬到目录里最大 `.open.e*` 之上，runner 用这个 epoch 开下一个分段。
- `Restore` 仍通过 `ListTasks()` 加载启动全量快照。
- `CreateTaskFromSpec`：整包校验后才 persist。
- `FAILED` 立刻 `Release` 租约，其他 Worker 不必等 TTL；`RETRY_BACKOFF` 继续占着。可再次 `StartTask`（改完源库配置后）。
- `NewMemoryLease`：无租约表时的进程内所有权门；`LeaseManager.Verify` 供封文件前验租。
- 仅 `SOURCE_UNREACHABLE` 连续失败最多重试 10 次；runner ready 会清零进程内连续失败计数，服务重启后重新计数，其他 retryable source code 不共享此封顶。
- 事件记录、文件元信息、上传补偿。
- `BinlogFile.State` 暴露 `OPEN/SEALED` 生命周期，运行中 `/files` 可见当前 segment。
- `ListFiles`：file store 返回非空时保持元数据结果。file store 未配置，或该任务结果为空且设置了 `WithDataDir` 时，扫描 `{data_dir}/{task_id}` 的封存名与 `.open.e<epoch>`，按源序号升序（同序号封存在前，再按 epoch 升序）。`limit` 保留序号最大的窗口。`file_name` 是源文件名，`file_path` 是磁盘路径。store 查询失败不改扫磁盘。磁盘列表不编造 checkpoint。没有 task store 且内存无此 id 时，目录里仍有分段则用同一扫描；有 task store 时未知 id 仍是 `task not found`。
- `WithInternalCallTimeouts`：注入内部调用超时（read/write/lease/upload），用于 store/lease/uploader 依赖边界治理。
- `IsLoopbackHost`：只用字面规则识别 localhost、显式 loopback literal（127/8、::1）及有效 IPv6 括号表示，不做 DNS 解析，供 metadata guard 与源身份共享。
- `SameSourceHost`：回环别名是同一台源，非回环仍精确匹配；lookup 与任务观测 host 过滤共用。
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
