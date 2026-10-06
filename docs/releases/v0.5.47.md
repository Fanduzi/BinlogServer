# Binlog Server v0.5.47

Release date: 2026-10-06

An error that is not on the retry allowlist fails the task the first time. The row becomes `FAILED`. The owner lease is cleared. One `TASK_FAILED` event is written. `desired_run` is `STOP` and `failed_spec_revision` is that row's `spec_revision`. `SOURCE_UNREACHABLE` still takes 10 consecutive failures. A transient metadata error has no task-level cap and returns to `RUNNING` without Start. `OBJECT_PURGE_FAILED` is not a task failure. MySQL 1236 with no stored GTID fails once, and `last_error` names 1236 and a purged binlog. A Stop already in progress stays `STOPPED` and does not write `TASK_FAILED`. There is no new metric and no notifier. This is ADR 0005 step 5 of 9. No new config key. No new migration. `minRequiredSchemaVersion` stays 3.

## Highlights

- While the task is still supposed to be running, an error outside the retry allowlist fails once. `GET /api/tasks/{id}` shows `FAILED` and `last_error`. `task_events` has one `TASK_FAILED`. `owner_worker_id` is empty. `desired_run` is `STOP` and `failed_spec_revision` equals `spec_revision`. That set includes `SOURCE_ACCESS_DENIED`, `SOURCE_LOG_BIN_OFF`, `SOURCE_IDENTITY_UNAVAILABLE`, `SEALED_FILE_EXISTS`, a non-transient `CHECKPOINT_WRITE_FAILED`, `SEGMENT_NOT_ON_WORKER`, `EPOCH_NOT_ACQUIRED`, MySQL 1236 when no GTID is stored, and any runner error `classifyRunError` does not recognize, including a local append or flush error. A lease handoff is not a failure and does not write `FAILED`. Tasks that used to sit in `RETRY_BACKOFF` on an unclassified error now go `FAILED` on the first occurrence. Operator Start after `FAILED` still arms the task.
- The allowlist, retried inside the same ownership, is three cases. `SOURCE_UNREACHABLE` still counts on `consecutive_source_failures`. The 10th consecutive failure is `FAILED` and the lease is released. A successful dump connect stores 0. Transient metadata errors use `tasks.IsTransientMetadataError`. `meta.IsTransientMySQLError` calls that function. The substrings are the same: deadlock, lock wait timeout, connection reset, connection refused, broken pipe, server has gone away, invalid connection, bad connection, read-only, read only, timeout, eof. There is no task-level cap. After metadata recovers, the task returns to `RUNNING` without an operator Start and without passing through `FAILED`. `OBJECT_PURGE_FAILED` is not a task failure. The next file open retries the object delete.
- A file and position resume that gets MySQL 1236 and has no stored GTID goes `FAILED` on that first error. It does not stay in `RETRY_BACKOFF`. `last_error` names 1236 and a purged binlog. One `TASK_FAILED` is written. The lease is released.
- A runner error observed while the task is already `STOPPING` or `STOPPED`, or while the stored row is a newer Stop, does not use the allowlist. The run finishes as `STOPPED` and does not append `TASK_FAILED`. A Stop whose `KILL` could not reach the source stays `STOPPED` with `pending_dump_cleanup`. That path is unchanged. Migration `000004` is already on main. This step does not add `000005`.
- An operator Stop no longer stays `STOPPING` when the dump exit would write `STOPPED` from a snapshot older than that Stop's `spec_revision`. The task upsert keeps the row when its `spec_revision` is newer, so the older `STOPPED` did not apply and `GET /api/tasks/{id}` stayed `STOPPING` with the owner and epoch still set. The exit now writes `STOPPED` on the Stop's spec, and writes it again when the stored row is still that Stop.

## Upgrade Notes

No new migration. `minRequiredSchemaVersion` stays 3. v0.5.47 starts on schema 3. Schema 4 (`000004_pending_dump_cleanup`) stays optional for one process and required for cluster mode, the same rule as v0.5.46. `migrations/` is still `000001`, `000002`, `000003`, and `000004`. The next new migration will be `000005`.

v0.5.47 does not start on schema 2. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md). Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations. The same fail-and-alert rule applies to the in-memory task.

Do not run `./migrate down` to schema 2 while v0.5.47 is running. `./migrate down --steps 1` from schema 4 drops only `pending_dump_cleanup` and returns to version 3. Stop the cluster workers before that down migration. A single process can keep running on schema 3.

A task that used to wait in `RETRY_BACKOFF` on an unclassified error, or on MySQL 1236 with no stored GTID, now stops at `FAILED`. Fix the cause, then Start. `SOURCE_UNREACHABLE` still waits for 10 consecutive failures. A transient metadata blip still returns to `RUNNING` by itself.

## Known behavior

The items below are still open. #189, #205, and #224 were already present before this release. ADR 0005 steps 6–9 are not in this release.

- After failover, a sealed segment of the same source name can have `end_pos` written as 0. Tracked as #189.
- A GTID start, or the fallback after MySQL 1236, writes a manual Rotate with no CRC, so checksum verification fails and a stray `task-N.binlog` is left behind. Tracked as #205.
- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.47.zh-CN.md
