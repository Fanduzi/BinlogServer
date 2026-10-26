# Binlog Server v0.5.79

Release date: 2026-10-26

v0.5.79 ships ADR 0011 PR-3 (#511, landing #507; refs #462). In cluster mode, deleting a sealed local segment now opens CRITICAL `SEGMENT_MISSING`, as it already did in standalone. The finding is opened by the verified chain owner: the lease holder that read the task directory on that pass. It closes only by `EVAL`, when the file is back in that verified directory or when local retention ages the catalog row out. A failover, an ownership change, a restart or an operator Start no longer closes it. Runner start no longer forgets young missing segments. The finding and `/api/health` name the node whose disk is missing the file, and an unreadable owner directory is logged and shown in the health detail. No new migration (schema up to 10, `minRequiredSchemaVersion` still 6), no new config keys, and the upgrade floor stays v0.5.51.

## Upgrade Notes

- **Cluster mode can now turn `/api/health` 503 for a missing sealed segment.** Before this release, cluster mode dropped `I6_MISSING_LOCAL`, so a deleted sealed segment raised nothing (#462). Now the lease holder that read the task directory derives it, and after two passes in a row (`Confirm`) it opens CRITICAL `SEGMENT_MISSING`. A sealed segment that was already missing before the upgrade can open it on the first passes after the upgrade.
- **Who may close it.** Only the recorded `chain_owner` can close it, on a later pass where it read the directory completely and the segment is no longer missing (reason `EVAL`). An ownership change does not close it with `OWNER_CHANGED` any more. A pass that could not read the directory does not close it and does not change `chain_owner`. A new holder without the task directory (per-node disks) gets `SEGMENT_NOT_ON_WORKER` as before and does not write `chain_owner`.
- **The catalog row of a missing segment is kept inside local retention.** With no uploader and a meta DSN, every runner start (Start, restart, epoch bump, takeover) used to delete the catalog row of any sealed `LOCAL_ONLY` segment whose file was gone, whatever its age. The next pass then had nothing to report and the finding closed by `EVAL` while the segment was still gone. Now such a row is dropped only when this purge removed the file, or when the row is past local retention by `sealed_at` (else `created_at`), the #209 leftover. A row inside retention is kept and logged as `retention: kept catalog row <name> task=<id> upload_state=<state>: local file is missing inside local retention`. A row with neither timestamp is kept too; its finding stays open until the file is restored.
- **Retention closes it.** When the missing segment reaches local retention, retention drops the row, writes `RETENTION_REMOVED`, and `SEGMENT_MISSING` closes by `EVAL`. A segment past retention is no longer owed by the chain.
- **The node is named.** The `I6_MISSING_LOCAL` detail, `GET /api/findings` and the `/api/health` reason now carry `chain_owner=<node>` in cluster mode or `node=<node>` in standalone, so you know which disk to look at. Standalone shows the node id from `{data_dir}/.node_id`.
- **Unreadable task directory.** A task directory that exists but cannot be read (including a path that is not a directory) is logged when it starts and when it ends (`health evaluator: task <id> directory <dir> is unreadable, chain.local is incomplete until it is: ...`). The same line is in the `/api/health` `detail`. Status, open findings and `chain_owner` do not change.
- **Non-owner nodes can lag on the close (#510).** After the owner closes `SEGMENT_MISSING`, another node's `/api/health` and `/api/findings` can keep listing it (and return 503). Tip QA saw this last 18 minutes on one node. Check the owner's `/api/health`, or the shared findings store, before acting on a non-owner's 503.
- No new migration and no new config keys. v0.5.78 runs on the same schema, so the binary can be rolled back. From v0.5.78, replace the binary. Tip QA upgraded v0.5.78 in place: schema stayed at 10 and not dirty, tasks resumed, and the segment written across the binary swap was md5-identical to the source. The v0.5.78 notes still apply when upgrading from v0.5.77 or earlier.

## Added

- **ADR 0011 PR-3: `SEGMENT_MISSING` owned by the verified chain owner (#511, landing #507; refs #462).** `health.Fence.ChainVerified` is set only when the cluster lease holder (not a proxy or delegated pass) read the task directory on that pass. `task_health.chain_owner` is written only then. `SEGMENT_MISSING` is exempt from the `OWNER_CHANGED` close and is not reopened on the same pass. Mutation runner: SM01–SM22 are each killed by a named test.

## Fixes

- **#507 QA P2:** runner start (`dropGoneLocalOnlyRows`) no longer deletes the catalog row of a missing sealed segment that is still inside local retention, so failover, rolling restart and operator Start no longer close `SEGMENT_MISSING` while the segment is gone.
- **#476 (P1), confirmed fixed:** a frozen owner holding a `RUNNING` transaction on its `task_leases` row no longer blocks takeover without a bound. Live with MySQL meta, the transaction is killed 5–7 s after the freeze and the other node takes over at +32 s (it used to be blocked for 3m47s or more). The server-side timeout landed in v0.5.77 (ADR 0011 PR-2). It is not yet proven live on MariaDB meta.
- **#509:** install docs point at v0.5.78 with its checksums. The next pin PR moves them to v0.5.79.

## Known issues

- **#473 (P1):** with per-node storage, a takeover after the owner pauses past the TTL hits `SEGMENT_NOT_ON_WORKER` and writes FAILED / desired STOP. ADR 0011 PR-4 is in progress.
- **#462 (P1):** the cluster silent gap is covered by unit tests in this release. Full closure (catalog comparison, monotonic findings across ownership changes) comes with ADR 0011 PR-5. The cluster e2e for this path is not in CI yet.
- **#513 (P2):** after a takeover, a resumed old owner can hit `CHECKPOINT_WRITE_FAILED` ("busy buffer") on a dead meta connection. That error is not treated as a lost lease, so the new owner's run is marked FAILED / desired STOP with health 503 until an operator Start. Timing-dependent; not caused by #511.
- **#510:** a non-owner keeps a closed finding in `/api/health` and `/api/findings` (see the upgrade notes).
- **#514 (P3):** standalone with task records: `GET /api/tasks/{id}/checkpoint` returns 404 after a restart while the task is running. Also in v0.5.78.
- **#512 (P3):** test and log gaps from the #507 QA: mutants A02 (age source swapped) and A03 (cutoff in hours) survive; the unreadable-directory line is logged at info; a row with no timestamp is logged as "inside local retention".
- **#490**, **#494**, **#500**, **#502**, **#503**, **#504** (P3) are still open, as are **#487**, **#488**, **#451**, **#452**, **#455**, **#456**, **#446**, **#437** and **#438**. `/api/findings` has no read timeout yet (P2-7). The verifier does not run in cluster mode (A4 PARTIAL).

## Changes since v0.5.78

`git log --oneline --first-parent v0.5.78..d7b96e6`, plus the commit of these notes, which v0.5.79 is tagged on:

| Commits | Change |
|---|---|
| `ddbf7d7` | PR #509: install docs point at v0.5.78 |
| `df2cee1`..`d7b96e6` | PR #511: ADR 0011 PR-3, `SEGMENT_MISSING` owned by the verified chain owner (lands #507) |

## QA summary

**PR #507 QA PASS (BinlogServerQA), round 2 at `a9ccf61e`, 2026-10-21.**
- Live with a shared `data_dir` and upload off: `SEGMENT_MISSING` stayed open through SIGTERM, SIGKILL and an operator Stop/Start, and closed by `EVAL` on the chain owner only after the file was restored.
- Per-node takeover and standalone keep the row too. `sealed_at` wins over `created_at`, the `created_at` fallback works, rows with no timestamp or a future `sealed_at` are kept, and lowering local retention at runtime from 3 to 1 plus Start drops the row and closes by `EVAL`.
- The #209 cleanup still drops old gone rows.
- Mutation runner: 287 killed, 0 survived, 3 equivalent.

**Tip QA of `d7b96e6b` PASS (BinlogServerQA), 2026-10-26.**
- Land fidelity: the interdiff between #511 and #507 at `a9ccf61e` is empty. CI run 40244920168 is green.
- go1.26.10: `go test ./...` and `go test -race ./...` pass 17/17.
- Snapshot build: `-w` only for both binaries; `govulncheck` is clean in source mode and in binary mode.
- Smoke: standalone and a 2-node cluster with LATEST, GTID and FILE_POS tasks, SIGTERM and SIGKILL failover.
- Upgrade from v0.5.78: schema stays at 10, not dirty; tasks resume; the segment across the swap is md5-identical.
- `SEGMENT_MISSING` live: opens with `chain_owner` named; kept through SIGTERM, SIGKILL and Start; closes by `EVAL` on restore and when retention ages the row out; the unreadable-directory detail shows up.
- #476 checked live on MySQL meta (see Fixes).

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.79 中文发布说明

> 对应版本：`v0.5.79`

发布日期：2026-10-26

v0.5.79 带上 ADR 0011 PR-3（#511，落地 #507；关联 #462）。集群模式下删掉一个本地封存段，现在会和单机一样打开 CRITICAL `SEGMENT_MISSING`。打开它的是已验证的链主人，也就是这一轮读到了任务目录的租约持有者。它只以 `EVAL` 关闭：文件回到同一份已验证目录，或本地保留让目录行到期。故障切换、所有权变化、重启和运维 Start 都不再关掉它。运行器启动不再忘掉还年轻的缺失段。finding 和 `/api/health` 会点名缺文件的那块磁盘所在的节点；owner 目录读不了时打日志，并写进健康 detail。没有新迁移（schema 最高 10，`minRequiredSchemaVersion` 仍为 6），没有新配置项，升级地板仍是 v0.5.51。

## 升级须知

- **集群模式下，缺失的封存段现在会让 `/api/health` 返回 503。** 这一版之前，集群模式丢掉了 `I6_MISSING_LOCAL`，删掉封存段什么都不报（#462）。现在读到任务目录的租约持有者会推导它，连续两轮推导出（`Confirm`）后打开 CRITICAL `SEGMENT_MISSING`。升级前就已经缺失的封存段，升级后的头几轮也可能打开它。
- **谁能关闭。** 只有记下的 `chain_owner` 能关：之后某一轮它完整读到目录，且该段不再缺失（原因 `EVAL`）。所有权变化不再以 `OWNER_CHANGED` 关闭它。没读到目录的一轮不关，也不改 `chain_owner`。没有任务目录的新持有者（每节点独立磁盘）照旧得到 `SEGMENT_NOT_ON_WORKER`，不写 `chain_owner`。
- **本地保留期内，缺失段的目录行留下。** 没配上传、配了 meta DSN 时，以前每次运行器启动（Start、重启、epoch 提升、接管）都会删掉文件已不在的 `LOCAL_ONLY` 封存行，不管它多老；下一轮就没东西可报，段还缺着 finding 就以 `EVAL` 关了。现在这种行只在两种情况下删：文件是这一轮保留删的；或者行按 `sealed_at`（为空则 `created_at`）已过本地保留（#209 的遗留）。保留期内的行留下，并打一行 `retention: kept catalog row <name> task=<id> upload_state=<state>: local file is missing inside local retention`。两个时间都没有的行也留下，它的 finding 一直开到文件恢复。
- **保留会关闭它。** 缺失段到了本地保留期，保留删掉这一行、写 `RETENTION_REMOVED`，`SEGMENT_MISSING` 以 `EVAL` 关闭。过了保留期的段不再算这条链欠下的数据。
- **点名节点。** `I6_MISSING_LOCAL` 的 detail、`GET /api/findings` 和 `/api/health` 的 reason 现在带上 `chain_owner=<node>`（集群）或 `node=<node>`（单机），方便到对应的磁盘上去找。单机显示 `{data_dir}/.node_id` 里的节点 id。
- **任务目录读不了。** 任务目录存在但读不了（含路径在但不是目录）时，开始和恢复各打一行日志（`health evaluator: task <id> directory <dir> is unreadable, chain.local is incomplete until it is: ...`），同一行写进 `/api/health` 的 `detail`。status、开着的 finding 和 `chain_owner` 都不变。
- **非 owner 节点的关闭会滞后（#510）。** owner 关掉 `SEGMENT_MISSING` 之后，别的节点的 `/api/health` 和 `/api/findings` 可能还列着它（并返回 503）。Tip QA 在一个节点上看到持续了 18 分钟。根据非 owner 的 503 采取行动之前，先看 owner 的 `/api/health` 或共享的 finding 存储。
- 没有新迁移，也没有新配置项。v0.5.78 在同一个 schema 上运行，可以回滚二进制。从 v0.5.78 升级只需替换二进制。Tip QA 原地升级了 v0.5.78：schema 仍是 10、不是 dirty，任务继续运行，跨二进制替换写的那个段与源 md5 一致。从 v0.5.77 或更早版本升级，v0.5.78 的说明仍然适用。

## 新增

- **ADR 0011 PR-3：`SEGMENT_MISSING` 归已验证的链主人（#511，落地 #507；关联 #462）。** 只有集群租约持有者（不是代写、不是委派）这一轮读到了任务目录，才置位 `health.Fence.ChainVerified`，`task_health.chain_owner` 只在这时写。`SEGMENT_MISSING` 不走 `OWNER_CHANGED` 关闭，同一轮也不再打开。变异运行器：SM01–SM22 每个都被一个有名字的测试打死。

## 修复

- **#507 QA P2：** 运行器启动（`dropGoneLocalOnlyRows`）不再删掉还在本地保留期内的缺失封存段的目录行，所以故障切换、滚动重启和运维 Start 不会在段还缺着时关掉 `SEGMENT_MISSING`。
- **#476（P1），确认已修：** owner 在 `task_leases` 行上持有 `RUNNING` 事务时被冻住，对端不再无限期接管不了。MySQL meta 实测：冻住后 5–7 秒事务被杀，另一节点在 +32 秒接管（以前要 3 分 47 秒以上，且没有上限）。服务端超时在 v0.5.77 落地（ADR 0011 PR-2）。MariaDB meta 上还没有实测证明。
- **#509：** 安装文档指向 v0.5.78 和它的校验和。下一个固定版本的 PR 会把它们改到 v0.5.79。

## 已知问题

- **#473（P1）：** 每节点独立存储时，owner 暂停超过 TTL 后接管会遇到 `SEGMENT_NOT_ON_WORKER` 并写 FAILED / desired STOP。ADR 0011 PR-4 正在做。
- **#462（P1）：** 集群下的静默缺口在这一版有单测覆盖。完整关闭（目录比对、所有权变化时 finding 单调）要等 ADR 0011 PR-5。这条路径的集群 e2e 还没进 CI。
- **#513（P2）：** 接管后，恢复运行的旧 owner 可能在已断开的 meta 连接上遇到 `CHECKPOINT_WRITE_FAILED`（"busy buffer"）。这个错误没有被当成失租，于是新 owner 的 run 被写成 FAILED / desired STOP，健康 503，直到运维 Start。跟时序有关；不是 #511 引入的。
- **#510：** 非 owner 节点保留已关闭的 finding（见升级须知）。
- **#514（P3）：** 单机带任务记录时，重启后任务在跑，`GET /api/tasks/{id}/checkpoint` 却返回 404。v0.5.78 也有。
- **#512（P3）：** #507 QA 留下的测试和日志缺口：变异 A02（年龄来源对调）和 A03（按小时算截止）存活；读不了目录的那行是 info 级别；没有时间的行日志写成 "inside local retention"。
- **#490**、**#494**、**#500**、**#502**、**#503**、**#504**（P3）仍未关闭，**#487**、**#488**、**#451**、**#452**、**#455**、**#456**、**#446**、**#437**、**#438** 也是。`/api/findings` 还没有读超时（P2-7）。集群模式不跑巡检（A4 PARTIAL）。

## 自 v0.5.78 以来的变更

`git log --oneline --first-parent v0.5.78..d7b96e6`，加上这份说明所在的提交（v0.5.79 打在这个提交上）：

| 提交 | 变更 |
|---|---|
| `ddbf7d7` | PR #509：安装文档指向 v0.5.78 |
| `df2cee1`..`d7b96e6` | PR #511：ADR 0011 PR-3，`SEGMENT_MISSING` 归已验证的链主人（落地 #507） |

## QA 摘要

**PR #507 QA PASS（BinlogServerQA），第 2 轮 `a9ccf61e`，2026-10-21。**
- 共享 `data_dir`、不上传的实测：`SEGMENT_MISSING` 经过 SIGTERM、SIGKILL 和运维 Stop/Start 都还开着，文件恢复后才由链主人以 `EVAL` 关闭。
- 每节点独立磁盘的接管和单机同样保留目录行。`sealed_at` 优先于 `created_at`，`created_at` 兜底可用，没有时间或 `sealed_at` 在未来的行留下，运行中把本地保留从 3 改成 1 再 Start 会删掉这一行并以 `EVAL` 关闭。
- #209 的清理仍删掉老的缺失行。
- 变异运行器：打死 287 个，存活 0 个，等价 3 个。

**main `d7b96e6b` 的 Tip QA PASS（BinlogServerQA），2026-10-26。**
- 落地一致性：#511 与 #507 `a9ccf61e` 的 interdiff 为空。CI run 40244920168 通过。
- go1.26.10：`go test ./...` 和 `go test -race ./...` 17/17 通过。
- snapshot 构建：两个二进制都只用 `-w`；`govulncheck` 源码模式和二进制模式都干净。
- 冒烟：单机和 2 节点集群，LATEST、GTID、FILE_POS 任务，SIGTERM 和 SIGKILL 故障切换。
- 从 v0.5.78 升级：schema 仍是 10、不是 dirty；任务继续；跨替换的段 md5 一致。
- `SEGMENT_MISSING` 实测：打开时点名 `chain_owner`；经过 SIGTERM、SIGKILL 和 Start 仍开着；文件恢复、或保留让目录行到期时以 `EVAL` 关闭；读不了目录的 detail 能看到。
- #476 在 MySQL meta 上实测（见修复）。
