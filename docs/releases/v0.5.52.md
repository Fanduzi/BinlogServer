# Binlog Server v0.5.52

Release date: 2026-10-07

A worker removes a `.takeover-*` temp left when a process crashes while takeover is downloading an `UPLOADED` object. The sweep runs at worker startup, before tasks are claimed, in every task directory under the data dir. `materializeUploaded` runs it again for that task directory before the next download and before `CreateTemp`. A temp this process is still writing stays. A sealed segment, an open segment, the active file, `task-<id>.binlog`, `notes.txt`, and every other name stay. A file that sits in the data dir itself stays. Another task's `.takeover-*` stays when this task materializes. Only a regular file whose name matches `.takeover-*` is removed. The log line is `takeover: removed stale temp <name> task=<id> size=<bytes>`. Retention still skips `.takeover-*`. There is no migration `000006`. Schema stays 5. `minRequiredSchemaVersion` stays 5. `uk_task_file_epoch` stays. No new config key. This release is ADR 0005 step 8 of 9. Step 9 is not in this release. Tip dogfood by BinlogServerQA passed on `dbf9f5c62a08a2868a1b6d2545b2d7cdac7bb7ab`. That run did not open a new issue. The v0.5.49 sealed-position rule (#189) still holds. The v0.5.48 empty `task-<id>.binlog` rule (#205) still holds. #224 is fixed in this release (PR #273).

## Highlights

- Worker startup deletes each regular `.takeover-*` file in every task directory under the data dir, before the worker claims tasks. A control-plane-only process does not run this sweep. A file in the data dir itself, including `.worker-id`, stays. A delete error stops the process before it listens. The message contains `remove stale takeover temps`.
- `materializeUploaded` deletes `.takeover-*` in that task directory before it opens the `UPLOADED` object and before `CreateTemp`. A sibling task directory is left alone, including that sibling's `.takeover-*`. A delete error returns before the download starts.
- A `.takeover-*` this process is still writing stays. A crashed leftover beside that download is removed. A download that fails while the process is still alive still removes the temp it created. `kill -9` still skips that removal. The next worker start, or the next `materializeUploaded` for that task, removes the file `kill -9` left.
- Other names stay: a sealed segment, an open segment, the active file, `task-<id>.binlog`, `notes.txt`, and any other file. A name that is not a regular file stays. Retention still skips every name `binlog.ClassifySegment` rejects, including `.takeover-*`. Retention is not this cleaner. The catalog gains no row for that temp name.
- The log line is `takeover: removed stale temp <name> task=<id> size=<bytes>`. `<name>` is the file base name. `<bytes>` is the size in bytes.
- `file_name` is unchanged. Schema stays 5. `migrations/` is still `000001` through `000005`. There is no `000006`. `uk_task_file_epoch` stays. `minRequiredSchemaVersion` stays 5. No new config key. ADR 0005 step 8 is marked landed. Step 9 is unchanged. It is still next work. #224 is fixed in this release.

## Upgrade Notes

No new migration. A database already on schema 5 from v0.5.51 stays on schema 5. Confirm `SELECT version, dirty FROM schema_migrations` is `(5, 0)`. `migrations/` is still `000001`, `000002`, `000003`, `000004`, and `000005`. The next new migration is still `000006`. `000006` is not in this release.

`minRequiredSchemaVersion` remains 5. v0.5.52 does not start on schema 4. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. That is the same refusal as v0.5.51 and v0.5.50. Schema 3 and schema 2 exit the same way. v0.5.49 still starts on schema 5, because `uk_task_file_epoch` is still there.

Replace the binary after schema 5 is already in place. If the database is already v0.5.51 / schema 5, there is no `./migrate` step. If it is not schema 5 yet, follow the [v0.5.50 upgrade](release-notes-v0.5.50.md) first, confirm `(5, 0)`, then start v0.5.52. A `.takeover-*` an older binary left in a task directory is removed when a worker running this binary starts.

Schema 4 (`000004_pending_dump_cleanup`) is still in the chain. Before v0.5.50, schema 4 was optional for one process and required for cluster mode. That rule still applies when you upgrade stepwise and the binary you start is older than v0.5.50. v0.5.52 does not start on schema 4.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md), then the v0.5.50 upgrade. Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations. Standalone is a worker, so it still removes `.takeover-*` at startup.

Do not run `./migrate down` past the schema the running binaries need. v0.5.52 needs schema 5. This release adds no migration, so there is no new down step. `./migrate down --steps 1` from schema 5 is still the v0.5.50 down: it drops only `uk_task_source_epoch` and restores the previous nullability. `source_file` is nullable again. `start_pos` and `end_pos` are `NOT NULL` again. It returns to version 4. It does not delete rows. If `start_pos` or `end_pos` is already NULL, that down fails. Do not fill that NULL with 0. v0.5.51 stores NULL when the end is unknown. It does not store 0. Do not run that down while v0.5.52 is running. v0.5.52 does not start on schema 4.

The v0.5.51 unknown-`end_pos` rule still holds. Unknown `end_pos` is JSON `null` on `GET /api/tasks/{id}/files`, and the Console cell is blank, when this process cannot read the local bytes. A readable local file shows the event span. Retention still deletes only a name `binlog.ClassifySegment` accepts as a sealed segment.

The v0.5.49 sealed-position rule (#189) and the v0.5.48 empty `task-<id>.binlog` rule (#205) still hold. Tip dogfood by BinlogServerQA passed on `dbf9f5c6`. That run did not open a new issue.

This release is ADR 0005 step 8 of 9. Step 9 is still next work.

## Known behavior

#224 is fixed in this release. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. This release does not change those earlier fixes. This release is ADR 0005 step 8. Step 9 is not in this release.

- ADR 0005 step 9 is still next work. Step 9 drops `uk_task_file_epoch`. `file_name` stays. This release keeps `uk_task_file_epoch`. It is step 8 only.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.52.zh-CN.md
