# Binlog Server v0.5.40

Release date: 2026-10-06

Stop, Start, lease renewal, and progress on one task used to wait while another task read or wrote events. Opening the next binlog loaded that task's whole file catalog. Taking over a large uploaded segment read the whole object into memory. A cluster or all-in-one Start that raced `GET /api/tasks/{id}` used to run the new dump at epoch 0. The open file was the bare `mysql-bin.NNNNNN`, the lease was not renewed, and `GET /api/tasks/{id}` stayed `STARTING`. The next Stop and Start went `FAILED` with `SEGMENT_NOT_ON_WORKER` even though that file was already on this machine. Start now keeps the owner and epoch it just acquired. The open file is `mysql-bin.NNNNNN.open.eN`, the lease keeps renewing, and the task reaches `RUNNING`. A task already left on that bare file by an older binary starts again from that file when the catalog row is still `OPEN` and the file is on this worker. Tracked as #177 (PR #219) and #220 (PR #221). QA dogfood on `05b49642` passed.

## Highlights

- `GET /api/tasks/{id}/events` looks the task up under the scheduler lock, then reads the event store after releasing it. A slow event read on one task does not block Stop, Start, lease renewal, or progress on another task.
- Event inserts update memory under the lock and write the event store afterward. Writes for one task stay in order. A slow insert on task A does not block those operations on task B. An event-store error does not fail the transition.
- Progress updates only touch memory under that lock. They used to stall because they shared the lock with event reads and inserts.
- Opening the next binlog, sealing it, and age retention read `binlog_files` 200 rows at a time, ordered by `(file_name, epoch)`. They do not load every row for the task. Local versus bucket retention, `LOCAL_ONLY`, `UPLOAD_FAILED`, and checksum `match` versus `mismatch` are unchanged.
- `GET /api/tasks/{id}/files` and replay still load that task's whole catalog and window it in Go, in the same order as before. The files-list response is unchanged. Lease takeover still reads every catalog row once per start, because it needs the unreadable tail and a covering uploaded object.
- Taking over an `UPLOADED` segment streams the object to a `.takeover-*` file in the task directory. The open name `mysql-bin.NNNNNN.open.eN` appears only after the copy matches the size reported when the object was opened. A short read or a failed download deletes the temp file and does not leave a truncated segment that resume would treat as valid. The catalog checksum stays `match` or `mismatch`. This copy does not hash the object and does not change that flag.
- On all-in-one and cluster (`meta_dsn` set), polling `GET /api/tasks/{id}` while you Stop and Start no longer starts the new run at epoch 0. Before this, a refresh during Start could put the previous `STOPPED` row back in memory.
- Start keeps the owner and epoch it just acquired. The open file is `mysql-bin.NNNNNN.open.eN`. The lease keeps renewing. The task reaches `RUNNING`.
- A task already left on the bare file by an older binary starts again from that file when the catalog row is still `OPEN` and the file is on this worker. `POST /api/tasks/{id}/start`. The bare file is renamed to `mysql-bin.NNNNNN.open.eN`. A file that is actually missing still fails with `SEGMENT_NOT_ON_WORKER` and names the path to mount or copy.
- On all-in-one and cluster, a start that does not keep a lease epoch goes `FAILED`. `last_error` begins with `EPOCH_NOT_ACQUIRED:` (the message is `this start did not keep its lease epoch. Start the task again`). The lease is released. The dump does not start. Start the task again. Standalone, with no metadata store, still has no lease epoch.

## Upgrade Notes

Rolling upgrade of the binaries from v0.5.39 is fine. This release has no schema migration. Neither #219 nor #221 added a migration. The metadata schema stays at version 2, the version introduced in v0.5.34. `migrations/` is still `000001` and `000002`. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

A task already left on the bare file `mysql-bin.NNNNNN` by an older binary, with the catalog row still `OPEN` and the file on this worker, starts again with `POST /api/tasks/{id}/start`. The file is renamed to `mysql-bin.NNNNNN.open.eN` and the dump continues from the last complete event in that file. A file that is actually missing still fails with `SEGMENT_NOT_ON_WORKER` and the path to mount or copy. A start that went `FAILED` with `last_error` beginning `EPOCH_NOT_ACQUIRED:` did not dump. Start it again.

## Known behavior

QA dogfood on `05b49642` passed for the stalls, the epoch-0 race, and the v0.5.39 regressions #204, #214, and #213 on standalone and on a short all-in-one outage. The items below are still open. #222, #223, #203, #189, #193, #205, and #209 were already present before this release. #224 is new with the temp-file copy in this release.

- On all-in-one, a `LATEST` task that has connected and saved a checkpoint, but has not yet written a complete event, plus a source outage of about 20 seconds, can go permanently `FAILED`. `last_error` begins with `SEGMENT_NOT_ON_WORKER`. The claim loop starts the owned `RETRY_BACKOFF` task again about every 2 seconds, and the epoch climbs. The open file is still on this machine. It holds only the 4-byte magic header. Transactions committed during the outage are not pulled. The task does not recover on its own. A short outage that comes back while the epoch is still 1, and the same outage on standalone, still continue from the saved file and position. This was reproduced on v0.5.39. Tracked as #222.
- After takeover of an `UPLOADED` segment, that same source file is sealed again as `mysql-bin.NNNNNN.sealed.eN` and uploaded again. `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` list that source index twice. The archive contains those transactions twice. The two objects have the same bytes. This was reproduced on v0.5.39. Tracked as #223.
- `kill -9` while a takeover is still downloading the object leaves a `.takeover-*` file in the task directory. A later start that finishes the takeover does not delete it. A short read or a failed download still deletes the temp file. Startup and retention do not remove a leftover `.takeover-*`. One file can be about as large as the segment being copied. This is new with the temp-file copy in this release. Tracked as #224.
- On all-in-one and cluster, the claim loop restarts an owned `RETRY_BACKOFF` task about every 2 seconds and resets the failure count, so the task never reaches `FAILED` there. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:`. A source that dies during an open dump counts toward that same budget. The claim loop still resets the count. Tracked as #203.
- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34, v0.5.35, v0.5.36, v0.5.37, v0.5.38, and v0.5.39. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. This is the same behavior already noted for v0.5.35, v0.5.36, v0.5.37, v0.5.38, and v0.5.39. Tracked as #193.
- A GTID-mode start, and a file and position resume that opens `StartSyncGTID` after MySQL 1236, writes a manual Rotate with no CRC. `mysqlbinlog --verify-binlog-checksum` reports `Event crc check failed!` on that event. Without that flag the file reads. The copied events themselves are intact. A GTID start also leaves a stray `task-N.binlog`. The catalog records that file `SEALED` and uploads it. Tracked as #205.
- With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY` with the old `file_path`. `GET /api/tasks/{id}/files` still lists that name, and the response has no `location` field. `GET /api/tasks/{id}/files/{name}` returns 404. The files list and the disk do not match. This was seen on the `4477d19e` dogfood. The retention code in this release still deletes that local file and leaves the row. Tracked as #209.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.40.zh-CN.md
