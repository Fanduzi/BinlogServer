# Binlog Server v0.5.54

Release date: 2026-10-07

Point-in-time restore can stop immediately before one MySQL transaction by GTID. `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` accept `stop_gtid=<server_uuid>:<seq>`. The response lists segments through the one that contains that `GTID_EVENT`. `command` is `TZ=UTC mysqlbinlog` with `--stop-position` at that event's starting byte in the last file. Everything before that byte applies. That transaction and everything after it do not. Optional `start_datetime` is still an inclusive lower bound, including the same second as the GTID event. `stop_datetime` and `stop_gtid` cannot both be set. `limit` is unchanged when neither stop is set. Plain-text 400s are `invalid stop_gtid`, `stop_gtid is not in this task's backed-up range`, `stop_gtid is not supported for this flavor`, `stop_datetime and stop_gtid cannot both be set`, and `start_datetime is after stop_gtid`. The Console point-in-time form has a stop GTID field. The parameter list is in `docs/guide/reference/api.md` section 5.8. Swagger was regenerated. No schema change. Schema stays 6. `minRequiredSchemaVersion` stays 6. ADR 0005 is unchanged. The floor stays v0.5.51. No new config key. Tip dogfood by BinlogServerQA passed on `8646266e1b9ac99abd620a9e9df241dd51899d5f`. That run did not open a new issue. The v0.5.49 sealed-position rule (#189) still holds. The v0.5.48 empty `task-<id>.binlog` rule (#205) still holds. The v0.5.52 `.takeover-*` cleanup still holds (#224, PR #273). The v0.5.53 drop of `uk_task_file_epoch` still holds (PR #276). `stop_gtid` landed in PR #279.

## Highlights

- Several transactions can share one event-header second. `stop_datetime` cuts on that second, so good transactions in the same second as the bad one are left out, or the bad transaction is applied. When the bad GTID is already known from `mysqlbinlog -v`, BinlogViz, or `SHOW BINLOG EVENTS`, pass `stop_gtid`. The published v0.5.53 package has no `stop_gtid`.
- `stop_gtid` is one MySQL GTID, `<server_uuid>:<seq>`. The UUID match ignores case. The sequence is a positive integer. It is not 0. It has no leading zero. It is not a range and it is not a GTID set. Surrounding spaces are trimmed. An empty value, and a string that is neither this shape nor MariaDB `domain-server-seq`, is plain-text 400 `invalid stop_gtid` before the task is looked up.
- MariaDB `domain-server-seq` looks like `0-1-10`. On `source.flavor` `mysql`, that shape is `invalid stop_gtid`. On any other flavor, a MySQL shape and a MariaDB shape are both `stop_gtid is not supported for this flavor`. This path does not parse a MariaDB GTID.
- The server walks the replay inventory in binlog order and stops at the first `GTID_EVENT` whose server UUID and sequence both match. A previous-GTIDs event is not that transaction. A UUID or sequence that is not a `GTID_EVENT` in this task's segments is plain-text 400 `stop_gtid is not in this task's backed-up range`.
- `paths` uses the same inventory as the limit replay: every sealed segment, plus the highest open epoch of each source index. It then keeps segments through the file that contains the GTID and leaves out later files. The same rule applies when the GTID is in a sealed file, in `.open.e*`, or in an object that exists only in the bucket. A local file wins. A missing local file is read only for a sealed `UPLOADED` row with a non-empty `object_key`. A `location` of `bucket` is still the catalog `file_path`. Download that file before you pass it to `mysqlbinlog`.
- `command` begins with `TZ=UTC mysqlbinlog`. When `start_datetime` is set, `--start-datetime='<UTC>'` comes first. Then `--stop-position=<N>`. Then the paths. `N` is the starting byte of that `GTID_EVENT` in the last file. It is not the `end_log_pos` in the event header. A segment copied from the middle of a source file has a format description in front, so those two numbers differ. `mysqlbinlog` applies `--stop-position` only to the last file on the command line. Events that start at `N` are not decoded. The command includes every transaction before that GTID. It does not include that transaction. It does not include anything after it.
- `start_datetime` stays the inclusive lower bound from section 5.7. A start equal to the GTID event's timestamp is accepted. A start later than that timestamp is plain-text 400 `start_datetime is after stop_gtid`. An earlier segment whose copied events are all before the start is left out of `paths`. The segment that holds the GTID stays.
- `limit` is ignored when `stop_gtid` or `stop_datetime` is set. When neither stop is set, the limit window is the same as v0.5.53 and the response has no `command` field. `start_datetime` alone is still plain-text 400 `stop_datetime is required`. An unparseable `start_datetime` is `invalid start_datetime`.
- Sending `stop_datetime` and `stop_gtid` together, including when one of the values is empty, is plain-text 400 `stop_datetime and stop_gtid cannot both be set`. That check runs before the task lookup.
- A missing task, when `stop_gtid` is a MySQL shape or a MariaDB shape, is HTTP 404 `task not found`. A selected segment that cannot be opened is HTTP 404 `segment not found on this process`.
- `GET /api/tasks/{id}/replay/archive` accepts the same `stop_gtid` and the optional `start_datetime`. The tar members are those basenames. The stop position stays in the JSON `command`. It is not a tar member. A missing, unsupported, or out-of-range GTID is the same plain-text error as the JSON route. The body is not a tar.
- Console task detail, on the point-in-time form, has a stop GTID field. A stop time or a stop GTID is enough to generate the command and to download `task-{id}-replay.tar`. Filling both sends both parameters, and the page shows `stop_datetime and stop_gtid cannot both be set`. That Console is in the embedded bundle a plain `go build` serves.
- No new config key. No schema migration. `minRequiredSchemaVersion` stays 6. `migrations/` is still `000001` through `000006`. There is no `000007`. ADR 0005 is unchanged. Steps 1–9 already shipped. Step 9 shipped in v0.5.53. This release is not a new ADR step.

## Upgrade Notes

No new migration. A database already on schema 6 from v0.5.53 stays on schema 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is still `000001`, `000002`, `000003`, `000004`, `000005`, and `000006`. There is no `000007`.

`minRequiredSchemaVersion` remains 6. v0.5.54 does not start on schema 5. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. That is the same refusal as v0.5.53. Schema 4 and schema 3 exit the same way.

Replace the binary after schema 6 is already in place. If the database is already v0.5.53 / schema 6, there is no `./migrate` step. If it is not schema 6 yet, follow the [v0.5.53 upgrade](release-notes-v0.5.53.md) first and confirm `(6, 0)`, then start v0.5.54.

The floor stays v0.5.51, the first release that ships both ADR 0005 step 6 and step 7. v0.5.50 shipped step 6 only. Stop every process that still requires `uk_task_file_epoch` before `./migrate up` to schema 6. v0.5.51 and v0.5.52 are those binaries. On schema 6 they refuse to start. The log contains `missing index` and `uk_task_file_epoch`. A process older than v0.5.51, including v0.5.50, is not the floor. Replace it before `000006`. Then stop v0.5.51 and v0.5.52. Then run `./migrate up`.

Schema 4 (`000004_pending_dump_cleanup`) is still in the chain. Before v0.5.50, schema 4 was optional for one process and required for cluster mode. That rule still applies when you upgrade stepwise and the binary you start is older than v0.5.50. v0.5.54 does not start on schema 4.

If this database is not already schema 3, follow the [v0.5.46 upgrade](release-notes-v0.5.46.md), then the v0.5.50 upgrade, then the v0.5.53 upgrade. Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

No new config key. Standalone with no metadata database does not run these migrations. `stop_gtid` reads the segments this process can open, so standalone has the same query.

Do not run `./migrate down` past the schema the running binaries need. v0.5.54 needs schema 6. This release adds no migration, so there is no new down step. `./migrate down --steps 1` from schema 6 is still the v0.5.53 down: it adds `uk_task_file_epoch` back. It does not delete rows. It does not drop `file_name`. It does not drop `uk_task_source_epoch`. It returns to version 5. The `binlog_files` row count is unchanged. If two rows already share `(task_id, file_name, epoch)`, that down fails. Do not delete rows to make it pass. The writer keeps `file_name` equal to `source_file`. Do not run that down while v0.5.54 is running. v0.5.54 does not start on schema 5. Production `./migrate down` stays blocked unless `ALLOW_DESTRUCTIVE_MIGRATE=1`.

The v0.5.53 index rule still holds. `SHOW INDEX FROM binlog_files` lists `uk_task_source_epoch` and does not list `uk_task_file_epoch`. The `file_name` column stays. A second insert of the same `(task_id, source_file, epoch)` fails. The duplicate key is `uk_task_source_epoch`.

The v0.5.52 `.takeover-*` cleanup still holds. A worker still removes a regular `.takeover-*` left in a task directory. The v0.5.51 unknown-`end_pos` rule still holds. Unknown `end_pos` is JSON `null` on `GET /api/tasks/{id}/files`, and the Console cell is blank, when this process cannot read the local bytes. A readable local file shows the event span.

The v0.5.49 sealed-position rule (#189) and the v0.5.48 empty `task-<id>.binlog` rule (#205) still hold. `stop_datetime` is unchanged. It still leaves out an event whose timestamp is greater than or equal to the stop. Tip dogfood by BinlogServerQA passed on `8646266e`. That run did not open a new issue.

This release is not an ADR 0005 step. Steps 1–9 already shipped. ADR 0005 is unchanged. v0.5.53 is the release that shipped step 9.

## Known behavior

#224 was fixed in v0.5.52. #189 was fixed in v0.5.49. #205 was fixed in v0.5.48. This release does not change those earlier fixes. This release is not an ADR 0005 step. ADR 0005 is unchanged.

- `stop_datetime` still cuts on the event-header second. Transactions that share that second are still all in or all out on the datetime path. Use `stop_gtid` when the bad GTID is known.
- Schema stays 6. The floor stays v0.5.51. There is no `000007`. `file_name` stays a column.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.54.zh-CN.md
