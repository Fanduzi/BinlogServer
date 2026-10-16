# Binlog Server v0.5.71

Release date: 2026-10-17

v0.5.71 fixes a P1 cluster split-brain on segment files (#417) by fencing every data-plane write on the task lease (ADR 0009 P2-6, design §23). It also includes the meta-pause recovery fix (#360, P2-6a) and the takeover open-segment copy (#415). No new migration: `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51. No new config keys.

## Upgrade Notes

- No migration. From v0.5.70, replace the binary and restart.
- **Cluster users should upgrade.** On v0.5.70 and earlier, a frozen owner that lost its lease could keep writing segment files after takeover, leaving the task in `STORAGE_INCONSISTENT` (#417).
- **Behaviour change:** while meta lease renewals keep failing, a node now stops writing after `0.8 × lease_ttl_sec` (about 12 s with the default TTL) instead of continuing until `lease_grace_sec` (about 30 s). Replication resumes once the lease is renewed or taken over.
- **Cluster mode requires a shared `data_dir` for now** (see known issue #421).
- The v0.5.67 upgrade notes still apply when upgrading from v0.5.66 or earlier.

## Fixes

- **#417 (P1): cluster split-brain on segment files** (#420). Every write boundary is now fenced on the lease `(owner, epoch)`:
  - a local lease clock refuses appends, new open files, seals and uploads once the epoch may have expired;
  - checkpoint and `binlog_files` writes lock and check `task_leases` (`FOR UPDATE`) in the same meta transaction;
  - seal renames use `RENAME_NOREPLACE`;
  - a fenced run exits as a lease handoff (`ErrWriteFenced`), not as FAILED.
- **#360: task left STOPPED with no owner after a meta pause** (#411, shipped in #420). An old owner that lost its lease could write STOPPING/STOPPED into a task row the new owner had just claimed. That write is now checked against `task_leases` in the same transaction.
- **#415: false `SEGMENT_UNREADABLE` on sealed segments** (#416, shipped in #420). Takeover used to rename the previous epoch's open segment, keeping the same inode, so the old owner's stale handle could append a 42-byte Rotate to a sealed file. The new epoch now copies the complete events into its own file and removes the old one. Takeover costs one extra local copy of at most one open segment.

## Known issues

- **#421 (P1): separate data dirs per node.** If cluster nodes use separate local `data_dir`s, the node that takes over holds the lease but writes nothing, because the catalog stores the previous owner's absolute paths. This is a silent failure and not a regression from v0.5.70. **Use a shared `data_dir` for all cluster nodes until it is fixed.**

## Changes since v0.5.70

`git log --oneline --first-parent v0.5.70..c42360d`, plus the merge of this notes PR, which v0.5.71 is tagged on:

| Commit | Change |
|---|---|
| `67e45cb` | PR #419: install docs point at v0.5.70 |
| `c42360d` | PR #420: P2-6 data-plane lease fence (#417), includes #411 (#360) and #416 (#415) |

## QA summary

Tip QA c42360d PASS (BinlogServerQA), 2026-10-17:
- cluster SIGSTOP soak (shared `data_dir`, two nodes, TTL 15 s, `-race`, moto S3, about 400 source rotations, 12 freezes of 20/60/120 s): 12/12 takeovers correct; 9 fence refusals and 0 old-epoch catalog rows after takeover; 359 sealed segments unchanged and all uploaded; 0 `SEGMENT_UNREADABLE`, 0 `STORAGE_INCONSISTENT`, 0 races; restore 5013/5013 rows, checksum matches;
- standalone smoke (50 rotations, uploads, evaluator on and off): restore checksum matches the source in both runs.

CI passed on `c42360d`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.71 中文发布说明

> 对应版本：`v0.5.71`

发布日期：2026-10-17

v0.5.71 修复集群段文件脑裂这个 P1（#417）：所有数据面写入都按任务 lease 加栅栏（ADR 0009 P2-6，设计 §23）。同时带上 meta 停顿后的恢复修复（#360，P2-6a）和接管时复制 open 分段（#415）。没有新迁移，`minRequiredSchemaVersion` 仍是 6，地板仍是 v0.5.51。没有新配置项。

## 升级须知

- 没有新迁移。从 v0.5.70 升级直接换二进制重启即可。
- **集群用户请升级。** v0.5.70 及更早版本中，被冻结、丢了 lease 的旧 owner 在接管后仍可能继续写段文件，任务停在 `STORAGE_INCONSISTENT`（#417）。
- **行为变化：** meta 续约持续失败时，节点在 `0.8 × lease_ttl_sec`（默认 TTL 下约 12 秒）后停止落盘，不再一直写到 `lease_grace_sec`（约 30 秒）。续约成功或被接管后复制恢复。
- **集群模式目前需要共享 `data_dir`**（见已知问题 #421）。
- 从 v0.5.66 或更早升级仍按 v0.5.67 的升级须知操作。

## 修复

- **#417（P1）：集群段文件脑裂**（#420）。每个写边界都带 lease `(owner, epoch)` 栅栏：
  - 本地 lease 时钟在 epoch 可能过期后拒绝追加、新建 open 文件、封存和上传；
  - checkpoint 和 `binlog_files` 的写在同一个 meta 事务里对 `task_leases` 加锁（`FOR UPDATE`）校验；
  - 封存改名使用 `RENAME_NOREPLACE`；
  - 被拒的 run 作为租约移交退出（`ErrWriteFenced`），不写 FAILED。
- **#360：meta 停顿后任务停在 STOPPED、无 owner**（#411，随 #420 发布）。丢了 lease 的旧 owner 可能把 STOPPING/STOPPED 写进新 owner 刚认领的任务行。现在这条写在同一事务里校验 `task_leases`。
- **#415：已封存段误报 `SEGMENT_UNREADABLE`**（#416，随 #420 发布）。接管时原来直接改名上一个 epoch 的 open 分段、沿用同一 inode，旧 owner 的旧句柄可能向已封存文件追加 42 字节 Rotate。现在新 epoch 把完整事件复制到自己的文件并删除旧文件。接管时多一次本地复制（最多一个 open 分段）。

## 已知问题

- **#421（P1）：各节点使用独立 data dir。** 集群节点各用本地 `data_dir` 时，接管的节点拿着 lease 却什么都不写，因为目录里存的是前一个 owner 的绝对路径。这是静默失败，不是相对 v0.5.70 的回归。**修复前请让所有集群节点使用共享 `data_dir`。**

## 自 v0.5.70 以来的变更

| 提交 | 变更 |
|---|---|
| `67e45cb` | PR #419：安装文档指向 v0.5.70 |
| `c42360d` | PR #420：P2-6 数据面 lease 栅栏（#417），包含 #411（#360）和 #416（#415） |

## QA 摘要

Tip QA c42360d PASS（BinlogServerQA），2026-10-17：集群 SIGSTOP soak（共享 `data_dir`、两节点、TTL 15 秒、`-race`、moto S3、约 400 次源库轮转、12 次 20/60/120 秒冻结）12/12 次接管正确，9 次栅栏拒绝，接管后旧 epoch 目录行 0，359 个封存段无改动且全部上传，`SEGMENT_UNREADABLE`、`STORAGE_INCONSISTENT`、race 均为 0，恢复 5013/5013 行、校验和一致；standalone 冒烟（50 次轮转、上传、评估器开和关）两次恢复校验和都与源一致。CI 在 `c42360d` 上通过。
