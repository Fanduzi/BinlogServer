# Binlog Server v0.5.70

Release date: 2026-10-17

v0.5.70 fixes a P1 crash in the health evaluator (#413) and adds two more P2-5 steps to it: object-side kinds with `UPLOAD_LATE` and `UPLOAD_UNVERIFIED` (P2-5.2), and the `GTID_HOLE` waiver, switch records and `CHECKPOINT_AHEAD` (P2-5.3). The evaluator still defaults to `off`. No new migration: `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51.

## Upgrade Notes

- No migration. From v0.5.69, replace the binary and restart. New config keys are optional and have defaults (see below).
- **If you run `health.evaluator: shadow` or `on`, upgrade to v0.5.70.** On v0.5.69 a node could crash with `fatal error: concurrent map read and map write` under frequent rotation (#413).
- `health.evaluator` still defaults to `off`. With it off, behaviour is the same as v0.5.69 (tip QA compared both).
- New API: `POST` / `DELETE /api/alerts/waive` for `GTID_HOLE` waivers only. The full alerts API comes in P2-7.
- The v0.5.67 upgrade notes still apply when upgrading from v0.5.66 or earlier.

## Fixes

- **#413 (P1): evaluator crash under frequent rotation.** `chainFacts` read the per-task rehash map after releasing the evaluator lock while `setRehash` wrote it under the lock, so the process could die with `concurrent map read and map write`. The bug dates from `ba1bd375`. Mismatching rehash entries are now copied while the lock is held (#414).
- **GTID_HOLE stayed open after an in-place repair.** After a damaged segment was repaired in place, cached chain facts were not dropped, so its CRITICAL `GTID_HOLE` never closed. The cache is now invalidated when the segment's checksum result changes (part of #412).

## Features

- **P2-5.2: object kinds, `UPLOAD_LATE`, `UPLOAD_UNVERIFIED`** (#408, design 0009 §21).
  - `UPLOAD_LATE`: a sealed segment not uploaded after `upload.grace_sec` (default 900) is WARNING, and CRITICAL once sealed for `health.upload_late_critical_after_sec` (default 86400). Both have per-task overrides (`upload.grace_task_sec`, `health.upload_late_critical_after_task_sec`).
  - `UPLOAD_UNVERIFIED` opens only after two passes in a row.
  - A missing or wrong-size object is reported on subject `<segment>#object`.
  - Objects are checked with one bounded S3 `ListObjects` per prefix, cached for `health.object_list_interval_sec` (default 300) and capped by `health.object_list_limit` (default 10000). A truncated or failed listing makes task health UNKNOWN; nothing is opened or closed from it.
  - Removing object storage from config closes open object findings with reason `CONFIG`.
- **P2-5.3: `GTID_HOLE` waiver, switch records, `CHECKPOINT_AHEAD`** (#412, design 0009 §22).
  - `GTID_HOLE` is managed and can be waived with exact canonical GTID evidence via `POST /api/alerts/waive`; `DELETE` removes the waiver and the finding reopens.
  - Recorded source switches (`.source-chain` stints, `SOURCE_SWITCHOVER`) no longer produce a false `GTID_HOLE` across a switch.
  - `CHECKPOINT_AHEAD` is managed: the checkpoint is read fresh on every pass.

## Known issues

Neither is a regression from v0.5.69; both also reproduce there.

- **#417: cluster split-brain on segment files.** If the owner is frozen (about 20 s) and ownership moves, the old owner can keep writing into a segment that is not sealed yet while the new owner writes it too. The task then stays in `STORAGE_INCONSISTENT` (re-dump damage). The #411 fence covers metadata writes only, not segment files. A fix is in progress.
- **#415: false `SEGMENT_UNREADABLE` on sealed segments.** After repeated takeovers a sealed segment can get an extra 42-byte Rotate event appended, and is then reported as `SEGMENT_UNREADABLE`. The fix is PR #416, in QA.

## Changes since v0.5.69

`git log --oneline --first-parent v0.5.69..5db0a9b`, plus the merge of this notes PR, which v0.5.70 is tagged on:

| Commit | Change |
|---|---|
| `360f1a8` | PR #410: install docs point at v0.5.69 |
| `f5a802f` | PR #408: P2-5.2 object kinds, `UPLOAD_LATE`, `UPLOAD_UNVERIFIED` |
| `b4cb506` | PR #414: copy rehash results under the evaluator lock (#413) |
| `5db0a9b` | PR #412: P2-5.3 `GTID_HOLE` waiver, switches, `CHECKPOINT_AHEAD`; drop cached facts on checksum change |

## QA summary

Tip QA 5db0a9b PASS (BinlogServerQA), 2026-10-17, no regression against v0.5.69:
- standalone smoke: 50 rotations, all uploaded, no findings; restore checksum matches the source;
- the three PRs together (`-race` build, two-node cluster, moto S3, frequent rotation, 12 kills): `SEGMENT_MISSING#object`, `GTID_HOLE` and `SEGMENT_UNREADABLE` open and close correctly; 0 races, 0 crashes; 186 segments uploaded, restore checksum matches;
- with the evaluator off, behaviour is identical to v0.5.69.

Per-PR QA: #408, #414 (80 rounds / 642 rotations under `-race`, 0 races; base raced within 2 minutes) and #412 all passed. CI passed on `5db0a9b`.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.70 中文发布说明

> 对应版本：`v0.5.70`

发布日期：2026-10-17

v0.5.70 修复健康评估器的一个 P1 崩溃（#413），并为评估器加入两步 P2-5：对象侧类别与 `UPLOAD_LATE`、`UPLOAD_UNVERIFIED`（P2-5.2），以及 `GTID_HOLE` 豁免、切换记录和 `CHECKPOINT_AHEAD`（P2-5.3）。评估器仍默认 `off`。没有新迁移，`minRequiredSchemaVersion` 仍是 6，地板仍是 v0.5.51。

## 升级须知

- 没有新迁移。从 v0.5.69 升级直接换二进制重启即可。新配置项都可选且有默认值（见下）。
- **如果开启了 `health.evaluator: shadow` 或 `on`，请升级到 v0.5.70。** v0.5.69 在频繁轮转时节点可能因 `fatal error: concurrent map read and map write` 崩溃（#413）。
- `health.evaluator` 仍默认 `off`，关闭时行为与 v0.5.69 一致（tip QA 已比对）。
- 新 API：`POST` / `DELETE /api/alerts/waive`，只用于 `GTID_HOLE` 豁免。完整告警 API 在 P2-7。
- 从 v0.5.66 或更早升级仍按 v0.5.67 的升级须知操作。

## 修复

- **#413（P1）：频繁轮转时评估器崩溃。** `chainFacts` 放锁后才读每个任务的 rehash map，而 `setRehash` 在锁内写它，进程可能以 `concurrent map read and map write` 退出。问题自 `ba1bd375` 起就存在。现在在持锁时拷贝不匹配的 rehash 条目（#414）。
- **原地修复后 GTID_HOLE 不关闭。** 损坏段原地修复后缓存的链事实没有清掉，CRITICAL `GTID_HOLE` 一直不关。现在段的校验结果变化时会清掉缓存（#412 的一部分）。

## 新功能

- **P2-5.2：对象类别、`UPLOAD_LATE`、`UPLOAD_UNVERIFIED`**（#408，设计 0009 §21）。
  - `UPLOAD_LATE`：已封存段超过 `upload.grace_sec`（默认 900）未上传为 WARNING，封存超过 `health.upload_late_critical_after_sec`（默认 86400）升为 CRITICAL。两者都可按任务覆盖（`upload.grace_task_sec`、`health.upload_late_critical_after_task_sec`）。
  - `UPLOAD_UNVERIFIED` 要连续两轮判出才打开。
  - 对象缺失或大小不对，报在 `<segment>#object` 上。
  - 对象检查每个前缀只做一次有上限的 S3 `ListObjects`，缓存 `health.object_list_interval_sec`（默认 300），上限 `health.object_list_limit`（默认 10000）。列表被截断或失败时任务健康为 UNKNOWN，不据此打开或关闭任何 finding。
  - 从配置中移除对象存储后，已打开的对象 finding 以 `CONFIG` 原因关闭。
- **P2-5.3：`GTID_HOLE` 豁免、切换记录、`CHECKPOINT_AHEAD`**（#412，设计 0009 §22）。
  - `GTID_HOLE` 由评估器接管，可用精确的规范 GTID 证据经 `POST /api/alerts/waive` 豁免；`DELETE` 删除豁免后 finding 重新打开。
  - 已记录的源切换（`.source-chain` stint、`SOURCE_SWITCHOVER`）不再在切换处误报 `GTID_HOLE`。
  - `CHECKPOINT_AHEAD` 由评估器接管，每轮都重新读取 checkpoint。

## 已知问题

两项都不是相对 v0.5.69 的回归，在 v0.5.69 上同样能复现。

- **#417：集群段文件脑裂。** owner 被冻结约 20 秒、所有权转移后，老 owner 会继续往尚未封存的段文件里写，同时新 owner 也在写，任务永久停在 `STORAGE_INCONSISTENT`（re-dump damage）。#411 的 fence 只挡元数据写入，不挡段文件。修复进行中。
- **#415：已封存段误报 `SEGMENT_UNREADABLE`。** 多次接管后已封存段可能被追加一个 42 字节的 Rotate 事件，随后被报为 `SEGMENT_UNREADABLE`。修复是 PR #416，正在 QA。

## 自 v0.5.69 以来的变更

| 提交 | 变更 |
|---|---|
| `360f1a8` | PR #410：安装文档指向 v0.5.69 |
| `f5a802f` | PR #408：P2-5.2 对象类别、`UPLOAD_LATE`、`UPLOAD_UNVERIFIED` |
| `b4cb506` | PR #414：在评估器锁内拷贝 rehash 结果（#413） |
| `5db0a9b` | PR #412：P2-5.3 `GTID_HOLE` 豁免、切换记录、`CHECKPOINT_AHEAD`；校验结果变化时清缓存 |

## QA 摘要

Tip QA 5db0a9b PASS（BinlogServerQA），2026-10-17，相对 v0.5.69 无回归：standalone 冒烟 50 次轮转全部上传、无 finding，恢复校验和与源一致；三个 PR 合跑（`-race`、两节点集群、moto S3、频繁轮转、12 次 kill）时 `SEGMENT_MISSING#object`、`GTID_HOLE`、`SEGMENT_UNREADABLE` 正确打开和关闭，0 race、0 崩溃，186 段全部上传，恢复校验和一致；评估器关闭时行为与 v0.5.69 一致。单 PR QA：#408、#414（`-race` 下 80 轮 / 642 次轮转 0 race，base 2 分钟内即报 race）、#412 均通过。CI 在 `5db0a9b` 上通过。
