# Binlog Server v0.5.74

Release date: 2026-10-19

v0.5.74 is ADR 0009 P2-5.4 (#434). It fixes #341 and #353. With upload configured, local retention now writes `RETENTION_REMOVED` and an optional catalog marker for each file it deletes. If that record cannot be written, `RETENTION_RECORD_FAILED` opens. When migration 000008 is rolled down and back up online, the same process writes seal facts again without a restart, and `SEAL_FACTS_ABSENT` reports the gap. One new optional migration (000010); the schema floor stays 6 (`minRequiredSchemaVersion` 6) and the upgrade floor stays v0.5.51. No new config key.

## Upgrade Notes

- **Optional migration 000010** (`binlog_files.retention_removed_at DATETIME(6) NULL`, additive). `./migrate up` to 10 is recommended when upload is configured and local retention is shorter than bucket retention. Schema 6–9 keep working: without the column, one WARN is logged and the event is still written. v0.5.73 runs on schema 10 unchanged, so you can roll the binary back after migrating up (passed QA, standalone and cluster).
- **Catch-up events on first start.** The first retention pass after the upgrade writes one `RETENTION_REMOVED` (and, with 000010, the marker) for every uploaded, checksum-match catalog row in this task directory whose local file is already gone and that is still inside the bucket window. That includes files an older binary deleted without an audit record. Expect a one-off burst. In QA, 4 such files were recorded about 6 s after the first pass, standalone and cluster rolling.
- **Without 000010, a restart repeats those events.** Idempotency is held in process memory only, so after each restart the first catch-up writes another `RETENTION_REMOVED` for each such row. These are duplicate audit events; no finding opens and no backup state changes. To deduplicate, take the earliest event per `(task_id, message)`, or migrate up to 10.
- The new kinds are produced by the health evaluator, which needs `health.evaluator: on` (default `off` in v0.5.x; it becomes the default in v0.6.0). With `off`, the events and markers are still written and seal facts still resume.
- From v0.5.73, run `./migrate up` (optional) and replace the binary; a rolling restart of a cluster from v0.5.73 passed QA. The v0.5.73 notes still apply when upgrading from v0.5.72 or earlier, the v0.5.72 notes from v0.5.71 or earlier, and the v0.5.67 notes from v0.5.66 or earlier.

## Fixes

- **#341: local retention with upload left no audit record** (#434). When `local_retention_days` is shorter than `bucket_retention_days`, retention deletes the local copy of an uploaded, checksum-match segment that is still inside the bucket window, and keeps the object and the catalog row. That delete wrote no `RETENTION_REMOVED`, so nothing recorded why the local window got shorter. Now the event is written first, then `retention_removed_at`. If the event or the marker cannot be written, `RETENTION_RECORD_FAILED` (WARNING, subject = file) opens, and it closes after the next retention pass that finishes without a record failure. A later pass catches up rows whose record is missing. A lease handoff (`ErrWriteFenced`) is not a record failure; the new owner catches up.
- **#353: seal facts stayed NULL after an online 000008 down/up** (#434). After 000008 was rolled down and back up online, the same process kept writing new sealed rows with `first_event_at`, `last_event_at` and `sha256` set to NULL until a restart, and logged nothing. Each seal and each evaluator sweep now probes the columns again. One WARN is logged when they go missing and one line when writes resume. Rows sealed while the columns were missing are not backfilled. Nothing downstream misreads them: the rehasher falls back to the `.seal-sha256` ledger, and `/window`, `/replay` and restore read event times and GTIDs from the segment bytes.

## Added

- **Managed `ORPHAN_ROW`** (WARNING, two sweeps to open). A row with the retention marker never raises `ORPHAN_ROW`. If a fully listed prefix shows the object of a marked row is missing, `SEGMENT_MISSING#object` (CRITICAL) opens instead, because the object was the remaining copy. Nothing is judged on an incompletely listed prefix. When object storage is removed from the config, an open `ORPHAN_ROW` closes with reason `CONFIG`.
- **`SEAL_FACTS_ABSENT`** (WARNING, opens within one sweep). It opens when the 000008 columns are missing and either the schema is at least 8 or this process has seen the columns before. It closes on the next probe that finds them. A deployment that has always stayed on schema 6 or 7 never sees it.
- **`RETENTION_RECORD_FAILED`** now also covers local retention with upload configured, not only the no-upload path.
- **Write fence.** The `RETENTION_REMOVED` event, the marker UPDATE and the seal-fact UPDATE run in the same transaction as the `task_leases` row lock when the run carries a lease token. A frozen old owner that wakes after a takeover writes nothing (QA: SIGSTOP and kill -9 handoffs). Deleting the local file first checks the local lease clock. `AppendEvent` without a token (the scheduler's event lane, the finding sink) stays a single INSERT with no retry, so a meta outage does not hold the ordered event lane through retry backoff.

## Known issues

- **#436 (P2): online rollback below 000009 makes the evaluator silent.** After `migrate goto 7` or `8` online, the health tables are gone. The evaluator only logs at INFO, cannot open any finding (including `SEAL_FACTS_ABSENT`), and `/api/health` stays `ok`. Do not roll meta back below 9 with the evaluator on.
- **#437 (P3):** after an online `migrate up` to 000010, rows deleted before the up are not marked until the next restart, which also writes a duplicate event. Until then, if such a row's object goes missing, it reports `ORPHAN_ROW` (WARNING) instead of `SEGMENT_MISSING#object` (CRITICAL).
- **#438 (P3):** retention deletes by file mtime, but the catch-up ages rows by the catalog's `sealed_at`. For a file whose mtime is older than its `sealed_at` (copied, restored or adopted files), a failed or interrupted record may never be caught up.
- **#441 (P3):** a false CRITICAL `CHECKPOINT_AHEAD` opens for 1–2 s when the source rotates its binlog, then closes on its own. Present since v0.5.73.
- `/api/findings` has no read timeout yet (P2-7).

## Changes since v0.5.73

`git log --oneline --first-parent v0.5.73..9bd6c9a`, plus the merge of this notes PR, which v0.5.74 is tagged on:

| Commit | Change |
|---|---|
| `1970a79` | PR #432: install docs point at v0.5.73 |
| `9bd6c9a` | PR #434: P2-5.4 retention marker (000010), `RETENTION_REMOVED` with upload, managed `ORPHAN_ROW`, `SEAL_FACTS_ABSENT` (#341, #353) |

## QA summary

PR #434 QA PASS (BinlogServerQA), 2026-10-19, `-race` build, MariaDB 11.4 meta, moto for S3, local retention 1 day / bucket 7 days, evaluator on:
- schema matrix 6/7/8/9: without 000010, one event per deleted file, no `RETENTION_RECORD_FAILED`, 0 warnings; online up to 10, then events and markers; online down again, retention continues; v0.5.73 runs on schema 10;
- #353: dropping only the 000008 columns opened `SEAL_FACTS_ABSENT` within one sweep and closed it within one sweep after they returned; seal facts were written again. A byte flipped in a segment with NULL seal facts opened `SEGMENT_UNREADABLE` within 5 s (ledger fallback);
- orphan matrix: marked row with its object deleted gave `SEGMENT_MISSING#object` only; unmarked row with both copies gone gave `ORPHAN_ROW`; an erroring listing judged nothing for 72 s; removing upload closed everything with `CONFIG`;
- meta refused and blackholed mid-retention (realistic ages): every deleted file got exactly one event and one marker after recovery; `RETENTION_RECORD_FAILED` opened and closed as designed; `/api/health` stayed bounded;
- lease handoff mid-retention (SIGSTOP and kill -9 of the owner): the old owner was fenced (no event, marker or delete); the new owner caught up the old owner's file; 6/6 files recorded exactly once;
- upgrade from the v0.5.73 release, standalone and cluster rolling, with and without 000010: catch-up within about 6 s, no duplicates while running, restore from S3 matches the source, rollback to v0.5.73 runs;
- idle 45 min, 2-node cluster: 0 findings, 0 races, 0 panics; meta load +4% (the per-sweep seal-fact probe, selects only).

Tip QA 9bd6c9a PASS (BinlogServerQA), 2026-10-19:
- CI green;
- idle 2-node cluster 30 min (`-race`, schema 10, upload, retention 1 day / 7 days): 30/30 samples clean, 0 findings, 0 transitions, 0 races, 0 panics, health ok on both nodes, meta load flat;
- smoke, standalone and 2-node cluster with rolling restart: all segments uploaded, 0 sha mismatches, GTID without gaps or duplicates, restore from S3 matches the source;
- upgrade from the v0.5.73 release and rollback, 4 variants (standalone and cluster, with and without 000010): catch-up of 4 events (and 4 markers with 000010) about 6 s after the first pass, restore matches, v0.5.73 runs after rollback;
- retention with realistic ages, standalone and cluster: each of 5 deleted files has exactly one event and one marker, the object stays, 0 `RETENTION_RECORD_FAILED`, 0 `ORPHAN_ROW`;
- meta blackholed 15 s, 2-node cluster: every poll degraded with `META_UNREACHABLE` within 1.006 s, one record per node within 0.03 s of the fault window, no `TASK_NO_PROGRESS`;
- console: HTTP-level checks only (`/ui/`, Swagger, task API, `/api/findings`, `/api/health`, `/metrics`), not rendered in a browser.
- The transient `CHECKPOINT_AHEAD` at rotation seen in QA is #441, present since v0.5.73.

CI passed on `9bd6c9a`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.74 中文发布说明

> 对应版本：`v0.5.74`

发布日期：2026-10-19

v0.5.74 是 ADR 0009 P2-5.4（#434），修复 #341 和 #353。配了上传时，本地保留每删一个文件都会写 `RETENTION_REMOVED`，并写一个可选的目录标记；记不下来时打开 `RETENTION_RECORD_FAILED`。在线把迁移 000008 降下去再升回来后，同一进程不用重启就会恢复写封存事实，缺列期间由 `SEAL_FACTS_ABSENT` 报出。新增一个可选迁移（000010）；schema 下限仍是 6（`minRequiredSchemaVersion` 6），升级地板仍是 v0.5.51。没有新配置项。

## 升级须知

- **可选迁移 000010**（`binlog_files.retention_removed_at DATETIME(6) NULL`，加法）。配了上传、且本地保留短于桶保留时，建议 `./migrate up` 到 10。schema 6–9 照常可用：没有这一列时打一条 WARN，事件照写。v0.5.73 在 schema 10 上照常运行，所以迁移之后仍可以把二进制回退（standalone 与集群都在 QA 中通过）。
- **第一次启动会补写事件。** 升级后的第一轮保留会给本任务目录里每一条「已上传、checksum match、本地文件已经不在、仍在桶窗口内」的目录行写一条 `RETENTION_REMOVED`（有 000010 时同时写标记），包括旧二进制删掉却没留审计的文件。预期会有一次性的一批事件。QA 中 standalone 与集群滚动升级都在第一轮后约 6 秒补齐了 4 个文件。
- **没有 000010 时，重启会重复写这些事件。** 幂等只记在进程内存里，所以每次重启后的第一轮补写会给每条这样的行再写一条 `RETENTION_REMOVED`。重复的是审计事件，不开 finding，也不改备份状态。去重请按 `(task_id, message)` 取最早一条，或者迁移到 10。
- 新的 kind 由健康评估器产生，需要 `health.evaluator: on`（v0.5.x 默认 `off`，v0.6.0 起默认 `on`）。保持 `off` 时事件和标记照写，封存事实也照样恢复写入。
- 从 v0.5.73 升级：（可选）`./migrate up`，然后换二进制重启；集群从 v0.5.73 滚动升级已在 QA 中通过。从 v0.5.72 或更早升级仍按 v0.5.73 的升级须知操作，v0.5.71 或更早按 v0.5.72，v0.5.66 或更早按 v0.5.67。

## 修复

- **#341：配了上传时本地保留删除不留审计**（#434）。`local_retention_days` 短于 `bucket_retention_days` 时，保留会删掉仍在桶窗口内、已上传且 checksum match 的分段的本地副本，对象和目录行留下。这次删除不写 `RETENTION_REMOVED`，本地窗口为什么变短没有任何记录。现在先写事件，再写 `retention_removed_at`。事件或标记写不进去时打开 `RETENTION_RECORD_FAILED`（WARNING，subject 为文件名），下一轮没有记录失败的保留结束后关闭。之后的一轮会补写缺失的记录。租约移交（`ErrWriteFenced`）不算记录失败，由新 owner 补写。
- **#353：在线 down/up 000008 后封存事实一直是 NULL**（#434）。在线把 000008 降下去再升回来后，同一进程直到重启都把新封存行的 `first_event_at`、`last_event_at`、`sha256` 写成 NULL，也不打日志。现在每次封存和每轮评估都会再探这些列，缺列时打一条 WARN，恢复写入时再打一行。缺列期间封存的行不回填，但下游不会因此误判：复核器回退到 `.seal-sha256` 账本，`/window`、`/replay` 和恢复从段文件本身读事件时间与 GTID。

## 新增

- **`ORPHAN_ROW` 改由评估器管理**（WARNING，连续两轮才打开）。带保留标记的行永远不报 `ORPHAN_ROW`；前缀列举完整、确认标记行的对象不在时，改报 `SEGMENT_MISSING#object`（CRITICAL），因为对象是剩下的那份副本。前缀没列举完时什么都不判断。配置里去掉对象存储后，已打开的 `ORPHAN_ROW` 以 `CONFIG` 原因关闭。
- **`SEAL_FACTS_ABSENT`**（WARNING，一轮内打开）。000008 的列不在，且 schema ≥ 8 或本进程见过这些列时打开；下一次探到列在时关闭。一直停在 schema 6 或 7 的部署不会出现。
- **`RETENTION_RECORD_FAILED`** 现在也覆盖配了上传时的本地保留，不只是没配上传的路径。
- **写栅栏。** run 带租约令牌时，`RETENTION_REMOVED` 事件、标记 UPDATE 和封存事实 UPDATE 与 `task_leases` 行锁在同一事务里。接管之后醒来的旧 owner 什么都写不进去（QA：SIGSTOP 和 kill -9 移交）。删本地文件前先问本地 lease 时钟。没有令牌的 `AppendEvent`（调度器事件通道、finding 汇）仍是一条 INSERT、不重试，meta 中断不会让有序事件通道卡在重试退避里。

## 已知问题

- **#436（P2）：在线回滚到 000009 以下后评估器静默。** 在线 `migrate goto 7` 或 `8` 后健康表没了，评估器只打 INFO 日志，任何 finding（包括 `SEAL_FACTS_ABSENT`）都开不了，`/api/health` 仍是 `ok`。评估器开着时不要把 meta 回滚到 9 以下。
- **#437（P3）：** 在线 `migrate up` 到 000010 之后，up 之前删掉的行要到下一次重启才写标记，重启时还会多写一条事件。在那之前，这些行的对象如果丢了，报的是 `ORPHAN_ROW`（WARNING）而不是 `SEGMENT_MISSING#object`（CRITICAL）。
- **#438（P3）：** 保留按文件 mtime 删，补写按目录的 `sealed_at` 判断年龄。文件 mtime 比 `sealed_at` 早时（拷贝、恢复或收养来的文件），一次失败或中断的记录可能永远补不上。
- **#441（P3）：** 源端轮转 binlog 时，CRITICAL `CHECKPOINT_AHEAD` 会误报 1–2 秒后自动关闭。v0.5.73 起就有。
- `/api/findings` 还没有读超时（P2-7）。

## 自 v0.5.73 以来的变更

| 提交 | 变更 |
|---|---|
| `1970a79` | PR #432：安装文档指向 v0.5.73 |
| `9bd6c9a` | PR #434：P2-5.4 保留期删除标记（000010）、配了上传时写 `RETENTION_REMOVED`、`ORPHAN_ROW` 由评估器管理、`SEAL_FACTS_ABSENT`（#341、#353） |

## QA 摘要

PR #434 QA PASS（BinlogServerQA），2026-10-19，`-race` 构建、MariaDB 11.4 meta、moto 模拟 S3、本地保留 1 天 / 桶 7 天、评估器 on：schema 6/7/8/9 矩阵，没有 000010 时每个删除的文件一条事件，没有 `RETENTION_RECORD_FAILED`，0 条警告，在线 up 到 10 后有事件和标记，再在线 down 回去保留照常，v0.5.73 能在 schema 10 上运行；#353 只删 000008 的列时一轮内打开 `SEAL_FACTS_ABSENT`，列回来后一轮内关闭并恢复写入，封存事实为 NULL 的段翻转一个字节 5 秒内打开 `SEGMENT_UNREADABLE`（账本回退有效）；孤儿矩阵中，有标记且对象被删只报 `SEGMENT_MISSING#object`，无标记且两份都没了报 `ORPHAN_ROW`，列举出错 72 秒内不做判断，去掉上传后全部以 `CONFIG` 关闭；保留进行中 meta 拒绝连接与黑洞（真实文件年龄）后每个删除的文件恰好一条事件、一个标记，`RETENTION_RECORD_FAILED` 按设计开关，`/api/health` 有界；保留进行中租约移交（owner 被 SIGSTOP 和 kill -9），旧 owner 被栅栏挡住（没有事件、标记或删除），新 owner 补写了旧 owner 删掉的文件，6/6 个文件各记一次；从 v0.5.73 发布版升级（standalone 与集群滚动，有无 000010），约 6 秒补齐，运行期间没有重复，从 S3 恢复与源库一致，回退到 v0.5.73 正常；两节点集群空闲 45 分钟，finding、race、panic 均为 0，meta 负载 +4%（每轮的封存事实探针，只有 SELECT）。

Tip QA 9bd6c9a PASS（BinlogServerQA），2026-10-19：CI 全绿；两节点集群空闲 30 分钟（`-race`、schema 10、上传、保留 1 天 / 7 天）30/30 次采样干净，finding、转移、race、panic 均为 0，两个节点 health 都是 ok，meta 负载平稳；standalone 与两节点集群滚动重启冒烟，全部分段上传、0 个 sha 不一致、GTID 无缺口无重复，从 S3 恢复与源库一致；从 v0.5.73 发布版升级再回退，4 种组合（standalone 与集群、有无 000010），第一轮后约 6 秒补齐 4 条事件（有 000010 时同时 4 个标记），恢复一致，回退后 v0.5.73 正常运行；真实年龄下的保留，standalone 与集群，5 个删除的文件各恰好一条事件、一个标记，对象留着，`RETENTION_RECORD_FAILED` 与 `ORPHAN_ROW` 均为 0；两节点集群 meta 黑洞 15 秒，每次轮询都在 1.006 秒内返回 degraded 并带 `META_UNREACHABLE`，每个节点一条记录、与故障窗口相差 0.03 秒内，没有 `TASK_NO_PROGRESS`；控制台只做了 HTTP 层检查（`/ui/`、Swagger、任务 API、`/api/findings`、`/api/health`、`/metrics`），没有在浏览器里渲染。QA 中轮转时短暂出现的 `CHECKPOINT_AHEAD` 是 #441，v0.5.73 起就有。CI 在 `9bd6c9a` 上通过。
