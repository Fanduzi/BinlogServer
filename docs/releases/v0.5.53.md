# Binlog Server v0.5.53

Release date: 2026-10-07

Migration `000006_drop_task_file_epoch_key` drops `binlog_files.uk_task_file_epoch`. A segment is only `(task_id, source_file, epoch)`, which is `uk_task_source_epoch`. The `file_name` column stays. `minRequiredSchemaVersion` is 6. On schema 5 this binary exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. v0.5.51 and v0.5.52 still require `uk_task_file_epoch`. On schema 6 they refuse to start. The log contains `missing index` and `uk_task_file_epoch`. Stop those processes before `./migrate up`. The floor stays v0.5.51, the first release that ships both ADR 0005 step 6 and step 7. v0.5.50 shipped step 6 only. This release is the first that ships step 9. No new config key. Tip dogfood by BinlogServerQA passed on `59a5e1e7efa55393e5a1227ac48b9f625a622d68`. That run did not open a new issue. The v0.5.49 sealed-position rule (#189) still holds. The v0.5.48 empty `task-<id>.binlog` rule (#205) still holds. The v0.5.52 `.takeover-*` cleanup still holds (#224, PR #273). Step 9 landed in PR #276.

## Highlights

- Migration `000006_drop_task_file_epoch_key` drops `uk_task_file_epoch` on `binlog_files`. It does not delete rows. It does not drop the `file_name` column. `uk_task_source_epoch` stays. `SHOW INDEX FROM binlog_files` lists `uk_task_source_epoch` and does not list `uk_task_file_epoch`.
- A segment is `(task_id, source_file, epoch)`. Seal, enroll, and the open-segment upsert already identify one segment by that key. `file_name` stays that source basename.
- `minRequiredSchemaVersion` is 6. v0.5.53 does not start on schema 5. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. Schema 4 and schema 3 exit the same way.
- v0.5.51 and v0.5.52 still require `uk_task_file_epoch`. After `000006` they refuse to start. The log contains `missing index` and `uk_task_file_epoch`. Stop them before `./migrate up`. The floor stays v0.5.51. v0.5.50 is step 6 only and is not the floor.
- A second insert of the same `(task_id, source_file, epoch)` fails. The duplicate key is `uk_task_source_epoch`.
- `migrations/` is `000001` through `000006`. No new config key. ADR 0005 step 9 is this release. It is the first release that ships the drop. v0.5.51 and v0.5.52 keep `uk_task_file_epoch`.

## Upgrade Notes

Stop every process that still requires `uk_task_file_epoch` before `./migrate up` to schema 6. v0.5.51 and v0.5.52 are those binaries. Confirm every running process is v0.5.51 or newer before you migrate. v0.5.51 is the first release that ships both ADR 0005 step 6 and step 7. v0.5.50 shipped step 6 only. A process older than v0.5.51, including v0.5.50 and v0.5.49, is not the floor. Replace it before `000006`. Then stop v0.5.51 and v0.5.52. Then run `./migrate up`.

`minRequiredSchemaVersion` is 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is `000001`, `000002`, `000003`, `000004`, `000005`, and `000006`.

Schema 5 to schema 6 is `000006_drop_task_file_epoch_key`. It drops `uk_task_file_epoch`. It does not delete rows. It does not drop `file_name`. `uk_task_source_epoch` stays. `SHOW INDEX FROM binlog_files` lists `uk_task_source_epoch` and does not list `uk_task_file_epoch`. The `file_name` column is still there.

v0.5.53 does not start on schema 5. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. Start v0.5.53 only after `schema_migrations` is `(6, 0)`. v0.5.51 and v0.5.52 refuse schema 6. The log contains `missing index` and `uk_task_file_epoch`. That refusal means one of those binaries is still in the inventory. Do not leave it deployed on schema 6.

If this database is not already schema 5, follow the [v0.5.50 upgrade](release-notes-v0.5.50.md) first and confirm `(5, 0)`. The floor check above still applies before `000006`. Schema 4 (`000004_pending_dump_cleanup`) is still in the chain. Before v0.5.50, schema 4 was optional for one process and required for cluster mode. That rule still applies when you upgrade stepwise and the binary you start is older than v0.5.50. v0.5.53 does not start on schema 4.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md), then the v0.5.50 upgrade. Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations. Standalone is a worker, so it still removes `.takeover-*` at startup.

Do not run `./migrate down` past the schema the running binaries need. v0.5.53 needs schema 6. `./migrate down --steps 1` from schema 6 adds `uk_task_file_epoch` back. It does not delete rows. It does not drop `file_name`. It does not drop `uk_task_source_epoch`. It returns to version 5. The `binlog_files` row count is unchanged. If two rows already share `(task_id, file_name, epoch)`, that down fails. Do not delete rows to make it pass. The writer keeps `file_name` equal to `source_file`. Do not run that down while v0.5.53 is running. v0.5.53 does not start on schema 5. Production `./migrate down` stays blocked unless `ALLOW_DESTRUCTIVE_MIGRATE=1`.

The v0.5.52 `.takeover-*` cleanup still holds. A worker still removes a regular `.takeover-*` left in a task directory. The v0.5.51 unknown-`end_pos` rule still holds. Unknown `end_pos` is JSON `null` on `GET /api/tasks/{id}/files`, and the Console cell is blank, when this process cannot read the local bytes. A readable local file shows the event span.

The v0.5.49 sealed-position rule (#189) and the v0.5.48 empty `task-<id>.binlog` rule (#205) still hold. Tip dogfood by BinlogServerQA passed on `59a5e1e7`: `000006` drops `uk_task_file_epoch`; this binary needs schema 6; schema 5 is refused; v0.5.52 refuses schema 6; a second insert of the same `(task_id, source_file, epoch)` fails; down leaves the row count unchanged. That run did not open a new issue.

This release is ADR 0005 step 9 of 9. It is the first release that ships step 9. v0.5.51 and v0.5.52 do not include it.

## Known behavior

#224 was fixed in v0.5.52. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. This release does not change those earlier fixes. This release is ADR 0005 step 9. Steps 1–8 already shipped. Step 9 is this release.

- `file_name` stays a column so existing `SELECT` lists keep working. Dropping the column is a later decision. This release drops only `uk_task_file_epoch`.
- ADR 0005 steps 1–9 have shipped. Step 9 is this release. It was not in v0.5.51 or v0.5.52. The floor for this drop stays v0.5.51.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.53.zh-CN.md
