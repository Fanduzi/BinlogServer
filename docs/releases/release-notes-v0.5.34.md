# Binlog Server v0.5.34

Release date: 2026-10-05

On Binlog Server before `v0.5.34`, `binlog_files` used unique key `(task_id, file_name)`, and `file_name` was the source binlog name. A later open of `<name>.open.eN` for that same source name updated the one row. The sealed path, upload state, checksum, and object key were replaced by the open file. Replay and point-in-time restore then listed only that short open file. Events that existed only in the earlier sealed segment were missing from the printed restore command. A second seal of the same source name reused the same object key and overwrote the object in the bucket. The published v0.5.33 package still does that.

## Highlights

- The unique key is `(task_id, file_name, epoch)`. `file_name` is still the source binlog name. Each epoch of that name is its own catalog row.
- Migration `000002_binlog_file_epoch_key` runs only when the operator runs `./migrate up`. The process does not migrate on startup. Before the unique key changes, a `file_path` that ends in `.open.eN` or `.sealed.eN` has `epoch` set to N. A plain sealed name such as `mysql-bin.000001` stays epoch 0. Object keys already stored on those rows are left as they are. An object that is already in the bucket does not need another upload for this upgrade.
- A later open writes its own row. Earlier sealed rows keep their path, `UPLOADED`, checksum, and object key.
- The first seal of a source name still uses the historical object key. The leaf is the source name. When a sealed file for that name is already on disk, or the catalog already has another epoch for that source name, a later seal uses leaf `name.sealed.eN`.
- `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive`, without `stop_datetime`, list every sealed segment in the `limit` window, plus the highest open segment for that source index. The window is still the tail of source-index order, then epoch. The default `limit` is 200, and each epoch counts as one row. A sealed file on this machine and a sealed file that exists only in the bucket are both listed. A lower open epoch of the same index is left out. When the open row that is kept carries the same object key, because those bytes were copied into the current open file, that sealed path is left out, so the same object is not listed twice. `locations` is still `local`, `bucket`, or `both`, in the same order as `paths`. Do not pass a `bucket` path to `mysqlbinlog` until the bytes are downloaded.
- `stop_datetime` on those same routes is the point-in-time window. `limit` is not applied. The full catalog is read, every sealed segment is eligible, and the highest open segment of each source index is eligible. The printed command keeps the segments whose copied events cover `[start, stop)`. A bucket-only sealed file in that cover is included. The same object-key rule still drops a sealed path whose bytes were copied into the current open file.
- Retention still ages each segment on its own row. `UPLOADED` with checksum `match` follows the same local window and bucket window as v0.5.33. `UPLOAD_FAILED` stays on local disk, with its catalog row. A checksum that does not match, or a checksum comparison that does not finish, is still recorded as `UPLOAD_FAILED`. A sealed file left by a crash during upload is still enrolled on the next start, including a lease takeover. The PUT right after seal is still bounded by `meta.timeout.upload_sec`. A task with no object storage still seals as `LOCAL_ONLY` with an empty object key. `POST /api/tasks/{id}/files/retry-upload` still returns HTTP 400, body `upload retry is not available`, for that task.
- When this worker opens a binlog file, other `OPEN` rows for that same source name at a different epoch are deleted. Sealed rows stay. `OPEN` rows in that task directory whose files are gone on this worker's disk are also deleted. An `OPEN` row for a different source name, with its path under another directory, stays.
- `GET /api/tasks/{id}/files` returns one row per epoch. `file_name` is the source name. `file_path` is that segment. Each row has its own `state`, `upload_state`, checksum, and object key. The Console file table shows one row per path, with that row's size, positions, location, upload state, checksum, and object key. The same default `limit` of 200 applies, and each epoch counts as one row.

## Known behavior

- After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0. `size_bytes` is the file size. The row can still be `UPLOADED` with checksum `match`. The segment stays in the files list, and download and replay still return it. Tracked as #189.
- A checksum is still compared once, after the upload. An object that already matched, and is then changed in the bucket, is not compared again.

## Upgrade Notes

**Stop every binlog-server process that uses this metadata database. Then run `./migrate up`. Then start only the v0.5.34 binaries.** Do not change the schema while an old process is still connected.

- Migration `000002_binlog_file_epoch_key` runs only from `./migrate up`. The server process does not migrate itself. Set `META_DSN`, or pass `--dsn`. The default migration path is `./migrations`.
- The v0.5.34 binary requires `schema_migrations` version at least 2, and index `binlog_files.uk_task_file_epoch`. Starting on schema 1 refuses to start. The error is `schema version too old: current=1 required>=2`. If version 2 is recorded and the index is missing, startup fails with `metadata schema is not up to date` and names `missing index binlog_files.uk_task_file_epoch`.
- A v0.5.33 binary against schema 2 refuses to start. Its version check accepts schema 2, because it requires version at least 1. It then requires index `binlog_files.uk_task_file`, which `000002` dropped. Startup fails with `metadata schema is not up to date` and names `missing index binlog_files.uk_task_file`.
- To roll back: stop every process, run `./migrate down --steps 1`, then start the v0.5.33 binaries. That down migration deletes every `binlog_files` row except the lowest `id` for each `(task_id, file_name)`, then restores unique key `uk_task_file`. The dropped rows are gone. When `ENV` or `MIGRATE_ENV` is `prod` or `production`, down refuses until `--allow-destructive` or `ALLOW_DESTRUCTIVE_MIGRATE=1` is set. The error begins `refusing destructive migrate command in production`.
- Object keys already stored are left as they are. Do not re-upload those objects as part of this upgrade.
- No new upload state. No new config key. `meta.timeout.upload_sec` stays 30 seconds by default. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.34.zh-CN.md
