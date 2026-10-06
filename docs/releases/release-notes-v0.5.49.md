# Binlog Server v0.5.49

Release date: 2026-10-07

After failover, a sealed segment of a source file the catalog already had stores `start_pos` and `end_pos` from the events in that file. `start_pos` is the first event's position. `end_pos` is the last event's end log position. Those two numbers are the span a point-in-time restore uses. Before this binary, that row could be `end_pos` 0, or `start_pos` and `end_pos` both equal to the resume cursor, while the file held events from position 4 through the last event. The same rule covers epoch 1, a third epoch of that name, and a segment sealed after a file and position resume gets MySQL 1236 and then continues from the stored GTID. A same-file artificial Rotate whose position is 0 no longer clears the position being copied. An already `UPLOADED` row is left as it is. Tip dogfood by BinlogServerQA passed on `e690c438`. That run did not open a new issue. This release is not an ADR 0005 step. No new config key. No new migration. `minRequiredSchemaVersion` stays 3.

## Highlights

- A new lease that seals the same source name, including epoch 1 and a third epoch of that name, writes `start_pos` as the first event position and `end_pos` as the last event end log position. Those numbers match the sealed file's event span, the same span `mysqlbinlog` reads. The row is not `end_pos` 0. It is not `start_pos` and `end_pos` both equal to the resume cursor. `GET /api/tasks/{id}/files` and the Console file table show those positions.
- A file and position resume that receives MySQL 1236 before any dump event, and then continues from the stored GTID, seals that source file with the same positions. The sealed row matches the first and last events in the file.
- A same-file artificial Rotate with position 0 does not set the copied position to 0. The following rotate does not seal the copied events at `end_pos` 0.
- An already `UPLOADED` row stays as it was. A row an older binary stored with `end_pos` 0, or with both positions equal to the resume cursor, keeps those numbers. Download and replay of that object stay as they were.
- A segment sealed by this binary still has no `task-<id>.binlog`. `mysqlbinlog --verify-binlog-checksum` on that segment still exits 0. That is the v0.5.48 rule (#205). This release does not change it.

## Upgrade Notes

No new migration. `minRequiredSchemaVersion` stays 3. v0.5.49 starts on schema 3. Schema 4 (`000004_pending_dump_cleanup`) stays optional for one process and required for cluster mode, the same rule as v0.5.46, v0.5.47, and v0.5.48. `migrations/` is still `000001`, `000002`, `000003`, and `000004`. The next new migration will be `000005`.

v0.5.49 does not start on schema 2. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md). Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations.

Do not run `./migrate down` to schema 2 while v0.5.49 is running. `./migrate down --steps 1` from schema 4 drops only `pending_dump_cleanup` and returns to version 3. Stop the cluster workers before that down migration. A single process can keep running on schema 3.

Replace the binary. A segment sealed after this binary is running stores `start_pos` and `end_pos` from the first and last events in the file. An already `UPLOADED` row stays as it was, including a row an older binary stored with `end_pos` 0. This release does not rewrite it. Tip dogfood by BinlogServerQA passed on `e690c438`. That run did not open a new issue.

This release is not ADR 0005 step 6, 7, 8, or 9. Steps 6–9 are still next work.

## Known behavior

The items below are still open. #224 was already present before this release. #189 is fixed in this release. #205 was fixed in v0.5.48. This release does not change that.

- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.
- ADR 0005 steps 6–9 are still next work. Step 6 is migration `000005`. This release is not step 6, 7, 8, or 9.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.49.zh-CN.md
