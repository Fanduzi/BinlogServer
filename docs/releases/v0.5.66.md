# Binlog Server v0.5.66

Release date: 2026-10-15

v0.5.66 detects a sealed source file stored twice in a task directory (#370, PR #372; ADR 0009 invariant I4 at runtime). No API change. No new config key. No new migration. `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on main `2c04c56513b5264b396e8c48b73e058ecc102d0e` with no new findings.

## Highlights

- **Duplicate sealed segment: `SEGMENT_OVERLAP` (PR #372, #370).** When a task directory holds two local copies of one sealed source file (for example `mysql-bin.000006` and `mysql-bin.000006.sealed.e3`):
  - a CRITICAL `SEGMENT_OVERLAP` finding opens, naming the later copy;
  - the task shows `storage_alert` `STORAGE_INCONSISTENT`;
  - `GET /window` is no longer continuous and names the copy as the break;
  - `/replay` and `/replay/archive` stop before both copies, with a warning, so `mysqlbinlog` never reads a zero-filled copy or applies the same transactions twice.
  - A half (truncated) copy is still reported as `SEGMENT_UNREADABLE`. Epochs that continue each other (a failback or split continuation) are not an overlap; replay sets for clean directories are unchanged from v0.5.65.

## Changes since v0.5.65

`git log --oneline --first-parent v0.5.65..2c04c56`:

| Commit | Change |
|---|---|
| `3afe192` | PR #371: docs, point install docs at v0.5.65 |
| `2c04c56` | PR #372: find a sealed source file stored twice; cut `/replay` and `/window` before both copies |

## Upgrade Notes

- Replace the binary and restart. No migration, no config change. `000007` and `000008` stay optional; `./migrate up` to 8 is still recommended.
- After upgrading, check every task for `storage_alert` and `SEGMENT_OVERLAP`. A task that shows it has a duplicate copy in its directory: compare the two files, keep the one that matches the source, move the other out of the task directory. The alert, the window break and the replay cut go away once the copy is gone.
- The v0.5.65 rules still hold, including the MariaDB 11.4+ full-backup advice. Coming from v0.5.61 or older, read the [v0.5.62 notes](release-notes-v0.5.62.md) first.

## Known behavior

- Like `SEGMENT_UNREADABLE`, the `SEGMENT_OVERLAP` finding is not resolved automatically after the copy is removed (the alert, break and cut do go away). Planned for the ADR 0009 phase 2 evaluator.
- #365–#368, #312, #341–#345, #352, #353, #360, #363 (P3) are still open.

## QA summary

BinlogServerQA, tip dogfood of main `2c04c56`, 2026-10-15. PASS, no new findings.

- DBA path: restores match on MySQL 8.0 (including failback), MariaDB 11.4 and 10.11.
- `SEGMENT_OVERLAP` on all three flavors: zero-filled and full copies raise it, a half copy raises `SEGMENT_UNREADABLE`, the replay set reads cleanly; removing the copy clears the alert, break and cut, live and after restart. Continuation epochs are not reported and replay matches v0.5.65.
- Upgrade from v0.5.65, no-meta `kill -9` + adopt, meta `kill -9`, read-only meta 3/3, blackhole `DUMP_STALLED`, `TASK_TRANSITION_STALLED` watchdog, `/ui/` 22/22, `go vet` and `go test`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.66.zh-CN.md
