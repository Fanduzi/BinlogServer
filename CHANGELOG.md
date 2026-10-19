# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

Maintenance rules:

- Add user-visible or operator-visible changes to `Unreleased` as they land.
- Before a release, review `Unreleased` and make sure schema, config, `sqlc`, and observability changes are clearly called out.
- When cutting a release, move `Unreleased` entries into a dated release section and start a fresh `Unreleased` block.

## [Unreleased]

### Fixed

- #436：`health.evaluator=on` 且迁移 000009 不在（在线 `migrate goto 7` 或 `8`，或进程就停在 schema 9 以下）时，健康评估停了，`/api/health` 仍是 `ok`，日志每轮一条 INFO。现在 `/api/health` 为 503 `degraded`，原因写明 `HEALTH_STORE_UNAVAILABLE: schema N < 9, run ./migrate up`（探不到版本或版本已经 ≥ 9 但表被删掉时，写明 `health tables are absent; run ./migrate up to 000009`）。同一条 CRITICAL finding（task `_cluster`，subject 为本节点）出现在 `/api/health` 和 `/api/findings`，写不进库时从内存提供。000009 回来之后，单机留下一对 OPEN/CLOSE；集群留下一行已关闭的 finding 和一条 `FINDING_RESOLVED`。`shadow` 只打一次 WARN，不把健康打成 degraded。`off` 不变。不提高 schema 地板，不加配置，不加迁移。只缺 000008、000009 仍在时，行为与 `SEAL_FACTS_ABSENT` 相同，不开这条。
- #441：源端 `FLUSH BINARY LOGS` 时，下一文件的 checkpoint 写在 `.open` 出现之前，中间还隔着封存上传，评估器会把这 1–2 秒看成 `CHECKPOINT_AHEAD`（CRITICAL）再自己关上。现在轮转把「写下一段 checkpoint」到「新文件已在目录里」放在与评估器读 checkpoint 和段列表同一把锁里，上传挪到打开文件之后。真的位点超前（链上没有的更后文件、越过封存末尾、GTID 超前）仍在一轮内打开。

## [v0.5.74] - 2026-10-19

发布说明：`docs/releases/v0.5.74.md`。可选迁移 `000010_binlog_files_retention_marker`（`binlog_files.retention_removed_at`，加法、可空）；schema 最高到 10，`minRequiredSchemaVersion` 仍为 6。没有新配置项。新 kind 由健康评估器产生，需要 `health.evaluator: on`（v0.5.x 默认 `off`）。

### Fixed

- #341：配了上传、且本地保留短于桶保留时，删掉仍在桶窗口内的本地文件不写 `RETENTION_REMOVED`，本地窗口为什么变短没有审计。现在先写这条事件，再写 `retention_removed_at`（迁移 000010，可选）。写失败打开 `RETENTION_RECORD_FAILED`，下一轮干净的保留结束关掉。带标记的行不再报 `ORPHAN_ROW`；对象被列举确认丢失时仍报 `SEGMENT_MISSING`（对象）。没有 000010 时事件照写，幂等只保持到进程重启。
- #353：在线把迁移 000008 降下去再升回来后，同一进程把新封存行的 `first_event_at` / `last_event_at` / `sha256` 留成 NULL，直到重启，也没有日志。现在每次封存和每轮健康评估都再探这些列；列不在且 schema ≥ 8（或本进程见过这些列）时一轮内打开 `SEAL_FACTS_ABSENT`，列回来就关闭并恢复写入。卡住窗口里已经写成 NULL 的旧行不回填。

### Added

- #434：`ORPHAN_ROW` 改为评估器管理（WARNING，Confirm）：带保留标记的行不报；前缀完整且标记行的对象不在时报 `SEGMENT_MISSING#object`（CRITICAL）；去掉对象存储后以 `CONFIG` 关闭。`SEAL_FACTS_ABSENT`（WARNING，一轮内打开）。`RETENTION_REMOVED` 事件、标记 UPDATE、封存事实 UPDATE 在有租约令牌时与 `task_leases` 行锁同一事务；没有令牌的 `AppendEvent` 仍是一条 INSERT、不重试。

### Upgrade notes

- 升级后第一轮保留会给本目录里「已上传、checksum match、本地已删、仍在桶窗口内」的行补写 `RETENTION_REMOVED`（有 000010 时同时写标记），包括旧二进制删掉的文件。没有 000010 时，每次重启后的第一轮会给这些行再写一条（重复的审计事件，不开 finding）。

### Known issues

- #436（P2）：在线回滚到 000009 以下后评估器只打 INFO、开不了任何 finding，`/api/health` 仍是 ok。
- #437（P3）：在线 up 到 000010 后，up 之前删掉的行要到重启才写标记，重启时多写一条事件；期间对象丢失报 `ORPHAN_ROW` 而不是 `SEGMENT_MISSING#object`。
- #438（P3）：保留按 mtime 删、补写按 `sealed_at` 判断，mtime 早于 `sealed_at` 的文件记录失败后可能永远补不上。
- #441（P3）：源端轮转时 CRITICAL `CHECKPOINT_AHEAD` 误报 1–2 秒（v0.5.73 起）。
- `/api/findings` 还没有读超时（P2-7）。

## [v0.5.73] - 2026-10-19

发布说明：`docs/releases/v0.5.73.md`。没有新迁移（schema 仍为 9，`minRequiredSchemaVersion` 仍为 6），没有新配置项。`/api/health` 的有界读在任何模式下都生效；`META_UNREACHABLE` 的 ping 与评估读两路观测需要 `health.evaluator: on`（v0.5.x 默认 `off`）。

### Fixed

- #427（P2）：meta 库被黑洞（连接挂住而不是被拒绝）时，所有节点的 `/api/health` 一直不返回，恢复后也没有任何 finding，中断无法审计。现在 `/api/health` 的全部 meta 读共用 1s 预算、同一时刻只有一个读，超时就用内存里的 finding 返回 503 `degraded`，原因含 `META_UNREACHABLE`；评估器管理的 finding 读不到时写明不可读，不再从汇总里消失。
- 健康存储写 `task_findings`、`finding_transitions`、`task_health` 的时间改为 UTC（`UTC_TIMESTAMP(6)`），以前用会话时区的 `NOW(6)`，与旧 finding 路径写入的 UTC 混在一起；评估器写入的 finding 行 `first_seen` 现在是 OPEN 的事实时间（第一次失败），`resolved_at` 是 CLOSE 的事实时间，不再是写入时刻。已有的旧行不迁移。
- 旧 finding 路径（`task_findings` 的 legacy upsert）：已解决的行再次打开时 `first_seen` 与 `count` 取新值，不再沿用上一次的首见时间、次数累加。集群里 `META_UNREACHABLE`（`_cluster`）每次中断另写一条（只写 RESOLVED）`task_events`（`task_id = _cluster`，`FINDING_RESOLVED META_UNREACHABLE <node>`，`event_time` = 恢复时刻，detail 写明起止），下一次中断复用同一行后上一次仍有记录；节点日志在打开、关闭时各一行。

### Added

- #427：新的 CRITICAL finding `META_UNREACHABLE`（task `_cluster`，subject 为节点 id，每个节点各报各的）：本节点对 meta 的 ping（每 1s，限时 1s，独立 goroutine，评估循环卡住也照发）、`/api/health` 读、评估器 sweep 的 lease 调用或评估读，从第一次失败算起连续失败满 10s 时打开，下一次应答时关闭；恢复时才满 10s 的中断（例如 12s 的暂停）也补记一次开与关，所以任何约 11s 以上的中断每个节点都会留下记录；只读（1290）、权限等 SQL 错误不算。standalone + meta 时一次中断在恢复后留下 OPEN（`fact_at` = 第一次失败）与 CLOSE（`fact_at` = 第一次应答）两条转移；集群里（P2-6 前 `_cluster` 走旧 finding 路径）留下一行 `first_seen` = 第一次失败、`resolved_at` = 第一次应答的已关闭 finding。需要 `health.evaluator: on` 才有 ping 与评估读两路观测；`/api/health` 的有界读在任何模式下都生效。

### Known issues

- P3：连续两次相隔很近的 meta 中断（15s / 间隔 10s / 15s）后，meta 恢复约 35s 时 `TASK_PERSIST_FAILED`（state LEASE_DEGRADED not written）、`EVENT_PERSIST_FAILED`、`TASK_LEASE_DEGRADED` 仍打开，`/api/health` 仍 degraded，任务已 RUNNING 并在写入；不确定之后是否自行关闭。单次中断后约 4s 内关闭。
- P3：standalone 下 `META_UNREACHABLE` 只写 `finding_transitions` 的 OPEN/CLOSE，不写 `FINDING_RESOLVED` 事件。
- `/api/findings` 还没有读超时（P2-7）。

## [v0.5.72] - 2026-10-19

发布说明：`docs/releases/v0.5.72.md`。新配置项：`health.no_progress_after_sec`（默认 `120`，`0` 为默认值，不可为负）。没有新迁移（schema 仍为 9，`minRequiredSchemaVersion` 仍为 6）。`TASK_FAILED`（无主任务）、`TASK_NO_PROGRESS` 和 `_health` lease 都由健康评估器产生，需要 `health.evaluator: on`（v0.5.x 默认 `off`）。

### Fixed

- #421 / #424（P1）：集群里没有节点评估无主任务：FAILED 会清掉任务行的 owner 并释放 lease，`TASK_FAILED` 从不打开，`task_health` 停在 HEALTHY，`/api/health` 仍是 ok，集群里的每个 FAILED 都是静默的。现在由 `_health` lease（`task_leases` 行 `_health`，TTL 3×sweep）选出一个节点（任何角色），用上一个任务 lease epoch 的代理栅栏只评估无主任务（FAILED、STOPPED、CREATED、未认领的 STARTING）的 `task.state`，FAILED 打开 CRITICAL `TASK_FAILED`。
- #421 / #424（P1）：旧 owner 醒来后把别的 epoch 写下的 FAILED 当成远端 Stop，无栅栏地改写成 `STOPPED / desired STOP / owner 空 / epoch 0`，看起来像有人手动停了任务。现在 FAILED 只能由操作员 Start 离开：正在跑的 run 看到别的 epoch 写的 FAILED 就停下且不写任何东西；`markStoppedLocked`、续约降级（`LEASE_DEGRADED`）和未完成 Stop 的收尾路径都不再覆盖 FAILED；任务行栅栏统一为 `tasks.CheckTaskRowFence`（lease 已释放时只接受在写入者 epoch 释放的）。部署文档写明集群运行任务的节点必须共享 `data_dir`。
- #421 / #424（P1）：集群节点各用独立 `data_dir` 时，接管节点能读到旧 owner 的绝对路径，就把分段续写到别人的目录里，自己的 `data_dir` 什么都不写，也不开 finding，`/api/health` 仍是 ok。现在接管只认本节点 `data_dir` 下的分段，不在其下的报 `SEGMENT_NOT_ON_WORKER`（任务 FAILED，打开 CRITICAL `TASK_FAILED`）。
- `data_dir` 配成相对路径时，接管把本节点自己的分段（目录记录的是绝对路径）误判为不在本节点，任务 FAILED（`SEGMENT_NOT_ON_WORKER`）。归属判断现在两边都解析成绝对路径并解析符号链接后再比较。
- #426（P1）：唯一的 worker 被 kill -9、只剩 control-plane 节点时，任务行一直停在 `RUNNING / 死 owner`，不开 finding，health ok。control-plane 节点的健康评估器 node id 为空，从不持有 `_health`；owner 非空的行也不算“无 owner”。现在 control-plane 节点用 `cluster.worker_id`（没配时用持久化的 `.node_id`）作为评估器身份，并代评 owner 的任务 lease 已过期的行，按 `TASK_NO_PROGRESS` 报出。多个 control-plane 节点共享 `data_dir` 时请各自配置 `cluster.worker_id`。
- #425（P1）：meta 库只读一段时间后，任务被静默写成 `STOPPED / desired STOP`、没有 owner、没有 finding，永不恢复；只读期间只有一个节点 degraded。原因：租约移交后旧 owner 仍把待重写的 `STOPPED / owner 空 / epoch 0` 行无栅栏地写回，新 owner 把这条旧行当成远端 Stop。现在移交时丢弃待重写行、重写带 `(owner, epoch)` 栅栏，新 owner 忽略 owner/epoch 不同的旧 `STOPPED / desired RUN` 行；`_health` lease 写失败在每个节点都报 `FINDINGS_PERSIST_FAILED`。
- 持有 lease 却在启动阶段（leftover dump KILL 失败）一直重试、从不开 run 的 owner 不会打开 `TASK_NO_PROGRESS`，只能等 `SOURCE_UNREACHABLE` 刚好在时限边上打开。现在进度账按持有的 epoch 记，在 claim + `no_progress_after_sec` + 一个 sweep 内打开。
- 任务行 `spec_revision` 为 0 时 FAILED 不会打开 `TASK_FAILED`（`FailedCurrent` 要求 `failed_spec_revision > 0`）；现在按 `failed_spec_revision == spec_revision` 判断。

### Added

- CRITICAL `TASK_NO_PROGRESS`（设计 0009 §24.9）：desired RUN 的任务，本进程的 run 在 `health.no_progress_after_sec` 内没有在自己的 `data_dir` 下推进位点、也没有确认追平源（心跳），或者没有任何 worker 在跑它（任务行停在 STARTING / STOPPED 等），就打开，`/api/health` 变为 degraded。作为“持有任务却不干活”这类问题的兜底，不依赖具体根因。

### Known issues

- #427（P2）：meta 库被黑洞（连接挂住、不返回 refused）时，所有节点的 `/api/health` 在 3 秒内都不响应，恢复后也不补记任何 finding。连接被拒绝时会正常 degraded。

## [v0.5.71] - 2026-10-17

发布说明：`docs/releases/v0.5.71.md`。没有新迁移，没有新配置项。

### Fixed

- #417（P1）：集群里 owner 被冻住超过 lease TTL、新 owner 接管后，旧 owner 醒来会继续往分段里追加，并写 checkpoint、目录行和上传结果，任务永久卡在 `STORAGE_INCONSISTENT`。现在每个写边界都带 lease `(owner, epoch)` 栅栏（ADR 0009 设计 §23）：本地 lease 时钟在追加、新建 open 文件、封存、上传之前拒绝过期的 epoch；checkpoint 和 `binlog_files` 的写在同一事务里对 `task_leases` 加锁校验。被拒的 run 作为租约移交退出，不写 FAILED。封存改名改用 `RENAME_NOREPLACE`。行为变化：meta 续约持续报错时，超过 `0.8 × lease_ttl_sec` 就停止落盘，不再写到 `lease_grace_sec`。
- #360（P3）：宿主机或 meta 停顿后，丢了 lease 的旧 owner 会把 STOPPING / STOPPED（`lease lost`，desired STOP）写进新 owner 刚认领、还没改写的任务行，新 owner 把它当成远端 Stop，任务停在 STOPPED、需要人工 Start。现在这条写在同一事务里对 `task_leases` 校验，lease 已有新持有者时不写（ADR 0009 P2-6a，§23.4.1）。#417 的数据面栅栏会让这个竞态在 meta 暂停超过 lease TTL 时必现，所以两者一起发布。
- #415（P2）：租约接管续写上一个 epoch 的 open 分段时直接改名，沿用同一个 inode。被 SIGSTOP 冻住的旧 owner 醒来后用旧句柄把缓冲的 Rotate 追加进新 owner 已封存的文件，封存文件比封存时多 42 字节，健康评估报 CRITICAL `SEGMENT_UNREADABLE`。现在新 epoch 把完整事件复制成自己的文件再删旧文件，旧句柄写不到续写或封存的分段。接管时多一次本地复制（最大一个 open 分段）。

### Changed

- meta 续约持续失败时，超过 `0.8 × lease_ttl_sec`（默认约 12 秒）就停止落盘，不再写到 `lease_grace_sec`（约 30 秒）。

### Known issues

- #421（P1）：集群节点各用独立 `data_dir` 时，接管节点持有 lease 却不写任何数据（目录存的是旧 owner 的绝对路径）；修复前请使用共享 `data_dir`。非 v0.5.70 回归。

## [v0.5.70] - 2026-10-17

发布说明：`docs/releases/v0.5.70.md`。没有新迁移；新增可选配置项 `upload.grace_sec`、`health.upload_late_critical_after_sec`、`health.object_list_interval_sec`、`health.object_list_limit`（及按任务覆盖）。

### Fixed

- #413（P1）：频繁轮转时评估器 `concurrent map read and map write` 崩溃；rehash 结果改在锁内拷贝（#414）。
- 损坏段原地修复后缓存的链事实未清，`GTID_HOLE` 不关闭；校验结果变化时清缓存（#412）。

### Added

- P2-5.2（#408）：`<segment>#object` 对象类别、`UPLOAD_LATE`（WARNING→24h CRITICAL）、`UPLOAD_UNVERIFIED`（两轮确认）；有界缓存的 S3 `ListObjects`，截断时健康 UNKNOWN；移除存储配置以 `CONFIG` 关闭。
- P2-5.3（#412）：`GTID_HOLE` 豁免（`POST`/`DELETE /api/alerts/waive`）、切换记录、`CHECKPOINT_AHEAD`。

### Known issues

- #417：集群段文件脑裂，任务停在 `STORAGE_INCONSISTENT`；修复进行中。非 v0.5.69 回归。
- #415：已封存段被追加 Rotate 后误报 `SEGMENT_UNREADABLE`；修复 PR #416 在 QA。非 v0.5.69 回归。

## [v0.5.69] - 2026-10-16

发布说明：`docs/releases/v0.5.69.md`。没有新迁移，没有新配置项。

### Fixed

- #405（P1）：standalone+meta 遗留 failback 升级修复后，续写的 `.open.e1` 与修复出的 `.sealed.e1` 同键，OPEN 行顶替 SEALED 行，`/replay` 无提示丢掉一段 stint，恢复报 `ERROR 1032`。Start 现在在磁盘上所有封存副本之上的 epoch 续写。
- #405：集群模式修复后只含 binlog 头的 `.open.eN` 被 I4 误判为副本，误报 `storage_alert`；只含头事件的打开文件不再算副本。

### Added

- P2-5.1：健康评估器（默认 `off`）接管 `SEGMENT_DAMAGED`、`CHAIN_INDEX_GAP`、`LEGACY_FAILBACK`、`SEGMENT_MISSING`（本地情形），新实例两轮确认后才打开；遗留段按已封存目录行大小比较。
- `scripts/e2e/upgrade-legacy-failback.sh` 支持 `FBUP_MODE=cluster|standalone`。

## [v0.5.68] - 2026-10-16

发布说明：`docs/releases/v0.5.68.md`。没有新迁移，没有新配置项。

### Fixed

- #401：任务 runner 归 `App.Run` 所有。runner 的 context 从 `App.Run` 派生（原为 `context.Background()`，关停时不取消，`Run` 返回后仍可能写 `data_dir`）。关停有界且有序：取消 → 等 runner（10s）→ 等后台 goroutine（含 5s 的 HTTP 优雅关闭）→ 补存欠下的任务记录 → 最后 flush finding（5s）。关停不改期望状态，下次启动自动恢复；关停中的 Start 返回 `ErrShuttingDown`。SIGINT/SIGTERM 后恢复默认信号处理（再发一次立即退出），30s 内未退出则以退出码 2 结束。修掉 `TestApp_SecondStandaloneOnSameDataDirFailsToStart` 偶发 "directory not empty" 的根因。

## [v0.5.67] - 2026-10-15

发布说明：`docs/releases/v0.5.67.md`。新配置项：`standalone.task_records`、`standalone.task_key_file`、`standalone.task_key_previous_file`、`health.evaluator`（默认 `off`）、`health.sweep_interval_sec`、`health.rehash_bytes_per_sec`、`health.unreachable_after_sec`、`health.unreachable_after_task_sec`、`health.signal_stale_after_sec`。新迁移 `000009`（只增不改、可选，`minRequiredSchemaVersion` 仍为 6）。存活探针请用 `/healthz`：`/api/health` 在有打开的 CRITICAL finding 时返回 503。

### Added

> 升级须知（#385 / ADR 0010）：standalone 升级后首次启动若未配置密钥，`TASK_RECORDS_FAILED/key_missing` 打开，用 `binlog-server task-key generate --out <path>` 生成并配置 `standalone.task_key_file`。`GET /api/health` 在有打开的 CRITICAL finding 时返回 503 `degraded`（`/healthz` 不变）；data_dir 写满时 finding 以内存为准继续由 `/api/findings` 返回，并出现合成的 CRITICAL `FINDINGS_PERSIST_FAILED`。同一 `data_dir` 只允许一个 standalone 进程：启动时对 `{data_dir}/.tasks.lock` 取 flock，取不到直接报错退出（NFS 上 flock 不可靠）。记录不可用时改动任务的请求返回 503 `TASK_RECORDS_UNAVAILABLE`（原为 500）；Stop/Delete 从不被拒绝，保存失败时只改内存、打开 `TASK_RECORDS_FAILED/write`（每 30s 重试保存），重启后任务会被恢复。环境变量中的密钥加载后即 unset。

- #385 / ADR 0010：standalone（无 `meta_dsn`）重启后任务自动恢复。任务配置（含源库密码）、期望状态、失败和删除墓碑以 AES-256-GCM 加密存于 `{data_dir}/.tasks.enc`（0600，原子写）。密钥来自 `BINLOG_SERVER_TASK_KEY` 或 `standalone.task_key_file`，`binlog-server task-key generate --out <path>` 生成；`standalone.task_key_previous_file` / `BINLOG_SERVER_TASK_KEY_PREVIOUS` 用于轮换。文件不存位置，续传位置总是从磁盘算；磁盘上一个段都没有的 LATEST 任务按 LATEST 续跑并记 `TASK_RESUMED_AT_LATEST` 事件（有段但读不出位置仍以 `RESUME_POSITION_UNKNOWN` 失败）；GTID 任务续传的 GTID 集合 = 起始集 ∪ `Ledger.SealedStored` ∪ 打开段尾（MySQL）。新配置 `standalone.task_records`（on 默认 / off）。
- 新 finding `TASK_RECORDS_FAILED`（CRITICAL，`_cluster`，subject `key_missing`/`key_invalid`/`wrong_key`/`corrupt`/`write`）。文件存在但无法读取时不恢复任务、拒绝改动任务的操作且不覆盖文件，目录报 `TASK_UNADOPTED`。没有记录的目录仍报 `TASK_UNADOPTED`（#388）。
- ADR 0009 健康评估器第二阶段 P2-1..P2-4（#375、#376、#377、#378、#380、#383），`health.evaluator` 默认 `off`：健康类型与 kind 注册表、迁移 000009 与健康存储（standalone 用 `.health.json`）、评估器与 rehasher、信号类 kind（socket 层源活性时钟驱动 `DUMP_STALLED` 与新增的 `SOURCE_UNREACHABLE`）。
- e2e `smoke-standalone-restart.sh`（MySQL 8.0、MariaDB 11.4），进入 `run-suite.sh` quick 档。

### Fixed

- Tip QA b4c52f4 P1：`health.evaluator=off`（默认）时 standalone 遗留目录（从 v0.5.66 升级的任务、未配置密钥时建的任务在加密钥重启后）不报任何 finding、`/api/health` 仍 200，备份静默停止。现在任何评估器模式下都打开 CRITICAL `TASK_UNADOPTED`（off/shadow 走旧 sink），`/api/health` 降级；adopt 后立即关闭，目录移出 data_dir 后 5s 内关闭。新增 `scripts/adopt-leftovers.sh` 按 JSON 映射批量 adopt（可选 start）。design 0009 §9 删去未实现的混版本自动退回 `shadow`。
- #381：被截断或比封存时短的封存段判为不可读（`SEGMENT_UNREADABLE`），`/window`、`/replay` 在它之前停下。
- #389：run 写 FAILED 与退出之间的 GET 不再把 FAILED 变成无声的 STOPPED。
- #391：切换源后续传集合包含所有封存段的 GTID（不再重复存储、误报 `SEGMENT_OVERLAP`）；checkpoint 与 OPEN 行在事务边界合并写入，不再每个事件写一次。
- standalone 下 `GET /api/findings` 现在列出评估器管理的 `_cluster` 结论；评估器建立前产生的信号 kind 失败会移交给评估器，不再停留在旧账本里不落盘。

## [v0.5.66] - 2026-10-15

### Fixed

- #370：任务目录里同一源文件存在第二个封存副本（如 `mysql-bin.000006` 与 `mysql-bin.000006.sealed.e3`，整份拷贝或全零）时，运行时现在会发现：任务带 `storage_alert`（STORAGE_INCONSISTENT，点名后一个副本），打开 CRITICAL `SEGMENT_OVERLAP` finding，`/window` 在两份之前断开并点名，`/replay` 只返回两份之前的分段并带 warning，不再把两份都交给还原（全零副本会让 `mysqlbinlog` 报 bad magic，整份副本会重复应用）。首尾相接的 epoch 不受影响。

## [v0.5.65] - 2026-10-14

See [docs/releases/release-notes-v0.5.65.md](docs/releases/release-notes-v0.5.65.md). Highlights: ADR 0009 phase 0/1 findings and invariants, `DUMP_STALLED`, `METADATA_WRITE_BLOCKED`, `SEGMENT_UNREADABLE`, seal facts (000008), dump lease fence (#355), MariaDB 11.4+ row events (#362), persist-first transitions and `TASK_TRANSITION_STALLED` (#364).

### Added

- #338 复测 2 修复：半同步任务的事件经 SynchronousEventHandler 送达，现在同样重置静默计时，不再每约 50s 误判停滞重连；dump 静默达到读超时即出 DUMP_STALLED 并立即结束本次运行（SOURCE_UNREACHABLE，任务离开 RUNNING，退避后重连），不再在已失联的路径上探测身份、重开 dump 或等待 Close/KILL，旧连接 id 由下一次启动先 KILL；METADATA_WRITE_BLOCKED 只认在元数据存储边界打上 `tasks.ErrMetadataStore` 标记的错误，源端 timeout/EOF/bad connection 不再误报；残留 Binlog Dump 的 KILL 重试不再因任务有活跃运行而暂停。

- 所有到源库的 SQL 连接（空闲位点探测 SHOW MASTER STATUS、server_uuid 探测、GTID 切换探测、KILL 残留 dump 线程）统一走 `connectSource`：拨号 5s、每次读/写 5s 超时；空闲探测不超过距读超时的剩余时间。源库被黑洞时，45s 左右出 DUMP_STALLED，约 50s 内重连，不再卡死 dump 循环。新增测试禁止绕过 `connectSource` 打开源库连接。

- Finding sink (ADR 0009, phase 0, #326). A failure that used to end in a log line now also leaves a persisted finding, a `FINDING_REPORTED` task event, and a metric. New in this change: a task state transition the metadata store did not take (`TASK_PERSIST_FAILED`, CRITICAL; the row is written again every 5s until it lands, then the finding resolves), a task event the store did not take (`EVENT_PERSIST_FAILED`), a retry budget that could not be read back (`EVENT_READ_FAILED`), a pending Binlog Dump KILL marker that was not written (`DUMP_CLEANUP_PERSIST_FAILED`), and a `data_dir` that cannot be listed (`DISK_LIST_FAILED`). `GET /api/tasks/{id}/findings` (open first, `all=1` adds resolved) and `GET /api/findings` (open, all tasks; task_id `_cluster` for process-wide ones). Metrics: `binlog_server_finding_open{task_id,kind,severity}` (always at least one series), `binlog_server_findings_reported_total{kind}`, `binlog_server_finding_sink_errors_total`, `binlog_server_finding_sink_pending`, `binlog_server_finding_store_active`.
- Upload and retention findings (ADR 0009, phase 0, #326): a background upload retry pass that fails (`RETRY_LOOP_FAILED`, subject `sync`/`task` CRITICAL, `count` WARNING), an upload result the store did not take (`UPLOAD_RECORD_FAILED`, WARNING), an object marked UPLOADED with no checksum match (`UPLOAD_UNVERIFIED`, WARNING), and a local-retention removal whose row or event was not written (`RETENTION_RECORD_FAILED`, WARNING). New gauge `binlog_server_upload_retry_pass_last_ts`.
- Dump and open-tail findings (ADR 0009, phase 0, #326): the Binlog Dump connection now has a 45s read timeout (three missed 15s heartbeats); a timed-out read reconnects as before and leaves `DUMP_STALLED` (WARNING, resolves on the next event). An unfinished trailing transaction that could not be cut on resume is `OPEN_TAIL_CUT_FAILED` (CRITICAL).
- Metric source and alert view findings (ADR 0009, phase 0, #326): a per-task progress, checkpoint or recovery-window read, or the worker heartbeat list, that fails during a scrape now exports `binlog_server_metric_source_up{task_id,source} 0` and leaves `METRIC_SOURCE_FAILED` (WARNING) instead of silently dropping the series. A RUNNING task whose progress read failed shows `ABNORMAL` / `PROGRESS_UNAVAILABLE` with the error instead of `NO_PROGRESS`. Dashboard rows carry `open_findings`, and the Console alert view now also lists rows with a `storage_alert` or open findings.
- Ignored-error guard (ADR 0009, phase 0, #326): `internal/lintguard` fails `go test ./...` on a new `_ = call()` or log-only `if err != nil` in `internal/{tasks,replication,binlog,upload}` unless it is in the reviewed allowlist with a reason.
- Stop records the intent before it cancels the dump: when the metadata store refuses the STOPPING row, Stop fails (`TASK_STOP_REJECTED` event) and the dump keeps running, instead of leaving a RUNNING row with no dump. A task whose last row write failed is never overwritten by an older store read, and `GET /api/tasks/{id}` returns its in-memory state until the retry lands. On schema 6, `binlog_server_finding_store_active` is 0 from startup; open findings are loaded at startup in meta mode too. `DISK_LIST_FAILED` is reported from every task-list path.
- `DUMP_STALLED` is decided by the silence (no event or heartbeat for the read timeout), not by the error type: go-mysql reports the timeout as a text-wrapped bad connection. It resolves on the first event of the next dump session, also after a restart. Closing a dead dump is bounded (5s; the old thread is KILLed with a 5s-bounded connection or left to the pending dump cleanup), so the reconnect happens about 45s after the source goes silent. `OPEN_TAIL_CUT_FAILED` carries the cut error and no longer claims the start resumes.
- `/metrics` answers when the task list cannot be read (metadata store down): `binlog_server_metric_source_up{task_id="_cluster",source="task_list"} 0`, `METRIC_SOURCE_FAILED`, and the finding gauges, instead of HTTP 500. A metric source finding shows in `finding_open` in the same scrape. Dashboard `open_findings` is counted per task in the store (`GROUP BY task_id`), not from the newest 1000 open findings.
- Stop records the intent before it cancels the dump: when the metadata store refuses the STOPPING row, Stop fails with "the stop is not applied and the task state is unchanged" (`TASK_STOP_REJECTED` event) instead of leaving a RUNNING row with no dump. A task whose last row write failed is never overwritten by an older store read, and `GET /api/tasks/{id}` returns its in-memory state until the retry lands. On schema 6, `binlog_server_finding_store_active` is 0 from startup; open findings are loaded at startup in meta mode too. `DISK_LIST_FAILED` is reported from every task-list path.
- Metadata store refusing writes (read-only, ADR 0009 §2.4), by design: the dump cannot record its progress, so it stops and retries with backoff rather than writing binlog the catalog does not know about. This is now explicit: a CRITICAL `METADATA_WRITE_BLOCKED` finding opens, the task shows `RETRY_BACKOFF`/`STARTING` (never `RUNNING`) with `last_error`, and the finding resolves once the task's `RUNNING` row is written again. The source keeps the binlog, so the retry catches up. Task events the event store refused are kept (up to 10000) and written again every 5s; their `EVENT_PERSIST_FAILED` finding resolves once they land.
- Migration `000007_task_findings` adds the `task_findings` table only. `minRequiredSchemaVersion` stays 6: on schema 6 the process starts, keeps findings in memory and in `/metrics`, logs once, and `binlog_server_finding_store_active` is 0. The floor moves to 7 in v0.6.0. Standalone (no `meta_dsn`) keeps findings in `{data_dir}/.findings.json`.
- Migration `000008_binlog_files_seal_facts` (ADR 0009 phase 1) adds nullable `first_event_at`, `last_event_at` and `sha256` to `binlog_files`; a seal writes the sealed file's first and last event-header time (UTC) and SHA-256. Additive and optional like `000007`: `minRequiredSchemaVersion` stays 6; on schema 6 or 7 the first write gets MySQL 1054, is skipped, and is not tried again until restart. Older rows keep NULL. Nothing reads the columns yet. The facts are read in one streaming pass (no extra memory per segment size), and the legacy failback repair writes fresh facts (or clears stale ones) on the rows it rewrites.

## [v0.5.64] - 2026-10-13

### Changed

- `/ui/` now serves the new React + shadcn/ui Console (#302; PR #315, #317, #319, #320, #321, #327, #328, #329, #332): Tasks/Alerts, the task drawer, the create/edit/adopt form, batch create, Overview, Sources and Workers, with `api_key` mode from the UI. Bookmarks and deep links under `/ui/` still work.

### Added

- `/ui-classic/` serves the previous Vue console for this one release (PR #332). It will be removed in a later release. Path-filtering proxies should allow it.

## [v0.5.63] - 2026-10-13

### Fixed

- `GET /api/tasks/{id}/replay/archive` (and its `stop_datetime` / `stop_gtid` / `start_gtid_set` variants) and the JSON `/replay` routes no longer return 500 (`lstat …open.eN: no such file`, `tar: write too long`) when a RUNNING task seals its open segment during rotation. The disk listing rereads until stable, members open against the inventory they were selected from, and an open member still missing after retries returns the existing 404 (#322, PR #325).
- Vue console: a failed `/auth/config` lookup no longer caches the Bearer fallback (only a 404 does), so `api_key` mode recovers without a reload; the keep-password hint shows only when editing (PR #320).

### Changed

- New React console in `console/`: batch create dialog with preview, and the PR #320 fixes (PR #321, #320, #302). Still not embedded: `/ui/` serves the Vue console.

## [v0.5.62] - 2026-10-13

### Changed

- **Compatibility:** every missing, malformed or wrong credential now returns HTTP 401 instead of 403 (PR #316). Before, a bearer header without the `Bearer ` prefix, a wrong token, or a wrong API key got 403. Bearer mode adds `WWW-Authenticate: Bearer realm="binlog-server"`. Update probes or scripts that expect 403.
- The `Bearer` scheme is matched case-insensitively, with spaces or tabs before the token (PR #318).

### Added

- `GET /auth/config` (no credential needed) returns `enabled`, `mode`, `header` and `scheme`, never the credential (PR #318).
- New React console in `console/` (shell, Tasks/Alerts, task detail drawer, create/edit/adopt dialog; PR #315, #317, #319, #302). Not embedded yet: `/ui/` still serves the Vue console.

### Fixed

- The Vue console works in `api_key` mode: it reads `/auth/config` and sends the configured header (PR #318).
- The task detail drawer shows the server's error text (e.g. `task not found`) for an unknown task (PR #318).

### Known behavior

- #322 (P2, also on v0.5.61): `/replay/archive` can return 500 `lstat …open.eN` while a rotation seals the open segment. A retry succeeds.

## [v0.5.61] - 2026-10-13

### Fixed

- `GET /api/tasks/{id}/replay` and `/replay/archive` reject a `limit` that is not a positive integer (0, negative, empty, decimal, non-numeric) with HTTP 400 `invalid limit: must be a positive integer` instead of silently returning the full set (#307). A `limit` sent with `stop_datetime`, `stop_gtid` or `start_gtid_set` is still ignored (point-in-time sets list every file they need), and the response now says so: `warning` in JSON, `X-Replay-Warning` on the archive. The API reference documents both. Server messages are English only (the API has no i18n); the Console does not send `limit`.

### Known behavior

- #312 (P3): a repeated `limit` uses only the first value (`limit=5&limit=abc` returns 200), and `+5`, ` 5` and int64 max are accepted.

## [v0.5.60] - 2026-10-13

### Fixed

- `GET /api/tasks/{id}/replay` and `/replay/archive` without `limit` no longer return only the newest 200 segment files with no warning (#305, PR #306, P1). They list every file of the retained chain, the same chain `/window` reads. An explicit `limit` window adds `omitted_files` and a `warning` (archive: `X-Replay-Omitted-Files`, `X-Replay-Warning`) when it leaves earlier files out. A `limit` of 0, negative, or non-numeric means no limit. The Console replay command and 下载回放集 no longer send `limit=80`.
- `smoke-worker-crash-recovery` no longer md5-compares the task's first file, which a LATEST start begins mid-binlog. It rotates once more and compares the newest two whole files.

### Changed

- Internal refactors with no behavior change: segment ledger (PR #300, ADR 0006), storage health (PR #301, ADR 0007), replication run phases (PR #304, ADR 0008).
- The `verify-binlog-server` agent skill moved to `.agents/skills` (PR #303).
- The repository history was rewritten on 2026-10-12. Commit hashes from before that date changed; release tags point at the rewritten commits.

## [v0.5.59] - 2026-10-09

### Fixed

- A legacy failback directory whose first-stint segment is kept only in object storage no longer restores with a silent gap (QA bbc2c88b). Before, Start repaired the names, `/window` reported `continuous: true` although its own `gtid_set` lacked the first stint's last transactions, `/replay` had no warning, and a restore applied with rc 0 while those transactions were missing. Now the plan notices the missing file (`detail` adds `not on disk: <file>`), and Start fetches `{prefix}/{cluster}/{identity}/<file>` back from object storage, checks that its transactions belong to that stint, and only then repairs; when the object is missing or holds other transactions Start refuses and changes nothing. As a safety net, on any task that switched servers, transactions a continued switch recorded as stored that no local segment holds, between stored ones, set `storage_alert` (`detail` `stored gtid hole: ...`, `missing_gtids`), and `/replay` and `/window` stop before the first segment after them. `/window` is never `continuous` when the stored GTIDs of one source have an interior gap the start set does not cover. Also: the background upload retry no longer uploads a legacy row under its old (wrong-server) key before the repair, so a refusal changes neither the catalog nor object storage; `STORAGE_REPAIRED` lists the old build's now unreferenced object keys after `orphaned_objects=` (they are not deleted); an unparseable switch `gtid_set` names a cut segment instead of an empty one, and a problem without a segment makes `/replay` return no files. Troubleshooting 5.4 documents fetching the file by hand, the orphaned objects, and that detection relies on the switch events (a meta restore from an older backup loses it). `scripts/e2e/upgrade-legacy-failback.sh` adds `FBUP_CASE=object-only` and `object-gone`.
- A task directory that v0.5.57 or 0a84917a wrote across a real failback (A→B→A) is no longer upgraded silently (QA 6c88ad5b P1). Those builds wrote A's third stint under A's first-stint names and pointed the catalog row at the later file, so `/files` and `/replay` left out the first-stint file with no warning (restore failed with ERROR 1032 or lost rows) and Start ran on and wrote a third copy of the name. Now reads set `storage_alert` (`STORAGE_INCONSISTENT`, `detail` `legacy failback: <files>`) and `/replay` and `/window` stop before the first affected file. Start, including the automatic resume after the upgrade, checks each file's transactions against the GTID sets recorded at each switch (repeated B→A events an old build recorded on restart are skipped). When every file fits exactly one stint it renames the later stint to `{identity}~{n}.`, lists the first-stint file in the catalog again for re-upload and checksum verification, fixes the checkpoint file name, writes `.source-chain` as A,B,A, and records `STORAGE_REPAIRED`, deleting nothing; a `.legacy-failback-repair` plan lets the next Start finish an interrupted repair. Otherwise Start fails with `STORAGE_INCONSISTENT` naming the files and changes nothing. Healthy directories are not read beyond the files from each re-entered server's switch point onward. Only files on local disk are checked. Troubleshooting 5.4 and the deployment upgrade notes describe how to recognize it and what to do after a refusal. `scripts/e2e/upgrade-legacy-failback.sh` reproduces it with real v0.5.57 and 0a84917a binaries.
- `GET /api/tasks/{id}/window` no longer reports a `gtid hole` at each failback boundary of a GTID task whose start set covers transactions inside the first file (a mysqldump seed; QA 6c88ad5b P2). The start set now counts as stored when a later server's file header is compared.
- A source-switch stop the task has run past stays resolved (QA 6c88ad5b P2). Before, a `resumed` task that was then stopped normally, or found `STOPPED` after a process restart, went back to `outcome` `stopped`, the stop banner returned, and `binlog_server_source_switchovers{outcome="stopped"}` went back to 1. A stop is now resolved once a `TASK_RUNNING` event follows its latest check. `docs/guide/reference/api.md` now describes the metric the same way as observability.md (one switch counted once, `stopped` only while the task sits on it).
- Failing back to an earlier server (A→B→A) after the old primary has caught up no longer writes A's new binlogs under the unprefixed names A used before the switch (#292 QA P1). Before, the task continued on A but kept `.source-chain` as A,B: A's new `mysql-bin.00000N` reused the first stint's names, `source_chain.current` stayed B, replay could list them before B's files, and every restart on A recorded another phantom B→A switch. Each `.source-chain` line is now one stint: A,B,A is three lines, and a server's n-th stint writes `{identity}~{n}.{binlog file}` (the third stint above is `A~2.`). `source_chain.servers[]` adds `prefix`, `current` is A, replay lists A, B, then `A~2`, and a restart on A records no switch. The switch event's `file` is the source binlog name without the disk prefix. `GET /api/tasks/{id}/files` adds optional `source_server` (1-based stint). No new config key, no schema migration, schema stays 6.
- A task stopped by a source switch in the middle of a dump can be started again (found while adding the failback smoke). Before, the stop sealed the open segment while the checkpoint still named it, so every later Start failed with `SEGMENT_NOT_ON_WORKER: ... is not on this worker`: the task could not re-check the switch, resume when the VIP pointed back, or continue after the old primary caught up. A stop now leaves the open segment open, as a stop found at connect time already did. When a later Start continues on another server, the previous server's open segment is sealed under that server first (moved onto the current epoch); before, a switch found at connect time left it open and the new epoch's cleanup deleted it, losing that server's last transactions.
- `GET /api/tasks/{id}/window` and the Console window no longer report a false `gtid hole` where the next server's first file header lacks transactions the backup already holds from the previous server (#292 QA P2). Across a server switch the header is compared with the whole stored set; a real hole is still reported.
- Starting a task that is stopped on a source switch again no longer adds another switch to `source_chain` and to `binlog_server_source_switchovers{outcome="stopped"}` (#292 QA P2): identical consecutive switches count once. When the task runs again (VIP pointed back), `source_chain.outcome` is `resumed`, the stop carries `resolved: true`, the Console drops the stop banner, and `outcome="stopped"` returns to 0. A stop followed by a continued switch is also `resolved`.

### Known issues

- After Start refuses a legacy failback directory because the first-stint object cannot be read, `storage_alert.detail` still says `(Start repairs)`; `last_error` is correct (QA 297 P3).
- Troubleshooting 5.4 "manual fetch": the old primary's same-named binlog must first be cut at the A→B switch position (`head -c <pos>`), or Start refuses it (QA 297 P3).
- `/replay` still lists a middle segment whose local file and object are both gone, without a `warning`; `/window` reports `segment is not readable`, the download returns 404, and `mysqlbinlog` stops there (QA 297 P3).

## [v0.5.58] - 2026-10-09

### Fixed

- A GTID task no longer loses transactions silently after its dump connection drops (PR #293). This affected v0.5.57. go-mysql in raw mode does not advance its GTID set, so its built-in reconnect re-dumped from the start set of that `StartSyncGTID`, not from the stored set. The Rotate at the end of the previous file was appended to the current segment and the resume position jumped to that file's length. The task first failed with `SEALED_FILE_EXISTS`; after another Start it showed `RUNNING` with lag 0 while new transactions were dropped as already written, and the checkpoint `gtid_set` and `/window` could still look continuous. A proxy or idle-timeout disconnect, a network blip, or a `KILL` of the dump thread was enough; no source switch was needed. go-mysql's auto-reconnect is now off, and this process starts the dump again from the stored GTID set (or the stored file and position in file/pos mode). A transaction is skipped only when its GTID is already stored, and a GTID that is not on disk is never written into the checkpoint. A directory with a hard trace of the damage (a Rotate inside a segment that points to itself or an earlier file, or an `end_log_pos` that goes backward inside a segment) gets a computed `storage_alert` on the task list, the dashboard, and `GET /api/tasks/{id}`, and Start refuses it with `STORAGE_INCONSISTENT`. **After upgrading from v0.5.57:** do not Start a task that shows `storage_alert`, do not restore past its `valid_segments`, and create a new GTID task from `restart_gtid_set` before the source purges the missing transactions; see `docs/guide/admin/troubleshooting.md`. `smoke-gtid-redump` is in the quick e2e profile. No schema migration. Schema stays 6.
- A GTID task no longer fails with a false `STREAM_REGRESSION` after a dump reconnect, a process restart, or an upgrade (QA evidence-293). This only affected builds of unreleased main after PR #293 (for example 0a84917a). v0.5.57 has no `STREAM_REGRESSION` code, so a task on v0.5.57 never failed this way and a direct upgrade from v0.5.57 needs no action. On those main builds the checkpoint is written after the segment is flushed, so it could lag the open segment by the last transactions, and a reconnect could land inside a transaction. The next GTID then looked new to the stored set and the task went `FAILED` with `last_error` `STREAM_REGRESSION: gtid <uuid>:N is ahead of stored set <set>`, although nothing was lost. Now every Start of a GTID task first reads the open segment's tail: the GTIDs of the complete transactions already in it are added to the checkpoint's set, and an unfinished trailing transaction is cut where the last complete one ended and fetched again whole (when the cut is not clean, its GTID is carried as in progress instead). The server log has one `open tail reconciled ... changed=<true|false> checkpoint_gtid=... resume_gtid=...` line per resume, and an `open tail cut unfinished transaction` line when a tail was cut. A dropped dump (a `KILL` of the dump thread, a proxy or idle-timeout disconnect, a `FLUSH` plus `KILL`) ends the run with a runner error, and the scheduler retries it as a full Start, so every such reconnect runs this reconciliation; QA saw only this path. Only an error classified as `SOURCE_UNREACHABLE` inside a run reopens the dump in process (`dump reconnect` in the log); that reopen resumes by the flushed file/pos and keeps the executed GTID set as the MySQL 1236 fallback. `STREAM_REGRESSION` is now only a rotate to the open or an older file, or a transaction event behind the stored position. A GTID hole, out-of-order GTIDs (a replica with parallel workers and `replica_preserve_commit_order=OFF`, or a manual `GTID_NEXT`), and several interleaved UUIDs never fail a task. A file header the source re-sends behind the cursor is ignored. XA `PREPARE` and a compressed `TRANSACTION_PAYLOAD` count as commits. **What a DBA does after upgrading:** tasks that went `FAILED` with `STREAM_REGRESSION: gtid ... is ahead of stored set ...` on such a main build are not damaged; `POST /api/tasks/{id}/start` (or Start in the Console) and they continue. Do not start a task that shows `storage_alert` / `STORAGE_INCONSISTENT`; that is real damage. Adopted directories (`KeepLocalSegments`) and damaged directories are never cut. No new config key. No schema migration. Schema stays 6.
- `GET /api/tasks/{id}/checkpoint` shows `gtid_set` for a GTID task whose stored row lags the open segment: the stored set (or the start set before the first row) plus the complete transactions already in that segment, which is the set Start resumes from. When Start would cut an unfinished trailing transaction, `pos` is that cut. The GET never changes the file. `updated_at` is omitted until a checkpoint row exists instead of `0001-01-01T00:00:00Z`. A task with no checkpoint row and no local segment to resume from (one that was never started) still answers 404 `checkpoint not found`; Start begins at the task's start position.

### Added

- `storage_alert` names what was damaged and what still restores. New optional fields: `segment` (first damaged segment), `detail`, `missing_gtids` (MySQL GTIDs the checkpoint lists that the start set and every segment lack), `valid_segments` (segments before the damage, still good to restore), and `restart_gtid_set` (start a new task in GTID mode from this set to continue right after those segments and fetch the missing transactions again, before the source purges them). `message` says the same in one sentence and still says to create a new task from a GTID the source still has. `GET /api/tasks/{id}/replay`, `stop_datetime`, `stop_gtid`, `start_gtid_set`, and `/replay/archive` stop before the damaged segment: it, every other copy of that binlog index, and every later segment are left out, and the JSON responses add `warning`. The Console shows the alert line by line in Chinese or English and warns in the replay section. `GET /api/tasks/{id}/window` and the Console recoverable window use the same cutoff: they cover only the segments before the damage, are not `continuous`, and add one break naming the damaged segment and every later segment left out. A `FAILED` task whose `last_error` still holds an older build's `STORAGE_INCONSISTENT` text shows the current `storage_alert` message there. Undamaged tasks are unchanged.
- A DBA can see a VIP source switch without reading the raw event list (no new config key, no schema migration, schema stays 6). `GET /api/tasks/{id}` adds optional `source_chain` when `{data_dir}/{task_id}/.source-chain` or a `SOURCE_SWITCHOVER` event exists. It lists each server identity, which one is current, and each switch (`old`, `new`, `file`, `pos`, `gtid_set`, `continued`, and a stop `reason` of `no_gtid`, `missing_transactions`, `purged`, `mariadb`, or `gtid_unreadable`). Existing task fields stay at the top level. The field is omitted only for a task that never reached its source; any task that has run has a chain. `GET /api/tasks/{id}/files` adds optional `source_identity` on each row. It is computed, not stored. The first identity owns unprefixed names. A later identity owns `{identity}.{binlog file}`. `GET /metrics` adds gauge `binlog_server_source_switchovers{task_id,outcome}` where `outcome` is `continued` or `stopped`. The value is how many stored `SOURCE_SWITCHOVER` events had that outcome. Both series are present for a task that was read, including 0. The Console task view shows the chain, marks a continued `RUNNING` task as still copying, and for a stopped switch states the reason and the next step: start a new task against the new primary, keep this backup, and do not delete `.source-chain`. The files table names the server that wrote each file. Switch, window, replay, and `stop_gtid` / `stop_datetime` behavior is unchanged. `smoke-source-switchover` also checks this surface. It stays opt-in and is not in the quick profile.

### Security

- Build with Go 1.26.9 and `golang.org/x/net` v0.60.0. govulncheck reported GO-2026-6599, GO-2026-6600, GO-2026-6605, GO-2026-6607, GO-2026-6608, GO-2026-6609, and GO-2026-6613 in the Go 1.26.7 standard library (`net/http`, `crypto/tls`, `mime/multipart`, `html/template`) and GO-2026-6603, GO-2026-6610, GO-2026-6611, GO-2026-6612, and GO-2026-6617 in `golang.org/x/net` v0.58.0. Building from source requires Go 1.26.9+. No other module changes.

## [v0.5.57] - 2026-10-07

### Added

- A task whose host:port is a VIP, DNS name, or proxy continues when that address reaches a different MySQL `@@server_uuid`, and only when this backup already has an executed GTID set and the new primary still holds every transaction already stored (PR #289). The open segment from the old server is sealed. Later files are named `{server_uuid}.{binlog file}`. The first server's files stay unprefixed. The object key stays `prefix/cluster_key/<writer identity>/<file name>`, and the sealed old segment uses the old identity. `GET /api/tasks/{id}/events` records one `SOURCE_SWITCHOVER`. On the continue path `message` is `source switched from <old> to <new> at <where>; continuing from the executed GTID set`. `detail` is `old=<old> new=<new> gtid_set=<set> file=<file> pos=<pos>`. The task stays `RUNNING`. Otherwise the task is `FAILED` and does not enter `RETRY_BACKOFF`. `last_error` is `SOURCE_SWITCHOVER: source switched from <old> to <new>. <why>. Start a new task against the new primary and keep this backup. This task will not mix the two servers.` `<why>` is no GTID set, the new primary is missing transactions this backup already has, the new primary has purged transactions this backup does not have yet, or MariaDB has no usable GTID path. That stop covers a stored `LATEST` backup and a `FILE_POS` backup with no stored GTID, including MySQL 1236 for the old file name. A `FILE_POS` backup that already has an executed GTID set continues, including when that 1236 is the first answer. The same `@@server_uuid` with no stored GTID still uses the purged-binlog text. Starting the task again runs the same check and does not mix the files. `GET /api/tasks/{id}/window` reports whether the chain is continuous. A missing source file is a break only inside one server. A MySQL GTID hole across the switch is still a break. `GET /api/tasks/{id}/files`, `/replay`, and `/replay/archive` list the older server first. `stop_datetime` and `stop_gtid` use that order. A GTID redump of an earlier file on the server already being written does not open a second copy. `{data_dir}/{task_id}/.source-chain` records one identity per line. The first line owns unprefixed names. The first start after upgrade records the server reached at that moment, so confirm the address still reaches the server that wrote those files before upgrading. `smoke-source-switchover` is opt-in and is not in the quick profile. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. `migrations/` is still `000001` through `000006`. There is no `000007`. ADR 0005 is unchanged. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on `62f2251ad2bb70458d05f5524f78f075b9feabaf` (evidence-289). That run did not open a new issue. The v0.5.56 recoverable window still holds. The v0.5.55 `start_gtid_set` rule still holds. The v0.5.54 `stop_gtid` rule still holds. The v0.5.53 drop of `uk_task_file_epoch` still holds. The v0.5.52 `.takeover-*` cleanup still holds. The v0.5.51 unknown-`end_pos` rule still holds. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. #224 was fixed in v0.5.52.

### Fixed

- `smoke-gtid-pitr` and `smoke-gtid-executed` wait for TCP login and an empty `gtid_executed` after `RESET MASTER` (`RESET BINARY LOGS AND GTIDS` on 8.4) before the restore server accepts replay (PR #288). The official image's temporary init server accepts a socket `SELECT 1` and then shuts down, so the following `RESET` returned 2002. This is CI only. It is not a product behavior change.

## [v0.5.56] - 2026-10-07

### Added

- A task's retained chain can be read before a point-in-time stop is chosen (PR #285). `GET /api/tasks/{id}/window` returns the earliest and latest restorable UTC event-header times. Each time is JSON `null` when the chain has no timed event. `continuous` is true when `breaks` is empty. `breaks` is always an array. An empty chain is continuous, with both times `null` and `breaks` `[]`. A missing task is HTTP 404 `task not found`. A segment whose bytes are not on this process is a break. Any other read failure is HTTP 500. The chain is the replay inventory: every sealed segment, plus the highest open epoch of each source index. A local file wins. A sealed `UPLOADED` object that exists only in the bucket stays in the window. Each break has `files` and a stable English `reason`: `missing source file <names> between <left> and <right>`, `checksum mismatch`, `segment is not durable off-host (UPLOAD_FAILED)` or `segment is not durable off-host (LOCAL_ONLY)` when object storage is configured and the sealed checksum is not `mismatch`, `segment is not readable`, and, for MySQL, `gtid hole between <left> and <right>`. A checksum mismatch is not also reported as not durable. An open segment is not a durability break. With no object storage configured, `LOCAL_ONLY` is not a durability break. MariaDB does not report a GTID hole. `gtid_set`, when present, is the union of stored MySQL `GTID_EVENT` rows. It is omitted for MariaDB and when the chain has no GTID event. A contiguous run is `<server_uuid>:1-3`. A gap inside one UUID uses the go-mysql form `<server_uuid>:30:32`. Sequences before the first stored event of a mid-file start stay out of the set and are not a hole. One missing MySQL file can be both a missing-source-file break and a GTID-hole break. `GET /metrics` adds `binlog_server_recovery_breaks{task_id}` and `binlog_server_recovery_earliest_age_seconds{task_id}`. Each scrape computes them by reading the chain. The age gauge is seconds from `earliest` to the scrape, and a task with no `earliest` has no age series. Console task detail shows earliest → latest and a warning when the chain is broken, in zh-CN and en. Break sentences stay API English. `smoke-recovery-window` is opt-in (`./scripts/e2e/run-suite.sh --scenarios smoke-recovery-window`) and is not in the quick profile. A stop one second after `latest` on a continuous chain still returns a replay command. A stop at or before `earliest` is still an empty command. A break does not change replay `paths`. Replay, retention, and upload are unchanged. The route is on the embedded Swagger page. Swagger was regenerated. No new config key. No schema migration. The request scans segment bytes and adds no column. Schema stays 6. `minRequiredSchemaVersion` stays 6. `migrations/` is still `000001` through `000006`. There is no `000007`. ADR 0005 is unchanged. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on `72d7d6b7bd9e8ed35d040e0e4ecca10e4fc00195` (evidence-285). That run did not open a new issue. The v0.5.55 `start_gtid_set` rule still holds. The v0.5.54 `stop_gtid` rule still holds. The v0.5.53 drop of `uk_task_file_epoch` still holds. The v0.5.52 `.takeover-*` cleanup still holds. The v0.5.51 unknown-`end_pos` rule still holds. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. #224 was fixed in v0.5.52.

## [v0.5.55] - 2026-10-07

### Added

- Point-in-time restore can roll forward from the executed GTID set of a restored full backup (PR #282). `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` accept `start_gtid_set` together with the existing `stop_gtid` or `stop_datetime`. The set is `@@gtid_executed`, mysqldump `GTID_PURGED`, or the GTID set in `xtrabackup_binlog_info`. Spaces and newlines are removed. A single `<server_uuid>:<seq>` is a one-transaction set. A segment whose transactions before the stop are already in the set is omitted from `paths` and from the tar. `command` is `TZ=UTC mysqlbinlog` with `--exclude-gtids` for the canonical set, then the existing stop flag. A canonical set that contains a comma is single-quoted. `--stop-position` is omitted when the file that contains `stop_gtid` has nothing new before that stop. There is no `--start-datetime`. `start_gtid_set` and a non-empty `start_datetime` cannot both be set. That body is `start_gtid_set and start_datetime cannot both be set`. An empty `start_datetime` is omitted. Neither stop is `stop_datetime or stop_gtid is required`. An empty value, whitespace only, MariaDB `domain-server-seq`, `<server_uuid>:0`, `<server_uuid>:0-5`, or any other unparseable value is `invalid start_gtid_set`. Any flavor other than mysql is `start_gtid_set is not supported for this flavor`. A set that does not cover the transactions before the first segment that would be applied, including sequences a GTID dump skipped between previous-GTIDs and the first copied event, is `start_gtid_set has a gap before this task's backed-up range`. A `stop_gtid` that is not a `GTID_EVENT` in this task is still `stop_gtid is not in this task's backed-up range`, and that sentence wins over the gap sentence. When every transaction up to the stop is already in the set, the response is HTTP 200 with `paths: []`, an empty `command`, and `note` `every transaction up to the stop is already in start_gtid_set`. When the stop is the first transaction, `paths` and `command` are empty and `note` is omitted. `limit` is ignored. `stop_datetime` and `stop_gtid` still cannot both be set, and that check runs first. A sealed `UPLOADED` object that exists only in the bucket uses the same selection. The Console point-in-time form has an executed GTID field,「已执行 GTID」. Docs are in `docs/guide/reference/api.md` section 5.9. Swagger was regenerated. No new config key. No schema migration. The request scans segment bytes and adds no column. Schema stays 6. `minRequiredSchemaVersion` stays 6. `migrations/` is still `000001` through `000006`. There is no `000007`. ADR 0005 is unchanged. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on `9f1d9b8651971d1ed4196171f2d51938cd0f609d`. The v0.5.54 `stop_gtid` rule still holds. The v0.5.53 drop of `uk_task_file_epoch` still holds. The v0.5.52 `.takeover-*` cleanup still holds. The v0.5.51 unknown-`end_pos` rule still holds. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. #224 was fixed in v0.5.52.

## [v0.5.54] - 2026-10-07

### Added

- Point-in-time restore can stop immediately before one MySQL transaction by GTID (PR #279). `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` accept `stop_gtid=<server_uuid>:<seq>`. The response lists segments through the one that contains that `GTID_EVENT`. `command` is `TZ=UTC mysqlbinlog` with `--stop-position` at that event's starting byte in the last file. Everything before that byte applies. That transaction and everything after it do not. `stop-position` is the event's starting byte, not the header `end_log_pos`. A mid-file copy prepends a format description, so those two numbers differ. `mysqlbinlog` applies `--stop-position` only to the last file. Optional `start_datetime` is still an inclusive lower bound, including the same second as the GTID event. A later start is plain-text 400 `start_datetime is after stop_gtid`. `stop_datetime` and `stop_gtid` cannot both be set. That body is `stop_datetime and stop_gtid cannot both be set`. `limit` is unchanged when neither stop is set, and that response still has no `command`. `limit` is ignored when either stop is set. The UUID match ignores case. The sequence is a positive integer with no leading zero, not a range, and not a GTID set. An empty or unparseable value is `invalid stop_gtid`. A UUID or sequence that is not a `GTID_EVENT` in this task is `stop_gtid is not in this task's backed-up range`. Previous-GTIDs is not that transaction. MariaDB `domain-server-seq` on flavor `mysql` is `invalid stop_gtid`. Any other flavor is `stop_gtid is not supported for this flavor`. The archive members are those basenames. The stop position stays in the JSON `command`. The Console point-in-time form has a stop GTID field. Docs are in `docs/guide/reference/api.md` section 5.8. Swagger was regenerated. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. `migrations/` is still `000001` through `000006`. There is no `000007`. ADR 0005 is unchanged. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on `8646266e1b9ac99abd620a9e9df241dd51899d5f`. That run did not open a new issue. The v0.5.53 drop of `uk_task_file_epoch` still holds. The v0.5.52 `.takeover-*` cleanup still holds. The v0.5.51 unknown-`end_pos` rule still holds. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. #224 was fixed in v0.5.52.

## [v0.5.53] - 2026-10-07

### Changed

- Migration `000006_drop_task_file_epoch_key` drops `binlog_files.uk_task_file_epoch` (PR #276, ADR 0005 step 9 of 9). A segment is only `(task_id, source_file, epoch)`, which is `uk_task_source_epoch`. The `file_name` column stays. The SQL does not delete rows. `minRequiredSchemaVersion` is 6. v0.5.53 does not start on schema 5. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. v0.5.51 and v0.5.52 still require `uk_task_file_epoch`. On schema 6 they refuse to start. The log contains `missing index` and `uk_task_file_epoch`. Stop those processes before `./migrate up`. The floor stays v0.5.51, the first release that ships both step 6 and step 7. v0.5.50 shipped step 6 only. This release is the first that ships step 9. A second insert of the same `(task_id, source_file, epoch)` fails. `./migrate down --steps 1` from schema 6 adds `uk_task_file_epoch` back and does not delete rows. No new config key. `migrations/` is `000001` through `000006`. Tip dogfood by BinlogServerQA passed on `59a5e1e7efa55393e5a1227ac48b9f625a622d68`. That run did not open a new issue. The v0.5.52 `.takeover-*` cleanup still holds. The v0.5.51 unknown-`end_pos` rule still holds. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. #224 was fixed in v0.5.52.

## [v0.5.52] - 2026-10-07

### Fixed

- A worker removes a `.takeover-*` temp left when a process crashes while takeover is downloading an `UPLOADED` object (#224, PR #273, ADR 0005 step 8 of 9). The sweep runs at worker startup, before tasks are claimed, in every task directory under the data dir. `materializeUploaded` runs it again in that task directory before the next download and before `CreateTemp`. A temp this process is still writing stays. A crashed leftover beside that download is removed. A sealed segment, an open segment, the active file, `task-<id>.binlog`, `notes.txt`, and every other name stay. A file in the data dir itself stays, including `.worker-id`. Another task's `.takeover-*` stays until a worker starts or that task materializes. Only a regular file is removed. The log line is `takeover: removed stale temp <name> task=<id> size=<bytes>`. A download that fails while the process is still alive still removes the temp it created. `kill -9` still skips that removal. The next worker start removes the file. Retention still skips every name `ClassifySegment` rejects, including `.takeover-*`. A control-plane-only process does not run this sweep. A delete error at startup exits before the process listens. The message contains `remove stale takeover temps`. Tip dogfood by BinlogServerQA passed on `dbf9f5c62a08a2868a1b6d2545b2d7cdac7bb7ab`. That run did not open a new issue. This release is ADR 0005 step 8. Step 9, dropping `uk_task_file_epoch`, is still next work. No new config key. No schema migration. `minRequiredSchemaVersion` stays 5. `migrations/` is still `000001` through `000005`. There is no `000006`. `uk_task_file_epoch` stays. `file_name` is unchanged. The v0.5.51 unknown-`end_pos` rule still holds. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. #224 is fixed in this release.

## [v0.5.49] - 2026-10-07

### Fixed

- After failover, a sealed segment of a source file the catalog already had stores `start_pos` and `end_pos` from the events in that file (#189, PR #264). `start_pos` is the first event position. `end_pos` is the last event end log position. Before this, the row could be `end_pos` 0, or `start_pos` and `end_pos` both equal to the resume cursor, while the file held events from position 4 through the last event. This includes epoch 1, a third epoch of the same name, and a segment sealed after a file/pos resume gets MySQL 1236 and continues from the stored GTID. A same-file artificial Rotate with position 0 no longer clears the position being copied. An already `UPLOADED` row is not rewritten. A segment sealed by this binary still does not create `task-<id>.binlog`, and `mysqlbinlog --verify-binlog-checksum` on that segment still exits 0 (#205, already fixed in v0.5.48). Tip dogfood by BinlogServerQA passed on `e690c438`. That run did not open a new issue. This release is not an ADR 0005 step. Steps 6–9 are still next work. No new config key. No schema migration. `minRequiredSchemaVersion` stays 3. `migrations/` is still `000001`, `000002`, `000003`, and `000004`. #224 was already present before this release. #189 is fixed in this release.

## [v0.5.48] - 2026-10-07

### Fixed

- A backup that starts in GTID mode, and a file/pos backup that gets MySQL 1236 before any event and continues from the stored GTID, no longer seals or uploads a magic-only placeholder or an empty `task-<id>.binlog` (#205, PR #261). The dump's artificial Rotate (flags `0x20`, timestamp 0, no CRC) is not written into the sealed segment. The runner waits for that Rotate to learn the real source file name before it opens the segment file, and does not create `task-<id>.binlog`. A segment that still has only the 4-byte magic header is deleted. It is not sealed, not cataloged, and not uploaded. A file/pos task that is not on this path still writes a real Rotate into the segment it seals. A source restart still seals the previous file at the last real event and continues on the next file. Business events already in a segment stay there. A segment sealed by this binary passes `mysqlbinlog --verify-binlog-checksum`. A segment an older binary already sealed with that no-CRC Rotate is not rewritten. Tip dogfood by BinlogServerQA passed. This release is not an ADR 0005 step. Steps 6–9 are still next work. No new config key. No schema migration. `minRequiredSchemaVersion` stays 3. `migrations/` is still `000001`, `000002`, `000003`, and `000004`. #189 and #224 were already present before this release. #205 is fixed in this release.

## [v0.5.47] - 2026-10-06

### Changed

- An error that is not on the retry allowlist fails the task on the first occurrence (ADR 0005 step 5). The allowlist, retried inside the same ownership, is: `SOURCE_UNREACHABLE` (still 10 consecutive failures on `consecutive_source_failures`, then `FAILED` and the lease is released), transient metadata errors (`tasks.IsTransientMetadataError`; `meta.IsTransientMySQLError` delegates to it; same substrings: deadlock, lock wait timeout, connection reset, connection refused, broken pipe, server has gone away, invalid connection, bad connection, read-only, read only, timeout, eof; no task-level cap; after metadata recovers the task returns to `RUNNING` without an operator Start and without passing through `FAILED`), and `OBJECT_PURGE_FAILED` (not a task failure; the next file open retries the object delete). Everything else, while the task is still supposed to be running, writes `FAILED` on the first occurrence, sets `last_error`, appends one `TASK_FAILED` event, releases the lease (`owner_worker_id` empty), and sets `desired_run=STOP` with `failed_spec_revision=spec_revision`. That includes `SOURCE_ACCESS_DENIED`, `SOURCE_LOG_BIN_OFF`, `SOURCE_IDENTITY_UNAVAILABLE`, `SEALED_FILE_EXISTS`, a non-transient `CHECKPOINT_WRITE_FAILED`, `SEGMENT_NOT_ON_WORKER`, `EPOCH_NOT_ACQUIRED`, MySQL 1236 when no GTID is stored, and any runner error `classifyRunError` does not recognize, including a local append or flush error. A lease handoff is not a failure and does not write `FAILED`. Tasks that used to sit in `RETRY_BACKOFF` on an unclassified error, or on MySQL 1236 with no stored GTID, now go `FAILED` immediately. For that 1236, `last_error` names 1236 and a purged binlog. Operator Start after `FAILED` still arms the task. A runner error observed while the task is already `STOPPING` or `STOPPED`, or while the stored row is a newer Stop, does not enter this allowlist. The run still finishes as `STOPPED` and does not append `TASK_FAILED`. A Stop whose KILL could not reach the source stays `STOPPED` with `pending_dump_cleanup` (migration `000004`, already on main; this step does not add `000005`). No new metric and no notifier. The operator signal for an allowlist miss is `FAILED`, `last_error`, and `TASK_FAILED`. No new migration and no new config key. `minRequiredSchemaVersion` stays 3. Schema 4 (`000004_pending_dump_cleanup`) stays optional for one process and required for cluster pending cleanup. `migrations/` is still `000001`, `000002`, `000003`, and `000004`.

### Fixed

- An operator Stop no longer stays `STOPPING` when the dump exit writes `STOPPED` from a snapshot older than that Stop's `spec_revision`. The task upsert keeps the row when its `spec_revision` is newer, so the older `STOPPED` did not apply and `GET /api/tasks/{id}` stayed `STOPPING` with the owner and epoch still set. The exit now writes `STOPPED` on the Stop's spec, and writes it again when the stored row is still that Stop. No new migration and no new config key.

## [v0.5.46] - 2026-10-06

### Fixed

- Metadata mode treats `backup_tasks.desired_run` (`RUN` or `STOP`) and `spec_revision` as the operator intent (ADR 0005 step 4 of 9, #193). Start writes `desired_run=RUN` and increments `spec_revision`. Stop writes `desired_run=STOP` and increments `spec_revision`. A change to source, start position, storage, or `cluster_key` increments `spec_revision`. The worker control loop is the only starter and stopper. It starts a dump, stops one, restarts one dump after a spec change, and leaves a row alone when desired state and the open dump already match. `Acquire` runs only when ownership actually changes. The same worker keeping its lease does not increase the epoch. `STOPPED` is written only after the dump connection `Close()` has returned, and `owner_worker_id` is cleared then. A Stop that can reach the source leaves no Binlog Dump thread for that task. `KILL` uses the task's current password, so Stop, change the source password, then Start leaves one Binlog Dump thread, and a later Stop leaves zero. Standalone with no metadata database uses the in-memory task the same way. Step 4 adds no migration and no config key. On startup, and on each claim pass, rows with `spec_revision=0` and `applied_spec_revision=0` get `desired_run` from state: `RUNNING`, `STARTING`, `RETRY_BACKOFF`, `LEASE_DEGRADED`, and `REBUILDING_FILE` become `RUN`; every other state becomes `STOP`. The revision columns stay 0. A later pass changes zero rows once they match. A row this process has started, stopped, or edited has `spec_revision>0` and is left alone, so a task v0.5.44 or v0.5.45 left as `RUNNING` with `desired_run=STOP` keeps running. Stop every v0.5.44 and v0.5.45 worker on that metadata database before starting v0.5.46. A mixed cluster flaps tasks because those binaries do not maintain `desired_run`. A startup refusal because the metadata schema is below 3 is written to the configured log file, then the process exits. The message contains `schema version too old` and `./migrate up` (#245).
- An operator Stop that cannot reach the source still writes `STOPPED` (#249). `GET /api/tasks/{id}` includes `pending_dump_cleanup` (`connection_id`, `host`, `port`, `warning`: `source Binlog Dump connection <id> may still be open; will KILL when source is reachable`). The Console task detail shows that warning, and it stays after `STOPPED`. One `DUMP_CLEANUP_PENDING` event is written when the marker is set, and one `DUMP_CLEANUP_CLEARED` when it clears. The worker retries `KILL` with the current task password once the source is reachable, first after 5s, then doubling up to 30s, until `KILL` succeeds, MySQL returns `ER_NO_SUCH_THREAD`, the id is no longer a Binlog Dump in the process list, or the task is deleted. `STOPPED` is not delayed for that `KILL`. A retry that is still writing the snapshot it opened does not cover a newer operator Stop. On schema 3 the marker lives in the process that held the dump, including after the runner is gone, so `GET` still shows it after `STOPPED` with `process_local` true (#252). Another process does not show it, and a restart clears it. Schema 4 stores the marker on `backup_tasks.pending_dump_cleanup`, so a restart still shows it. An empty column drops this process's copy. A task has at most one Binlog Dump (#255). Start `KILL`s the old connection with the current password and opens a new dump only after the process list or `ER_NO_SUCH_THREAD` confirms it is gone. If that `KILL` is not confirmed, the task stays in `RETRY_BACKOFF` with the warning and does not dump. The worker that holds the dump renews its lease until `Close()` returns, and stops its own dump as soon as the lease is lost. Another process does not write `STOPPED` for a dump it did not close. While the dump is open, schema 4 stores the connection id as `held`, and the API does not show that as a warning. If the holder's lease expires first, the id becomes a normal pending cleanup and any worker that claims the task retries `KILL` on every state. Cluster mode on schema 3 logs `cluster mode needs migration 000004` and does not finish or take over another worker's dump. A single process on schema 3 still starts. Migration `000004_pending_dump_cleanup` only adds `backup_tasks.pending_dump_cleanup VARCHAR(512) NOT NULL DEFAULT ''`. It is online. `minRequiredSchemaVersion` stays 3. `./migrate up` is optional for one process and required for cluster mode. Restart after the migration so the process reads the column. `migrations/` is `000001`, `000002`, `000003`, and `000004`. No new config key. A clean close, where FIN or RST reaches the source, is about one `master_heartbeat_period` (15s; a FIN close was observed around 26–29s) (#253). A half-open path does not fill the source send buffer, so `net_write_timeout` does not abort the thread. The source keeps it until that host gives up TCP retransmission (Linux `tcp_retries2`, about 15 minutes by default; one half-open close was measured at about 957s). This process cannot shorten that wait while the path stays blackholed. A DBA can `KILL <id>` on the source. A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). `kill -9` during a takeover download leaves a `.takeover-*` temp file in the task directory that is never cleaned (#224). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). #189, #205, and #224 were already present before this release. #193, #245, #249, #252, #253, and #255 are fixed in this release.

## [v0.5.45] - 2026-10-06

### Fixed

- On all-in-one and cluster, `retry_attempt` and `consecutive_source_failures` are stored on `backup_tasks` and written with the task row on each `SOURCE_UNREACHABLE` failure and on a successful dump connect (#239, ADR 0005 step 3 of 9, PR #244). A restart continues that streak. The process tries the source once immediately, then the backoff delay continues from the stored attempt (8s, then 16s, then 30s, and so on) instead of starting again at 1s. The delay cap stays 30s. Two restarts inside one streak, `SIGTERM` and `kill -9` in either order, still reach `FAILED` on the 10th consecutive failure. The consecutive count stays at most 10. `last_error` begins with `SOURCE_UNREACHABLE:`, and the lease is released. Operator Start stores 0. A successful dump connect stores 0. A claim does not. The same worker renewing its lease, or reclaiming its own lease after that lease has expired, keeps the epoch. A different worker taking an expired lease increases the epoch by 1. The `FAILED` row sets `desired_run` to `STOP` and `failed_spec_revision` to `spec_revision`. Lease renewal no longer rewrites that row as `STOPPING`. `minRequiredSchemaVersion` is 3. If the database is not already schema 3 from v0.5.44, run `./migrate up` while those processes stay up, confirm `SELECT version, dirty FROM schema_migrations` is `(3, 0)`, then replace binaries. Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. A v0.5.45 process on schema 2 exits with code 1. The message contains `schema version too old` and `./migrate up`, and it is printed to stderr only, not the log file. Replacing the binary with v0.5.44 works on schema 3. Do not run `./migrate down` to schema 2 while v0.5.45 is running. Schema 1 to schema 2 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. v0.5.45 needs schema 3 before start. The first start after upgrading from a binary that only had the event streak copies that streak into the columns once when both columns are still 0. That one start begins the backoff delay at 1s again. Later restarts use the stored attempt. Standalone, with no metadata database, keeps the counters in memory. They reset when the process exits. No new config key. This release does not add a migration. `migrations/` is still `000001`, `000002`, and `000003`. QA passed on `ec1e4f1d` for all-in-one and a 2-worker cluster. A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). `kill -9` during a takeover download leaves a `.takeover-*` file in the task directory that is never cleaned (#224). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. #189, #193, #205, and #224 were already present before this release. #239 is fixed in this release.

## [v0.5.44] - 2026-10-06

### Added

- Migration `000003_task_desired_and_retry_budget` adds six `NOT NULL` columns with defaults to `backup_tasks`: `desired_run`, `spec_revision`, `applied_spec_revision`, `failed_spec_revision`, `retry_attempt`, and `consecutive_source_failures` (ADR 0005 step 2 of 9, #236). Backfill sets `desired_run=RUN` for `RUNNING`, `STARTING`, `RETRY_BACKOFF`, `LEASE_DEGRADED`, and `REBUILDING_FILE`, and `desired_run=STOP` for `CREATED`, `STOPPING`, `STOPPED`, and `FAILED`. Revisions and counters are 0. The runtime does not read or write these columns. `minRequiredSchemaVersion` stays 2, so v0.5.44 runs on schema 2 or schema 3, and v0.5.43 keeps running on schema 3. `./migrate up` is online while processes keep running. No restart is required. That was checked with v0.5.43 still running. Confirm `SELECT version, dirty FROM schema_migrations` is `(3, 0)`. `SHOW COLUMNS FROM backup_tasks` lists the six columns. Then replace binaries in any order. Rollback is `./migrate down --steps 1`, which returns to version 2 and drops only those six columns. `backup_tasks` and `binlog_files` row counts are unchanged. Run the migration now. ADR 0005 step 3, a persisted retry budget, raises `minRequiredSchemaVersion` to 3. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. No new config key. No runtime behavior change. `migrations/` is `000001`, `000002`, and `000003`. Two restarts inside one failure streak can take up to 14 failures before `FAILED`, because a claim-written `TASK_STARTED` is indistinguishable from an operator Start. The backoff delay restarts at 1s after a restart. The structural fix is ADR 0005 step 3, a persisted retry budget column (#239). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). `kill -9` during a takeover download leaves a `.takeover-*` file in the task directory that is never cleaned (#224). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. #239, #189, #193, #205, and #224 were already present before this release.

## [v0.5.43] - 2026-10-06

### Added

- ADR 0005 is accepted (`docs/adr/0005-single-task-authority-and-segment-catalog.md`). Metadata MySQL is the single authority for task state, and a segment is keyed by `(task_id, source_file, epoch)`. The record is a roadmap. This release does not change runtime behavior for it. No new config key. No schema migration.

### Fixed

- On all-in-one and cluster, an owned task whose source cannot be reached now stays in `RETRY_BACKOFF` for the backoff interval and reaches `FAILED` after ten consecutive `SOURCE_UNREACHABLE` failures, the same budget as standalone (#203, #234). `GET /api/tasks/{id}` shows `last_error` beginning with `SOURCE_UNREACHABLE:`, and the lease is released. The claim loop, about every 2 seconds, no longer starts a live owned run while this process is already waiting to retry, so the failure count is not cleared and the lease epoch does not climb during backoff. Epoch still changes when a different worker takes over an expired lease. A same-owner claim with no local run continues the count from `TASK_RUNNER_ERROR` events already stored for that task. The count resets when the dump connects, or when an operator Starts the task. A source that dies after the dump is open still uses this same budget. No new config key. No schema migration.
- Restarting all-in-one or a cluster worker during a `SOURCE_UNREACHABLE` streak no longer starts the count over (#237, #238). Four stored failures, then a restart (`SIGTERM` or `kill -9`), still reach `FAILED` on the tenth failure total. The same-owner claim and an expired-lease takeover both read stored events oldest-first. The 200-event window stays the newest rows. No new config key. No schema migration. Schema stays at version 2 from v0.5.34. `migrations/` is still `000001` and `000002`. Binaries drop in from v0.5.42 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. Two restarts inside one failure streak can take up to 14 failures before `FAILED`, because a claim-written `TASK_STARTED` is indistinguishable from an operator Start. The backoff delay restarts at 1s after a restart. The structural fix is ADR 0005 step 3, a persisted retry budget column (#239). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). `kill -9` during a takeover download leaves a `.takeover-*` file in the task directory that is never cleaned (#224). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. #205, #224, #189, and #193 were already present before this release. #203 and #237 are fixed in this release. #239 is open.

## [v0.5.42] - 2026-10-06

### Fixed

- With no upload configured, age retention no longer leaves a deleted sealed binlog in the catalog (#209, #231). The pass still deletes the local sealed file older than the local window. It now deletes the `binlog_files` row for a sealed file in this task directory that has no remote copy once that local file is gone. `GET /api/tasks/{id}/files`, the Console file table, `GET /api/tasks/{id}/replay`, and `GET /api/tasks/{id}/replay/archive` no longer list that name, so a restore that followed the list does not download a file the server already removed and get 404. `GET /api/tasks/{id}/events` gets one `RETENTION_REMOVED`. The message is `<name> removed by local retention`. The log line is `retention: removed <name> task=<id> upload_state=<state>`. A row an older binary left behind, still `SEALED` / `LOCAL_ONLY` with a `file_path` that is already gone, is removed on the next retention pass. That pass runs when this process opens a binlog, including Start. A second pass does not write the event again. A row whose `file_path` is outside this task directory stays. An open row stays. A sealed file still inside the window stays. An `UPLOADED` row with an object key stays. With object storage configured, a sealed file past the local window and still inside the bucket window loses only the local file. `location` is `bucket`. Download, replay, and the archive still return the object. No `RETENTION_REMOVED` is written for that row. No new config key. No schema migration. Schema stays at version 2 from v0.5.34. `migrations/` is still `000001` and `000002`. Binaries drop in from v0.5.41 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. On all-in-one and cluster, the claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the failure count, so the task never reaches `FAILED` there. The lease epoch keeps climbing across those retries. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:` (#203). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). `kill -9` during a takeover download leaves a `.takeover-*` file in the task directory that is never cleaned (#224). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. #203, #205, #224, #189, and #193 were already present before this release. #209 is fixed in this release.

## [v0.5.41] - 2026-10-06

### Fixed

- Taking over a task whose checkpoint already sits at the end of an uploaded binlog no longer saves that file a second time (#223, #226). The next worker, or the same worker after the local file is gone, reads the object back and opens it as `mysql-bin.NNNNNN.open.eN`. The following file rotation used to seal that copy as `mysql-bin.NNNNNN.sealed.eN` and upload it again. The new catalog row stored `start_pos` and `end_pos` as the same end position, the bytes matched the object already in the bucket, and `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` listed both. A restore then applied those transactions twice when the target was not using GTID, or when the client passed `--skip-gtids`. That rotation now drops the open row and the local copy when another verified `UPLOADED` row of the same source file already covers the resume position and the local file has no complete event past it. Replay and the archive list one path for each source file. A catalog that already has the extra row does the same, without a manual delete. A row with `end_pos` 0 stays. `GET /api/tasks/{id}/files` still lists that extra row. A binlog that is still receiving new events is still sealed and uploaded once. No new config key. No schema migration.

- On all-in-one and cluster, a `LATEST` task whose open file holds only the 4-byte magic header, or only a header of events whose `end_log_pos` is 0 and that ends on an event boundary, no longer goes permanently `FAILED` with `SEGMENT_NOT_ON_WORKER` after a source outage that raises the lease epoch (#222, #228). The file was on this machine. The claim loop still starts the owned `RETRY_BACKOFF` task again about every 2 seconds and the epoch still climbs (#203). Once the epoch is greater than 1, Start keeps the saved file and position. `gtid_set` stays empty. The open file is renamed to `mysql-bin.NNNNNN.open.eN`. The task returns to `RUNNING`. The transactions committed during the outage are copied once, in order, so the GTID sequence in the local file stays continuous. Standalone, with no metadata store, and a retry that is still epoch 1, already continued from that anchor. A file that is actually missing, or a torn tail, still fails with `SEGMENT_NOT_ON_WORKER` and names the path. This release has no schema migration. Schema stays at version 2 from v0.5.34. `migrations/` is still `000001` and `000002`. Binaries roll from v0.5.40 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. No new config key. On all-in-one and cluster, the claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the failure count, so the task never reaches `FAILED` there. The lease epoch keeps climbing across those retries. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:` (#203). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY`. `GET /api/tasks/{id}/files` still lists that name. `GET /api/tasks/{id}/files/{name}` returns 404 (#209). `kill -9` during a takeover download leaves a `.takeover-*` file in the task directory that is never cleaned (#224). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. #203, #205, #209, #224, #189, and #193 were already present before this release.

## [v0.5.40] - 2026-10-06

### Fixed

- Stop, Start, lease renewal, and progress on one task no longer wait while another task reads or writes events (#177, #219). `GET /api/tasks/{id}/events` looks the task up under the scheduler lock, then reads the event store after releasing it. Event inserts update memory under the lock and write the store afterward. Writes for one task stay in order. A slow read or insert on task A does not block Stop, Start, lease renewal, or progress on task B. An event-store error stays best-effort and does not fail the transition. Progress updates only touch memory under that lock. They stalled before because they shared it with event reads and inserts. Opening the next binlog, sealing it, and age retention read `binlog_files` 200 rows at a time, ordered by `(file_name, epoch)`, instead of loading every row for the task. Local versus bucket retention, `LOCAL_ONLY`, `UPLOAD_FAILED`, and checksum `match` versus `mismatch` are unchanged. `GET /api/tasks/{id}/files` and replay still load that task's whole catalog and window it in Go, in the same order as before. Lease takeover still reads every catalog row once per start, because it needs the unreadable tail and a covering uploaded object. Taking over an `UPLOADED` segment streams the object to a `.takeover-*` file in the task directory. The open name `mysql-bin.NNNNNN.open.eN` appears only after the copy matches the size reported when the object was opened. A short read or a failed download deletes that temp file and does not leave a truncated segment. The catalog checksum stays the upload comparison flag (`match` or `mismatch`). This copy does not hash the object and does not change that flag. No new config key. No schema migration.

- On all-in-one and cluster (`meta_dsn` set), polling `GET /api/tasks/{id}` while you Stop and Start no longer starts the new run at epoch 0 (#220, #221). Before this, a refresh during Start could put the previous `STOPPED` row back in memory. The dump then wrote a bare `mysql-bin.NNNNNN` file, did not renew the lease, and `GET /api/tasks/{id}` stayed `STARTING`. The next Stop and Start went `FAILED` with `SEGMENT_NOT_ON_WORKER` even though that file was already on this machine, and Start could not pick it up. Start now keeps the owner and epoch it just acquired. The open file is `mysql-bin.NNNNNN.open.eN`, the lease keeps renewing, and the task reaches `RUNNING`. A task already left on the bare file by an older binary starts again from that file when the catalog row is still `OPEN` and the file is on this worker. The bare file is renamed to `mysql-bin.NNNNNN.open.eN`. A file that is actually missing still fails with `SEGMENT_NOT_ON_WORKER` and the path to mount or copy. On all-in-one and cluster, a start that does not keep a lease epoch goes `FAILED` with `last_error` beginning `EPOCH_NOT_ACQUIRED:` instead of dumping at epoch 0. The lease is released. Start the task again. Standalone, with no metadata store, still has no lease epoch. This release has no schema migration. Schema stays at version 2 from v0.5.34. `migrations/` is still `000001` and `000002`. Binaries roll from v0.5.39 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. No new config key. QA dogfood passed at `05b49642`: 30 plus 30 race rounds, with no epoch-0 hit, and tasks already stuck on the bare file recovered. While one task held the metadata lock, other tasks' API calls were about 1-4 ms at p50. Rotate of a catalog with 450 or more rows was about 27 ms at p50. Takeover of a 266 MB uploaded segment peaked near 70 MB RSS, against 636 MB on v0.5.39, and the sha256 matched. The v0.5.39 regressions #204, #214, and #213 passed on standalone and on a short all-in-one outage. On all-in-one, a `LATEST` task that has not yet written a complete event, plus a source outage of about 20 seconds, can still go permanently `FAILED` with `SEGMENT_NOT_ON_WORKER` and miss the outage's transactions (#222). After takeover of an `UPLOADED` segment the same source file can be sealed again as `mysql-bin.NNNNNN.sealed.eN` and uploaded again, so replay lists that source index twice (#223). `kill -9` during a takeover download leaves a `.takeover-*` file in the task directory that is never cleaned (#224). That temp file is new with the streamed copy. After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. On all-in-one and cluster, the claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the failure count, so the task never reaches `FAILED` there. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:` (#203). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY`. `GET /api/tasks/{id}/files` still lists that name. `GET /api/tasks/{id}/files/{name}` returns 404 (#209). #222, #223, #203, #189, #193, #205, and #209 were already present before this release.

## [v0.5.39] - 2026-10-06

### Fixed

- A source that goes down while a binlog dump is already open no longer stays `RUNNING` for the whole outage (#204, #212). The dump library may reconnect that open connection 5 times, about one second apart. A blip that comes back within those 5 tries leaves the task `RUNNING` and does not change `last_error`. If the source is still down after that, `GET /api/tasks/{id}` and the Console show `RETRY_BACKOFF`, and `last_error` begins with `SOURCE_UNREACHABLE:`. Each of those failures counts toward the same ten-failure `SOURCE_UNREACHABLE` budget as a failed first connect. On standalone, with no metadata store, the tenth consecutive failure is `FAILED` and the lease is released. On all-in-one and cluster, the claim loop still starts an owned `RETRY_BACKOFF` task again about every 2 seconds and resets that count, so the task never reaches `FAILED` there (#203). This change does not alter that. When the source accepts connections again, the task returns to `RUNNING` and continues from its checkpoint. No new config key. No schema migration.

- A `LATEST` task that has already connected no longer skips transactions committed while the source is down (#213, #215). The first time `LATEST` resolves, that file and position are saved as the task checkpoint, with no `gtid_set`. `GET /api/tasks/{id}/checkpoint` returns that file and position. The task's configured start stays `LATEST`. When the dump then fails and the task enters `RETRY_BACKOFF`, the next attempt continues from that file and position, or from the last dumped event if one was already written. It does not read `SHOW MASTER STATUS` and start at the new tip. Before this, a task that had connected but had not yet flushed an event resolved `LATEST` again after the source came back, so rows committed during the outage were never pulled. A task that already has a checkpoint is unchanged. A `GTID` start still keeps and extends `gtid_set` on each flushed event. A `LATEST` checkpoint still has no `gtid_set`: events copied from the middle of a file are not a complete executed set. With no metadata store, this process keeps the same file and position for the retry. No new config key. No schema migration.

- A source restart in the middle of a dump no longer leaves an all-in-one task permanently `FAILED` with `SEGMENT_NOT_ON_WORKER` (#214, #216). On reconnect MySQL sends an artificial Rotate whose `end_log_pos` is 0. That event is not stored in the source binlog, and it names the next file. It is not written into the local segment. The open file is sealed at the last real event and the dump continues on the next file. A local segment that already ends with that Rotate still resumes from the last event whose `end_log_pos` is not 0. Those extra bytes are dropped the next time the segment is opened. The same worker keeps the task when the catalog `file_path` is that local file. A task already `FAILED` on this path starts again from that event. A manual truncate is not required. `mysqlbinlog --verify-binlog-checksum` still reads the local file. This release has no schema migration. Schema stays at version 2 from v0.5.34. Binaries roll from v0.5.38 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. No new config key. QA dogfood passed at `3460c45`. Stop, Start, lease renewal, and progress updates for every task on one process share one lock with event reads and event inserts. A slow metadata write, or the Console reading events, holds the others. Opening the next binlog reads that task's whole file catalog on the dump path. Taking over a large uploaded segment reads the whole object into memory on that worker (#177). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. On all-in-one and cluster, the claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the failure count, so the task never reaches `FAILED` there. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:` (#203). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY`. `GET /api/tasks/{id}/files` still lists that name. `GET /api/tasks/{id}/files/{name}` returns 404 (#209). These were already present before this release.

## [v0.5.38] - 2026-10-05

### Fixed

- A `PUT /api/tasks/{id}` on a task in `RUNNING`, `STARTING`, `LEASE_DEGRADED`, or `RETRY_BACKOFF` that changes source, start, storage, or `cluster_key` returns HTTP 400. The body is plain text `stop the task before changing source, start, storage, or cluster_key: state <STATE>` (#179, #208). `<STATE>` is that task's state. The saved row stays as it was, so `GET /api/tasks/{id}` still shows the source, start, storage, and `cluster_key` the dump is using. Before this, the API saved the new values and the running dump kept the copy taken at start, so GET did not match what was in effect. `RETRY_BACKOFF` is included because the next attempt uses the copy this process already holds. Source includes host, port, user, password, flavor, `server_id`, and `semi_sync`. Start includes mode, file, position, and `gtid_set`. Storage includes `dir`, `retention_days`, `local_retention_days`, and `bucket_retention_days`. An omitted field keeps the stored value. An empty `source.password` keeps the stored password. Sending the same source, start, storage, and `cluster_key` again is accepted. A PUT that changes only `name` returns HTTP 200 and the dump keeps running. `CREATED`, `STOPPING`, `STOPPED`, and `FAILED` still accept a PUT that changes those fields. A password written while the row is `STOPPING` is the password the next Start uses. Stop, wait until the row is `STOPPED`, PUT, then `POST /api/tasks/{id}/start`. Start reads the updated row and resumes from the checkpoint. A task in `RETRY_BACKOFF` with a wrong host or port used to take the corrected host or port from an in-place PUT. That PUT now returns HTTP 400 with `state RETRY_BACKOFF`. Stop, PUT the host or port, then Start. Listen address, upload endpoint, lease TTL, and encryption key are process settings and are not fields on this PUT. This release has no schema migration. Schema stays at version 2 from v0.5.34. Binaries roll from v0.5.37 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. No new config key. Checked on MySQL 8.0.46, built from source, at `4477d19e`. Stop, Start, lease renewal, and progress updates for every task on one process share one lock with event reads and event inserts. A slow metadata write, or the Console reading events, holds the others. Opening the next binlog reads that task's whole file catalog on the dump path. Taking over a large uploaded segment reads the whole object into memory on that worker (#177). After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. On all-in-one and cluster roles, the claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the `SOURCE_UNREACHABLE` count, so the ten-attempt cap does not fire and the task stays in `RETRY_BACKOFF`. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures (#203). A source that goes down while a dump is already open can leave the task showing `RUNNING`. The scheduler does not see that stream retry, so the task does not enter `RETRY_BACKOFF` and does not count `SOURCE_UNREACHABLE` (#204). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY`. `GET /api/tasks/{id}/files` still lists that name. `GET /api/tasks/{id}/files/{name}` returns 404 (#209). These were already present before this release.

## [v0.5.37] - 2026-10-05

### Fixed

- A local condition that will not clear on its own no longer stays in `RETRY_BACKOFF`, renews its lease, and reconnects to the source (#178). `sealed file already exists` moves the task to `FAILED`. `last_error` is `SEALED_FILE_EXISTS: sealed file already exists: <path>`. The lease is released and that runner is not called again. A checkpoint write that is not a transient metadata error moves the task to `FAILED` with `last_error` beginning `CHECKPOINT_WRITE_FAILED`, and the lease is released. Transient checkpoint errors stay `RETRY_BACKOFF`. They match the metadata client's list: deadlock, lock wait timeout, connection reset, connection refused, broken pipe, server has gone away, invalid connection, bad connection, read-only, timeout, and eof. `context.Canceled` and `context.DeadlineExceeded` stay retryable.

- A lease epoch mismatch is a handoff. The runner that lost the epoch stops and releases only that epoch. It does not write `FAILED` or `RETRY_BACKOFF`, so it does not replace the task row of the worker that holds the task now. With no metadata store, this process shows `STOPPED` and appends `TASK_LEASE_YIELDED`. Start works again from `STOPPED`. `SOURCE_UNREACHABLE` is unchanged: ten consecutive failures, then `FAILED`, and a ready runner resets that count. Other retryable source errors, `OBJECT_PURGE_FAILED`, and MySQL 1236 with no stored GTID stay in `RETRY_BACKOFF` with no new cap. The metadata client already retries a transient error five times inside one call; the scheduler retry is what lets a task return to `RUNNING` across a metadata failover, and a cap of ten would mark that backup `FAILED` and require a manual start. `OBJECT_PURGE_FAILED` is retried on the next file open, and failing the task would stop capture until the bucket is back and someone starts the task. MySQL 1236 with an empty `gtid_set` remains the v0.5.36 resume gap. A local binlog append or flush error is still returned as-is and retried; it is not the sealed-file conflict or the checkpoint write. This release has no schema migration. Schema stays at version 2 from v0.5.34. Binaries roll from v0.5.36 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. No new config key. On all-in-one and cluster roles, the claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the `SOURCE_UNREACHABLE` count, so the ten-attempt cap does not fire and the task stays in `RETRY_BACKOFF`. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures (#203). A source that goes down while a dump is already open can leave the task showing `RUNNING`. The scheduler does not see that stream retry, so the task does not enter `RETRY_BACKOFF` and does not count `SOURCE_UNREACHABLE` (#204). A GTID-mode start writes a manual Rotate with no CRC and can leave a stray `task-N.binlog` (#205). These three were already present before this release. After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread.

## [v0.5.36] - 2026-10-05

### Fixed

- A task created with `start.mode=GTID` lost that GTID on the first checkpoint flush. Resume asked the source for a file and position. If that binlog had been purged, MySQL returned 1236 (`Could not find first log file name in binary log index file`) and the task stayed in `RETRY_BACKOFF` (#175, #197). The checkpoint `gtid_set` also stayed at the set from task creation (#198). A flushed checkpoint, including the rotate onto the next file, now keeps the executed set. The set grows when the transaction commits (XID, `COMMIT`, `ROLLBACK`, or an autocommit statement). The GTID event is not added before that commit. `ROLLBACK TO` does not add it. `GET /api/tasks/{id}/checkpoint` returns `gtid_set` when the file and position are the stored checkpoint. An epoch greater than 1 that rewinds to position 4 leaves `gtid_set` out of that response. The stored row still has the set, and a purged file still uses it. Resume still tries file and position first. A file that is still on the source stays a file and position dump. `StartSync` returns before MySQL sends 1236. That error arrives on the first stream read, before any dump event. When the checkpoint has a GTID, this process closes that dump and opens `StartSyncGTID`. The log line is `file/pos resume hit MySQL 1236 before any event; opening GTID dump`. The task reaches `RUNNING`. A GTID dump that also returns 1236 is returned to the scheduler in that attempt. The missing file is not opened again inside the same attempt. A file and position task that has no GTID does not gain one from the stream. A 1236 on that task is still 1236. With `binlog_transaction_compression=ON`, the commit sits inside a transaction payload. A compressed transaction is not added to `gtid_set`, and the set does not grow for it. CI job `e2e-gtid-purge` turns that setting off, checks that `gtid_set` grows past the seed, purges the checkpoint file, and checks that Start reaches `RUNNING`, then turns compression back on. This release has no schema migration. Schema stays at version 2 from v0.5.34. Binaries roll from v0.5.35 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. No new config key. A checkpoint whose `gtid_set` is already empty is not rebuilt from the local file. While the checkpoint file is still on the source, Start continues at that file and position. The next flushed event writes the task's configured start GTID. Commits after this binary is running extend that set. Transactions copied while the column was empty are not filled in. If that file is already purged and the column is empty, Start still receives 1236 and the task stays in `RETRY_BACKOFF`. If `gtid_set` does not contain transactions this task already copied, a later purge resumes from that incomplete set. The source sends the missing transactions again, and they are copied into the task's local segments again. If the source has already purged those missing GTIDs, the GTID dump returns 1236 and the task goes to `RETRY_BACKOFF`. After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread.

## [v0.5.35] - 2026-10-05

### Fixed

- On a control-plane plus workers deployment, Stop from the API process used to write the task `STOPPED` and clear the owner while the worker kept the dump and the lease. The Console showed Stopped, and the source still had a Binlog Dump thread (#174). When this process is not dumping and another worker owns the lease, Stop writes `STOPPING` and keeps the owner, the epoch, and the source config. `POST /api/tasks/{id}/stop` still returns HTTP 204. The Console shows Stopping. The worker claim loop, about every 2 seconds, cancels a live dump whose row is `STOPPING` or `STOPPED`. When that dump exits, the worker writes `STOPPED` and releases the lease. A store sync and `GetTask` on that worker also cancel the dump and leave the in-memory owner and epoch in place, so the lease can be released. The final `STOPPED` row keeps the latest stored source config. A source password changed while the row is `STOPPING` is the password the next Start uses. A `STOPPING` row whose lease has expired is written `STOPPED`, and that claim does not start a dump. All-in-one and standalone still cancel the local dump and write `STOPPED` when that dump exits. This release has no schema migration. Schema stays at version 2 from v0.5.34. Binaries roll from v0.5.34 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. On the split topology, HTTP 204 means Stop was accepted. The row stays `STOPPING` until the worker finishes, and a Start in that state returns HTTP 400 `cannot start from state STOPPING`. After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread.

## [v0.5.34] - 2026-10-05

### Fixed

- A later open of the same source binlog name no longer overwrites the sealed catalog row. `binlog_files` unique key is `(task_id, file_name, epoch)`. `file_name` stays the source name. Each epoch is its own row, with its own path, upload state, checksum, and object key. Before this release the unique key was `(task_id, file_name)`. Opening `<name>.open.eN` replaced the sealed path, `UPLOADED`, checksum, and object key, so replay and point-in-time restore listed only the short open file, and a second seal of that name reused the object key and overwrote the bucket object. The published v0.5.33 package still does that. Migration `000002_binlog_file_epoch_key` runs only from `./migrate up`. The process does not migrate on startup. Before the unique key changes, a `file_path` ending in `.open.eN` or `.sealed.eN` gets `epoch` N. A plain sealed name stays epoch 0. Stored object keys are left unchanged, so already-uploaded objects do not need another upload. A later open writes its own row. The first seal of a source name still uses the historical object key, leaf equal to the source name. When a sealed file for that name is already on disk, or the catalog already has another epoch for that source name, a later seal uses leaf `name.sealed.eN`. `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` list every sealed segment in the window, on disk or bucket-only, plus the highest open segment for that source index. `stop_datetime` reads the full catalog and then keeps the segments whose copied events cover the time window. The same object is not listed twice when those bytes were copied into the current open file. `limit` still defaults to 200, and each epoch counts as one row. Retention still ages each segment on its own row. `UPLOADED` with checksum `match` follows the v0.5.33 local and bucket windows. `UPLOAD_FAILED` stays local. Checksum mismatch and an unfinished checksum check stay `UPLOAD_FAILED`. Crash enrollment, the `meta.timeout.upload_sec` deadline, and `LOCAL_ONLY` with an empty object key when object storage is not configured are unchanged. `POST /api/tasks/{id}/files/retry-upload` still returns HTTP 400 `upload retry is not available` for that task. Opening a binlog file deletes other `OPEN` rows for that source name at a different epoch. Sealed rows stay. `OPEN` rows in that task directory whose files are gone on this worker's disk are also deleted. An `OPEN` row for a different source name under another directory stays. `GET /api/tasks/{id}/files` and the Console file table show one row per epoch. After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct. Download and replay still return that segment. A checksum is still compared once after upload. Stop every binlog-server process that uses this metadata database, run `./migrate up`, then start only the v0.5.34 binaries. The new binary requires `schema_migrations` at least 2 and index `uk_task_file_epoch`, and refuses to start without them. A v0.5.33 binary against schema 2 refuses to start because it still expects `uk_task_file`. Rolling back is stop every process, `./migrate down --steps 1`, then start the old binaries. That down migration drops every `binlog_files` row except the lowest `id` per `(task_id, file_name)` before restoring `uk_task_file`. No new upload state. No new config key.

## [v0.5.33] - 2026-10-05

### Fixed

- A worker that dies while a just-sealed binlog is uploading, or in the moment after that file is renamed and before the upload is recorded, no longer leaves the sealed file stuck as `LOCAL_ONLY` or outside the retry set. With object storage, the catalog row is written before the PUT: `state` `SEALED`, `upload_state` `UPLOAD_FAILED`, `upload_error` `upload pending`, object key set. The checkpoint for the next file is written before that PUT. The PUT uses the existing `meta.timeout.upload_sec` deadline (30 seconds by default), the same deadline as the background retry. A timeout leaves the row `UPLOAD_FAILED` and replication opens the next file. `upload_error` stays `upload pending` after that timeout, because the catalog write that would record it uses the same cancelled deadline. On the next start, including a lease takeover, a sealed file on disk that never became `UPLOADED` is recorded the same way. When the checkpoint is still on that sealed file and the last complete event is the rotate that names the next file, the checkpoint moves to that next file, so replication continues there. The background retry uploads the file and checks the checksum. The worker that pulls binlog runs that retry. A control-plane-only process does not. `GET /api/tasks/{id}/files` shows `UPLOAD_FAILED` and `location` `local` while the file is waiting, then `UPLOADED`, checksum `match`, and `location` `both` while the file is still on disk. With object storage and a catalog, retention keeps the local file and the catalog row until the checksum is `match`. A file past the local retention window stays, and that pass writes one `RETENTION_SKIPPED_NOT_UPLOADED`. For a file enrolled after a crash, `sealed_at` is the time this process opened the file: the open row has no separate seal time, the catalog writes `sealed_at` with the same value as `created_at`, and enrollment keeps that time. While the PUT is hanging, new local binlog events wait until the timeout fires, and the background retry may PUT the same object key again with the same sealed bytes. The next run with object storage uploads a sealed `LOCAL_ONLY` file that is still on disk. Up to `v0.5.32`, turning object storage on later left that row unuploaded. A task with no object storage still seals as `LOCAL_ONLY` with an empty object key. `POST /api/tasks/{id}/files/retry-upload` returns HTTP 400 `upload retry is not available`. A checksum is still compared once after upload. An object changed in the bucket after it matched is not compared again. No new upload state. No new config key. No schema migration.

## [v0.5.32] - 2026-10-05

### Fixed

- A sealed upload whose stored object does not match the local file, or whose checksum comparison does not finish, is recorded as `UPLOAD_FAILED` instead of a durable `UPLOADED` copy. `upload_error` is `checksum mismatch`, or it begins with `checksum verify failed:`. `checksum` stays `mismatch` or empty. Empty is not `match` and not `mismatch`. Replication keeps running. With object storage and a catalog, retention does not delete that local file or its catalog row, and it writes one `RETENTION_SKIPPED_NOT_UPLOADED` event. The background upload retry and `POST /api/tasks/{id}/files/retry-upload` upload a checksum mismatch again. An unfinished check is compared again and is not uploaded again. When that later check matches, the row becomes `UPLOADED` with checksum `match`, and a later retention pass can delete it. An `UPLOADED` row already stored with `mismatch` or an empty checksum keeps its local file; the next retention pass records it as `UPLOAD_FAILED` so that same retry can verify it. A checksum of `match` is unchanged: retention still deletes the object, the catalog row, and the local file, including the longer bucket window that deletes only the local file. PITR, download, and replay still read a sealed `UPLOADED` object when the local file is already gone. `LOCAL_ONLY` retention is unchanged. A bucket-only `UPLOADED` row is still aged as before. No new upload state. No new config key. No schema migration. The Console already shows `upload_state` and `checksum`, and the existing retry button appears for `UPLOAD_FAILED`.

## [v0.5.31] - 2026-10-05

### Fixed

- `GET /api/tasks/{id}/replay` with `stop_datetime`, and the same query on `GET /api/tasks/{id}/replay/archive`, no longer treats the source file's format description or previous-GTIDs timestamp as coverage. Those headers record when the source binlog was created. A `LATEST` backup, and any start in the middle of a file, writes that format description in front of the first copied event. Asking for a UTC time before that first copied event returns an empty window: `paths` is `[]` and `command` is empty. A time that contains a copied event still returns that file. The `mysqlbinlog` flags are unchanged. No new config key. No schema migration.

## [v0.5.30] - 2026-10-05

### Fixed

- A backup that starts in the middle of a source binlog, which is what `LATEST` does, writes the format description MySQL sends before the first copied event. `mysqlbinlog --verify-binlog-checksum` can read that file. The description's own end position (126 on MySQL 8, or 0) is not the checkpoint, and its timestamp is not lag. A quiet source that has not sent a later event still leaves the segment as the 4-byte magic header, so stop then start does not rewind to that description. A segment that already has events does not gain a second description on the next start. No new config key. No schema migration.

## [v0.5.29] - 2026-10-05

### Added

- Local disk and the bucket can keep a sealed binlog for different numbers of days. `storage.retention_days` stays required, range 1..3650. `storage.local_retention_days` and `storage.bucket_retention_days` are optional. Omitted or 0 uses `retention_days`, and those keys are omitted from the task JSON. When the effective local days and the effective bucket days are equal, that shared number is the one cutoff. A task that sets only `retention_days` is this case. An expired uploaded segment that is still on disk is removed from the object, the catalog row, and the local file together, as in v0.5.28. On that path a catalog row whose local file is already gone stays, and `sealed_at` and `uploaded_at` are not used. Create (`POST /api/tasks`) and update (`PUT /api/tasks/{id}`) reject a bucket window shorter than the local window. The comparison uses the effective days, so `local_retention_days` above `retention_days` with `bucket_retention_days` omitted is rejected too. `local_retention_days` below `retention_days` with `bucket_retention_days` omitted leaves the bucket window at `retention_days`, so the longer bucket window applies when object storage and a catalog are configured. Omitting `local_retention_days` and setting `bucket_retention_days` above `retention_days` does the same. The message contains `shorter than local retention`. Create returns JSON `code` `INVALID_REQUEST`. Update returns that sentence as plain text. Both are HTTP 400. A non-zero value outside 1..3650 is HTTP 400. Equal windows are accepted. The published v0.5.28 package has no split and still uses one cutoff. No schema migration. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

- The longer bucket window runs only with object storage and a catalog (`meta_dsn`). A sealed `UPLOADED` segment with an object key, older than the local window and still inside the bucket window, loses only the local file. The object and the catalog row stay. While the file is on disk, both windows use the local file modification time. If that time is older than the bucket window, the object, the catalog row, and the local file are removed together. Retention still runs when the replication loop opens a binlog file. The active open segment and every other open segment stay. `GET /api/tasks/{id}/files` sets `location` to `local`, `bucket`, or `both`. `bucket` means the catalog `file_path` is not on this process. `GET /api/tasks/{id}/files/{name}`, `GET /api/tasks/{id}/replay` (the limit window and `stop_datetime`), and `GET /api/tasks/{id}/replay/archive` still return the object bytes when this process has object storage configured. A process with no object storage configured still returns 404 `segment not found on this process` for a file that is not on disk. Replay `locations` follows `paths`. Do not pass a `bucket` path to `mysqlbinlog` until the bytes are downloaded. After the local file is gone, bucket age is `sealed_at`, then `uploaded_at` when `sealed_at` is empty. A row with neither timestamp stays. Once that timestamp is past the bucket window, the object and the catalog row are removed. `UPLOAD_FAILED` and `LOCAL_ONLY` are still kept when object storage and a catalog are configured. Age for that keep is the local file modification time, compared with the local window. Replication keeps running. `GET /api/tasks/{id}/events` gets one `RETENTION_SKIPPED_NOT_UPLOADED` per kept file. Opening another binlog does not append that event again for the same file while this process stays up. After a restart, the next retention pass that still keeps the file appends that event once more. The process that runs retention exposes `binlog_server_retention_blocked_files{task_id}` on `GET /metrics`. A control-plane-only process does not run retention, so the gauge stays 0 there. `LOCAL_ONLY` is still not picked up by the background upload retry or by `POST /api/tasks/{id}/files/retry-upload`. Without upload configured, the sealed file is still deleted by the local window, a catalog row stays when one exists, and that event is not written. Standalone without `meta_dsn` still deletes the local sealed file by the local age, including one that never reached the bucket. A longer bucket window is not applied without a catalog. Do not use that mode as the only copy when the bucket can be down longer than the local retention. If the object delete fails, the local file stays, `checksum` is left as it was, and `last_error` begins with `OBJECT_PURGE_FAILED`. Console task detail shows the effective local days and bucket days. The file table shows `location` (`local`, `bucket`, or `both`). A `bucket` segment shows the hint that the path is not on this machine. That Console is in the embedded bundle a plain `go build` serves.

## [v0.5.28] - 2026-10-05

### Fixed

- With object storage and a catalog (`meta_dsn`), age retention no longer deletes the only copy of an expired sealed binlog whose `upload_state` is `UPLOAD_FAILED` or `LOCAL_ONLY`. Age is still the local file modification time, compared with `storage.retention_days`. The local file and the catalog row stay. Replication keeps running. It does not set `last_error` and does not enter `RETRY_BACKOFF`. `GET /api/tasks/{id}/events` gets one `RETENTION_SKIPPED_NOT_UPLOADED` per kept file. The message names the file and its `upload_state`, for example `mysql-bin.000003 upload_state=LOCAL_ONLY`. Opening another binlog does not append that event again for the same file while this process stays up. After a restart, the next retention pass that still keeps the file appends that event once more. The process that runs retention exposes `binlog_server_retention_blocked_files{task_id}` on `GET /metrics`: how many of those expired files the latest successful pass is still keeping. The gauge drops when a later pass deletes them. After a restart it stays 0 until the next retention pass. A control-plane-only process does not run retention, so the gauge stays 0 there. The event remains on `GET /api/tasks/{id}/events`. The published v0.5.27 package still deletes that local file. No new config key. No schema migration.

- Once the row is `UPLOADED`, the next retention pass deletes the object, then the catalog row, then the local file. A sealed `UPLOAD_FAILED` row is still picked up by the background upload retry and by the existing manual retry `POST /api/tasks/{id}/files/retry-upload`. Either path can make that row `UPLOADED`. `LOCAL_ONLY` is not picked up by either path. A sealed file that never had object storage configured stays on disk past retention, and the gauge keeps counting it. Configuring object storage later does not upload that row. Already-`UPLOADED` rows, `OBJECT_PURGE_FAILED`, and local retention when upload is not configured are unchanged. Standalone without `meta_dsn` still deletes the local sealed file by age, including one that never reached the bucket. Do not use that mode as the only copy when the bucket can be down longer than `storage.retention_days`.

## [v0.5.27] - 2026-10-05

### Added

- An existing backup task can be asked for the binlog window covering a UTC stop time. `GET /api/tasks/{id}/replay` takes `stop_datetime` and an optional `start_datetime`. The window is half-open `[start, stop)`: the stop is excluded, and a start, when set, is included. The same query on `GET /api/tasks/{id}/replay/archive` downloads that selection as `task-{id}-replay.tar`. The response includes `command` only when `stop_datetime` is present. `mysql` uses `mysqlbinlog`, `mariadb` uses `mariadb-binlog`, and the command is prefixed `TZ=UTC`. Omitting both datetimes keeps the limit replay: `flavor`, `client`, `client_hint`, `paths`, and no `command` field. One path per source index still uses the sealed-versus-open rule. `limit` is not applied on the datetime path. `start_datetime` equal to `stop_datetime` is an empty window: HTTP 200, empty `paths`, empty `command`. It is not a 400. A bad datetime or a start after the stop is plain-text 400. A missing task is 404 `task not found`. The published v0.5.26 package has no stop-time window. No new config key. No schema migration. Restore the full backup yourself, then use the printed command. Binlog Server does not restore that backup.

- Console task detail has the same window: a UTC stop time, an optional start time, generate, copy, and download of `task-{id}-replay.tar`. That Console is in the embedded bundle a plain `go build` serves.

## [v0.5.26] - 2026-10-04

### Added

- A worker that has object storage configured retries sealed `UPLOAD_FAILED` segments in the background. Standalone and all-in-one processes are workers, so they run this loop. A cluster worker runs it too. A control-plane-only process does not. A process with no object storage configured does not run this loop. The loop uses the same path as the existing manual retry. A successful row becomes `UPLOADED`, including checksum, the same way a manual retry does. Open segments stay unuploaded. A sealed file that is not on this machine is skipped, and its catalog row is left unchanged. Upload failure still does not stop binlog pull and does not change task state. The default cadence is every 15 seconds, up to 100 sealed `UPLOAD_FAILED` rows per task per pass. The published v0.5.25 package leaves those rows at `UPLOAD_FAILED` until the retry API is called. No new config key. No schema migration.

- `POST /api/tasks/{id}/files/retry-upload` and `RetryFailedUploads(taskID string, limit int)` stay as they are. While a task's background pass is running, a manual retry of that task still returns `upload retry already in progress`. Retention, lease takeover, and the v0.5.25 object-read behavior are unchanged. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`.

## [v0.5.25] - 2026-10-04

### Fixed

- After a worker dies and its lease expires, another worker takes the task. The lease moves to the live worker. The segment directory stays at the `file_path` already stored on `binlog_files`. It does not follow the lease. When this worker can read that directory, it continues from the last complete event there. The same source file is not sealed a second time, and this worker does not open a fresh `{data_dir}/{task_id}/` and pull from position 4. When the open segment, or a sealed segment that is not yet `UPLOADED`, is not readable on this worker, and the checkpoint is not already inside a sealed `UPLOADED` object, the task becomes `FAILED`. `last_error` begins with `SEGMENT_NOT_ON_WORKER` and names the missing path. This worker does not create a new directory and keep pulling. Mount or copy that path onto this worker, then `POST /api/tasks/{id}/start`. Same-host kill -9, adopt, then start still uses this worker's own directory. The published v0.5.24 package does not continue from that directory. No new config key. No schema migration. `cluster.failover_policy` is still not a switch for this path.

- When the checkpoint is already inside a sealed `UPLOADED` object, this worker reads the object back with the upload client already configured, even when the local file is absent, and continues from the last complete event in that object. A checkpoint already on the next file at position 4, with every sealed row `UPLOADED`, still starts on that next file. Retention is unchanged: delete the object, then the catalog row, then the local file; `OBJECT_PURGE_FAILED` leaves the local file; the next open continues on the next file.

## [v0.5.24] - 2026-10-04

### Fixed

- When the replication loop opens a file, a sealed segment older than `storage.retention_days` (the local file modification time, not `sealed_at`) that is `UPLOADED` and has an object key is removed from the bucket, then its catalog row is removed, then the local file is removed. A segment still inside the retention window stays. An open segment is not deleted, and retention does not delete an object for one. Without upload configured, local retention is unchanged. The published v0.5.23 package deletes the local file and leaves the object in the bucket. No new config key. No schema migration.

- If deleting the object fails, the local file stays, `checksum` is left as it was (`match`, `mismatch`, or empty), and the row stays `UPLOADED`. Empty is not `match` and not `mismatch`. The task retries with `last_error` beginning `OBJECT_PURGE_FAILED`. An object that is already gone (HTTP 204 or 404) counts as success so the retry can finish. After a segment is sealed, the checkpoint moves to the next file before retention runs. If opening that next file then fails, the retry continues on the next file. It does not try to seal the file that was just sealed, so it does not stop on `sealed file already exists`. A task already stopped on `sealed file already exists`, with its checkpoint still on that sealed file, is not repaired by this release. Move that sealed file aside once so the next retry can continue.

## [v0.5.23] - 2026-10-04

### Added

- After a sealed segment is uploaded, the server HEADs the object once and compares its ETag with the sealed file. At or under 16MiB that is the whole-file MD5. Above that it is the same 16MiB multipart ETag, up to about 160GiB. There is no periodic re-check. The result is stored in the existing `binlog_files.checksum` column as `match` or `mismatch`. `GET /api/tasks/{id}/files` returns that value. The Console file table shows it in 校验. An open segment is not checksummed, so that cell is `--`. `match` means this ETag comparison found the same bytes. `mismatch` means the comparison finished and the bytes differ. `mismatch` stays `UPLOADED`. It is not treated as verified, and it does not stop replication. A failed upload stays `UPLOAD_FAILED` and the checksum is cleared. If the object HEAD fails, checksum stays empty on an `UPLOADED` row. Empty is not `match` and not `mismatch`. `GET /api/tasks/{id}/files` omits `checksum` when it is empty. The Console shows that cell as `--`. An uploader that cannot read the object also leaves checksum empty. Empty is not verified. A blank 校验 cell is neither a pass nor a fail. No new config key. No schema migration.

## [v0.5.22] - 2026-10-04

### Added

- A stopped task's Console detail shows two identities. Start (起点) is the configured start: `LATEST`, or `FILE_POS` as `file:pos`, or `GTID` as `gtid_set`. Resume (续传) is the position the next Start continues from. A stopped `LATEST` task still shows `LATEST` as its start. It does not get rewritten to `FILE_POS`. The resume line is the file and position of the last complete event in the highest local open segment. `GET /api/tasks/{id}/checkpoint` returns that same resume the runner uses on Start: `file` and `pos`. `gtid_set` is included only when the stored checkpoint is that same file and position. With neither a checkpoint row nor a complete local event, the response stays 404 `checkpoint not found`. The configured start stays on the task's `start` object, not inside the checkpoint JSON. No new config key. No schema migration.

### Fixed

- A stop that races the run exit no longer leaves the task stuck in `STOPPING`. A later write of an older `STOPPING` snapshot does not overwrite `STOPPED`.

## [v0.5.21] - 2026-10-04

### Added

- `GET /api/tasks/{id}/replay/archive` returns one `application/x-tar` (ustar) of the same `limit` and the same replay-file window as `GET /api/tasks/{id}/replay`. Each member is that window's basename, in that window's order, not a host path. One source index is still one member: a sealed name and `.open.e*` keep the highest-epoch open segment. Member bytes follow single-segment download: the local file wins, including an open segment while the task is `RUNNING` or `STOPPED`; a missing sealed file is read from object storage only when its catalog row is `UPLOADED` and `object_key` is non-empty. `Content-Disposition` is `attachment` with filename `task-{id}-replay.tar`. The route uses the same auth as other `/api/tasks/*` routes. An empty window is HTTP 200 and an empty ustar (1024 zero bytes). A missing task is 404 `task not found`. Every selected segment is opened before the tar is written; if one cannot be opened or read, the response is an error and is not a partial archive. `LOCAL_ONLY`, `UPLOAD_FAILED`, an empty `object_key`, an open segment with no local file, and a process with no object store configured stay 404 `segment not found on this process`. Console task detail has 下载回放集 next to the existing copy-replay action and uses the same `limit=80` as the task drawer. Single-segment download, the files list, and the JSON replay command are unchanged. No new config key. No schema migration.

## [v0.5.20] - 2026-10-04

### Added

- `GET /api/tasks/{id}/files/{name}` and the Console files-table Download stream a sealed segment that is already `UPLOADED` when `{data_dir}/{id}/{name}` is not on this process. The catalog row must include a non-empty `object_key`. `{name}` is that basename, not the object key. `Content-Type` is `application/octet-stream`. `Content-Disposition` uses that basename. The body length is the object size at open. A local file still wins, including `*.open.e*` while the task is `RUNNING` or `STOPPED`. `LOCAL_ONLY`, `UPLOAD_FAILED`, an empty `object_key`, an open segment with no local file, and a process with no object store configured stay 404 `segment not found on this process`. The response does not follow catalog `file_path` and does not invent open-segment bytes from object storage. A slash, backslash, `..`, or other traversal form is still 400 `invalid segment name`. A missing task is still 404 `task not found`. `GET /api/tasks/{id}/files` and `GET /api/tasks/{id}/replay` are unchanged: the files list still lists every name, and replay is still one path per source index. No new config key. No schema migration.

## [v0.5.19] - 2026-10-04

### Added

- `GET /api/tasks/{id}/files/{name}` streams the raw bytes of one on-disk basename already listed by `GET /api/tasks/{id}/files` (a sealed name or `*.open.e*`), over the authenticated API and the Console, without SSH onto the backup host. `Content-Type` is `application/octet-stream`. `Content-Disposition` uses that basename. The body length is the file size at open. A slash, backslash, `..`, or other traversal form is 400 `invalid segment name`. A missing task is 404 `task not found`. A basename that is not under this process's task directory is 404 `segment not found on this process`. A control plane with no local file does not invent bytes from the catalog path or object storage. An open segment can be downloaded while the task is RUNNING or STOPPED. The Console files table has Download on each row. `GET /api/tasks/{id}/files` still lists every name. `GET /api/tasks/{id}/replay` and the Console copy command are unchanged: one path per source index, and when a sealed name and `.open.e*` share an index the path is the highest-epoch open segment and keeps the `.open.e*` suffix. No new config key. No schema migration.

## [v0.5.18] - 2026-10-04

### Added

- `GET /api/tasks/{id}/replay` returns one on-disk path per source index, inside the same inventory window as `GET /api/tasks/{id}/files`. Paths are ascending. When a sealed name and one or more `.open.e*` files share an index, the path is the highest-epoch open segment and keeps the `.open.e*` suffix. A small `limit` keeps the highest indexes, not the newest `sealed_at`. `source.flavor` `mysql` sets `client` to `mysqlbinlog` and `client_hint` to `MySQL mysqlbinlog`. `mariadb` sets both to `mariadb-binlog`. An empty flavor leaves the client empty. An empty directory returns `paths: []`. The files API is unchanged and still lists every name. The Console task detail shows that command and copies it. No new config key. No schema migration. Stop, resume, adopt, and kill-9 resume are unchanged.

## [v0.5.17] - 2026-10-04

### Fixed

- With `meta_dsn`, `GET /api/tasks/{id}/files` and the Console files table list catalog rows in the same order as the standalone disk scan: ascending source index, and for one index the sealed name before `.open.e*` epochs. `limit` keeps the highest indexes. The no-meta disk scan is unchanged. No new API. No new config key. No schema migration.

## [v0.5.16] - 2026-10-04

### Fixed

- Kill the process while a backup is `RUNNING`. This is not an API stop. The on-disk backup then has no task metadata. Adopt it without starting, then start it. The dump continues from the end position of the last complete event in the open segment. It does not jump to the current `SHOW MASTER STATUS`. It does not restart at position 4. When that open segment's last complete event already ends at the resume position, the segment is renamed onto the new epoch and new events are appended, so one source filename spans the crash. The file still starts with the format description, keeps the events written before the process died, and includes events the source committed while the process was down, once each. There is no gap and no duplicate event at the boundary. v0.5.15 already does this for a clean stop. The published v0.5.15 package does not do it after `kill -9`: adopt then start opened a new epoch whose first bytes were the magic plus events after the offset, so the format description and the pre-crash events stayed in the old segment and `mysqlbinlog` could not replay the new file. Direct start of that backup, before adopt, is still rejected because there is no task metadata. A clean stop and resume is unchanged. `delay_seconds` of 0, an omitted sample, a dump preamble that is not lag, and the 30-second real-lag threshold are unchanged from v0.5.15. No new API. No new config key. No schema migration.

## [v0.5.15] - 2026-10-04

### Fixed

- Stop a `RUNNING` backup, let the source keep writing, then start the same task. Standalone with no `meta_dsn`, and a catalog task, both resume at the end position of the last complete event in the highest open segment. The dump does not jump to the current `SHOW MASTER STATUS`. It does not rewind to position 4 and delete that segment. A dump that started mid-file does not resume at the file size. The stopped open segment is renamed into the new epoch and appended, so one source filename spans the stop, with no gap and no duplicate event at the boundary. A partial event past that end position is dropped before the append. A catalog takeover with no complete event on disk still starts at position 4. Standalone with no checkpoint and no complete event still starts at the current master status. `KeepLocalSegments` adopt still uses the saved file size and opens a new epoch. A task already at the source tip still reports at tip. `delay_seconds` of 0, an omitted sample, a dump preamble that is not lag, and the 30-second real-lag threshold are unchanged from v0.5.14. No new API. No new config key. No schema migration.

## [v0.5.14] - 2026-10-03

### Fixed

- A caught-up `FILE_POS` resume, including standalone adopt at the highest segment size, no longer reports `DELAY_EXCEEDS_THRESHOLD` from the dump preamble. MySQL sends the binlog format description before any new event, with `log_pos` 0 or the original end position (126 on MySQL 8). That header time is when the file was created. The preamble is not written and does not move the cursor. If the resume position is already at `SHOW MASTER STATUS`, `delay_seconds` is 0 / `NORMAL` as soon as StartSync succeeds. Catch-up that is still behind the tip still uses `now - last_event_at`.
- A caught-up RUNNING task includes `delay_seconds` as JSON `0` on `GET /api/tasks/{id}/replication` and the dashboard replication object. `omitempty` on `int64` was dropping that zero, so the Console showed `--` seconds while status stayed `NORMAL`. A task with no event-time sample still omits the field. The 30-second threshold, `DELAYED`, and the dump-preamble behavior are unchanged. No new config key. No schema migration.

## [v0.5.13] - 2026-10-03

### Added

- The Console is the surface on top of `POST /api/tasks/{id}/adopt` already in v0.5.11. The published v0.5.12 package still has no Console adopt button. Standalone with no `meta_dsn`: a leftover directory is listed. In the Chinese Console its detail actions are 认领, 启动, 停止, and 删除, and there is no 编辑. A task that already has source credentials shows 编辑, 启动, 停止, and 删除, and no 认领. 认领 submits `cluster_key` and source. Leaving the start mode at the default (highest segment end) does not send an override. The success toast is 任务已认领. The task stays 已停止, shows the source and cluster key, does not show the password, and does not start replication. The existing 启动 action then starts it. Dogfood on tip `39981ead` (`v0.0.0-20261003150852-39981ead3fd5`) saw `mysql-bin.000005.open.e2` in the same directory while the earlier segment sizes stayed unchanged. With `meta_dsn`, leftover directories are not listed (the Console table is empty; ids 99 and 42 do not appear) and there is no 认领. `POST /api/tasks/99/adopt` returns 404 with body `task not found`. `backup_tasks` stays 0. Adopt does not create a task from a directory. The API is unchanged. No schema migration. No new config key.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.13`.

## [v0.5.12] - 2026-10-03

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.12`.

### Docs

- This patch only corrects the shipped guide. Behavior is unchanged from v0.5.11. English `docs/guide/admin/deployment.md` now says that adopting an id that is not a catalog task returns 404 with body `task not found`. Chinese section 7.4 describes the same adopt path: the leftover directory is listed, adopt before start, 200 and `STOPPED`, the password is not returned, an omitted start is `FILE_POS` at the highest segment name and size, start is 204 in the same directory, and with `meta_dsn` a directory that is not a catalog task is 404 `task not found`. The published v0.5.11 tarball still has the old Chinese 7.4 and the English sentence that omits 404. The Console has no adopt button.

## [v0.5.11] - 2026-10-03

### Added

- Standalone with no `meta_dsn`: `POST /api/tasks/{id}/adopt` attaches `cluster_key` and source credentials to a leftover `{data_dir}/{id}/` directory and keeps that id. The response is 200, password is omitted, and the state stays `STOPPED`. When `start` is omitted, the saved position is `FILE_POS` at the size of the highest sealed or `.open.e*` segment. An explicit `start.mode` overrides that. `POST /api/tasks/{id}/start` then returns 204 and writes the next epoch in the same directory. Existing sealed and open segments stay. Before adopt, update and start still return 400 `on-disk backup has no task metadata`. A configured `meta_dsn` still does not discover directories from disk. No new config key. No schema migration.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.11`.

## [v0.5.10] - 2026-10-03

### Fixed

- Standalone with no `meta_dsn`, after the process exits and a new process starts on the same `data_dir`: leftover `{data_dir}/{task_id}/` directories that still contain sealed or `.open.e<epoch>` segments show up in `GET /api/tasks` and the Console without a known id. The id is the directory name. `GET /api/tasks/{id}/files` uses the existing disk-scan contract (`file_name` is the source name, an open `file_path` keeps `.open.e<epoch>`, `start_pos` and `end_pos` are 0, ascending source index). Checkpoint stays 404 `checkpoint not found`. The row has no source credentials. Start returns 400 with body `on-disk backup has no task metadata` and does not start replication. A configured `meta_dsn` does not discover directories from disk. A non-empty `binlog_files` catalog still wins, and extra files on disk are not listed. No new config key. No schema migration.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.10`.

### Docs

- `docs/guide/admin/configuration.md` no longer says that `GET /api/tasks/{id}/files` returns `[]` when `meta_dsn` is unset. This is a guide correction, not a behavior change. The published v0.5.9 tarball still has that sentence.

## [v0.5.9] - 2026-10-03

### Fixed

- Standalone with no `meta_dsn`, or a meta catalog with no `binlog_files` rows for that task: `GET /api/tasks/{id}/files` scans `{data_dir}/{task_id}/` and returns sealed files and `.open.e<epoch>` segments. `file_name` is the source file name. `file_path` is the on-disk path and includes `.open.e<epoch>` for an open segment. Order is ascending source index; the same index lists the sealed name, then open epochs. `limit` keeps the highest indexes. `start_pos` and `end_pos` are 0 because the disk scan has no offsets. Checkpoint stays 404 `checkpoint not found` when there is no checkpoint row. A non-empty `binlog_files` catalog is unchanged (`sealed_at` descending) and is not replaced by the disk scan. A store error does not fall back to disk. The Console task files table shows the on-disk name and `file_path`.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.9`.

### Docs

- The replay section says a MySQL source requires MySQL's own `mysqlbinlog`, and a MariaDB source requires `mariadb-binlog`. If `mysqlbinlog --version` prints MariaDB, do not use it against MySQL: the pipe fails at `check_constraint_checks` with error 1193 and no rows land. The files are not corrupt. The v0.5.9 archive includes `docs/guide`.

## [v0.5.8] - 2026-10-03

### Fixed

- With `api.auth.enabled=true` and a bearer token, `GET /ui/` and the Console assets load without an `Authorization` header. Settings collects the bearer token, and later API calls send `Authorization: Bearer`. `/api/*`, `/metrics`, and `/swagger/*` still require that credential. `/healthz` stays open. Config keys are unchanged.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.8`.

### Docs

- The admin guide explains replaying on-disk segments with `mysqlbinlog` or `mariadb-binlog`. The path is `{data_dir}/{task_id}/mysql-bin.NNNNNN`. A `.open.e<epoch>` name is a filename suffix; the tool reads that path as-is. Standalone without `meta_dsn` returns `[]` from the files API and 404 for checkpoint, so replay the disk. Object storage keeps only sealed successful uploads. The v0.5.7 archive omitted `docs/guide`. The v0.5.8 archive includes it, so the replay section is in the unpacked package.

## [v0.5.7] - 2026-10-02

### Fixed

- Task reads (`GetTask`, task lists, list counts, checkpoints) retry transient meta MySQL errors and recycle pooled connections, so a brief meta failover does not return 500. A missing task is still not found.
- A `flavor=mysql` probe that finds no `@@server_uuid` stays `SOURCE_IDENTITY_UNAVAILABLE` and tells the operator the source looks like MariaDB and to set `flavor=mariadb`. `flavor=mariadb` identity is unchanged.
- A heartbeats-less local process is not an offline Worker. Overview reports `worker_count=0` and `single_process=true`, `GET /api/workers` is empty, and the Console says this process pulls tasks.
- The embedded Console create-task and batch-create Flavor dropdown renders again. The hint no longer contains a raw `@@server_uuid` token, which vue-i18n@9 rejects while compiling production messages. CI compiles locale catalogs before the UI build.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.7`.
- Landing console screenshots ship as PNG assets. CI and the release workflow run `scripts/check-landing-assets.sh`.
- The landing page and production template describe the Day-1 install and first backup path.

## [v0.5.6] - 2026-10-02

### Added

- `scripts/failover-dogfood.sh` and the verify-binlog-server failover-lease skill prove dual-worker lease takeover. The lease state machine is unchanged.
- A project-local verify-binlog-server agent skill drives a released binary through the HTTP API and the embedded Console.

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.6`.
- `GET /api/dashboard` and `GET /api/summary` count states and sources with SQL `GROUP BY` and page rows with `LIMIT/OFFSET` instead of scanning every matching task row.
- When `api.auth.enabled=true`, `/ui/*` and `/swagger/*` use the API auth middleware. `/healthz` stays open.
- Go modules: validator `v10.30.5`.

### Fixed

- The embedded Console bundle parses again. `v0.5.5` shipped a stray `}` in the entry import, so Chrome threw `SyntaxError` and the shell never mounted. CI and the release workflow run `scripts/check-ui-bundle.sh` (`node --check --input-type=module`).

### Security

- `PRODUCTION=true` refuses to start when `--encryption-key` is empty.

## [v0.5.5] - 2026-09-21

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.5`.
- Task-observation host filters and source lookup share `SameSourceHost`; loopback spellings are one source.
- Cluster overview, worker task counters, and metrics task/owner views read the unfiltered store ownership copy.
- The console task page requires dashboard paging fields and no longer locally re-pages a legacy payload.
- Console list refresh uses the task owner/epoch copy for lease risk instead of per-row `/lease`.
- Go modules: validator `v10.30.4`, MySQL driver `v1.10.1`, migrate `v4.20.1`, `golang.org/x/time` `v0.16.0`.

### Fixed

- `GetTask` fails loud on store miss or store error instead of returning a stale in-memory ownership copy.
- Source lookup reads the store ownership copy instead of the boot-time memory list.
- Retry-upload E2E pulls MinIO from Quay so CI is not blocked on Docker Hub `minio/minio:latest`.

### Docs

- README includes Console, task detail, and Swagger screenshots.

## [v0.5.4] - 2026-09-07

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.4`.
- Standalone workers inject an in-process lease table and `worker_id=standalone`, so standalone and cluster share the same ownership door.
- Process boot and the claim tick both call `ClaimRunnableTasks` instead of a separate resume path.
- First sealed-file upload verifies the current lease, then shares `ApplySealedUpload` with retry-upload.
- Dashboard and summary load matching tasks with one filtered read (`Limit<=0` unbounded), then page in process.

### Fixed

- `FAILED` releases the lease immediately so another worker can start the task without waiting for TTL.
- Expired-lease and failed-upload lookups fail loud when the store does not implement the dedicated query, instead of silently scanning an empty result.

## [v0.5.3] - 2026-09-06

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.3`.
- Control-plane `listen_addr` that is not loopback (`:8080`, `0.0.0.0:8080`, and any non-`127.0.0.1`/`localhost`/`::1` bind) now requires `api.auth.enabled`, `protect_api`, and `protect_metrics` at `App.Run`. Loopback binds may stay unauthenticated for local demo. `PRODUCTION=true` still fail-closes independently and is not weakened.
- When `api.auth.enabled=true`, unset `protect_api` / `protect_metrics` now default to true. Explicit `false` is still honored at config load, but non-loopback listen and `PRODUCTION=true` still reject that combination. `listen_addr` default remains `:8080`.
- `GET /api/tasks` and dashboard task pages filter and page in SQL (`COUNT` + `LIMIT/OFFSET`). `GetTask` uses the primary key. Worker claim ticks query unowned `STARTING` instead of loading every task. Dashboard summary/sources still aggregate in-process delay. Public `{items,total,limit,offset}` shape is unchanged.
- OpenTelemetry Go API/SDK/trace/OTLP HTTP exporter moved from 1.45.0 to 1.46.0.

### Security

- Source passwords in `backup_tasks.source_json` are encrypted with AES-256-GCM (`enc:aes256:`) when `--encryption-key` is provided. Other source fields stay plaintext JSON. Without a key, plaintext persist is unchanged so existing deploys keep starting.

### Fixed

- Cluster workers claim `RUNNING`/`LEASE_DEGRADED`/`RETRY_BACKOFF` tasks whose lease has expired and resume dump after a successful Acquire, without going through `StopTask`.
- A newly started cluster worker no longer Stop+Starts another worker's live `RUNNING` task, which previously persisted `STOPPED` while dump continued on the original owner.
- FILE_POS/GTID catch-up no longer sticks `atTip` (and therefore `delay_seconds=0`) after a quiet 2s idle gap. Tip is confirmed against `SHOW MASTER STATUS` / `SHOW BINLOG STATUS`. Fresh LATEST start at tip is unchanged.
- Release tarballs include `config.example.yaml` and `config.production.example.yaml`, matching the documented production start path.

## [v0.5.2] - 2026-08-30

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.2`.

### Fixed

- LATEST start at the source tip no longer reports `DELAYED` from a days-old binlog event header. `delay_seconds` is 0 / `NORMAL` as soon as StartSync succeeds; FILE_POS/GTID catch-up that is still behind the tip keeps real lag. `last_event_at` may still show the last event header time.
- Task list and dashboard pages sort numeric string ids as integers, so page 1 is `1,2,3` rather than `1,10,100`.

## [v0.5.1] - 2026-08-30

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.1`. They still showed `v0.4.3` after the `v0.5.0` tag.

### Notes

- Runtime behavior is unchanged from `v0.5.0`. Isolated `make e2e-scale` (1000 control-plane tasks / 100 live streams) and production-template bearer auth were recorded locally; one MySQL fixture supplied the dump clients, so this is not independent-cluster capacity.

## [v0.5.0] - 2026-08-30

### Added

- `POST /api/tasks/batch` accepts 1..100 task-create requests and returns ordered per-item success or structured error results, so a valid batch can report partial success without stopping later items.
- Task and dashboard queries support bounded `limit`/`offset` paging and `state` filtering. Dashboard summary and source aggregates cover the complete filtered result, while task details are paged; the frontend uses server paging.
- Production deployments can start from `config.production.example.yaml`, which enables bearer authentication and protects both `/api/*` and `/metrics`; unresolved credential placeholders are rejected. An opt-in `make e2e-scale` harness writes a JSON evidence report for its isolated 1000-control-task / 100-live-stream scenario.

### Changed

- **API contract change:** `GET /api/tasks` now returns `{items,total,limit,offset}` instead of a raw JSON array `[...]`. Upgrade API clients to read `items` and use the returned page metadata before deploying `v0.5.0`.
- Summaries report `STARTING` separately from `RUNNING`.

### Fixed

- Unreachable replication sources now fail after bounded retries instead of retrying indefinitely; disconnected source streams follow the bounded retry path.

## [v0.4.3] - 2026-08-30

### Fixed

- Metadata/source isolation now treats `localhost`, IPv4 loopback literals in `127/8`, and IPv6 loopback literals such as `::1` as one same-port endpoint identity without DNS resolution; create, update, and start reject aliases, and source lookup returns the same matches.

## [v0.4.2] - 2026-08-21

### Fixed

- Create task now validates the full spec before persist. HTTP 400 no longer leaves a `LATEST` task behind.
- `flavor=mariadb` probes `server_id` + `gtid_domain_id` instead of MySQL-only `@@server_uuid`. `log_bin=off` fails as `SOURCE_LOG_BIN_OFF`.
- Access denied (ERROR 1045) is `SOURCE_ACCESS_DENIED` and enters `FAILED` instead of infinite retry. `POST /start` can restart a `FAILED` task after the operator fixes credentials.
- Silent masters no longer report multi-hour `DELAYED` lag: an idle dump wait treats lag as 0 / `NORMAL`. Heartbeat events are not written into backup files.
- Bind failures no longer dump cobra `Usage`.
- `GET /api/health` returns `{"status":"ok"}` (keep `/healthz`).

### Changed

- Quick Start is download/verify/extract/`./binlog-server`. `go run` moved to Development.
- Standalone without `meta_dsn` is documented as in-memory control plane. Restartable tasks need `meta_dsn`.

## [v0.1.2] - 2026-03-27

### Added

- Frontend development mock mode for Vite dev with shared scenario assets reused by Playwright E2E.
- New frontend mock scenarios for cluster/lease resilience coverage, including control-plane-down worker-running views.
- Workspace-C planning docs for ops console IA redesign in `docs/develop/plans/2026-03-27-ops-console-workspace-c-*.md`.

### Changed

- Reorganized the ops console into workspace-oriented views (`overview`, `tasks`, `sources`, `workers`, `alerts`) with deep-link routing semantics.
- Moved task filters to task/alert context and source lookup to source context to reduce cross-page cognitive load.
- Updated left navigation to full-height docked behavior with bottom-docked collapse control and compact icon alignment in collapsed mode.
- Normalized KPI navigation behavior so task-oriented metrics consistently route to task-focused workflows.
- Adjusted lease risk evaluation to use dashboard reference time in mock-driven views, avoiding false-risk inflation from local clock skew.

## [v0.1.1] - 2026-03-25

### Added

- Playwright-based frontend E2E acceptance coverage for empty-state, KPI filtering, task drawer, retry-upload, and auth-required flows.

### Changed

- Reworked the embedded web UI into a more operator-focused console with alert-first KPI hierarchy, reduced row-action noise, and a stronger detail drawer workflow.
- Localized frontend auth guidance and added in-app settings-driven recovery for `401` API responses.
- Added retry-upload affordance in the task detail drawer and refreshed embedded static UI assets.

## [v0.1.0] - 2026-03-09

### Added

- Root `SECURITY.md` security policy for vulnerability reporting.
- Root `.golangci.yml` baseline lint configuration.
- CI vulnerability scanning with `govulncheck`.

### Changed

- Foundation hardening work completed across auth, timeout governance, retry standardization, SQL access generation, API validation, Prometheus metrics, and OpenTelemetry tracing.

## [2026-03-08]

### Added

- Closure snapshot for the foundation hardening program in `docs/develop/plans/2026-03-08-foundation-hardening-closure.md`.
