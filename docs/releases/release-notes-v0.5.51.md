# Binlog Server v0.5.51

Release date: 2026-10-07

`GET /api/tasks/{id}/files` and the Console file table leave an unknown end empty. Unknown `end_pos` is JSON `null`. It is not `0`, and it is not a copy of the resume cursor. The Console cell is blank. When the local file has readable events, `start_pos` and `end_pos` are that event span. The end is the last complete event's end log position, the same number `DurableCursor` returns and the same span `binlog.EventSpan` reads. A stored span whose end is already greater than its start stays as stored. An object-only row stays as stored. A historical `end_pos` of 0 is JSON `null` when this process cannot read the local bytes. The catalog value stays 0 until a worker that can read those bytes opens that task directory. That open repairs the catalog positions and does not upload the object again. Standalone, with no metadata DSN, lists positions from the file's events when they are readable and `null` when they are not. Retention deletes only a name `binlog.ClassifySegment` accepts as a sealed segment. `.takeover-*`, `notes.txt`, and `task-<id>.binlog` stay on disk. `file_name` is unchanged. There is no migration `000006`. Schema stays 5. `minRequiredSchemaVersion` stays 5. `uk_task_file_epoch` stays. No new config key. This release is ADR 0005 step 7 of 9. Steps 8 and 9 are not in this release. Tip dogfood by BinlogServerQA passed on `a886271248c4a31ac4be3ee11e87b11ba896724e`. That run did not open a new issue. The v0.5.49 sealed-position rule (#189) still holds. The v0.5.48 empty `task-<id>.binlog` rule (#205) still holds.

## Highlights

- `GET /api/tasks/{id}/files` returns JSON `null` for an unknown `end_pos`. The Console end-position cell is blank. The value is not `0` and not the resume cursor. When this process can read events in the local file, `start_pos` and `end_pos` are that event span. The end matches `DurableCursor` and `binlog.EventSpan`. A stored span whose end is already greater than its start is left as stored. An object-only row is left as stored.
- A historical `end_pos` of 0 is JSON `null` on that API when this process cannot read the file. The catalog row stays 0 until a worker that can read the local bytes opens the task directory. That open writes the repaired `start_pos` and `end_pos`. It does not upload the object again. This binary does not store `end_pos` 0. An upsert that does not know the end does not assign `end_pos`.
- Standalone, with no metadata DSN, lists a leftover directory from the file's events when they are readable, and `null` when they are not.
- Retention deletes an aged name `binlog.ClassifySegment` accepts as a sealed segment. The active file stays. An open segment stays. A rejected name stays, including `.takeover-*`, `notes.txt`, and `task-<id>.binlog`.
- `file_name` is unchanged. Schema stays 5. `migrations/` is still `000001` through `000005`. There is no `000006`. `uk_task_file_epoch` stays. `minRequiredSchemaVersion` stays 5. No new config key. ADR 0005 step 7 is marked landed. Steps 8 and 9 are unchanged. They are still next work.

## Upgrade Notes

No new migration. A database already on schema 5 from v0.5.50 stays on schema 5. Confirm `SELECT version, dirty FROM schema_migrations` is `(5, 0)`. `migrations/` is still `000001`, `000002`, `000003`, `000004`, and `000005`. The next new migration is still `000006`. `000006` is not in this release.

`minRequiredSchemaVersion` remains 5. v0.5.51 does not start on schema 4. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. That is the same refusal as v0.5.50. Schema 3 and schema 2 exit the same way. v0.5.49 still starts on schema 5, because `uk_task_file_epoch` is still there.

Replace the binary after schema 5 is already in place. If the database is already v0.5.50 / schema 5, there is no `./migrate` step. If it is not schema 5 yet, follow the [v0.5.50 upgrade](release-notes-v0.5.50.md) first, confirm `(5, 0)`, then start v0.5.51.

Schema 4 (`000004_pending_dump_cleanup`) is still in the chain. Before v0.5.50, schema 4 was optional for one process and required for cluster mode. That rule still applies when you upgrade stepwise and the binary you start is older than v0.5.50. v0.5.51 does not start on schema 4.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md), then the v0.5.50 upgrade. Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations.

Do not run `./migrate down` past the schema the running binaries need. v0.5.51 needs schema 5. This release adds no migration, so there is no new down step. `./migrate down --steps 1` from schema 5 is still the v0.5.50 down: it drops only `uk_task_source_epoch` and restores the previous nullability. `source_file` is nullable again. `start_pos` and `end_pos` are `NOT NULL` again. It returns to version 4. It does not delete rows. If `start_pos` or `end_pos` is already NULL, that down fails. Do not fill that NULL with 0. This release stores NULL when the end is unknown. It does not store 0. Do not run that down while v0.5.51 is running. v0.5.51 does not start on schema 4.

A historical `end_pos` of 0 is JSON `null` on `GET /api/tasks/{id}/files`, and the Console cell is blank, when this process cannot read the local bytes. Repair of the catalog positions happens when a worker that can read those bytes opens that directory. That repair does not upload the object again.

The v0.5.49 sealed-position rule (#189) and the v0.5.48 empty `task-<id>.binlog` rule (#205) still hold. Tip dogfood by BinlogServerQA passed on `a8862712`: unknown `end_pos` is JSON `null`; a readable sealed file shows the `EventSpan`; standalone does the same; a historical `end_pos` of 0 is JSON `null` on the API; retention keeps `.takeover-*`, `notes.txt`, and `task-*.binlog` and deletes an aged sealed segment; #189 and #205 do not regress; schema is still 5 and there is no `000006`. That run did not open a new issue.

This release is ADR 0005 step 7 of 9. Steps 8 and 9 are still next work.

## Known behavior

The items below are still open. #224 was already present before this release. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. This release does not change those fixes. This release is ADR 0005 step 7. Steps 8 and 9 are not in this release.

- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Retention does not remove it either. Tracked as #224. ADR 0005 step 8 is the cleanup of these files.
- ADR 0005 steps 8 and 9 are still next work. Step 8 is `.takeover-*` cleanup on startup and `materializeUploaded`. Step 9 drops `uk_task_file_epoch`. This release is step 7 only. It is not step 8 or 9.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.51.zh-CN.md
