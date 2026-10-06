# Binlog Server v0.5.42

Release date: 2026-10-06

With no upload configured, age retention deleted a sealed binlog from disk and left the catalog row `SEALED` / `LOCAL_ONLY` with the old `file_path`. `GET /api/tasks/{id}/files`, the Console file table, `GET /api/tasks/{id}/replay`, and `GET /api/tasks/{id}/replay/archive` still listed that name. A restore that followed the list got 404 from `GET /api/tasks/{id}/files/{name}` in the middle of recovery. This was seen on the `4477d19e` dogfood. That pass now deletes the catalog row for a sealed file in this task directory that has no remote copy once the local file is gone. `GET /api/tasks/{id}/events` gets one `RETENTION_REMOVED` event that names the file, and the log writes the same removal. A row left by an older binary, whose file is already gone, is removed on the next retention pass, including Start. An `UPLOADED` row stays. `location` is `bucket` after the local file is gone, and download still reads the object. Tracked as #209 (PR #231).

## Highlights

- With no upload configured, age retention still deletes a sealed local file older than the local window. The active open file and every other open segment stay. The catalog row for that sealed file is deleted in the same pass when the file is in this task directory and has no remote copy. `GET /api/tasks/{id}/files` and the Console file table no longer list that name. `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` no longer include it.
- `GET /api/tasks/{id}/events` gets one `RETENTION_REMOVED` per removed row. The message is `<name> removed by local retention`. The log line is `retention: removed <name> task=<id> upload_state=<state>`. A second pass does not write that event again, because the row is already gone.
- A catalog row left by an older binary, still `SEALED` / `LOCAL_ONLY` with a `file_path` that is no longer on disk, is deleted on the next retention pass. That pass runs when this process opens a binlog, including Start. No manual delete.
- A row whose `file_path` is outside this task directory stays. An open row stays. A sealed file still inside the retention window stays. Standalone, with no metadata store, still deletes the local sealed file by age. There is no catalog row to remove.
- An `UPLOADED` row with an object key is not removed. With object storage configured, a sealed file past the local window and still inside the bucket window loses only the local file. The catalog row stays, `location` is `bucket`, and `GET /api/tasks/{id}/files/{name}`, replay, and the archive still return the object. No `RETENTION_REMOVED` event is written for that row.

## Upgrade Notes

Drop-in upgrade of the binaries from v0.5.41 is fine. This release has no schema migration. #231 did not add a migration. The metadata schema stays at version 2, the version introduced in v0.5.34. `migrations/` is still `000001` and `000002`. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

A catalog that already lists a sealed file an older binary deleted, with no upload configured, does not need a manual delete. The next retention pass, including Start, removes that row. `UPLOADED` rows stay, and a file whose local copy is already gone stays downloadable from the bucket.

## Known behavior

The items below are still open. #203, #205, #224, #189, and #193 were already present before this release. #209 is fixed in this release. #203 still raises the epoch on each retry.

- On all-in-one and cluster, the claim loop restarts an owned `RETRY_BACKOFF` task about every 2 seconds and resets the failure count, so the task never reaches `FAILED` there. The lease epoch keeps climbing across those retries. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:`. A source that dies during an open dump counts toward that same budget. The claim loop still resets the count. This release does not change that. Tracked as #203.
- A GTID-mode start, and a file and position resume that opens `StartSyncGTID` after MySQL 1236, writes a manual Rotate with no CRC. `mysqlbinlog --verify-binlog-checksum` reports `Event crc check failed!` on that event. Without that flag the file reads. The copied events themselves are intact. A GTID start also leaves a stray `task-N.binlog`. The catalog records that file `SEALED` and uploads it. Tracked as #205.
- `kill -9` while a takeover is still downloading the object leaves a `.takeover-*` file in the task directory. A later start that finishes the takeover does not delete it. A short read or a failed download still deletes the temp file. Startup and retention do not remove a leftover `.takeover-*`. One file can be about as large as the segment being copied. Tracked as #224.
- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34, v0.5.35, v0.5.36, v0.5.37, v0.5.38, v0.5.39, v0.5.40, and v0.5.41. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. This is the same behavior already noted for v0.5.35, v0.5.36, v0.5.37, v0.5.38, v0.5.39, v0.5.40, and v0.5.41. Tracked as #193.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.42.zh-CN.md
