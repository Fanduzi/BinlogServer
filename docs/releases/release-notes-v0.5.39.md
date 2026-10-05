# Binlog Server v0.5.39

Release date: 2026-10-06

A source that dies while a dump is already open used to leave the task `RUNNING`. The lease kept renewing. The dump library logged `retry sync err, wait 1s`. The scheduler did not see that error, so the task did not enter `RETRY_BACKOFF` and did not count `SOURCE_UNREACHABLE`. After about 5 reconnect attempts, about one second apart, `GET /api/tasks/{id}` and the Console show `RETRY_BACKOFF`. `last_error` begins with `SOURCE_UNREACHABLE:`. Each of those failures counts toward the same ten-failure budget as a failed first connect. On standalone, with no metadata store, the tenth consecutive failure is `FAILED` and the lease is released. On all-in-one and cluster, the claim loop restarts an owned `RETRY_BACKOFF` task about every 2 seconds and resets the failure count, so the task never reaches `FAILED` there. A short blip stays `RUNNING`, and `last_error` is not changed. When the source accepts connections again, the task returns to `RUNNING` and continues from its checkpoint. A `LATEST` task that had connected but had not yet written an event used to resolve `LATEST` again after that outage and skip transactions committed while the source was down. The first resolved file and position are now saved as the checkpoint, with an empty `gtid_set`, so the retry does not start at the new tip. The artificial Rotate MySQL sends after a restart has `end_log_pos` 0. It is no longer written into the local binlog. Durable resume ignores a trailing event whose position is 0, so an all-in-one task no longer stays `FAILED` with `SEGMENT_NOT_ON_WORKER` after a source restart. Tracked as #204 (PR #212), #213 (PR #215), and #214 (PR #216). QA dogfood on `3460c45` passed.

## Highlights

- The dump library may reconnect an already-open connection 5 times, about one second apart. A blip that comes back within those 5 tries leaves the task `RUNNING`. `last_error` is not changed.
- If the source is still down after that, `GET /api/tasks/{id}` and the Console show `RETRY_BACKOFF`. `last_error` begins with `SOURCE_UNREACHABLE:` (for example `SOURCE_UNREACHABLE: dial tcp ... connection refused`). Each failure counts toward the same ten-failure `SOURCE_UNREACHABLE` budget as a failed first connect.
- On standalone, with no metadata store, the tenth consecutive failure is `FAILED` and the lease is released. On all-in-one and cluster, the claim loop restarts an owned `RETRY_BACKOFF` task about every 2 seconds and resets the failure count, so the task never reaches `FAILED` there. This release does not change that. Tracked as #203.
- When the source accepts connections again, the task returns to `RUNNING` and continues from its checkpoint. The checkpoint file and position line up with `SHOW MASTER STATUS`.
- The first time a `LATEST` task resolves, that file and position are saved as the checkpoint before the dump. `gtid_set` is empty. `GET /api/tasks/{id}/checkpoint` returns that file and position. The task row still shows `start.mode=LATEST`.
- After `RETRY_BACKOFF`, the next attempt continues from that file and position, or from the last dumped event if one was already written. It does not resolve `LATEST` again, and it does not start at the new `SHOW MASTER STATUS`. Transactions committed during the outage are pulled. A task that already had a checkpoint is unchanged. A `GTID` checkpoint still keeps and extends `gtid_set`. A `LATEST` checkpoint still has no `gtid_set`. With no metadata store, this process keeps the same file and position for the retry.
- The artificial Rotate MySQL sends on reconnect after a restart has `end_log_pos` 0. It is not stored in the source binlog, and it names the next file. It is not written into the local segment. The open file is sealed at the last real event, and the dump continues on the next file.
- A local segment that already ends with that Rotate still resumes from the last event whose `end_log_pos` is not 0. Those extra bytes are dropped the next time the segment is opened. The catalog `file_path` still points at the local file, and that file is not treated as missing. A task already `FAILED` on this path, with `last_error` beginning `SEGMENT_NOT_ON_WORKER`, starts again from that event. `POST /api/tasks/{id}/start`. A manual truncate is not required.
- `mysqlbinlog --verify-binlog-checksum` still reads the local file. The decoded file has no Rotate with `end_log_pos` 0.

## Upgrade Notes

Rolling upgrade of the binaries from v0.5.38 is fine. This release has no schema migration. The metadata schema stays at version 2, the version introduced in v0.5.34. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

A task already `FAILED` with `last_error` beginning `SEGMENT_NOT_ON_WORKER`, because the open segment ends in an artificial Rotate with `end_log_pos` 0, starts again with `POST /api/tasks/{id}/start`. Start continues from the last event whose position is not 0 and drops the extra bytes. A manual truncate is not required.

## Known behavior

These were already present before this release. QA dogfood on `3460c45` passed. This release does not change them.

- On all-in-one and cluster, the claim loop restarts an owned `RETRY_BACKOFF` task about every 2 seconds and resets the failure count, so the task never reaches `FAILED` there. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:`. A source that dies during an open dump now counts toward that same budget. The claim loop still resets the count. Tracked as #203.
- Stop, Start, lease renewal, and progress updates for every task on one process share one lock with event reads and event inserts. A slow metadata write, or the Console reading events, holds the others. Opening the next binlog reads that task's whole file catalog on the dump path. Taking over a large uploaded segment reads the whole object into memory on that worker. Tracked as #177.
- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34, v0.5.35, v0.5.36, v0.5.37, and v0.5.38. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. This is the same behavior already noted for v0.5.35, v0.5.36, v0.5.37, and v0.5.38. Tracked as #193.
- A GTID-mode start, and a file and position resume that opens `StartSyncGTID` after MySQL 1236, writes a manual Rotate with no CRC. `mysqlbinlog --verify-binlog-checksum` reports `Event crc check failed!` on that event. Without that flag the file reads. The copied events themselves are intact. A GTID start also leaves a stray `task-N.binlog`. The catalog records that file `SEALED` and uploads it. Tracked as #205.
- With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY` with the old `file_path`. `GET /api/tasks/{id}/files` still lists that name, and the response has no `location` field. `GET /api/tasks/{id}/files/{name}` returns 404. The files list and the disk do not match. This was seen on the `4477d19e` dogfood. The retention code in this release is the same as before. Tracked as #209.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.39.zh-CN.md
