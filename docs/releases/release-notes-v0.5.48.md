# Binlog Server v0.5.48

Release date: 2026-10-07

A GTID start, and a file and position resume that gets MySQL 1236 before any event and then continues from the stored GTID, no longer seals or uploads a magic-only placeholder or an empty `task-<id>.binlog`. The dump's artificial Rotate (flags `0x20`, timestamp 0, no CRC) is not written into a sealed segment. The runner waits for that Rotate, and opens the segment file only after it has the real source file name. A segment that still has only the 4-byte magic header is deleted. It is not sealed, not cataloged, and not uploaded. A real file and position Rotate is still written into the segment it seals. A source-restart Rotate still seals the previous file at the last real event and continues on the next file. A segment sealed by this binary passes `mysqlbinlog --verify-binlog-checksum`. Tip dogfood by BinlogServerQA passed. This release is not an ADR 0005 step. No new config key. No new migration. `minRequiredSchemaVersion` stays 3.

## Highlights

- A task whose start mode is `GTID` no longer leaves a sealed `task-<id>.binlog`. The runner does not open a segment until the dump's artificial Rotate names the source file. That name is the segment name. The Rotate itself is not written. `GET /api/tasks/{id}/files` does not list `task-<id>.binlog` for a start on this binary, and the bucket does not receive that object.
- The same dump Rotate (flags `0x20`, timestamp 0, no CRC) is not appended to a sealed segment. `mysqlbinlog --verify-binlog-checksum` on a segment sealed by this binary exits 0. Events already copied stay in the segment.
- A file and position resume that receives MySQL 1236 before any dump event, and then opens a GTID dump, can still have opened the purged name as a file that holds only the 4-byte magic header. That file is deleted. It is not sealed, not written to `binlog_files`, and not uploaded. The GTID dump then opens the source file the Rotate names.
- A file and position task that is not on this path still writes a real Rotate into the segment it seals. A source restart still sends an artificial Rotate that names the next file. That event is not written. The previous file is sealed at the last real event, and the dump continues on the next file. That is the same rule as v0.5.39 (#214).
- A segment an older binary already sealed with that no-CRC Rotate, including a `task-<id>.binlog` already in the catalog or the bucket, is left as it is. `checksum` and the object bytes stay as they were.

## Upgrade Notes

No new migration. `minRequiredSchemaVersion` stays 3. v0.5.48 starts on schema 3. Schema 4 (`000004_pending_dump_cleanup`) stays optional for one process and required for cluster mode, the same rule as v0.5.46 and v0.5.47. `migrations/` is still `000001`, `000002`, `000003`, and `000004`. The next new migration will be `000005`.

v0.5.48 does not start on schema 2. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md). Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations.

Do not run `./migrate down` to schema 2 while v0.5.48 is running. `./migrate down --steps 1` from schema 4 drops only `pending_dump_cleanup` and returns to version 3. Stop the cluster workers before that down migration. A single process can keep running on schema 3.

Replace the binary. A GTID start and a 1236 fallback after this binary is running seal source file names only. Segments already uploaded with the no-CRC artificial Rotate stay in the bucket. This release does not rewrite them. Run `mysqlbinlog --verify-binlog-checksum` on a segment sealed after the upgrade. It exits 0. Tip dogfood by BinlogServerQA passed on this fix.

This release is not ADR 0005 step 6, 7, 8, or 9. Steps 6–9 are still next work.

## Known behavior

The items below are still open. #189 and #224 were already present before this release. #205 is fixed in this release. ADR 0005 steps 6–9 are not in this release.

- After failover, a sealed segment of the same source name can have `end_pos` written as 0. Tracked as #189.
- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.48.zh-CN.md
