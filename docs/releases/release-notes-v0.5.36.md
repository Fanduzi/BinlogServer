# Binlog Server v0.5.36

Release date: 2026-10-05

A task created with `start.mode=GTID` lost that GTID on the first checkpoint flush. Resume asked the source for a file and position. If that binlog had been purged, MySQL returned 1236 (`Could not find first log file name in binary log index file`) and the task stayed in `RETRY_BACKOFF`. The checkpoint `gtid_set` also stayed at the set from task creation, so a GTID dump would have started too early. This release keeps the executed set on every flush, extends it when a transaction commits, and opens the dump again with `StartSyncGTID` when that file is gone. The task reaches `RUNNING`. Tracked as #175, #197, and #198.

## Highlights

- A flushed checkpoint keeps `gtid_set`. That includes the rotate onto the next file. The first flush no longer stores an empty column over the start set.
- The set grows when the transaction commits: an XID, a `COMMIT`, a `ROLLBACK`, or an autocommit statement. The GTID event is not added before that commit. `ROLLBACK TO` does not add it.
- `GET /api/tasks/{id}/checkpoint` returns `gtid_set` when the file and position are the stored checkpoint. An epoch greater than 1 that rewinds to position 4 leaves `gtid_set` out of that response. The stored row still has the set. A purged file still uses that stored set.
- Resume still tries file and position first. A file that is still on the source stays a file and position dump. `StartSync` returns before MySQL sends 1236. That error arrives on the first stream read, before any dump event. When the checkpoint has a GTID, this process closes that dump and opens `StartSyncGTID`. The log line is `file/pos resume hit MySQL 1236 before any event; opening GTID dump`. The task reaches `RUNNING`.
- A GTID dump that also returns 1236 is returned to the scheduler in that attempt. The missing file is not opened again inside the same attempt.
- A file and position task that has no GTID does not gain one from the stream. A 1236 on that task is still 1236.

## Upgrade Notes

Rolling upgrade of the binaries from v0.5.35 is fine. This release has no schema migration. The metadata schema stays at version 2, the version introduced in v0.5.34. When `schema_migrations` is already version 2, `./migrate up` is not required.

A metadata database still on schema 1 must be migrated before this binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key.

Before the source runs `PURGE BINARY LOGS` on the checkpoint file, read `GET /api/tasks/{id}/checkpoint` and compare `gtid_set` with the source `GTID_EXECUTED` (`GTID_SUBSET`). The purge resume uses that stored set.

A checkpoint whose `gtid_set` is already empty was cleared by a flush on v0.5.35 or earlier. This binary does not rebuild it from the local file. While the checkpoint file is still on the source, Start continues at that file and position. The next flushed event writes the task's configured start GTID. Commits after this binary is running extend that set. Transactions copied while the column was empty are not filled in. If the source has already purged that file and the column is empty, Start still receives MySQL 1236 and the task stays in `RETRY_BACKOFF`. There is no stored set to open with `StartSyncGTID`.

If `gtid_set` does not contain transactions this task already copied, a later purge resumes from that incomplete set. The source sends the missing transactions again, and they are copied into the task's local segments again. If the source has already purged those missing GTIDs, the GTID dump returns 1236 and the task goes to `RETRY_BACKOFF`.

## Known behavior

- With `binlog_transaction_compression=ON`, MySQL writes the commit inside a transaction payload. This process does not open that payload. A compressed transaction is not added to `gtid_set`, and the set does not grow for it. CI job `e2e-gtid-purge` turns `binlog_transaction_compression` off so GTID and XID stay separate events, then turns it back on.
- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. This is the same behavior already noted for v0.5.34 and v0.5.35. Tracked as #189.
- After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. After a later Stop that reaches `STOPPED`, one Binlog Dump thread may still be connected and needs a manual `KILL` on the source. An ordinary Stop from the control plane, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread. This is the same behavior already noted for v0.5.35. Tracked as #193.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.36.zh-CN.md
