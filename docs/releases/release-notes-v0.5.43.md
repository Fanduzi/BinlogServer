# Binlog Server v0.5.43

Release date: 2026-10-06

On all-in-one and cluster, the claim loop used to start an owned `RETRY_BACKOFF` task again about every 2 seconds. That start cleared the failure count, so an unreachable source never reached `FAILED`, and the lease epoch kept climbing. The claim loop no longer starts a live owned run. The task stays in `RETRY_BACKOFF` for the backoff interval and reaches `FAILED` after ten consecutive `SOURCE_UNREACHABLE` failures, the same budget as standalone. `GET /api/tasks/{id}` shows `last_error` beginning with `SOURCE_UNREACHABLE:`, and the lease is released. The epoch does not climb while this process is waiting to retry. A restart of this process, `SIGTERM` or `kill -9`, carries the streak. Four stored failures, then a restart, still reach `FAILED` on the tenth failure total. Operator Start resets the count. ADR 0005 is accepted. It is a roadmap: metadata MySQL is the single authority for task state, and a segment is keyed by `(task_id, source_file, epoch)`. That record does not change runtime behavior. Tracked as #203 (PR #234) and #237 (PR #238).

## Highlights

- On all-in-one and cluster, an owned task whose source cannot be reached stays in `RETRY_BACKOFF` for the backoff interval. The claim loop, about every 2 seconds, no longer starts that task again while this process is already waiting to retry. The failure count is not cleared. After ten consecutive `SOURCE_UNREACHABLE` failures the task is `FAILED`, the same budget as standalone. `GET /api/tasks/{id}` shows `last_error` beginning with `SOURCE_UNREACHABLE:`. The lease is released.
- The lease epoch does not climb during that backoff. Epoch still changes when a different worker takes over an expired lease. A source that dies after the dump is open still uses this same budget. The count resets when the dump connects, or when an operator Starts the task.
- Restarting all-in-one or a cluster worker during the streak carries the count. Four stored failures, then `SIGTERM` or `kill -9`, still reach `FAILED` on the tenth failure total. A same-owner claim with no local run, and an expired-lease takeover, both continue from `TASK_RUNNER_ERROR` events already stored for that task. Both reads walk those events oldest-first. The 200-event window stays the newest rows.
- ADR 0005 (`docs/adr/0005-single-task-authority-and-segment-catalog.md`) is accepted. Metadata MySQL is the single authority for desired task state and for observed task state. A segment is keyed by `(task_id, source_file, epoch)`. The record is a roadmap. This release does not change runtime behavior for it.

## Upgrade Notes

Drop-in upgrade of the binaries from v0.5.42 is fine. This release has no schema migration. #234 and #238 did not add a migration. The metadata schema stays at version 2, the version introduced in v0.5.34. `migrations/` is still `000001` and `000002`. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

An owned task that stayed in `RETRY_BACKOFF` on v0.5.42, because the claim loop kept restarting it, reaches `FAILED` after ten consecutive `SOURCE_UNREACHABLE` failures on this binary. Operator Start resets that count. One restart of this process in the middle of the streak does not. ADR 0005 does not change the running process. That record adds no column and no config key.

## Known behavior

The items below are still open. #189, #193, #205, and #224 were already present before this release. #203 and #237 are fixed in this release. #239 is open on this release.

- Two restarts inside one failure streak can take up to 14 failures before `FAILED`. A claim-written `TASK_STARTED` is indistinguishable from an operator Start, so the second restart keeps only the failures after the previous claim. The backoff delay starts again at 1s after a restart. The structural fix is ADR 0005 step 3, a persisted retry budget column. Tracked as #239.
- After failover, a sealed segment of the same source name can have `end_pos` written as 0. Tracked as #189.
- After the source password is changed during Stop, then Start, the source may keep an extra Binlog Dump connection. Tracked as #193.
- A GTID start, or the fallback after MySQL 1236, writes a manual Rotate with no CRC, so checksum verification fails and a stray `task-N.binlog` is left behind. Tracked as #205.
- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.43.zh-CN.md
