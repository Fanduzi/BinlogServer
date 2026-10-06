# Binlog Server v0.5.50

Release date: 2026-10-07

Migration `000005_binlog_source_epoch_key` keys one segment by `(task_id, source_file, epoch)`. It backfills an empty `source_file` from `file_name`, sets `source_file` `NOT NULL`, adds `UNIQUE KEY uk_task_source_epoch (task_id, source_file, epoch)`, keeps `uk_task_file_epoch`, and makes `start_pos` and `end_pos` nullable. The SQL does not rewrite stored positions. A row whose `end_pos` is 0 stays 0. Seal, enroll, and the open-segment upsert identify that same key. `file_name` stays that source basename, so both unique keys hit the same row, and a v0.5.49 upsert still updates it. `minRequiredSchemaVersion` is 5. On schema 4 this binary exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. This release is ADR 0005 step 6 of 9. Steps 7–9 are not in this release. No new config key. Tip dogfood by BinlogServerQA passed on `e122bd5f`. That run did not open a new issue. The v0.5.49 sealed-position rule (#189) still holds. The v0.5.48 empty `task-<id>.binlog` rule (#205) still holds.

## Highlights

- Migration `000005_binlog_source_epoch_key` backfills `source_file = file_name` where `source_file` is NULL or empty. It then sets `source_file` `NOT NULL`, adds `UNIQUE KEY uk_task_source_epoch (task_id, source_file, epoch)`, and keeps `uk_task_file_epoch`. `start_pos` and `end_pos` become nullable. The SQL does not `UPDATE` stored positions. A historical `end_pos` of 0 stays 0. The migration does not delete rows.
- Seal, enroll, and the open-segment upsert identify one segment by `(task_id, source_file, epoch)`. `file_name` stays equal to that source basename. Both unique keys match the same row. A v0.5.49 upsert still updates that row.
- `minRequiredSchemaVersion` is 5. v0.5.50 does not start on schema 4. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. Schema 3 and schema 2 exit the same way. v0.5.49 still starts on schema 5, because `uk_task_file_epoch` is still there.
- The sealed-position rule from v0.5.49 still holds (#189). A newly sealed segment stores `start_pos` and `end_pos` from the events in that file. An already `UPLOADED` row stays as it was, including a row an older binary stored with `end_pos` 0. Tip dogfood on `e122bd5f` checked that rule.
- A segment sealed by this binary still has no `task-<id>.binlog`. `mysqlbinlog --verify-binlog-checksum` on that segment still exits 0. That is the v0.5.48 rule (#205). This release does not change it.

## Upgrade Notes

Run `./migrate up` to schema 5 before starting v0.5.50. `minRequiredSchemaVersion` is 5. Confirm `SELECT version, dirty FROM schema_migrations` is `(5, 0)`. `migrations/` is `000001`, `000002`, `000003`, `000004`, and `000005`. The next new migration will be `000006`.

Schema 4 to schema 5 is `000005_binlog_source_epoch_key`. It backfills an empty `source_file` from `file_name`, sets `source_file` `NOT NULL`, adds `uk_task_source_epoch`, keeps `uk_task_file_epoch`, and makes `start_pos` and `end_pos` nullable. Stored positions stay as they were, including `end_pos` 0. The migration does not delete rows.

v0.5.50 does not start on schema 4. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. v0.5.49 still starts on schema 5, because `uk_task_file_epoch` is kept. Start v0.5.50 only after `schema_migrations` is `(5, 0)`.

Schema 4 (`000004_pending_dump_cleanup`) is still in the chain. Before this release, schema 4 was optional for one process and required for cluster mode, the same rule as v0.5.46, v0.5.47, v0.5.48, and v0.5.49. That rule still applies when you upgrade stepwise and the binary you start is older than v0.5.50. v0.5.50 does not start on schema 4.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md). Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations.

Do not run `./migrate down` past the schema the running binaries need. v0.5.50 needs schema 5. `./migrate down --steps 1` from schema 5 drops only `uk_task_source_epoch` and restores the previous nullability. `source_file` is nullable again. `start_pos` and `end_pos` are `NOT NULL` again. It returns to version 4. It does not delete rows. The `binlog_files` row count is unchanged. If `start_pos` or `end_pos` is already NULL, that down fails. Do not fill that NULL with 0. This release does not store NULL in those columns. Do not run that down while v0.5.50 is running. A further `./migrate down --steps 1` from schema 4 drops only `pending_dump_cleanup` and returns to version 3. Stop the cluster workers before that down. A single process older than v0.5.50 can keep running on schema 3. v0.5.50 does not start on schema 3.

Replace the binary after the database is schema 5. Stored positions, including a historical `end_pos` of 0, stay as they were. The v0.5.49 sealed-position rule and the v0.5.48 empty `task-<id>.binlog` rule still hold. Tip dogfood by BinlogServerQA passed on `e122bd5f`. That run did not open a new issue.

This release is ADR 0005 step 6 of 9. Steps 7–9 are still next work.

## Known behavior

The items below are still open. #224 was already present before this release. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. This release does not change those. This release is ADR 0005 step 6. Steps 7–9 are not in this release.

- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.
- ADR 0005 steps 7–9 are still next work. Enroll still writes `end_pos`. It does not omit an unknown end. That omission is step 7. This release does not drop `uk_task_file_epoch`. This release is step 6. It is not step 7, 8, or 9.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.50.zh-CN.md
