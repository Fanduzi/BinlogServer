# Binlog Server v0.5.73

Release date: 2026-10-19

v0.5.73 fixes #427 (P2, #430): a meta outage where connections hang instead of being refused no longer leaves `/api/health` unanswered and no longer goes unrecorded. `/api/health` now answers within about 1 s with 503 `degraded` while meta does not answer, and every node records a CRITICAL `META_UNREACHABLE` finding for any outage of about 11 s or more, from the first failure to the first answer, in UTC. No new migration and no new config key: the schema stays 9, `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51.

## Upgrade Notes

- No migration and no config change. From v0.5.72, replace the binary and restart. A rolling restart of a cluster from v0.5.72 passed tip QA.
- **The bounded `/api/health` works in every mode.** With meta configured, `/api/health` no longer hangs when meta does not answer: all its meta reads share a 1 s budget, at most one read is in flight, and on timeout it answers 503 `degraded` from the findings held in memory, with a reason containing `META_UNREACHABLE`. Probes with a timeout above about 1.5 s now get an answer instead of a timeout.
- **`META_UNREACHABLE` recording needs the health evaluator.** The independent 1 s ping and the evaluator's reads are only observed with `health.evaluator: on` (default `off` in v0.5.x; it becomes the default in v0.6.0). With `off`, `/api/health` is still bounded, but no outage record is written.
- Times written by the health store (`task_findings`, `finding_transitions`, `task_health`) are now UTC. Rows written by earlier versions are not migrated, so older evaluator rows may be in the meta session time zone.
- The v0.5.72 upgrade notes still apply when upgrading from v0.5.71 or earlier, and the v0.5.67 notes when upgrading from v0.5.66 or earlier.

## Fixes

- **#427 (P2): meta blackholed, `/api/health` hung and nothing was recorded** (#430). When connections to meta hang (firewall DROP, partition, frozen host), `/api/health` on every node did not answer, and after recovery no finding showed that there had been an outage. All meta reads of `/api/health` now share a 1 s budget counted from the request, run on their own goroutine with at most one in flight (single-flight), and on timeout the answer is 503 `degraded` built from memory, with the reason `META_UNREACHABLE: findings store read did not answer within 1s`. A managed finding that cannot be read is reported as unreadable instead of disappearing from the summary.
- **Health store times are UTC** (#430). `task_findings`, `finding_transitions` and `task_health` were written with the session time zone's `NOW(6)`, mixed with UTC rows from the older finding path. They now use `UTC_TIMESTAMP(6)`. An evaluator finding's `first_seen` is the time of the OPEN fact (the first failure) and `resolved_at` the time of the CLOSE fact, not the time of the write.
- **A reopened finding starts over** (#430). On the older finding path, a resolved row that opened again kept the earlier `first_seen` and added to the old `count`. It now takes the new values.

## Added

- **CRITICAL `META_UNREACHABLE`** (#427, design 0009 §25, task `_cluster`, subject = node id; each node reports its own). Three observations feed it: an independent meta ping every 1 s with a 1 s timeout on its own goroutine (it keeps going when the evaluator loop is stuck), the bounded `/api/health` reads, and the evaluator sweep's lease calls and reads. It opens after 10 s of consecutive failures counted from the first failure, and closes on the next answer. An outage that only reaches 10 s at recovery (for example a 12 s pause) is still recorded with its open and close, so every outage of about 11 s or more leaves a record on every node. Read-only (1290), permission and other SQL errors do not count; they have their own kinds.
- **One record per outage.** In standalone with meta, an outage leaves an OPEN (`fact_at` = first failure) and a CLOSE (`fact_at` = first answer) transition. In a cluster, each node writes one `task_events` row per outage (`task_id = _cluster`, `FINDING_RESOLVED META_UNREACHABLE <node>`, `event_time` = recovery, the window in the detail), so an earlier outage still has a record after the next one reuses the finding row. Each node logs one line when it opens and one when it resolves (`META_UNREACHABLE opened on …` / `resolved on …: outage <start> to <end>`).

## Known issues

- **P3: findings can linger after back-to-back outages.** After two meta outages close together (15 s, 10 s gap, 15 s), `TASK_PERSIST_FAILED` ("state LEASE_DEGRADED not written"), `EVENT_PERSIST_FAILED` and `TASK_LEASE_DEGRADED` were still open, and `/api/health` still degraded, about 35 s after meta recovered, while the task was RUNNING and writing. The QA run ended there, so it is not known whether they clear later. After single outages they cleared within about 4 s. This reports too much, not too little.
- **P3: standalone writes no `FINDING_RESOLVED` event** for `META_UNREACHABLE`. The outage is recorded only as the OPEN and CLOSE transitions in `finding_transitions`.
- `/api/findings` has no read timeout yet (P2-7); with meta blackholed it can hang. `/api/health` is bounded.

## Changes since v0.5.72

`git log --oneline --first-parent v0.5.72..99621d2`, plus the merge of this notes PR, which v0.5.73 is tagged on:

| Commit | Change |
|---|---|
| `3e4befe` | PR #429: install docs point at v0.5.72 |
| `99621d2` | PR #430: bounded `/api/health` under a blackholed meta, `META_UNREACHABLE` on every node from the first failure, UTC health store times (#427) |

## QA summary

PR #430 QA round 2 PASS (BinlogServerQA), 2026-10-19, `-race` build, 2-node cluster with shared `data_dir`, evaluator on, MariaDB 11.4 meta behind a TCP proxy (SIGSTOP = blackhole, kill = refused):
- meta blackholed 100 s: every `/api/health` poll on both nodes answered 503 `degraded` with `META_UNREACHABLE` within 1.003 s from the first poll; one record per node of about 100 s, one `FINDING_RESOLVED` per node; standalone left an OPEN/CLOSE pair;
- connection refused 40 s: degraded throughout, `META_UNREACHABLE` from fault + 10–11 s, one record per node;
- pauses of 12 s and 30 s: a record on every node, the task owner included, `first_seen` within 0.01 s of the fault and `resolved_at` within 0.62 s of recovery, all UTC although the meta server runs in UTC+8; restore checksum matches;
- back-to-back 15 s / 10 s / 15 s: one `FINDING_RESOLVED` per node per outage (the lingering findings above are the P3);
- binlog consistent in every run (GTID contiguous, sealed segments unchanged), 0 races, 0 panics.

Tip QA 99621d2 PASS (BinlogServerQA), 2026-10-19:
- CI green;
- idle cluster about 30 min (`-race`): 30/30 samples clean, 0 findings, 0 races, 0 panics, health ok;
- smoke, standalone and 2-node cluster with rolling restart: segments continuous, all uploaded with matching sha256, restore from S3 matches the source;
- upgrade from the v0.5.72 release binary, standalone and cluster rolling: byte-continuous, segments written by v0.5.72 unchanged, restore matches. The lease stayed on the restarted node, so this covered restarts, not a takeover;
- meta blackholed 15 s: `META_UNREACHABLE` recorded and resolved on both nodes (about 15.02 s), no other findings, restore matches;
- console: HTTP-level checks only (`/ui/`, Swagger, task API, `/api/health`), not rendered in a browser.

CI passed on `99621d2`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.73 中文发布说明

> 对应版本：`v0.5.73`

发布日期：2026-10-19

v0.5.73 修复 #427（P2，#430）：meta 库的连接挂住而不是被拒绝时，`/api/health` 不再一直不返回，中断也不再没有任何记录。meta 不应答期间，`/api/health` 在约 1 秒内返回 503 `degraded`；任何约 11 秒以上的中断，每个节点都会记下一条 CRITICAL `META_UNREACHABLE`，从第一次失败到第一次应答，时间为 UTC。没有新迁移，没有新配置项：schema 仍是 9，`minRequiredSchemaVersion` 仍是 6，地板仍是 v0.5.51。

## 升级须知

- 没有迁移，没有配置变更。从 v0.5.72 升级直接换二进制重启即可。集群从 v0.5.72 滚动重启升级已在 tip QA 中通过。
- **`/api/health` 的有界读在任何模式下都生效。** 配置了 meta 时，meta 不应答不会再让 `/api/health` 挂住：它的全部 meta 读共用 1 秒预算，同一时刻最多一个读，超时就用内存里的 finding 返回 503 `degraded`，原因含 `META_UNREACHABLE`。超时设在约 1.5 秒以上的探针现在能拿到应答，而不是超时。
- **`META_UNREACHABLE` 的记录需要健康评估器。** 独立的 1 秒 ping 和评估器的读只有在 `health.evaluator: on` 时才会被观测（v0.5.x 默认 `off`，v0.6.0 起默认 `on`）。保持 `off` 时 `/api/health` 仍然有界，但不会写中断记录。
- 健康存储（`task_findings`、`finding_transitions`、`task_health`）写入的时间现在是 UTC。旧版本写下的行不迁移，所以较早的评估器行可能是 meta 会话时区的时间。
- 从 v0.5.71 或更早升级仍按 v0.5.72 的升级须知操作；从 v0.5.66 或更早升级仍按 v0.5.67 的升级须知操作。

## 修复

- **#427（P2）：meta 被黑洞时 `/api/health` 挂住，事后什么都不记**（#430）。连到 meta 的连接挂住时（防火墙 DROP、网络分区、主机卡死），所有节点的 `/api/health` 都不响应，恢复后也没有任何 finding 说明发生过中断。现在 `/api/health` 的全部 meta 读从请求进入起共用 1 秒预算，在单独的 goroutine 里跑，同一时刻最多一个（single-flight），超时就用内存里的内容返回 503 `degraded`，原因是 `META_UNREACHABLE: findings store read did not answer within 1s`。读不到的评估器管理的 finding 会写明不可读，不再从汇总里消失。
- **健康存储时间改为 UTC**（#430）。`task_findings`、`finding_transitions`、`task_health` 以前用会话时区的 `NOW(6)` 写入，与旧 finding 路径写的 UTC 混在一起，现在用 `UTC_TIMESTAMP(6)`。评估器 finding 的 `first_seen` 是 OPEN 的事实时间（第一次失败），`resolved_at` 是 CLOSE 的事实时间，不再是写入时刻。
- **重新打开的 finding 从头计**（#430）。旧 finding 路径上，已解决的行再次打开时沿用上一次的 `first_seen`，`count` 继续累加；现在取新值。

## 新增

- **CRITICAL `META_UNREACHABLE`**（#427，设计 0009 §25，task `_cluster`，subject 为节点 id，每个节点各报各的）。三路观测：独立 goroutine 上每 1 秒一次、限时 1 秒的 meta ping（评估循环卡住也照发）、`/api/health` 的有界读、评估器 sweep 的 lease 调用和读。从第一次失败算起连续失败满 10 秒打开，下一次应答时关闭。恢复时才满 10 秒的中断（例如 12 秒的暂停）也补记一次开和关，所以任何约 11 秒以上的中断在每个节点上都留下记录。只读（1290）、权限等 SQL 错误不算，它们各有自己的 kind。
- **每次中断一条记录。** standalone + meta 下，一次中断留下 OPEN（`fact_at` = 第一次失败）和 CLOSE（`fact_at` = 第一次应答）两条转移。集群里每个节点每次中断写一条 `task_events`（`task_id = _cluster`，`FINDING_RESOLVED META_UNREACHABLE <node>`，`event_time` = 恢复时刻，detail 写明起止），下一次中断复用同一 finding 行后，上一次中断仍有记录。每个节点在打开、关闭时各打一行日志（`META_UNREACHABLE opened on …` / `resolved on …: outage <起> to <止>`）。

## 已知问题

- **P3：连续两次中断后 finding 可能残留。** 两次相隔很近的 meta 中断（15 秒、间隔 10 秒、再 15 秒）之后，meta 恢复约 35 秒时 `TASK_PERSIST_FAILED`（"state LEASE_DEGRADED not written"）、`EVENT_PERSIST_FAILED`、`TASK_LEASE_DEGRADED` 仍是打开的，`/api/health` 仍是 degraded，而任务已经 RUNNING 并在写入。QA 在那时结束，不确定之后是否会自己关闭。单次中断后它们在约 4 秒内关闭。这是多报，不是漏报。
- **P3：standalone 不写 `FINDING_RESOLVED` 事件。** standalone 下 `META_UNREACHABLE` 只记在 `finding_transitions` 的 OPEN 和 CLOSE 转移里。
- `/api/findings` 还没有读超时（P2-7），meta 被黑洞时可能挂住；`/api/health` 是有界的。

## 自 v0.5.72 以来的变更

| 提交 | 变更 |
|---|---|
| `3e4befe` | PR #429：安装文档指向 v0.5.72 |
| `99621d2` | PR #430：meta 被黑洞时 `/api/health` 有界返回，每个节点从第一次失败起记录 `META_UNREACHABLE`，健康存储时间改为 UTC（#427） |

## QA 摘要

PR #430 第二轮 QA PASS（BinlogServerQA），2026-10-19，`-race` 构建、两节点集群共享 `data_dir`、评估器 on，MariaDB 11.4 meta 放在 TCP 代理后面（SIGSTOP = 黑洞，kill = 拒绝连接）：meta 黑洞 100 秒，两个节点每次 `/api/health` 轮询从第一次起都在 1.003 秒内返回 503 `degraded` 并带 `META_UNREACHABLE`，每个节点一条约 100 秒的记录、一条 `FINDING_RESOLVED`，standalone 留下 OPEN/CLOSE 一对；拒绝连接 40 秒，全程 degraded，`META_UNREACHABLE` 在故障后 10–11 秒出现，每个节点一条记录；暂停 12 秒和 30 秒，每个节点（包括任务 owner）都有记录，`first_seen` 与故障相差 0.01 秒内，`resolved_at` 与恢复相差 0.62 秒内，meta 服务器是 UTC+8 时区但写入全是 UTC，恢复校验和一致；连续两次 15 秒 / 间隔 10 秒 / 15 秒的中断，每个节点每次中断一条 `FINDING_RESOLVED`（上面残留的 finding 就是那个 P3）；每一轮 binlog 都一致（GTID 连续，封存段未改动），race 和 panic 均为 0。

Tip QA 99621d2 PASS（BinlogServerQA），2026-10-19：CI 全绿；集群空闲约 30 分钟（`-race`）30/30 次采样干净，finding、race、panic 均为 0，health ok；standalone 与两节点集群滚动重启冒烟，分段连续、全部上传且 sha256 一致，从 S3 恢复与源库一致；从 v0.5.72 发布二进制升级（standalone 与集群滚动）字节连续，v0.5.72 写的分段未改动，恢复一致，但 lease 一直留在重启的节点上，测的是重启而不是接管；meta 黑洞 15 秒，两个节点都记下并关闭了 `META_UNREACHABLE`（约 15.02 秒），没有其他 finding，恢复一致；控制台只做了 HTTP 层检查（`/ui/`、Swagger、任务 API、`/api/health`），没有在浏览器里渲染。CI 在 `99621d2` 上通过。
