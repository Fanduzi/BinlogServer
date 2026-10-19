# Binlog Server v0.5.72

Release date: 2026-10-19

v0.5.72 makes cluster failures loud instead of silent (#424). It fixes three P1s: a takeover on a node with its own `data_dir` that silently stopped backing up (#421), a task left STOPPED with no owner and no finding after the meta database was read-only (#425), and a cluster with only control-plane nodes left that reported ok after the only worker died (#426). It adds the CRITICAL `TASK_NO_PROGRESS` backstop and one new config key, `health.no_progress_after_sec`. No new migration: the schema stays 9, `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51.

## Upgrade Notes

- No migration. From v0.5.71, replace the binary and restart.
- **Cluster users should upgrade.** On v0.5.71 and earlier, several cluster failures stopped backups with no finding and `/api/health` still ok (#421, #425, #426).
- **The new alerts come from the health evaluator.** `TASK_FAILED` for unowned tasks, `TASK_NO_PROGRESS`, the `_health` lease and its `FINDINGS_PERSIST_FAILED` need `health.evaluator: on` (default `off` in v0.5.x; it becomes the default in v0.6.0). In a cluster, switch it on only after every node runs v0.5.72. With `off`, the takeover ownership check (`SEGMENT_NOT_ON_WORKER` → FAILED), FAILED preservation and the #425 stale-write fence still apply, but no finding opens.
- **New config key `health.no_progress_after_sec`** (default `120`; `0` means the default; negative values are rejected). `TASK_NO_PROGRESS` opens after this many seconds without progress.
- **Control-plane-only nodes: set `cluster.worker_id`.** The evaluator on a control-plane-only node uses `cluster.worker_id` as its identity, or else the `.node_id` persisted in `data_dir`. Several control-plane nodes that share a `data_dir` would share that id, so give each one its own `cluster.worker_id`.
- **Cluster nodes that run tasks must share one `data_dir`.** This is now in the deployment guide. A takeover on a node whose `data_dir` does not hold the previous owner's segments now fails loudly with `SEGMENT_NOT_ON_WORKER` instead of silently stalling; it does not resume the backup. Mount or copy the directory, then Start the task.
- **FAILED now stays FAILED until an operator Start.** An old owner that wakes up can no longer rewrite it as STOPPED.
- Cluster upgrade from v0.5.71 was not exercised in tip QA; the standalone upgrade from v0.5.71 was (see QA summary).
- The v0.5.67 upgrade notes still apply when upgrading from v0.5.66 or earlier.

## Fixes

- **#421 (P1): a cluster FAILED was always silent** (#424). FAILED clears the row's owner and releases the lease, and only the lease holder evaluated a task, so `TASK_FAILED` never opened and `/api/health` stayed ok. A `_health` lease (row `_health` in `task_leases`, TTL 3 × sweep) now elects one node of any role. It evaluates only `task.state` of unowned tasks (FAILED, STOPPED, CREATED, unclaimed STARTING), using the proxy fence of the last task lease epoch, and opens CRITICAL `TASK_FAILED` for FAILED.
- **#421 (P1): the old owner erased the failure** (#424). An owner that woke up read another epoch's FAILED as a remote Stop and wrote `STOPPED / desired STOP / owner "" / epoch 0` without a fence. Now FAILED leaves only through an operator Start: a live run that sees FAILED from another epoch stops and writes nothing, and the stop, lease-degraded and unfinished-Stop paths never write over FAILED. The task-row fence is one rule, `tasks.CheckTaskRowFence`.
- **#421 (P1): takeover with a readable foreign path** (#424). With separate `data_dir`s on one host, or the same path mounted on another disk, the new owner continued in the previous owner's directory and wrote nothing to its own, with no finding. A catalog path must now be under this node's own `data_dir` (both sides made absolute and symlinks resolved); otherwise the task goes FAILED with `SEGMENT_NOT_ON_WORKER` and CRITICAL `TASK_FAILED` opens. Uploaded segments whose local path is foreign are read back into this node's `data_dir`.
- **Relative `data_dir`** (#424). With a relative `data_dir`, a takeover took this node's own segments for foreign ones and failed with `SEGMENT_NOT_ON_WORKER`. Fixed by the same absolute-path comparison.
- **#426 (P1): only control-plane nodes left, no alert** (#424). After the only worker was killed, the task row stayed `RUNNING / dead owner`, no finding opened, and the control-plane node reported ok: its evaluator had an empty node id and never took `_health`, and a row with a (dead) owner was not counted as unowned. Every node role now runs the unowned-task evaluator, and rows whose owner's lease has expired are evaluated and reported as `TASK_NO_PROGRESS`.
- **#425 (P1): task silently STOPPED after meta was read-only** (#424). After a lease handoff, the old owner's pending rewrite of `STOPPED / owner "" / epoch 0` was written back without a fence once meta was writable again, and the new owner took it for a remote Stop. The pending rewrite is now dropped on handoff, rewrites carry the `(owner, epoch)` fence, and the new owner ignores a stale `STOPPED / desired RUN` row from another owner or epoch. A failed `_health` lease write reports `FINDINGS_PERSIST_FAILED` on every node, so all nodes show degraded while meta is read-only.
- An owner that held the lease but kept retrying at startup (leftover dump KILL failing) never opened `TASK_NO_PROGRESS`. Progress is now tracked per held epoch, so it opens within claim + `no_progress_after_sec` + one sweep.
- A FAILED row with `spec_revision` 0 did not open `TASK_FAILED`. It is now matched on `failed_spec_revision == spec_revision`.

## Added

- **CRITICAL `TASK_NO_PROGRESS`** (design 0009 §24.9). For a task with desired RUN: opens when this node's run has not advanced its position under its own `data_dir` and has not confirmed it is caught up with the source (heartbeat) within `health.no_progress_after_sec`, or when no worker is running it (row stuck in STARTING, STOPPED and so on). `/api/health` turns degraded. It is a backstop for "holds the task but does nothing", whatever the cause. An idle source does not trigger it.

## Known issues

- **#427 (P2): meta blackholed.** When connections to meta hang instead of being refused (firewall DROP, partition, frozen host), `/api/health` on every node does not answer within 3 s, and no finding is recorded afterwards. A refused connection still reports degraded. Probes that treat a timeout as down will still catch it.

## Changes since v0.5.71

`git log --oneline --first-parent v0.5.71..38090f3`, plus the merge of this notes PR, which v0.5.72 is tagged on:

| Commit | Change |
|---|---|
| `fdc36e0` | PR #423: install docs point at v0.5.71 |
| `38090f3` | PR #424: unowned-task health, FAILED preservation, `SEGMENT_NOT_ON_WORKER` for foreign paths, `TASK_NO_PROGRESS` (#421, #425, #426) |

## QA summary

Tip QA 38090f3 PASS (BinlogServerQA), 2026-10-19, `-race` build, evaluator on, `no_progress_after_sec=30`, MariaDB 11.4 + moto S3:
- only a control-plane node left after the owner is killed: `TASK_NO_PROGRESS` CRITICAL within 60 s, node degraded; closes after the worker restarts;
- meta `read_only` for 100 s (non-SUPER user): both nodes degraded from +10 s; after recovery the task is RUNNING and writing again;
- separate `data_dir` takeover (sc1): loud `SEGMENT_NOT_ON_WORKER`, CRITICAL `TASK_FAILED`, recovers after Start;
- shared `data_dir` SIGSTOP soak, 8/8 takeovers: 123 sealed segments all uploaded and unchanged, 0 `STORAGE_INCONSISTENT`, 0 races, restore 2325/2325 rows, checksum matches;
- idle source 120 s: no false alert; meta pause 12 s then 30 s: restore 356/356 rows match;
- standalone dogfood (50 rotations, uploads, API, console) and standalone upgrade from the v0.5.71 release binary mid-file: byte-continuous, 80 segments uploaded, restore across both binaries 288/288 rows match; schema 9 on both, `migrate up` reports no change.
- Not run: cluster upgrade from v0.5.71.

CI passed on `38090f3`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.72 中文发布说明

> 对应版本：`v0.5.72`

发布日期：2026-10-19

v0.5.72 让集群里的失败不再静默（#424）。修复三个 P1：节点各用独立 `data_dir` 时接管后静默停备（#421）；meta 库只读恢复后任务被留在 STOPPED、无 owner、无 finding（#425）；唯一的 worker 挂掉、只剩 control-plane 节点时仍报 ok（#426）。新增 CRITICAL `TASK_NO_PROGRESS` 兜底和一个配置项 `health.no_progress_after_sec`。没有新迁移：schema 仍是 9，`minRequiredSchemaVersion` 仍是 6，地板仍是 v0.5.51。

## 升级须知

- 没有新迁移。从 v0.5.71 升级直接换二进制重启即可。
- **集群用户请升级。** v0.5.71 及更早版本中，多种集群故障会让备份停止，却不开 finding，`/api/health` 仍是 ok（#421、#425、#426）。
- **新告警由健康评估器产生。** 无主任务的 `TASK_FAILED`、`TASK_NO_PROGRESS`、`_health` lease 及其 `FINDINGS_PERSIST_FAILED` 需要 `health.evaluator: on`（v0.5.x 默认 `off`，v0.6.0 起默认 `on`）。集群里请等所有节点都升到 v0.5.72 再打开。保持 `off` 时，接管归属检查（`SEGMENT_NOT_ON_WORKER` → FAILED）、FAILED 不被改写、#425 的过期写栅栏仍然生效，但不会开 finding。
- **新配置项 `health.no_progress_after_sec`**（默认 `120`，`0` 表示默认值，不可为负）。超过这么多秒没有进展就打开 `TASK_NO_PROGRESS`。
- **仅 control-plane 的节点请配置 `cluster.worker_id`。** 这类节点的评估器以 `cluster.worker_id` 作为身份，没配时用 `data_dir` 里持久化的 `.node_id`。多个 control-plane 节点共享 `data_dir` 时会拿到同一个 id，所以请各自配置不同的 `cluster.worker_id`。
- **集群里运行任务的节点必须共享同一个 `data_dir`。** 部署文档已写明。接管节点的 `data_dir` 里没有前一个 owner 的分段时，现在会以 `SEGMENT_NOT_ON_WORKER` 明确失败，不再静默空转；但不会自动续备。把目录挂过来或拷过来后再 Start。
- **FAILED 现在会一直保持到操作员 Start。** 旧 owner 醒来也不能再把它改写成 STOPPED。
- tip QA 没有跑集群模式从 v0.5.71 升级；standalone 从 v0.5.71 升级已通过（见 QA 摘要）。
- 从 v0.5.66 或更早升级仍按 v0.5.67 的升级须知操作。

## 修复

- **#421（P1）：集群里的 FAILED 总是静默**（#424）。FAILED 会清掉任务行的 owner 并释放 lease，而只有 lease 持有者会评估任务，所以 `TASK_FAILED` 从不打开，`/api/health` 一直是 ok。现在由 `_health` lease（`task_leases` 里的 `_health` 行，TTL 3 × sweep）选出一个节点（任何角色），用上一个任务 lease epoch 的代理栅栏，只评估无主任务（FAILED、STOPPED、CREATED、未认领的 STARTING）的 `task.state`，FAILED 打开 CRITICAL `TASK_FAILED`。
- **#421（P1）：旧 owner 抹掉失败**（#424）。醒来的旧 owner 把别的 epoch 写下的 FAILED 当成远端 Stop，无栅栏地写成 `STOPPED / desired STOP / owner 空 / epoch 0`。现在 FAILED 只能由操作员 Start 离开：正在跑的 run 看到别的 epoch 写的 FAILED 就停下，什么都不写；停止、续约降级、未完成 Stop 的收尾路径都不再覆盖 FAILED。任务行栅栏统一为一条规则 `tasks.CheckTaskRowFence`。
- **#421（P1）：接管时能读到别人的路径**（#424）。同一台机器上各用独立 `data_dir`，或者同路径挂了别的盘时，新 owner 在旧 owner 的目录里续写，自己的 `data_dir` 什么都不写，也不开 finding。现在目录里的路径必须在本节点自己的 `data_dir` 下（两边都转成绝对路径并解析符号链接后比较），否则任务 FAILED（`SEGMENT_NOT_ON_WORKER`），打开 CRITICAL `TASK_FAILED`。本地路径在别处的已上传分段会读回到本节点 `data_dir`。
- **相对路径的 `data_dir`**（#424）。`data_dir` 配成相对路径时，接管把本节点自己的分段当成别人的，以 `SEGMENT_NOT_ON_WORKER` 失败。同样的绝对路径比较修复了这个问题。
- **#426（P1）：只剩 control-plane 节点时没有告警**（#424）。唯一的 worker 被 kill 后，任务行停在 `RUNNING / 死 owner`，不开 finding，control-plane 节点报 ok：它的评估器 node id 为空，从不持有 `_health`，而 owner 非空（已死）的行也不算无主。现在所有节点角色都运行无主任务评估，owner 的 lease 已过期的行也会被评估，按 `TASK_NO_PROGRESS` 报出。
- **#425（P1）：meta 只读后任务被静默写成 STOPPED**（#424）。租约移交后，旧 owner 待重写的 `STOPPED / owner 空 / epoch 0` 行在 meta 恢复可写后被无栅栏地写回，新 owner 把它当成远端 Stop。现在移交时丢弃待重写行，重写带 `(owner, epoch)` 栅栏，新 owner 忽略来自别的 owner 或 epoch 的旧 `STOPPED / desired RUN` 行。`_health` lease 写失败在每个节点上都报 `FINDINGS_PERSIST_FAILED`，meta 只读期间所有节点都显示 degraded。
- 持有 lease 却在启动阶段一直重试（leftover dump KILL 失败）的 owner 从不打开 `TASK_NO_PROGRESS`。现在进度按持有的 epoch 记，在 claim + `no_progress_after_sec` + 一个 sweep 内打开。
- `spec_revision` 为 0 的 FAILED 行不会打开 `TASK_FAILED`。现在按 `failed_spec_revision == spec_revision` 判断。

## 新增

- **CRITICAL `TASK_NO_PROGRESS`**（设计 0009 §24.9）。desired RUN 的任务，本节点的 run 在 `health.no_progress_after_sec` 内既没有在自己的 `data_dir` 下推进位点、也没有确认已追平源（心跳），或者没有任何 worker 在跑它（任务行停在 STARTING、STOPPED 等）时打开，`/api/health` 变为 degraded。这是对“持有任务却不干活”的兜底，不依赖具体原因。源库空闲不会触发。

## 已知问题

- **#427（P2）：meta 被黑洞。** 连到 meta 的连接挂住而不是被拒绝时（防火墙 DROP、网络分区、主机卡死），所有节点的 `/api/health` 3 秒内都不响应，事后也不补记任何 finding。连接被拒绝时仍会报 degraded。把超时当成 down 的探针仍能发现。

## 自 v0.5.71 以来的变更

| 提交 | 变更 |
|---|---|
| `fdc36e0` | PR #423：安装文档指向 v0.5.71 |
| `38090f3` | PR #424：无主任务健康评估、FAILED 保持、别人路径报 `SEGMENT_NOT_ON_WORKER`、`TASK_NO_PROGRESS`（#421、#425、#426） |

## QA 摘要

Tip QA 38090f3 PASS（BinlogServerQA），2026-10-19，`-race` 构建、评估器 on、`no_progress_after_sec=30`、MariaDB 11.4 + moto S3：owner 被杀、只剩 control-plane 节点时 60 秒内打开 CRITICAL `TASK_NO_PROGRESS`、节点 degraded，worker 重启后关闭；meta `read_only` 100 秒（非 SUPER 账号）从 +10 秒起两个节点都 degraded，恢复后任务 RUNNING 并继续写入；独立 `data_dir` 接管（sc1）明确报 `SEGMENT_NOT_ON_WORKER` 和 CRITICAL `TASK_FAILED`，Start 后恢复；共享 `data_dir` SIGSTOP soak 8/8 次接管，123 个封存段全部上传且未改动，`STORAGE_INCONSISTENT` 和 race 均为 0，恢复 2325/2325 行、校验和一致；源库空闲 120 秒无误报；meta 先暂停 12 秒再 30 秒，恢复 356/356 行一致；standalone dogfood（50 次轮转、上传、API、控制台）以及从 v0.5.71 发布二进制在文件写到一半时升级：字节连续，80 段全部上传，跨两个版本恢复 288/288 行一致，两版 schema 都是 9，`migrate up` 报 no change。未跑：集群模式从 v0.5.71 升级。CI 在 `38090f3` 上通过。
