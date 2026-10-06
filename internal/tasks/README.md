# internal/tasks Module

## Files
- `scheduler.go`: 调度器核心类型、选项注入、`TaskStore`（含 GetTask/ListTasksPage/ListStartingUnownedTasks）与通用辅助函数；`ExpiredLeaseTaskLister` 供 cluster 过期租约查询（缺实现返回 `ErrExpiredLeaseLookupNotAvailable`）。
- `memory_lease.go`: 进程内 `LeaseManager`，单机与测试走同一扇任务所有权门；`Release` 立刻腾出租约。
- `scheduler_task_ops.go`: 任务 CRUD 与配置更新（含整包 CreateTaskFromSpec、`AdoptDiskBackup`）；`GetTask` 按主键刷新，store 失败不退回内存旧抄本；没有 store 时，内存未命中再认 `data_dir` 里仍有分段的目录。`ListClusterObservation` 有 store 时读 `store.ListTasks` 全库所有权抄本；`ListTasksPage` 在有 store 时走 SQL 分页；没有 store 时名单含这些磁盘目录。`DashboardCounters` 在 store 实现 `TaskDashboardRollup` 时走 SQL 计数，否则一次过滤读取。启停不在本文件。
- `disk_files.go`: `{data_dir}/{task_id}` 的封存名与 `.open.e<epoch>` 扫描，standalone 无 task store 时的剩余目录发现，以及 adopt 默认 `FILE_POS`（最高分段的源文件名和字节大小）和下一个 open epoch。`WindowBinlogFilesForReplay` 把目录行或磁盘行排成同一回放顺序：源序号升序，同序号封存在前，再按 epoch 升序；`limit` 保留序号最大的窗口。`SelectReplayFiles` 在这个窗口里留下每个序号的全部分封存分段，再加该序号 epoch 最高的 open。更早的封存路径不会被后一次 `.open.e*` 丢掉。只有当这条 open 行带着和封存行相同的 object key（接管时把对象字节写进了 open 文件）时，回放才省略那条封存路径。同一序号里 `start_pos` 等于 `end_pos` 且都大于 0 的封存行，如果另一条有实际区间的封存行已经盖住这个点，回放也省略它：这是接管时把已上传对象又封了一次，位点记的是续传光标。`end_pos` 为 0 的行仍留下。`.sealed.e<epoch>` 按封存、按它自己的 epoch 排序。`ReplayLocations` 与这些路径对齐，取值 `local` / `bucket` / `both`。`ReplayClient` 把 `source.flavor` 的 `mysql` 映射成 `mysqlbinlog` / `MySQL mysqlbinlog`，`mariadb` 映射成 `mariadb-binlog`。
- `segment_download.go`: `OpenTaskSegment` 按文件清单里的磁盘文件名打开本进程 `{data_dir}/{task_id}/{name}`。封存名和当前 `.open.e*` 都可读，读到打开时的字节长度。本地文件存在时只读本地，不读对象存储。本地没有时，封存行 `upload_state=UPLOADED` 且 `object_key` 非空，并且进程已配置上传，则按该 key 读对象，长度是打开对象时的大小。`LOCAL_ONLY`、`UPLOAD_FAILED`、空 key、open 分段、未配置上传仍是 `segment not found on this process`。名字含 `/`、`\` 或 `..` 返回 `invalid segment name`。不跟 catalog `file_path`。
- `replay_archive.go`: `OpenReplayArchive` 用 `ListFiles` 的同一窗口和 `SelectReplayFiles` 选出全部分封存分段和每个源序号最高的 open，再按 `OpenTaskSegment` 打开。`OpenPITRArchive` 打的是 `PITRReplay` 的同一组路径。成员名是 basename。全部能读完才留下一个 ustar 临时文件；任一打开或读取失败则删除临时文件并返回错误，不留下半个包。窗口为空时是没有成员的空 tar。
- `pitr.go`: `PITRReplay` 在整份清单上按 `SelectReplayFiles` 留下全部分封存分段和每个序号最高的 open，再按复制事件的事件头时间留下盖住 `[start, stop)` 的路径。格式描述和 previous-GTIDs 的时间戳是源文件创建时间，不拿来判断窗口。`stop` 必填且不含这个时刻，与 `mysqlbinlog --stop-datetime` 相同；`start` 可选且含这个时刻。`start` 与 `stop` 相同时空窗口，不选任何分段。`command` 在有客户端时以 `TZ=UTC` 加 `mysqlbinlog` 或 `mariadb-binlog` 开头。没有分段时 `paths` 为空、`command` 为空。打不开的分段返回 `OpenTaskSegment` 的错误。
- `scheduler_lifecycle.go`: 启停、`ClaimRunnableTasks`（先处理共享行上的停止，再无主 STARTING + 过期租约 + 自己名下空闲）、重试退避、FAILED 立刻放租约。cluster 下 runner 用的是 StartTask 刚 Acquire 的 owner/epoch；落库放开锁期间的 GetTask 或 store 同步不能把这份身份换成旧的 STOPPED / epoch 0。epoch 0 不会进入 dump。封存文件已存在、以及非瞬时的 checkpoint 写入，进入 FAILED 并放租约，不再连源。租约 epoch 不匹配时本进程停掉，只放自己的 epoch，不把共享行写成 FAILED 或 RETRY_BACKOFF。控制面没有本进程执行、且别的 worker 仍占着租约时，Stop 只把行写成 STOPPING，不写成 STOPPED。本进程仍在拉流时，认领或同步看到 STOPPING/STOPPED 会取消这次 dump，退出后再放租约并收成 STOPPED。租约已过期的 STOPPING 收成 STOPPED，不重新拉起。只存在于磁盘上的目录拒绝 start/stop。已 adopt 的目录在 start 时把 epoch 抬到现有 `.open.e*` 之上。
- `task_list.go`: 数字 id 排序、host/port/state 过滤（host 走 `SameSourceHost`）、内存分页、`SummarizeTaskStates` / `SummarizeTasksBySource` / `RunningRefs`，以及 `FailedUploadFiles` / `StartingUnownedTasks`，供 standalone 与测试 fake 复用。
- `scheduler_transitions.go`: 私有生命周期转换规则（状态、事件、错误、ownership 与持久化）。
- `errors.go`: 稳定操作员错误类型（永久的 1045 / log_bin off / 身份不可用 / `SEGMENT_NOT_ON_WORKER` / `SEALED_FILE_EXISTS` / `CHECKPOINT_WRITE_FAILED` / `EPOCH_NOT_ACQUIRED`，可重试的 `SOURCE_UNREACHABLE`，以及租约移交 `ErrLeaseHandoff`）。
- `scheduler_cluster_lease.go`: cluster lease 续租与降级/失租处理。
- `scheduler_observability.go`: 复制进度（含 at-tip）、checkpoint、事件/文件/运行历史查询。无 task store 时，剩余目录的 files 走磁盘扫描。`GetCheckpoint` 仍只读已存储的 checkpoint 行，不从磁盘编造。`ResumePosition` 返回下次 Start 会用的 file/pos；本地 open 分段有完整事件时用该事件，file+pos 与存储行一致时带上 `gtid_set`。epoch 大于 1 把位点回拨到 4 时 pos 已变，不带那个后来的 `gtid_set`。`RetentionBlockedFiles` 把 runner 的过期未上传封存文件计数交给 `/metrics`；runner 没有这个计数时返回空 map。
- `resume.go`: `NextResumePosition` 与 `ResolveTakeover`。adopt 的 `KeepLocalSegments` 不改用本地事件。没有本地事件时用 checkpoint；epoch 大于 1 且没有别的分段目录时把该 checkpoint 回拨到位置 4，并且不附带回拨前位点上的 `gtid_set`。`ResolveTakeover` 在本机 data dir 没有 `.open.eN` 完整事件时看 catalog `file_path`：目录可读就从那里的最后一个完整事件续；`state=OPEN` 且文件是裸源文件名（epoch 0）并在本机可读时，从该文件的最后一个完整事件续，而不是 `SEGMENT_NOT_ON_WORKER`。封存的裸名不是 open。本机 `.open.eN` 只有 magic，或只有 `end_log_pos` 为 0 的文件头、没有完整事件时，用已保存的 checkpoint 续，而不是 `Missing`。open 分段或未上传封存分段不可读则 `Missing` 点名该路径；checkpoint 已在 `UPLOADED` 封存对象内则不回拨。
- `scheduler_retry_upload.go`: 上传失败补偿。手动 `RetryFailedUploads` 与 worker 后台循环走同一条补传。`checksum mismatch` 会再上传；`upload_error` 以 `checksum verify failed:` 开头的未完成校验只再核对对象，不重新上传。后台只补本机存在的已封存 `UPLOAD_FAILED`；open 分段不上传。失败原因聚合也在这个文件。
- `sealed_upload.go`: 尽力上传的唯一调用方（`ApplySealedUpload`、`ObjectKey`）；首次封文件后与重试共用。核对为 `match` 时行是 `UPLOADED`。字节不同是 `mismatch`，行是 `UPLOAD_FAILED`，`upload_error` 为 `checksum mismatch`，且不失败调用方。对象 HEAD 失败时 `checksum` 留空，行是 `UPLOAD_FAILED`，`upload_error` 以 `checksum verify failed:` 开头。不能读对象的上传器仍是 `UPLOADED` 且 `checksum` 留空。空值不是已校验。
- `model.go`: 任务领域模型与状态定义（含复制进度 `AtTip`，以及不进 JSON 的 `KeepLocalSegments`）。
- 各 `*_test.go`: 状态机、租约、上传重试、事件等测试。
- `source_guard_test.go`: metadata/source 同端点拒绝策略的公开任务接口回归测试，覆盖 localhost、127/8、::1 与 IPv6 括号表示。
- `event_store_test.go` 中 fake store 为并发安全实现，用于 `-race` 校验稳定性。
- `scheduler_lock_test.go`: 一个任务的事件读取或插入被堵住时，另一个任务的 Stop、Start、续租和进度仍能返回。
- `start_epoch_race_test.go`: Start 正在把 STARTING 落库、store 还是上一轮 STOPPED 时，GetTask 和 store 同步不能让 runner 以 epoch 0 启动。跑起来的是刚 Acquire 的 owner/epoch，状态到 RUNNING，并且续租。

## Exports
- 任务 CRUD、启动停止、状态推进。
- `GetTask` 走 store 主键查询：store 说没有就是没有，其它错误原样失败，不退回内存里的旧主人/epoch 抄本，也不改扫磁盘。这次读若跨过了更新的一次落库，不把读到的旧行写回内存。返回给调用方的仍是 store 读到的那一行。没有 store 时先读内存名单；没有该 id 时，若 `{data_dir}/{id}` 仍有封存或 `.open.e<epoch>` 分段，返回只读身份（`STOPPED`，无 source）。`ListTasks` / `ListTasksPage` / dashboard 在没有 store 时把这些目录并进名单，Console 任务表因此能打开它们的文件。有 store 时不发现磁盘目录。`ListClusterObservation` 返回全库所有权抄本（有 store 读 `store.ListTasks`，失败原样返回；没有 store 时用含磁盘目录的同一份名单），不走任务观测过滤。`ListTasksPage` 返回 `{page, total}`；`DashboardCounters` 忽略 limit/offset，rollup store 用 `GROUP BY`，其它 store 一次过滤读取。`ClaimStartingTasks` 不扫描整表。新建 standalone 任务的数字 id 会跳过已有分段目录，避免复用该目录。
- `ClaimRunnableTasks`：开机和平时同一条「把该我跑的跑起来」（无主 STARTING + 过期租约 + 自己名下空闲 active）。同一轮先看共享行：本进程还在跑、行已经是 STOPPING 或 STOPPED，就取消拉流；租约已经不在的 STOPPING 收成 STOPPED。`ClaimStartingTasks` / `ClaimExpiredTasks` 仍可单独调用。`ClaimExpiredTasks` 对过期 STOPPING 只收尾，不调用 Start。cluster 下 store 必须实现 `ExpiredLeaseTaskLister`，否则返回 `ErrExpiredLeaseLookupNotAvailable`。
- `RetryFailedUploads` 只走失败文件查询（`ListFailedUploadBinlogFiles`）。file store 未实现该查询时返回 `ErrFailedUploadLookupNotAvailable`，不得用限量 `ListBinlogFiles` 冒充没有失败文件。内存 fake 用 `FailedUploadFiles` 做等价实现。签名仍是 `RetryFailedUploads(taskID string, limit int)`。本机没有该文件时，手动补传仍会尝试上传并把失败写回目录行。
- `RunBackgroundUploadRetry`：对象存储已配置时，worker 进程每 15 秒（调用方传入 `<=0` 时）对内存里的任务各补最多 100 条。file store 能数失败行且当前是 0 时，这一轮不再逐个任务查询。成功后的目录行与手动补传一样是 `UPLOADED` 且 `checksum` 为 `match`。`checksum mismatch` 会再上传；未完成的校验只再核对。open 分段不上传：目录里的 file_name 仍是源文件名，只要 `state=OPEN` 或路径里带 `.open.e` 就跳过。本机读不到的文件跳过，不改目录行，避免别的 worker 把已上传成功的行写回 `UPLOAD_FAILED`。没有新配置项。某一轮正在补某个任务时，手动调用该任务返回 `ErrUploadRetryInProgress`。上传失败不改变任务状态，也不停止拉流。
- `StartTask` 允许在 Acquire 成功后接管过期的 RUNNING/LEASE_DEGRADED；仍拒绝抢占未过期租约或本机仍在跑的任务。磁盘剩余目录返回 `ErrDiskBackupReadOnly`，不占租约。`AdoptDiskBackup` 把 source 和 `cluster_key` 接到同一 id 上，状态保持 `STOPPED`，不自动启动。没传 `start.mode` 时起点是最高分段末尾的 `FILE_POS`。显式 `start.mode` 覆盖它。有 task store 时不从磁盘 adopt。adopt 之后的 start 在单机 `MemoryLease` 上把 epoch 抬到目录里最大 `.open.e*` 之上，runner 用这个 epoch 开下一个分段。
- `Restore` 仍通过 `ListTasks()` 加载启动全量快照。
- `CreateTaskFromSpec`：整包校验后才 persist。
- `UpdateTask`：`RUNNING`、`STARTING`、`LEASE_DEGRADED`、`RETRY_BACKOFF` 时，改 source、start、storage 或 `cluster_key` 返回 `ErrTaskDumpConfigLocked`，已保存的配置不变。只改名字，或把这四项原样再提交，可以成功。`STOPPING`、`STOPPED`、`FAILED`、`CREATED` 仍可改；`STOPPING` 期间改的密码留给下一次 start。
- `FAILED` 立刻 `Release` 租约，其他 Worker 不必等 TTL；`RETRY_BACKOFF` 继续占着。可再次 `StartTask`（改完源库配置后）。`sealed file already exists` 记为 `SEALED_FILE_EXISTS` 并走这条 FAILED 路径，runner 不再连源。checkpoint 写入若不是瞬时元数据错误，记为 `CHECKPOINT_WRITE_FAILED`，同样 FAILED 并放租约。
- 封文件前发现租约 epoch 已经不是本进程时，这是移交而不是失败：本进程停掉 runner、取消续租，defer 只 `Release` 自己的 epoch。不把任务行写成 `FAILED` 或 `RETRY_BACKOFF`，避免盖住新主人。没有 store 时内存状态是 `STOPPED`，事件是 `TASK_LEASE_YIELDED`。有 store 时共享行保持新主人已经写上的内容。
- `NewMemoryLease`：无租约表时的进程内所有权门；`LeaseManager.Verify` 供封文件前验租。
- 仅 `SOURCE_UNREACHABLE` 连续失败最多重试 10 次；runner ready 会清零进程内连续失败计数，服务重启后重新计数，其他 retryable source code 不共享此封顶。瞬时 checkpoint/元数据错误（与 `meta.IsTransientMySQLError` 同一组文本：deadlock、connection reset、lock wait timeout、server has gone away、read-only 等）、`OBJECT_PURGE_FAILED`、以及没有已存 GTID 时的 MySQL 1236，仍是 `RETRY_BACKOFF` 且没有新的次数上限。元数据客户端每次调用里已经重试 5 次；外层继续重试是为了元数据库切换期间任务能自己回到 `RUNNING`，10 次就把备份打成 `FAILED` 并要人工再 Start。`OBJECT_PURGE_FAILED` 下次打开文件会再删对象，打成 `FAILED` 会在桶恢复前停掉拉流。1236 且没有 GTID 是 v0.5.36 的约定，留在 `RETRY_BACKOFF`。本地 append/flush 错误仍原样返回并重试：它们不是这次封存冲突或 checkpoint 写入，也没有和瞬时元数据同一套判定。
- 事件记录、文件元信息、上传补偿。`ListEvents` 和事件落库不再占着调度锁做元数据 I/O。一个任务的慢查询或慢插入不会挡住其它任务的 Stop、Start、续租和进度。同一任务的事件仍按写入顺序落库。
- `BinlogFile.State` 暴露 `OPEN/SEALED` 生命周期，运行中 `/files` 可见当前 segment。`BinlogFile.Checksum` 为 `match` 或 `mismatch`；对象 HEAD 未完成时为空，随文件清单返回。只有 `match` 写在 `UPLOADED` 行上。`mismatch` 和未完成的校验是 `UPLOAD_FAILED`。
- `ResumePosition`：`GET /api/tasks/{id}/checkpoint` 使用。本机有完整事件时与 `NextResumePosition` 相同。epoch 大于 1 且 file store 有目录行时，可读的 `file_path` 目录用那里的最后一个完整事件；本机 open 分段只有 magic 或 log_pos 0 的文件头时，返回已保存的 checkpoint，Start 从那里续。读不到未上传尾部时返回存储的 checkpoint，不回拨到位置 4，Start 再以 `SEGMENT_NOT_ON_WORKER` 失败。没有本地完整事件、也没有有效 checkpoint 时返回未命中。`GetCheckpoint` 不因磁盘文件而变成命中。epoch 大于 1 回拨到位置 4 时响应不带 `gtid_set`。
- `ListFiles`：file store 返回非空时保持元数据结果。封存文件在补传完成前是 `UPLOAD_FAILED`、`location` 为 `local`；补传校验为 `match` 之后是 `UPLOADED`，文件仍在磁盘上时 `location` 为 `both`。MySQL 目录按源序号升序（同序号封存在前，再按 epoch 升序），`limit` 保留序号最大的窗口，与磁盘扫描相同。file store 未配置，或该任务结果为空且设置了 `WithDataDir` 时，扫描 `{data_dir}/{task_id}` 的封存名与 `.open.e<epoch>`，按同一顺序。`file_name` 是源文件名，`file_path` 是磁盘路径。每行 `location` 为 `local`、`bucket` 或 `both`，不入库。store 查询失败不改扫磁盘。磁盘列表不编造 checkpoint。没有 task store 且内存无此 id 时，目录里仍有分段则用同一扫描；有 task store 时未知 id 仍是 `task not found`。`storage.local_retention_days` 与 `storage.bucket_retention_days` 省略时等于 `retention_days`；桶短于本地则创建和更新拒绝。
- `OpenTaskSegment`：下载用。名字必须是 `ListFiles` 同一份清单里的磁盘文件名（`file_path` 的 base；没有 `file_path` 时用 `file_name`）。本地 `{data_dir}/{task_id}/{name}` 存在时只读该文件，长度停在打开时的大小。任务是 RUNNING 或 STOPPED 都不会因为分段仍是 OPEN 而跳过。本地没有时，封存且 `UPLOADED`、`object_key` 非空、且已配置对象上传，才从对象存储读，长度是打开对象时的大小；否则 `segment not found on this process`。不跟 catalog `file_path`，也不为 open 分段向对象存储编造字节。未配置上传时与以前一样只认本地。
- `OpenReplayArchive`：把 `SelectReplayFiles` 在 `ListFiles(limit)` 窗口里留下的 basename 打成一个完整 ustar。字节规则与 `OpenTaskSegment` 相同。没有分段时返回空 tar。任一选中分段打不开或读不完时返回错误，并且不留下临时 tar。
- `PITRReplay` / `OpenPITRArchive`：`stop_datetime` 路径。整份清单里，每个序号的全部分封存分段和最高 open，再留下复制事件时间盖住窗口的那些路径。格式描述和 previous-GTIDs 的创建时间不算覆盖。`command` 是 UTC 时钟的 `--stop-datetime`，有开始时间时还有 `--start-datetime`。`start` 与 `stop` 相同时不选分段。空窗口的 tar 仍是空的。目录里同一源文件名可以有多行，靠 `epoch` 区分。
- `WithInternalCallTimeouts`：注入内部调用超时（read/write/lease/upload），用于 store/lease/uploader 依赖边界治理。
- `IsLoopbackHost`：只用字面规则识别 localhost、显式 loopback literal（127/8、::1）及有效 IPv6 括号表示，不做 DNS 解析，供 metadata guard 与源身份共享。
- `SameSourceHost`：回环别名是同一台源，非回环仍精确匹配；lookup 与任务观测 host 过滤共用。
- `WithMetadataSourceEndpoint`：注入 metadata TCP 端点，并在任务 create/update/configure/start 时拒绝同端点 source。
- 控制面 Stop 在没有本进程 dump、且行上的主人不是自己时停在 STOPPING，主人、epoch 和 source 配置都留着。Worker 认领循环（以及 store 同步、GetTask）看到 STOPPING 或 STOPPED 才取消本进程拉流。STOPPED 的落库用 store 里当时最新的配置，避免把控制面刚改的密码盖回旧值。下次 Start 若内存还停在 STOPPING，会先回读 store，行已经是 STOPPED 才能再派发。Stop 路径 lease release 使用独立超时上下文（不复用已取消 runner ctx）。run 退出在放下调度锁之前关闭 `done`，所以 StopTask 不会把已经结束的执行留在 `STOPPING`。持久化放开锁之后，如果这次写入的快照已经不是最新一代，会再写当时最新的快照，避免后完成的 `STOPPING` 覆盖先落库的 `STOPPED`。
- cluster fail-safe stop 一旦进入 `STOPPING/STOPPED`，会拒绝后续正常复制进度上报，避免失租后继续暴露健康运行态进度。
- runner/lease 的自动转换保持 best-effort 持久化语义，并统一记录持久化失败日志。

## Dependencies
- Upstream: `internal/api`, `internal/app`。
- Downstream: `internal/replication` runner, `internal/binlog` durable resume cursor, `internal/meta` stores。

## Update Rule
- 状态机规则、调度策略、外部接口变化时，更新本文件。

## Package Comment Rule

- Go 文件的软件包注释采用 `Package [package name] ...` 形式。
