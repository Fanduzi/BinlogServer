# Binlog Server v0.5.41

Release date: 2026-10-06

After a takeover of an already-`UPLOADED` segment, the next file rotation used to seal that same source file again as `mysql-bin.NNNNNN.sealed.eN` and upload it again. `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` listed that source index twice. A restore applied those transactions twice when the target was not using GTID, or when the client passed `--skip-gtids`. That rotation now drops the takeover copy. Replay and the archive keep one path per source index, including a catalog that already has the extra row. `GET /api/tasks/{id}/files` still lists that extra row. On all-in-one, a `LATEST` task whose open file held only the 4-byte magic header used to go permanently `FAILED` with `SEGMENT_NOT_ON_WORKER` after a source outage long enough for the claim loop to raise the epoch. The file was on this machine. Transactions committed during the outage were not pulled. The task now returns to `RUNNING`. Those transactions are copied once, in order, and the GTID sequence in the local file stays continuous. Standalone, and a retry that is still epoch 1, already did this. Tracked as #223 (PR #226) and #222 (PR #228).

## Highlights

- Taking over an `UPLOADED` segment still reads the object back and opens it as `mysql-bin.NNNNNN.open.eN`. When the dump rotates to the next file, that copy is no longer sealed and no longer uploaded if another verified `UPLOADED` row of the same source file already covers the resume position and the local file has no complete event past it. The open catalog row and the local copy are removed. Checksum stays `match` or `mismatch` on the row that was already uploaded.
- `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` list one path for each source index. A sealed row whose `start_pos` and `end_pos` are the same position greater than 0, and which another sealed row of that index already covers, is left out. A catalog written by an older version that already has that extra row does the same, without a manual delete. A row with `end_pos` 0 stays. A binlog that is still receiving new events is still sealed and uploaded once.
- `GET /api/tasks/{id}/files` still lists every catalog row, including that extra sealed row from an older version. The files-list response is unchanged.
- On all-in-one and cluster (`meta_dsn` set), a `LATEST` task that has connected and saved a checkpoint, but whose open file is only the 4-byte magic header, or only a header of events whose `end_log_pos` is 0 and that ends on an event boundary, no longer goes permanently `FAILED` after a source outage. `last_error` used to begin with `SEGMENT_NOT_ON_WORKER`. The claim loop still starts the owned `RETRY_BACKOFF` task again about every 2 seconds and the epoch still climbs (#203). Once the epoch is greater than 1, Start keeps the saved file and position. `gtid_set` stays empty. The open file is renamed to `mysql-bin.NNNNNN.open.eN` for the current epoch. The task returns to `RUNNING`.
- The transactions committed during the outage are copied once, in order. The GTID sequence in the local file stays continuous. A short outage that returns while the epoch is still 1, and the same outage on standalone, already continued from that anchor. That path is unchanged.
- A file that is actually missing, a different source name, or a torn tail still fails with `SEGMENT_NOT_ON_WORKER` and names the path.

## Upgrade Notes

Rolling upgrade of the binaries from v0.5.40 is fine. This release has no schema migration. Neither #226 nor #228 added a migration. The metadata schema stays at version 2, the version introduced in v0.5.34. `migrations/` is still `000001` and `000002`. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

A catalog that already lists the same source file twice, from a takeover on an older binary, does not need a manual delete. Replay and the archive keep one path. The files list still shows both rows. A task already `FAILED` with `last_error` beginning `SEGMENT_NOT_ON_WORKER` because the open file held only the magic header starts again with `POST /api/tasks/{id}/start`. Start continues from the saved file and position and pulls the transactions committed during the outage.

## Known behavior

The items below are still open. #203, #205, #209, #224, #189, and #193 were already present before this release. #222 and #223 are fixed in this release. #203 still raises the epoch on each retry.

- On all-in-one and cluster, the claim loop restarts an owned `RETRY_BACKOFF` task about every 2 seconds and resets the failure count, so the task never reaches `FAILED` there. The lease epoch keeps climbing across those retries. Standalone, with no metadata store, still reaches `FAILED` after ten consecutive failures. `last_error` begins with `SOURCE_UNREACHABLE:`. A source that dies during an open dump counts toward that same budget. The claim loop still resets the count. This release does not change that. Tracked as #203.
- A GTID-mode start, and a file and position resume that opens `StartSyncGTID` after MySQL 1236, writes a manual Rotate with no CRC. `mysqlbinlog --verify-binlog-checksum` reports `Event crc check failed!` on that event. Without that flag the file reads. The copied events themselves are intact. A GTID start also leaves a stray `task-N.binlog`. The catalog records that file `SEALED` and uploads it. Tracked as #205.
- With no upload configured, age retention deletes the local sealed file and leaves the catalog row `SEALED` / `LOCAL_ONLY` with the old `file_path`. `GET /api/tasks/{id}/files` still lists that name, and the response has no `location` field. `GET /api/tasks/{id}/files/{name}` returns 404. The files list and the disk do not match. This was seen on the `4477d19e` dogfood. The retention code in this release still deletes that local file and leaves the row. Tracked as #209.
- `kill -9` while a takeover is still downloading the object leaves a `.takeover-*` file in the task directory. A later start that finishes the takeover does not delete it. A short read or a failed download still deletes the temp file. Startup and retention do not remove a leftover `.takeover-*`. One file can be about as large as the segment being copied. Tracked as #224.
- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34, v0.5.35, v0.5.36, v0.5.37, v0.5.38, v0.5.39, and v0.5.40. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. This is the same behavior already noted for v0.5.35, v0.5.36, v0.5.37, v0.5.38, v0.5.39, and v0.5.40. Tracked as #193.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.41.zh-CN.md
