# Binlog Server v0.5.69

Release date: 2026-10-16

v0.5.69 fixes a P1 in the legacy failback upgrade repair (#405) and adds the first P2-5 chain integrity kinds to the health evaluator (P2-5.1, off by default). No new migration and no new config key: `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51.

## Upgrade Notes

- No migration, no config change, no API change. From v0.5.68, replace the binary and restart.
- **If you run standalone mode with a metadata store and the task directory was written by a build older than the legacy failback repair, upgrade to v0.5.69 before you repair it with Start.** On v0.5.68 and earlier, the repair could drop one stint from `/replay` without warning, and a restore then failed with `ERROR 1032`. In cluster mode the repair was correct, but a false `storage_alert` showed until the next transaction arrived.
- If you already repaired such a directory on standalone+meta with an older build, check `GET /api/tasks/{id}/window` and `/replay` after upgrading. v0.5.69 resumes above the repaired `.sealed.eN`, so the next seal no longer fails with `SEALED_FILE_EXISTS`.
- `health.evaluator` still defaults to `off`. With it off, API output is identical to v0.5.68 (QA compared 35 outputs). With `shadow` or `on`, the evaluator now also owns the P2-5.1 kinds listed below.
- The v0.5.67 upgrade notes still apply when upgrading from v0.5.66 or earlier.

## Fixes

- **#405 (P1): standalone+meta legacy failback upgrade silently dropped a stint.** Standalone uses an in-memory lease, so the epoch restarts at 1 on every start. After the repair, the resumed open file was `.open.e1`. That collided with the repaired `.sealed.e1` on the same (file name, epoch) catalog key, so the OPEN row replaced the SEALED row. `/replay` then skipped the sealed copy without warning, leaving a GTID gap that a restore reported as `ERROR 1032`. The next seal would also have failed with `SEALED_FILE_EXISTS`. Start now resumes at an epoch above every sealed copy on disk.
- **#405: false `storage_alert` after the repair in cluster mode.** The resumed `.open.e2` held only the 197-byte binlog header, which is a byte prefix of the sealed copy, so the I4 open-copy rule flagged it as a second copy until the next transaction arrived. An open file that contains only header events (FDE, PREVIOUS_GTIDS, ROTATE and the MariaDB equivalents, at any log position) is no longer treated as a copy.

## Features

- **P2-5.1: chain integrity kinds in the health evaluator** (`health.evaluator`, default `off`). The evaluator now owns these kinds, judged from the local chain only:
  - `SEGMENT_DAMAGED`;
  - `CHAIN_INDEX_GAP`;
  - `LEGACY_FAILBACK`;
  - `SEGMENT_MISSING`, for the local cases only.

  A new instance of one of these kinds opens only after two passes in a row find it, which absorbs transient results such as a rotation racing the directory listing. A legacy segment with no sha256 ledger line is compared with its sealed catalog row size; a mismatch is reported as `SEGMENT_MISSING` with evidence `local=<n> row=<m>`. Object-side checks come in a later release.

## Tooling

- `scripts/e2e/upgrade-legacy-failback.sh` takes `FBUP_MODE=cluster|standalone` (default `cluster`). `standalone` runs the legacy failback upgrade in standalone mode with a metadata store, which is the case behind #405.

## Changes since v0.5.68

`git log --oneline --first-parent v0.5.68..31ba843`, plus the merge of this notes PR, which v0.5.69 is tagged on:

| Commit | Change |
|---|---|
| `9db1b56` | PR #403: install docs point at v0.5.68 |
| `bc50e6f` | PR #404: P2-5.1 chain.local integrity kinds with two-pass confirm; legacy segment row size rule |
| `31ba843` | PR #407: standalone Start resumes above a repaired `.sealed.eN`; a header-only open file is not an I4 copy; `FBUP_MODE` (#405) |

## QA summary

Tip QA 31ba843 PASS (BinlogServerQA), 2026-10-16:
- standalone and cluster smoke tests restore MATCH, including after a restart;
- `upgrade-legacy-failback.sh` passes in both `FBUP_MODE=standalone` and `FBUP_MODE=cluster`, and the restore checksum matches the source;
- with the evaluator on and a fast-rotating healthy chain, there were 0 finding transitions;
- with the evaluator off, 35 API outputs are identical to v0.5.68.

`unit-and-e2e` and the other CI checks passed on `31ba843`. MySQL 8.0 only this round.

## Chinese Release Notes

Chinese version:

# Binlog Server v0.5.69 中文发布说明

> 对应版本：`v0.5.69`

发布日期：2026-10-16

v0.5.69 修复遗留 failback 升级修复中的一个 P1（#405），并为健康评估器加入第一批 P2-5 链完整性类别（P2-5.1，默认关闭）。没有新迁移，没有新配置项，`minRequiredSchemaVersion` 仍是 6，地板仍是 v0.5.51。

## 升级须知

- 没有新迁移，没有新配置项，API 不变。从 v0.5.68 升级直接换二进制重启即可。
- **standalone + 元数据库模式下，如果任务目录由早于遗留 failback 修复的版本写出，请先升级到 v0.5.69 再用 Start 修复。** v0.5.68 及更早版本的修复可能让 `/replay` 无提示地丢掉一段 stint，恢复时报 `ERROR 1032`。集群模式下修复本身正确，但在下一个事务到来前会误报 `storage_alert`。
- 如果已经用旧版本在 standalone+meta 下修复过这样的目录，升级后请检查 `GET /api/tasks/{id}/window` 与 `/replay`。v0.5.69 会在修复出的 `.sealed.eN` 之上续写，下一次封存不再报 `SEALED_FILE_EXISTS`。
- `health.evaluator` 仍默认 `off`；关闭时 API 输出与 v0.5.68 一致（QA 比对了 35 个输出）。设为 `shadow` 或 `on` 时，评估器还接管下面的 P2-5.1 类别。
- 从 v0.5.66 或更早升级仍按 v0.5.67 的升级须知操作。

## 修复

- **#405（P1）：standalone+meta 遗留 failback 升级无提示地丢掉一段 stint。** standalone 用内存租约，每次启动 epoch 都从 1 开始。修复后续写的打开文件是 `.open.e1`，与修复出的 `.sealed.e1` 落在同一个（文件名, epoch）目录键上，OPEN 行顶替了 SEALED 行。`/replay` 随之无提示地跳过封存副本，留下 GTID 缺口，恢复时报 `ERROR 1032`；下一次封存还会报 `SEALED_FILE_EXISTS`。现在 Start 会在磁盘上所有封存副本之上的 epoch 续写。
- **#405：集群模式修复后的误报 `storage_alert`。** 续写的 `.open.e2` 只有 197 字节的 binlog 头，是封存副本的字节前缀，I4 打开副本规则在下一个事务到来前把它当成第二份副本。只含头事件（FDE、PREVIOUS_GTIDS、ROTATE 及 MariaDB 对应事件，任意 log 位置）的打开文件不再算作副本。

## 新功能

- **P2-5.1：健康评估器的链完整性类别**（`health.evaluator`，默认 `off`）。评估器现在只依据本地链判定这些类别：`SEGMENT_DAMAGED`、`CHAIN_INDEX_GAP`、`LEGACY_FAILBACK`，以及 `SEGMENT_MISSING` 的本地情形。这些类别的新实例要连续两轮都判出才打开，用来吸收轮转与目录列举竞争之类的瞬时结果。没有 sha256 账本行的遗留段会与其已封存目录行的大小比较，不一致报 `SEGMENT_MISSING`，证据为 `local=<n> row=<m>`。对象侧检查在后续版本提供。

## 工具

- `scripts/e2e/upgrade-legacy-failback.sh` 支持 `FBUP_MODE=cluster|standalone`（默认 `cluster`）。`standalone` 在 standalone + 元数据库模式下跑遗留 failback 升级，即 #405 的场景。

## 自 v0.5.68 以来的变更

| 提交 | 变更 |
|---|---|
| `9db1b56` | PR #403：安装文档指向 v0.5.68 |
| `bc50e6f` | PR #404：P2-5.1 本地链完整性类别与两轮确认；遗留段目录行大小规则 |
| `31ba843` | PR #407：standalone Start 在修复出的 `.sealed.eN` 之上续写；只含头的打开文件不算 I4 副本；`FBUP_MODE`（#405） |

## QA 摘要

Tip QA 31ba843 PASS（BinlogServerQA），2026-10-16：standalone 与集群冒烟恢复 MATCH（含重启后）；`upgrade-legacy-failback.sh` 在 `FBUP_MODE=standalone` 与 `cluster` 下都通过，恢复校验和与源一致；评估器开启、健康链快速轮转时 0 次 finding 转换；评估器关闭时 35 个 API 输出与 v0.5.68 一致。`unit-and-e2e` 与其余 CI 检查在 `31ba843` 上通过。本轮只测了 MySQL 8.0。
