# internal/tasks Module

## Files
- `scheduler.go`: 调度器核心类型、选项注入、`TaskStore`（含 GetTask/ListTasksPage/ListStartingUnownedTasks）与通用辅助函数；`ExpiredLeaseTaskLister` 供 cluster 过期租约查询（缺实现返回 `ErrExpiredLeaseLookupNotAvailable`）。
- `memory_lease.go`: 进程内 `LeaseManager`，单机与测试走同一扇任务所有权门；`Release` 立刻腾出租约。
- `scheduler_task_ops.go`: 任务 CRUD 与配置更新（含整包 CreateTaskFromSpec、`AdoptDiskBackup`）；`GetTask` 按主键刷新，store 失败不退回内存旧抄本；没有 store 时，内存未命中再认 `data_dir` 里仍有分段的目录。`ListClusterObservation` 有 store 时读 `store.ListTasks` 全库所有权抄本；`ListTasksPage` 在有 store 时走 SQL 分页；没有 store 时名单含这些磁盘目录。`DashboardCounters` 在 store 实现 `TaskDashboardRollup` 时走 SQL 计数，否则一次过滤读取。启停不在本文件。
- `disk_files.go`: `{data_dir}/{task_id}` 的封存名与 `.open.e<epoch>` 扫描，standalone 无 task store 时的剩余目录发现，以及 adopt 默认 `FILE_POS`（最高分段的源文件名和字节大小）和下一个 open epoch。`WindowBinlogFilesForReplay` 把目录行或磁盘行排成同一回放顺序：源序号升序，同序号封存在前，再按 epoch 升序；`limit` 保留序号最大的窗口。`SelectReplayFiles` 在这个窗口里每个序号只留一条：封存名和 `.open.e*` 同时存在时留 epoch 最大的 open 路径。`ReplayClient` 把 `source.flavor` 的 `mysql` 映射成 `mysqlbinlog` / `MySQL mysqlbinlog`，`mariadb` 映射成 `mariadb-binlog`。
- `segment_download.go`: `OpenTaskSegment` 按文件清单里的磁盘文件名打开本进程 `{data_dir}/{task_id}/{name}`。封存名和当前 `.open.e*` 都可读，读到打开时的字节长度。本地文件存在时只读本地，不读对象存储。本地没有时，封存行 `upload_state=UPLOADED` 且 `object_key` 非空，并且进程已配置上传，则按该 key 读对象，长度是打开对象时的大小。`LOCAL_ONLY`、`UPLOAD_FAILED`、空 key、open 分段、未配置上传仍是 `segment not found on this process`。名字含 `/`、`\` 或 `..` 返回 `invalid segment name`。不跟 catalog `file_path`。
- `replay_archive.go`: `OpenReplayArchive` 用 `ListFiles` 的同一窗口和 `SelectReplayFiles` 选出每个源序号一条，再按 `OpenTaskSegment` 打开。成员名是 basename。全部能读完才留下一个 ustar 临时文件；任一打开或读取失败则删除临时文件并返回错误，不留下半个包。窗口为空时是没有成员的空 tar。
- `scheduler_lifecycle.go`: 启停、`ClaimRunnableTasks`（无主 STARTING + 过期租约 + 自己名下空闲）、重试退避、FAILED 立刻放租约。只存在于磁盘上的目录拒绝 start/stop。已 adopt 的目录在 start 时把 epoch 抬到现有 `.open.e*` 之上。
- `task_list.go`: 数字 id 排序、host/port/state 过滤（host 走 `SameSourceHost`）、内存分页、`SummarizeTaskStates` / `SummarizeTasksBySource` / `RunningRefs`，以及 `FailedUploadFiles` / `StartingUnownedTasks`，供 standalone 与测试 fake 复用。
- `scheduler_transitions.go`: 私有生命周期转换规则（状态、事件、错误、ownership 与持久化）。
- `errors.go`: 稳定操作员错误类型（永久的 1045 / log_bin off / 身份不可用，以及可重试的 `SOURCE_UNREACHABLE`）。
- `scheduler_cluster_lease.go`: cluster lease 续租与降级/失租处理。
- `scheduler_observability.go`: 复制进度（含 at-tip）、checkpoint、事件/文件/运行历史查询。无 task store 时，剩余目录的 files 走磁盘扫描。`GetCheckpoint` 仍只读已存储的 checkpoint 行，不从磁盘编造。`ResumePosition` 返回下次 Start 会用的 file/pos；本地 open 分段有完整事件时用该事件，file+pos 与存储行一致时带上 `gtid_set`。
- `resume.go`: `NextResumePosition`。adopt 的 `KeepLocalSegments` 不改用本地事件。没有本地事件时用 checkpoint；epoch 大于 1 时把该 checkpoint 回拨到位置 4。
- `scheduler_retry_upload.go`: 上传失败补偿重试（只走失败文件查询，缺查询报错）与失败原因聚合。
- `sealed_upload.go`: 尽力上传的唯一调用方（`ApplySealedUpload`、`ObjectKey`）；首次封文件后与重试共用。上传成功后若 uploader 能读对象，`checksum` 为 `match` 或 `mismatch`；对不上不失败调用方，也不能写成 `match`。不能读对象时 `checksum` 留空。
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
- `BinlogFile.State` 暴露 `OPEN/SEALED` 生命周期，运行中 `/files` 可见当前 segment。`BinlogFile.Checksum` 为 `match` 或 `mismatch`，随文件清单返回。
- `ResumePosition`：`GET /api/tasks/{id}/checkpoint` 使用。与 runner 的 `NextResumePosition` 相同。没有本地完整事件、也没有有效 checkpoint 时返回未命中。`GetCheckpoint` 不因磁盘文件而变成命中。
- `ListFiles`：file store 返回非空时保持元数据结果。MySQL 目录按源序号升序（同序号封存在前，再按 epoch 升序），`limit` 保留序号最大的窗口，与磁盘扫描相同。file store 未配置，或该任务结果为空且设置了 `WithDataDir` 时，扫描 `{data_dir}/{task_id}` 的封存名与 `.open.e<epoch>`，按同一顺序。`file_name` 是源文件名，`file_path` 是磁盘路径。store 查询失败不改扫磁盘。磁盘列表不编造 checkpoint。没有 task store 且内存无此 id 时，目录里仍有分段则用同一扫描；有 task store 时未知 id 仍是 `task not found`。
- `OpenTaskSegment`：下载用。名字必须是 `ListFiles` 同一份清单里的磁盘文件名（`file_path` 的 base；没有 `file_path` 时用 `file_name`）。本地 `{data_dir}/{task_id}/{name}` 存在时只读该文件，长度停在打开时的大小。任务是 RUNNING 或 STOPPED 都不会因为分段仍是 OPEN 而跳过。本地没有时，封存且 `UPLOADED`、`object_key` 非空、且已配置对象上传，才从对象存储读，长度是打开对象时的大小；否则 `segment not found on this process`。不跟 catalog `file_path`，也不为 open 分段向对象存储编造字节。未配置上传时与以前一样只认本地。
- `OpenReplayArchive`：把 `SelectReplayFiles` 在 `ListFiles(limit)` 窗口里留下的 basename 打成一个完整 ustar。字节规则与 `OpenTaskSegment` 相同。没有分段时返回空 tar。任一选中分段打不开或读不完时返回错误，并且不留下临时 tar。
- `WithInternalCallTimeouts`：注入内部调用超时（read/write/lease/upload），用于 store/lease/uploader 依赖边界治理。
- `IsLoopbackHost`：只用字面规则识别 localhost、显式 loopback literal（127/8、::1）及有效 IPv6 括号表示，不做 DNS 解析，供 metadata guard 与源身份共享。
- `SameSourceHost`：回环别名是同一台源，非回环仍精确匹配；lookup 与任务观测 host 过滤共用。
- `WithMetadataSourceEndpoint`：注入 metadata TCP 端点，并在任务 create/update/configure/start 时拒绝同端点 source。
- Stop 路径 lease release 使用独立超时上下文（不复用已取消 runner ctx）。run 退出在放下调度锁之前关闭 `done`，所以 StopTask 不会把已经结束的执行留在 `STOPPING`。持久化放开锁之后，如果这次写入的快照已经不是最新一代，会再写当时最新的快照，避免后完成的 `STOPPING` 覆盖先落库的 `STOPPED`。
- cluster fail-safe stop 一旦进入 `STOPPING/STOPPED`，会拒绝后续正常复制进度上报，避免失租后继续暴露健康运行态进度。
- runner/lease 的自动转换保持 best-effort 持久化语义，并统一记录持久化失败日志。

## Dependencies
- Upstream: `internal/api`, `internal/app`。
- Downstream: `internal/replication` runner, `internal/binlog` durable resume cursor, `internal/meta` stores。

## Update Rule
- 状态机规则、调度策略、外部接口变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
