# Binlog Server v0.5.75

Release date: 2026-10-19

v0.5.75 is ADR 0009 §27 (#445). It fixes #436 and #441. With the health evaluator on and meta below migration 000009, the evaluator no longer goes silent: `HEALTH_STORE_UNAVAILABLE` (CRITICAL) opens from memory and `/api/health` returns 503. A source `FLUSH BINARY LOGS` no longer raises a false CRITICAL `CHECKPOINT_AHEAD`. No new migration (schema up to 10, `minRequiredSchemaVersion` still 6), the upgrade floor stays v0.5.51, and there is no new config key.

## Upgrade Notes

- **Evaluator on with meta below 9 now reports degraded.** If you run `health.evaluator: on` against a meta schema below 9, `/api/health` returns 503 `degraded` right after the upgrade, with the reason `HEALTH_STORE_UNAVAILABLE: schema N < 9, run ./migrate up`. Run `./migrate up` (to 9 or 10) before or after replacing the binary; health recovers within one sweep without a restart. `shadow` and `off` do not change health.
- No new migration and no new config key. v0.5.74 runs on the same schema, so the binary can be rolled back.
- The new kind is produced by the health evaluator, which needs `health.evaluator: on` (default `off` in v0.5.x; it becomes the default in v0.6.0). The rotation fix (#441) applies in every mode.
- From v0.5.74, replace the binary; a rolling restart of a 3-node cluster from v0.5.74 passed QA. The v0.5.74 notes still apply when upgrading from v0.5.73 or earlier, the v0.5.73 notes from v0.5.72 or earlier, the v0.5.72 notes from v0.5.71 or earlier, and the v0.5.67 notes from v0.5.66 or earlier.

## Fixes

- **#436: evaluator on with schema below 000009 was silent** (#445). After an online `migrate goto 7` or `8`, or when the process started on a schema below 9, the health tables were missing. The evaluator logged one INFO line per sweep, could not open any finding, and `/api/health` stayed `ok`. Now `HEALTH_STORE_UNAVAILABLE` (CRITICAL, task `_cluster`, subject = this node's id) opens within one sweep and is served from memory on `/api/health` and `/api/findings`, since it cannot be written to the missing tables. `/api/health` returns 503 `degraded` with the reason `HEALTH_STORE_UNAVAILABLE: schema N < 9, run ./migrate up`; when the version cannot be read, or the version is at least 9 but the tables were dropped, the reason is `health tables are absent; run ./migrate up to 000009`. One WARN is logged when it opens and one when it closes. The health store is probed on every sweep, not only at startup, so a cluster node that owns no task and holds no `_health` lease also degrades, and every node recovers after `migrate up` without a restart. On recovery a standalone node writes an OPEN/CLOSE pair; a cluster writes one resolved finding row and a `FINDING_RESOLVED` event per node. `shadow` logs exactly one WARN and keeps health `ok`; `off` does not probe. When only the 000008 columns are missing and 000009 is present, `SEAL_FACTS_ABSENT` reports it as before and this kind does not open.
- **#441: false `CHECKPOINT_AHEAD` when the source rotates** (#445). On `FLUSH BINARY LOGS`, the checkpoint for the next file was written before the new `.open` file existed, with the sealed upload in between, so the evaluator saw a CRITICAL `CHECKPOINT_AHEAD` for 1–2 s that then closed on its own. Rotation now writes the next checkpoint and creates the new file under the same per-task chain lock the evaluator holds while it reads the checkpoint and the segment list, so the evaluator sees one consistent snapshot. The upload runs after the new file is open and the lock is released. A real checkpoint ahead of the chain (a later file that is not on the chain, a position past the sealed end, or a GTID set ahead) still opens within one sweep.
- **The evaluator never waits on a stuck rotation** (#445, §27.8). The evaluator waits at most 2 s for the chain lock. If a rotation is stuck holding it (a hung checkpoint write, a blocked disk), that sweep marks the local chain UNKNOWN, keeps already-open chain findings open, and evaluates everything else, so `TASK_NO_PROGRESS` still fires for the stuck task. The checkpoint read under the lock is bounded by `meta.timeout.read`; a failed or timed-out read marks the chain UNKNOWN instead of being treated as "no checkpoint", which could have closed a real `CHECKPOINT_AHEAD`.

## Added

- **`HEALTH_STORE_UNAVAILABLE`** (CRITICAL, Managed, opens within one sweep, no Confirm). It is counted in the existing `binlog_server_finding_open{task_id="_cluster",kind="HEALTH_STORE_UNAVAILABLE",severity="critical"}` metric; no new metric name.

## Known issues

- **#446 (P3): rehasher log flood at schema below 9.** With the evaluator `on` or `shadow` and meta below 9, the owner node's rehasher logs an INFO line saying its progress table (migration 000009) doesn't exist about 2,400 (schema 8) to 4,500 (schema 7) times per 20 s. It is log volume only, with no extra meta load. Present since before v0.5.74. In standalone mode the `HEALTH_STORE_UNAVAILABLE` subject is the process's random node id, which changes on every restart.
- **#437 (P3):** after an online `migrate up` to 000010, rows deleted before the up are not marked until the next restart, which also writes a duplicate event. Until then, if such a row's object goes missing, it reports `ORPHAN_ROW` (WARNING) instead of `SEGMENT_MISSING#object` (CRITICAL).
- **#438 (P4):** retention deletes by file mtime, but the catch-up ages rows by the catalog's `sealed_at`. For a file whose mtime is older than its `sealed_at`, a failed or interrupted record may never be caught up.
- A process killed between committing the next checkpoint and creating the new file can show one short `CHECKPOINT_AHEAD` on the first sweep after restart; it closes once the file is opened (under 2 s in QA). This is a crash window, not the per-`FLUSH` behaviour fixed by #441.
- `/api/findings` has no read timeout yet (P2-7).

## Changes since v0.5.74

`git log --oneline --first-parent v0.5.74..4308d0f`, plus the merge of this notes PR, which v0.5.75 is tagged on:

| Commit | Change |
|---|---|
| `a2b0ab2` | PR #443: install docs point at v0.5.74 |
| `4308d0f` | PR #445: `HEALTH_STORE_UNAVAILABLE` when the health tables are absent, consistent chain snapshot across rotation, bounded chain-lock wait and checkpoint read (#436, #441) |

## QA summary

PR #445 QA PASS (BinlogServerQA), 2026-10-19, `-race` build, MariaDB 11.4 meta, moto for S3, sweep 5 s:
- evaluator on, online `goto 8`, `goto 7`, then `up`, standalone and 3-node cluster: 503 with the right reason within one sweep (standalone +0.54 s), exactly one CRITICAL per node, no `SEAL_FACTS_ABSENT`, one WARN; recovery without a restart (standalone OPEN/CLOSE pair; cluster three resolved rows and three `FINDING_RESOLVED`);
- `shadow`: health `ok`, no findings, exactly one WARN; `off`: health `ok`, no WARN;
- rotation storms: 12 runs of 22 `FLUSH BINARY LOGS` under steady writes (264 rotations), standalone and cluster with the write fence: zero `CHECKPOINT_AHEAD`;
- a real `CHECKPOINT_AHEAD` (checkpoint moved to a later file) opened in 2.6–5.1 s and closed within 4.6 s after it was moved back;
- kill -9 loop, 10 rounds at rotation points with slow uploads: every unuploaded sealed file had an `UPLOAD_FAILED` row and retries finished (11/11 uploaded), GTIDs 4..2202 with no gaps or duplicates, restore from S3 matches the source;
- stalled checkpoint write (optional): `TASK_NO_PROGRESS` opened at about 30 s and closed after the unlock. The injection never caught a rotation holding the chain lock, so the lock-wait skip path is covered by unit tests only;
- a cluster node with no task and no `_health` lease degraded within one sweep and recovered without a restart;
- upgrade from the v0.5.74 release, standalone and 3-node cluster rolling: clean, zero `CHECKPOINT_AHEAD`, the first item repeated OK;
- 0 races, 0 panics.

Tip QA 4308d0f PASS (BinlogServerQA), 2026-10-19:
- CI green;
- idle 2-node cluster 30 min (`-race`, schema 10, evaluator on, upload): 30/30 samples clean, 0 findings, 0 transitions, 0 races, 0 panics, no UNKNOWN health; meta load about 852 queries/min, flat (v0.5.74 gate: 831);
- #445 regression: standalone 503 at +0.54 s and back 5.6 s after `up`; 3-node cluster (including the node with no task or lease) degraded within 5.1 s and back within 4.1 s, with 3 `FINDING_RESOLVED`; 4 runs of 22 flushes with zero `CHECKPOINT_AHEAD`;
- upgrade from the v0.5.74 release, standalone and cluster rolling n1→n2→n3: clean, flush check zero, regression repeated OK;
- smoke, standalone and 2-node cluster with rolling restart: all segments uploaded, 0 sha mismatches, GTID without gaps or duplicates, restore from S3 matches the source.
- The #446 log flood showed up as expected at schema below 9.

CI passed on `4308d0f`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.75 中文发布说明

> 对应版本：`v0.5.75`

发布日期：2026-10-19

v0.5.75 是 ADR 0009 §27（#445），修复 #436 和 #441。评估器开着、meta 低于迁移 000009 时，评估器不再静默：从内存打开 `HEALTH_STORE_UNAVAILABLE`（CRITICAL），`/api/health` 返回 503。源端 `FLUSH BINARY LOGS` 不再误报 CRITICAL `CHECKPOINT_AHEAD`。没有新迁移（schema 最高 10，`minRequiredSchemaVersion` 仍为 6），升级地板仍是 v0.5.51，没有新配置项。

## 升级须知

- **评估器 on 且 meta 低于 9 时现在会报 degraded。** 如果 `health.evaluator: on` 而 meta schema 低于 9，升级后 `/api/health` 立刻返回 503 `degraded`，原因是 `HEALTH_STORE_UNAVAILABLE: schema N < 9, run ./migrate up`。换二进制前后执行 `./migrate up`（到 9 或 10）即可，一轮之内恢复，不用重启。`shadow` 和 `off` 不改变健康状态。
- 没有新迁移，没有新配置项。v0.5.74 在同样的 schema 上照常运行，二进制可以回退。
- 新 kind 由健康评估器产生，需要 `health.evaluator: on`（v0.5.x 默认 `off`，v0.6.0 起默认 `on`）。轮转修复（#441）在任何模式下都生效。
- 从 v0.5.74 升级：换二进制重启；三节点集群从 v0.5.74 滚动升级已在 QA 中通过。从 v0.5.73 或更早升级仍按 v0.5.74 的升级须知操作，v0.5.72 或更早按 v0.5.73，v0.5.71 或更早按 v0.5.72，v0.5.66 或更早按 v0.5.67。

## 修复

- **#436：评估器 on、schema 低于 000009 时静默**（#445）。在线 `migrate goto 7` 或 `8` 之后，或进程本来就在 schema 9 以下启动时，健康表不在。评估器每轮只打一条 INFO，任何 finding 都开不了，`/api/health` 仍是 `ok`。现在一轮之内打开 `HEALTH_STORE_UNAVAILABLE`（CRITICAL，task `_cluster`，subject 为本节点 id）；表不在、写不进去，所以从内存提供给 `/api/health` 和 `/api/findings`。`/api/health` 返回 503 `degraded`，原因写明 `HEALTH_STORE_UNAVAILABLE: schema N < 9, run ./migrate up`；探不到版本，或版本已经 ≥ 9 但表被删掉时，原因是 `health tables are absent; run ./migrate up to 000009`。打开和关闭各打一条 WARN。健康存储每个 sweep 都探一次，不只在启动时，所以集群里不负责任何任务、也没拿 `_health` 租约的节点同样会 degraded，`migrate up` 之后每个节点都不用重启就恢复。恢复时，单机写下一对 OPEN/CLOSE；集群每个节点写一行已关闭的 finding 和一条 `FINDING_RESOLVED`。`shadow` 只打一次 WARN，健康保持 `ok`；`off` 不探。只缺 000008 的列、000009 仍在时，照旧由 `SEAL_FACTS_ABSENT` 报出，不开这一条。
- **#441：源端轮转时误报 `CHECKPOINT_AHEAD`**（#445）。`FLUSH BINARY LOGS` 时，下一文件的 checkpoint 写在新 `.open` 文件出现之前，中间还隔着封存上传，评估器会看到 1–2 秒的 CRITICAL `CHECKPOINT_AHEAD`，然后自己关上。现在轮转在每个任务一把的链锁里写下一段 checkpoint、创建新文件；评估器读 checkpoint 和段列表时持同一把锁，看到的是一致的快照。上传挪到新文件打开、放锁之后。真的位点超前（链上没有的更后文件、越过封存末尾、GTID 集合超前）仍在一轮内打开。
- **评估器不会被卡住的轮转拖住**（#445，§27.8）。评估器取链锁最多等 2 秒。轮转卡在锁里时（checkpoint 写挂住、磁盘阻塞），这一轮把本地链记为 UNKNOWN，已打开的链 finding 不关，其它照常评估，所以卡住的任务仍能报 `TASK_NO_PROGRESS`。锁内的 checkpoint 读受 `meta.timeout.read` 约束；读失败或超时记为 UNKNOWN，不当作「没有 checkpoint」，以免把一条真的 `CHECKPOINT_AHEAD` 关掉。

## 新增

- **`HEALTH_STORE_UNAVAILABLE`**（CRITICAL，由评估器管理，一轮内打开，不需要确认）。计入已有的 `binlog_server_finding_open{task_id="_cluster",kind="HEALTH_STORE_UNAVAILABLE",severity="critical"}` 指标，不加新指标名。

## 已知问题

- **#446（P3）：schema 低于 9 时 rehasher 日志刷屏。** 评估器 `on` 或 `shadow`、meta 低于 9 时，负责任务的节点上 rehasher 每 20 秒打约 2400 条（schema 8）到 4500 条（schema 7）「进度表（迁移 000009）不存在」的 INFO 日志。只是日志量，meta 负载不增加。v0.5.74 之前就有。单机模式下 `HEALTH_STORE_UNAVAILABLE` 的 subject 是进程随机生成的节点 id，每次重启都会变。
- **#437（P3）：** 在线 `migrate up` 到 000010 之后，up 之前删掉的行要到下一次重启才写标记，重启时还会多写一条事件。在那之前，这些行的对象如果丢了，报的是 `ORPHAN_ROW`（WARNING）而不是 `SEGMENT_MISSING#object`（CRITICAL）。
- **#438（P4）：** 保留按文件 mtime 删，补写按目录的 `sealed_at` 判断年龄。文件 mtime 比 `sealed_at` 早时，一次失败或中断的记录可能永远补不上。
- 进程恰好在提交下一段 checkpoint 之后、创建新文件之前被杀掉时，重启后的第一轮可能出现一条短暂的 `CHECKPOINT_AHEAD`，文件打开后关闭（QA 中不到 2 秒）。这是崩溃窗口，不是 #441 修掉的每次 `FLUSH` 都有的误报。
- `/api/findings` 还没有读超时（P2-7）。

## 自 v0.5.74 以来的变更

| 提交 | 变更 |
|---|---|
| `a2b0ab2` | PR #443：安装文档指向 v0.5.74 |
| `4308d0f` | PR #445：健康表缺失时打开 `HEALTH_STORE_UNAVAILABLE`、轮转时链快照一致、链锁等待与 checkpoint 读有上限（#436、#441） |

## QA 摘要

PR #445 QA PASS（BinlogServerQA），2026-10-19，`-race` 构建、MariaDB 11.4 meta、moto 模拟 S3、sweep 5 秒：评估器 on，在线 `goto 8`、`goto 7` 再 `up`，单机与三节点集群都在一轮内返回 503 且原因正确（单机 +0.54 秒），每个节点恰好一条 CRITICAL，没有 `SEAL_FACTS_ABSENT`，一条 WARN，不重启即恢复（单机一对 OPEN/CLOSE，集群三行已关闭的 finding 和三条 `FINDING_RESOLVED`）；`shadow` 健康 `ok`、没有 finding、恰好一条 WARN，`off` 健康 `ok`、没有 WARN；轮转风暴共 12 轮、每轮 22 次 `FLUSH BINARY LOGS`（264 次轮转），持续写入，单机与带写栅栏的集群，`CHECKPOINT_AHEAD` 为零；真的 `CHECKPOINT_AHEAD`（把 checkpoint 改到更后的文件）2.6–5.1 秒打开，改回后 4.6 秒内关闭；在轮转点 kill -9 循环 10 轮并配慢上传，每个未上传的封存文件都有 `UPLOAD_FAILED` 行，重试全部完成（11/11 已上传），GTID 4..2202 无缺口无重复，从 S3 恢复与源库一致；卡住 checkpoint 写（可选项）时 `TASK_NO_PROGRESS` 约 30 秒打开、解锁后关闭，但注入没能让轮转卡在链锁里，锁等待跳过的路径只有单元测试覆盖；集群里没有任务、没拿 `_health` 租约的节点一轮内 degraded，不重启即恢复；从 v0.5.74 发布版升级（单机与三节点集群滚动）干净，`CHECKPOINT_AHEAD` 为零，第一项重做正常；race、panic 均为 0。

Tip QA 4308d0f PASS（BinlogServerQA），2026-10-19：CI 全绿；两节点集群空闲 30 分钟（`-race`、schema 10、评估器 on、上传）30/30 次采样干净，finding、转移、race、panic 均为 0，没有 UNKNOWN 健康状态，meta 负载约每分钟 852 次查询、平稳（v0.5.74 发布门禁为 831）；#445 回归：单机 +0.54 秒 503、`up` 后 5.6 秒恢复，三节点集群（含没有任务和租约的节点）5.1 秒内 degraded、4.1 秒内恢复，3 条 `FINDING_RESOLVED`，4 轮 22 次 flush 零 `CHECKPOINT_AHEAD`；从 v0.5.74 发布版升级（单机与集群 n1→n2→n3 滚动）干净，flush 检查为零，回归重做正常；单机与两节点集群滚动重启冒烟，全部分段上传、0 个 sha 不一致、GTID 无缺口无重复，从 S3 恢复与源库一致。schema 低于 9 时如预期出现 #446 的日志刷屏。CI 在 `4308d0f` 上通过。
