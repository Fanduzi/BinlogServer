# Binlog Server v0.5.37

Release date: 2026-10-05

A task that hits a local error that will not clear on its own no longer stays in `RETRY_BACKOFF`, renews its lease, and reconnects to the source. The task goes to `FAILED`. The lease is released. That runner is not called again, so the source Binlog Dump thread is not reconnected. Fix the cause and Start again. Tracked as #178.

## Highlights

- `sealed file already exists` moves the task to `FAILED`. `last_error` is `SEALED_FILE_EXISTS: sealed file already exists: <path>`. The lease is released and that runner is not called again. Remove the stray sealed file, then `POST /api/tasks/{id}/start`.
- A checkpoint write that is not a transient metadata error moves the task to `FAILED` with `last_error` beginning `CHECKPOINT_WRITE_FAILED`, and the lease is released. Fix the metadata error named in `last_error`, then Start again. Transient checkpoint errors stay `RETRY_BACKOFF`. They match the metadata client's list: deadlock, lock wait timeout, connection reset, connection refused, broken pipe, server has gone away, invalid connection, bad connection, read-only, timeout, and eof. `context.Canceled` and `context.DeadlineExceeded` stay retryable.
- A lease epoch mismatch is a handoff. The runner that lost the epoch stops and releases only that epoch. It does not write `FAILED` or `RETRY_BACKOFF`, so it does not replace the task row of the worker that holds the task now. With no metadata store, this process shows `STOPPED` and appends `TASK_LEASE_YIELDED`. Start works again from `STOPPED`.
- `SOURCE_UNREACHABLE` is unchanged: ten consecutive failures, then `FAILED`, and a ready runner resets that count. Other retryable source errors, `OBJECT_PURGE_FAILED`, and MySQL 1236 with no stored GTID stay in `RETRY_BACKOFF` with no new cap.
- The metadata client already retries a transient error five times inside one call. The scheduler retry is what lets a task return to `RUNNING` across a metadata failover, and a cap of ten would mark that backup `FAILED` and require a manual start. `OBJECT_PURGE_FAILED` is retried on the next file open, and failing the task would stop capture until the bucket is back and someone starts the task. MySQL 1236 with an empty `gtid_set` remains the v0.5.36 resume gap.
- A local binlog append or flush error is still returned as-is and retried. It is not the sealed-file conflict or the checkpoint write.

## Upgrade Notes

Rolling upgrade of the binaries from v0.5.36 is fine. This release has no schema migration. The metadata schema stays at version 2, the version introduced in v0.5.34. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

## Known behavior

These were already present before this release. QA checked them on `9c350c07`. This release does not change them.

- On all-in-one and cluster roles, the `SOURCE_UNREACHABLE` cap of ten never fires. The claim loop, about every 2 seconds, starts an owned `RETRY_BACKOFF` task again and resets the consecutive-failure count, so the task stays in `RETRY_BACKOFF`. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:`. The same split is on the parent of this change. Tracked as #203.
- A source that goes down while the dump is already open can leave the task showing `RUNNING`. The lease keeps renewing. The dump library logs `retry sync err, wait 1s`. The scheduler does not see that error, so the task does not enter `RETRY_BACKOFF` and does not count `SOURCE_UNREACHABLE`. When the source comes back, the task continues and the position lines up. Tracked as #204.
- A GTID-mode start, and a file and position resume that opens `StartSyncGTID` after MySQL 1236, writes a manual Rotate with no CRC. `mysqlbinlog --verify-binlog-checksum` reports `Event crc check failed!` on that event. Without that flag the file reads. The copied events themselves are intact. A GTID start also leaves a stray `task-N.binlog`. The catalog records that file `SEALED` and uploads it. Tracked as #205.
- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34, v0.5.35, and v0.5.36. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. This is the same behavior already noted for v0.5.35 and v0.5.36. Tracked as #193.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.37.zh-CN.md
