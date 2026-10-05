# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

Maintenance rules:

- Add user-visible or operator-visible changes to `Unreleased` as they land.
- Before a release, review `Unreleased` and make sure schema, config, `sqlc`, and observability changes are clearly called out.
- When cutting a release, move `Unreleased` entries into a dated release section and start a fresh `Unreleased` block.

## [Unreleased]

## [v0.5.35] - 2026-10-05

### Fixed

- On a control-plane plus workers deployment, Stop from the API process used to write the task `STOPPED` and clear the owner while the worker kept the dump and the lease. The Console showed Stopped, and the source still had a Binlog Dump thread (#174). When this process is not dumping and another worker owns the lease, Stop writes `STOPPING` and keeps the owner, the epoch, and the source config. `POST /api/tasks/{id}/stop` still returns HTTP 204. The Console shows Stopping. The worker claim loop, about every 2 seconds, cancels a live dump whose row is `STOPPING` or `STOPPED`. When that dump exits, the worker writes `STOPPED` and releases the lease. A store sync and `GetTask` on that worker also cancel the dump and leave the in-memory owner and epoch in place, so the lease can be released. The final `STOPPED` row keeps the latest stored source config. A source password changed while the row is `STOPPING` is the password the next Start uses. A `STOPPING` row whose lease has expired is written `STOPPED`, and that claim does not start a dump. All-in-one and standalone still cancel the local dump and write `STOPPED` when that dump exits. This release has no schema migration. Schema stays at version 2 from v0.5.34. Binaries roll from v0.5.34 with no `./migrate` when the database is already at version 2. A database still on schema 1 still follows the v0.5.34 notes: stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer. On the split topology, HTTP 204 means Stop was accepted. The row stays `STOPPING` until the worker finishes, and a Start in that state returns HTTP 400 `cannot start from state STOPPING`. After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may still show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct (#189). After a Stop during which the source password was changed, then Start, the source may briefly show two Binlog Dump threads. A later Stop that reaches `STOPPED` may leave one Binlog Dump thread that needs a manual `KILL` (#193). An ordinary control-plane Stop, and a Stop on an all-in-one process, leave the source process list without that task's Binlog Dump thread.

## [v0.5.34] - 2026-10-05

### Fixed

- A later open of the same source binlog name no longer overwrites the sealed catalog row. `binlog_files` unique key is `(task_id, file_name, epoch)`. `file_name` stays the source name. Each epoch is its own row, with its own path, upload state, checksum, and object key. Before this release the unique key was `(task_id, file_name)`. Opening `<name>.open.eN` replaced the sealed path, `UPLOADED`, checksum, and object key, so replay and point-in-time restore listed only the short open file, and a second seal of that name reused the object key and overwrote the bucket object. The published v0.5.33 package still does that. Migration `000002_binlog_file_epoch_key` runs only from `./migrate up`. The process does not migrate on startup. Before the unique key changes, a `file_path` ending in `.open.eN` or `.sealed.eN` gets `epoch` N. A plain sealed name stays epoch 0. Stored object keys are left unchanged, so already-uploaded objects do not need another upload. A later open writes its own row. The first seal of a source name still uses the historical object key, leaf equal to the source name. When a sealed file for that name is already on disk, or the catalog already has another epoch for that source name, a later seal uses leaf `name.sealed.eN`. `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` list every sealed segment in the window, on disk or bucket-only, plus the highest open segment for that source index. `stop_datetime` reads the full catalog and then keeps the segments whose copied events cover the time window. The same object is not listed twice when those bytes were copied into the current open file. `limit` still defaults to 200, and each epoch counts as one row. Retention still ages each segment on its own row. `UPLOADED` with checksum `match` follows the v0.5.33 local and bucket windows. `UPLOAD_FAILED` stays local. Checksum mismatch and an unfinished checksum check stay `UPLOAD_FAILED`. Crash enrollment, the `meta.timeout.upload_sec` deadline, and `LOCAL_ONLY` with an empty object key when object storage is not configured are unchanged. `POST /api/tasks/{id}/files/retry-upload` still returns HTTP 400 `upload retry is not available` for that task. Opening a binlog file deletes other `OPEN` rows for that source name at a different epoch. Sealed rows stay. `OPEN` rows in that task directory whose files are gone on this worker's disk are also deleted. An `OPEN` row for a different source name under another directory stays. `GET /api/tasks/{id}/files` and the Console file table show one row per epoch. After failover, a later sealed segment such as `mysql-bin.NNNNNN.sealed.e1` may show `end_pos` 0 while `size_bytes` and `UPLOADED` with checksum `match` are correct. Download and replay still return that segment. A checksum is still compared once after upload. Stop every binlog-server process that uses this metadata database, run `./migrate up`, then start only the v0.5.34 binaries. The new binary requires `schema_migrations` at least 2 and index `uk_task_file_epoch`, and refuses to start without them. A v0.5.33 binary against schema 2 refuses to start because it still expects `uk_task_file`. Rolling back is stop every process, `./migrate down --steps 1`, then start the old binaries. That down migration drops every `binlog_files` row except the lowest `id` per `(task_id, file_name)` before restoring `uk_task_file`. No new upload state. No new config key.

## [v0.5.33] - 2026-10-05

### Fixed

- A worker that dies while a just-sealed binlog is uploading, or in the moment after that file is renamed and before the upload is recorded, no longer leaves the sealed file stuck as `LOCAL_ONLY` or outside the retry set. With object storage, the catalog row is written before the PUT: `state` `SEALED`, `upload_state` `UPLOAD_FAILED`, `upload_error` `upload pending`, object key set. The checkpoint for the next file is written before that PUT. The PUT uses the existing `meta.timeout.upload_sec` deadline (30 seconds by default), the same deadline as the background retry. A timeout leaves the row `UPLOAD_FAILED` and replication opens the next file. `upload_error` stays `upload pending` after that timeout, because the catalog write that would record it uses the same cancelled deadline. On the next start, including a lease takeover, a sealed file on disk that never became `UPLOADED` is recorded the same way. When the checkpoint is still on that sealed file and the last complete event is the rotate that names the next file, the checkpoint moves to that next file, so replication continues there. The background retry uploads the file and checks the checksum. The worker that pulls binlog runs that retry. A control-plane-only process does not. `GET /api/tasks/{id}/files` shows `UPLOAD_FAILED` and `location` `local` while the file is waiting, then `UPLOADED`, checksum `match`, and `location` `both` while the file is still on disk. With object storage and a catalog, retention keeps the local file and the catalog row until the checksum is `match`. A file past the local retention window stays, and that pass writes one `RETENTION_SKIPPED_NOT_UPLOADED`. For a file enrolled after a crash, `sealed_at` is the time this process opened the file: the open row has no separate seal time, the catalog writes `sealed_at` with the same value as `created_at`, and enrollment keeps that time. While the PUT is hanging, new local binlog events wait until the timeout fires, and the background retry may PUT the same object key again with the same sealed bytes. The next run with object storage uploads a sealed `LOCAL_ONLY` file that is still on disk. Up to `v0.5.32`, turning object storage on later left that row unuploaded. A task with no object storage still seals as `LOCAL_ONLY` with an empty object key. `POST /api/tasks/{id}/files/retry-upload` returns HTTP 400 `upload retry is not available`. A checksum is still compared once after upload. An object changed in the bucket after it matched is not compared again. No new upload state. No new config key. No schema migration.

## [v0.5.32] - 2026-10-05

### Fixed

- A sealed upload whose stored object does not match the local file, or whose checksum comparison does not finish, is recorded as `UPLOAD_FAILED` instead of a durable `UPLOADED` copy. `upload_error` is `checksum mismatch`, or it begins with `checksum verify failed:`. `checksum` stays `mismatch` or empty. Empty is not `match` and not `mismatch`. Replication keeps running. With object storage and a catalog, retention does not delete that local file or its catalog row, and it writes one `RETENTION_SKIPPED_NOT_UPLOADED` event. The background upload retry and `POST /api/tasks/{id}/files/retry-upload` upload a checksum mismatch again. An unfinished check is compared again and is not uploaded again. When that later check matches, the row becomes `UPLOADED` with checksum `match`, and a later retention pass can delete it. An `UPLOADED` row already stored with `mismatch` or an empty checksum keeps its local file; the next retention pass records it as `UPLOAD_FAILED` so that same retry can verify it. A checksum of `match` is unchanged: retention still deletes the object, the catalog row, and the local file, including the longer bucket window that deletes only the local file. PITR, download, and replay still read a sealed `UPLOADED` object when the local file is already gone. `LOCAL_ONLY` retention is unchanged. A bucket-only `UPLOADED` row is still aged as before. No new upload state. No new config key. No schema migration. The Console already shows `upload_state` and `checksum`, and the existing retry button appears for `UPLOAD_FAILED`.

## [v0.5.31] - 2026-10-05

### Fixed

- `GET /api/tasks/{id}/replay` with `stop_datetime`, and the same query on `GET /api/tasks/{id}/replay/archive`, no longer treats the source file's format description or previous-GTIDs timestamp as coverage. Those headers record when the source binlog was created. A `LATEST` backup, and any start in the middle of a file, writes that format description in front of the first copied event. Asking for a UTC time before that first copied event returns an empty window: `paths` is `[]` and `command` is empty. A time that contains a copied event still returns that file. The `mysqlbinlog` flags are unchanged. No new config key. No schema migration.

## [v0.5.30] - 2026-10-05

### Fixed

- A backup that starts in the middle of a source binlog, which is what `LATEST` does, writes the format description MySQL sends before the first copied event. `mysqlbinlog --verify-binlog-checksum` can read that file. The description's own end position (126 on MySQL 8, or 0) is not the checkpoint, and its timestamp is not lag. A quiet source that has not sent a later event still leaves the segment as the 4-byte magic header, so stop then start does not rewind to that description. A segment that already has events does not gain a second description on the next start. No new config key. No schema migration.

## [v0.5.29] - 2026-10-05

### Added

- Local disk and the bucket can keep a sealed binlog for different numbers of days. `storage.retention_days` stays required, range 1..3650. `storage.local_retention_days` and `storage.bucket_retention_days` are optional. Omitted or 0 uses `retention_days`, and those keys are omitted from the task JSON. When the effective local days and the effective bucket days are equal, that shared number is the one cutoff. A task that sets only `retention_days` is this case. An expired uploaded segment that is still on disk is removed from the object, the catalog row, and the local file together, as in v0.5.28. On that path a catalog row whose local file is already gone stays, and `sealed_at` and `uploaded_at` are not used. Create (`POST /api/tasks`) and update (`PUT /api/tasks/{id}`) reject a bucket window shorter than the local window. The comparison uses the effective days, so `local_retention_days` above `retention_days` with `bucket_retention_days` omitted is rejected too. `local_retention_days` below `retention_days` with `bucket_retention_days` omitted leaves the bucket window at `retention_days`, so the longer bucket window applies when object storage and a catalog are configured. Omitting `local_retention_days` and setting `bucket_retention_days` above `retention_days` does the same. The message contains `shorter than local retention`. Create returns JSON `code` `INVALID_REQUEST`. Update returns that sentence as plain text. Both are HTTP 400. A non-zero value outside 1..3650 is HTTP 400. Equal windows are accepted. The published v0.5.28 package has no split and still uses one cutoff. No schema migration. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

- The longer bucket window runs only with object storage and a catalog (`meta_dsn`). A sealed `UPLOADED` segment with an object key, older than the local window and still inside the bucket window, loses only the local file. The object and the catalog row stay. While the file is on disk, both windows use the local file modification time. If that time is older than the bucket window, the object, the catalog row, and the local file are removed together. Retention still runs when the replication loop opens a binlog file. The active open segment and every other open segment stay. `GET /api/tasks/{id}/files` sets `location` to `local`, `bucket`, or `both`. `bucket` means the catalog `file_path` is not on this process. `GET /api/tasks/{id}/files/{name}`, `GET /api/tasks/{id}/replay` (the limit window and `stop_datetime`), and `GET /api/tasks/{id}/replay/archive` still return the object bytes when this process has object storage configured. A process with no object storage configured still returns 404 `segment not found on this process` for a file that is not on disk. Replay `locations` follows `paths`. Do not pass a `bucket` path to `mysqlbinlog` until the bytes are downloaded. After the local file is gone, bucket age is `sealed_at`, then `uploaded_at` when `sealed_at` is empty. A row with neither timestamp stays. Once that timestamp is past the bucket window, the object and the catalog row are removed. `UPLOAD_FAILED` and `LOCAL_ONLY` are still kept when object storage and a catalog are configured. Age for that keep is the local file modification time, compared with the local window. Replication keeps running. `GET /api/tasks/{id}/events` gets one `RETENTION_SKIPPED_NOT_UPLOADED` per kept file. Opening another binlog does not append that event again for the same file while this process stays up. After a restart, the next retention pass that still keeps the file appends that event once more. The process that runs retention exposes `binlog_server_retention_blocked_files{task_id}` on `GET /metrics`. A control-plane-only process does not run retention, so the gauge stays 0 there. `LOCAL_ONLY` is still not picked up by the background upload retry or by `POST /api/tasks/{id}/files/retry-upload`. Without upload configured, the sealed file is still deleted by the local window, a catalog row stays when one exists, and that event is not written. Standalone without `meta_dsn` still deletes the local sealed file by the local age, including one that never reached the bucket. A longer bucket window is not applied without a catalog. Do not use that mode as the only copy when the bucket can be down longer than the local retention. If the object delete fails, the local file stays, `checksum` is left as it was, and `last_error` begins with `OBJECT_PURGE_FAILED`. Console task detail shows the effective local days and bucket days. The file table shows `location` (`local`, `bucket`, or `both`). A `bucket` segment shows the hint that the path is not on this machine. That Console is in the embedded bundle a plain `go build` serves.

## [v0.5.28] - 2026-10-05

### Fixed

- With object storage and a catalog (`meta_dsn`), age retention no longer deletes the only copy of an expired sealed binlog whose `upload_state` is `UPLOAD_FAILED` or `LOCAL_ONLY`. Age is still the local file modification time, compared with `storage.retention_days`. The local file and the catalog row stay. Replication keeps running. It does not set `last_error` and does not enter `RETRY_BACKOFF`. `GET /api/tasks/{id}/events` gets one `RETENTION_SKIPPED_NOT_UPLOADED` per kept file. The message names the file and its `upload_state`, for example `mysql-bin.000003 upload_state=LOCAL_ONLY`. Opening another binlog does not append that event again for the same file while this process stays up. After a restart, the next retention pass that still keeps the file appends that event once more. The process that runs retention exposes `binlog_server_retention_blocked_files{task_id}` on `GET /metrics`: how many of those expired files the latest successful pass is still keeping. The gauge drops when a later pass deletes them. After a restart it stays 0 until the next retention pass. A control-plane-only process does not run retention, so the gauge stays 0 there. The event remains on `GET /api/tasks/{id}/events`. The published v0.5.27 package still deletes that local file. No new config key. No schema migration.

- Once the row is `UPLOADED`, the next retention pass deletes the object, then the catalog row, then the local file. A sealed `UPLOAD_FAILED` row is still picked up by the background upload retry and by the existing manual retry `POST /api/tasks/{id}/files/retry-upload`. Either path can make that row `UPLOADED`. `LOCAL_ONLY` is not picked up by either path. A sealed file that never had object storage configured stays on disk past retention, and the gauge keeps counting it. Configuring object storage later does not upload that row. Already-`UPLOADED` rows, `OBJECT_PURGE_FAILED`, and local retention when upload is not configured are unchanged. Standalone without `meta_dsn` still deletes the local sealed file by age, including one that never reached the bucket. Do not use that mode as the only copy when the bucket can be down longer than `storage.retention_days`.

## [v0.5.27] - 2026-10-05

### Added

- An existing backup task can be asked for the binlog window covering a UTC stop time. `GET /api/tasks/{id}/replay` takes `stop_datetime` and an optional `start_datetime`. The window is half-open `[start, stop)`: the stop is excluded, and a start, when set, is included. The same query on `GET /api/tasks/{id}/replay/archive` downloads that selection as `task-{id}-replay.tar`. The response includes `command` only when `stop_datetime` is present. `mysql` uses `mysqlbinlog`, `mariadb` uses `mariadb-binlog`, and the command is prefixed `TZ=UTC`. Omitting both datetimes keeps the limit replay: `flavor`, `client`, `client_hint`, `paths`, and no `command` field. One path per source index still uses the sealed-versus-open rule. `limit` is not applied on the datetime path. `start_datetime` equal to `stop_datetime` is an empty window: HTTP 200, empty `paths`, empty `command`. It is not a 400. A bad datetime or a start after the stop is plain-text 400. A missing task is 404 `task not found`. The published v0.5.26 package has no stop-time window. No new config key. No schema migration. Restore the full backup yourself, then use the printed command. Binlog Server does not restore that backup.

- Console task detail has the same window: a UTC stop time, an optional start time, generate, copy, and download of `task-{id}-replay.tar`. That Console is in the embedded bundle a plain `go build` serves.

## [v0.5.26] - 2026-10-04

### Added

- A worker that has object storage configured retries sealed `UPLOAD_FAILED` segments in the background. Standalone and all-in-one processes are workers, so they run this loop. A cluster worker runs it too. A control-plane-only process does not. A process with no object storage configured does not run this loop. The loop uses the same path as the existing manual retry. A successful row becomes `UPLOADED`, including checksum, the same way a manual retry does. Open segments stay unuploaded. A sealed file that is not on this machine is skipped, and its catalog row is left unchanged. Upload failure still does not stop binlog pull and does not change task state. The default cadence is every 15 seconds, up to 100 sealed `UPLOAD_FAILED` rows per task per pass. The published v0.5.25 package leaves those rows at `UPLOAD_FAILED` until the retry API is called. No new config key. No schema migration.

- `POST /api/tasks/{id}/files/retry-upload` and `RetryFailedUploads(taskID string, limit int)` stay as they are. While a task's background pass is running, a manual retry of that task still returns `upload retry already in progress`. Retention, lease takeover, and the v0.5.25 object-read behavior are unchanged. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`.

## [v0.5.25] - 2026-10-04

### Fixed

- After a worker dies and its lease expires, another worker takes the task. The lease moves to the live worker. The segment directory stays at the `file_path` already stored on `binlog_files`. It does not follow the lease. When this worker can read that directory, it continues from the last complete event there. The same source file is not sealed a second time, and this worker does not open a fresh `{data_dir}/{task_id}/` and pull from position 4. When the open segment, or a sealed segment that is not yet `UPLOADED`, is not readable on this worker, and the checkpoint is not already inside a sealed `UPLOADED` object, the task becomes `FAILED`. `last_error` begins with `SEGMENT_NOT_ON_WORKER` and names the missing path. This worker does not create a new directory and keep pulling. Mount or copy that path onto this worker, then `POST /api/tasks/{id}/start`. Same-host kill -9, adopt, then start still uses this worker's own directory. The published v0.5.24 package does not continue from that directory. No new config key. No schema migration. `cluster.failover_policy` is still not a switch for this path.

- When the checkpoint is already inside a sealed `UPLOADED` object, this worker reads the object back with the upload client already configured, even when the local file is absent, and continues from the last complete event in that object. A checkpoint already on the next file at position 4, with every sealed row `UPLOADED`, still starts on that next file. Retention is unchanged: delete the object, then the catalog row, then the local file; `OBJECT_PURGE_FAILED` leaves the local file; the next open continues on the next file.

## [v0.5.24] - 2026-10-04

### Fixed

- When the replication loop opens a file, a sealed segment older than `storage.retention_days` (the local file modification time, not `sealed_at`) that is `UPLOADED` and has an object key is removed from the bucket, then its catalog row is removed, then the local file is removed. A segment still inside the retention window stays. An open segment is not deleted, and retention does not delete an object for one. Without upload configured, local retention is unchanged. The published v0.5.23 package deletes the local file and leaves the object in the bucket. No new config key. No schema migration.

- If deleting the object fails, the local file stays, `checksum` is left as it was (`match`, `mismatch`, or empty), and the row stays `UPLOADED`. Empty is not `match` and not `mismatch`. The task retries with `last_error` beginning `OBJECT_PURGE_FAILED`. An object that is already gone (HTTP 204 or 404) counts as success so the retry can finish. After a segment is sealed, the checkpoint moves to the next file before retention runs. If opening that next file then fails, the retry continues on the next file. It does not try to seal the file that was just sealed, so it does not stop on `sealed file already exists`. A task already stopped on `sealed file already exists`, with its checkpoint still on that sealed file, is not repaired by this release. Move that sealed file aside once so the next retry can continue.

## [v0.5.23] - 2026-10-04

### Added

- After a sealed segment is uploaded, the server HEADs the object once and compares its ETag with the sealed file. At or under 16MiB that is the whole-file MD5. Above that it is the same 16MiB multipart ETag, up to about 160GiB. There is no periodic re-check. The result is stored in the existing `binlog_files.checksum` column as `match` or `mismatch`. `GET /api/tasks/{id}/files` returns that value. The Console file table shows it in 校验. An open segment is not checksummed, so that cell is `--`. `match` means this ETag comparison found the same bytes. `mismatch` means the comparison finished and the bytes differ. `mismatch` stays `UPLOADED`. It is not treated as verified, and it does not stop replication. A failed upload stays `UPLOAD_FAILED` and the checksum is cleared. If the object HEAD fails, checksum stays empty on an `UPLOADED` row. Empty is not `match` and not `mismatch`. `GET /api/tasks/{id}/files` omits `checksum` when it is empty. The Console shows that cell as `--`. An uploader that cannot read the object also leaves checksum empty. Empty is not verified. A blank 校验 cell is neither a pass nor a fail. No new config key. No schema migration.

## [v0.5.22] - 2026-10-04

### Added

- A stopped task's Console detail shows two identities. Start (起点) is the configured start: `LATEST`, or `FILE_POS` as `file:pos`, or `GTID` as `gtid_set`. Resume (续传) is the position the next Start continues from. A stopped `LATEST` task still shows `LATEST` as its start. It does not get rewritten to `FILE_POS`. The resume line is the file and position of the last complete event in the highest local open segment. `GET /api/tasks/{id}/checkpoint` returns that same resume the runner uses on Start: `file` and `pos`. `gtid_set` is included only when the stored checkpoint is that same file and position. With neither a checkpoint row nor a complete local event, the response stays 404 `checkpoint not found`. The configured start stays on the task's `start` object, not inside the checkpoint JSON. No new config key. No schema migration.

### Fixed

- A stop that races the run exit no longer leaves the task stuck in `STOPPING`. A later write of an older `STOPPING` snapshot does not overwrite `STOPPED`.

## [v0.5.21] - 2026-10-04

### Added

- `GET /api/tasks/{id}/replay/archive` returns one `application/x-tar` (ustar) of the same `limit` and the same replay-file window as `GET /api/tasks/{id}/replay`. Each member is that window's basename, in that window's order, not a host path. One source index is still one member: a sealed name and `.open.e*` keep the highest-epoch open segment. Member bytes follow single-segment download: the local file wins, including an open segment while the task is `RUNNING` or `STOPPED`; a missing sealed file is read from object storage only when its catalog row is `UPLOADED` and `object_key` is non-empty. `Content-Disposition` is `attachment` with filename `task-{id}-replay.tar`. The route uses the same auth as other `/api/tasks/*` routes. An empty window is HTTP 200 and an empty ustar (1024 zero bytes). A missing task is 404 `task not found`. Every selected segment is opened before the tar is written; if one cannot be opened or read, the response is an error and is not a partial archive. `LOCAL_ONLY`, `UPLOAD_FAILED`, an empty `object_key`, an open segment with no local file, and a process with no object store configured stay 404 `segment not found on this process`. Console task detail has 下载回放集 next to the existing copy-replay action and uses the same `limit=80` as the task drawer. Single-segment download, the files list, and the JSON replay command are unchanged. No new config key. No schema migration.

## [v0.5.20] - 2026-10-04

### Added

- `GET /api/tasks/{id}/files/{name}` and the Console files-table Download stream a sealed segment that is already `UPLOADED` when `{data_dir}/{id}/{name}` is not on this process. The catalog row must include a non-empty `object_key`. `{name}` is that basename, not the object key. `Content-Type` is `application/octet-stream`. `Content-Disposition` uses that basename. The body length is the object size at open. A local file still wins, including `*.open.e*` while the task is `RUNNING` or `STOPPED`. `LOCAL_ONLY`, `UPLOAD_FAILED`, an empty `object_key`, an open segment with no local file, and a process with no object store configured stay 404 `segment not found on this process`. The response does not follow catalog `file_path` and does not invent open-segment bytes from object storage. A slash, backslash, `..`, or other traversal form is still 400 `invalid segment name`. A missing task is still 404 `task not found`. `GET /api/tasks/{id}/files` and `GET /api/tasks/{id}/replay` are unchanged: the files list still lists every name, and replay is still one path per source index. No new config key. No schema migration.

## [v0.5.19] - 2026-10-04

### Added

- `GET /api/tasks/{id}/files/{name}` streams the raw bytes of one on-disk basename already listed by `GET /api/tasks/{id}/files` (a sealed name or `*.open.e*`), over the authenticated API and the Console, without SSH onto the backup host. `Content-Type` is `application/octet-stream`. `Content-Disposition` uses that basename. The body length is the file size at open. A slash, backslash, `..`, or other traversal form is 400 `invalid segment name`. A missing task is 404 `task not found`. A basename that is not under this process's task directory is 404 `segment not found on this process`. A control plane with no local file does not invent bytes from the catalog path or object storage. An open segment can be downloaded while the task is RUNNING or STOPPED. The Console files table has Download on each row. `GET /api/tasks/{id}/files` still lists every name. `GET /api/tasks/{id}/replay` and the Console copy command are unchanged: one path per source index, and when a sealed name and `.open.e*` share an index the path is the highest-epoch open segment and keeps the `.open.e*` suffix. No new config key. No schema migration.

## [v0.5.18] - 2026-10-04

### Added

- `GET /api/tasks/{id}/replay` returns one on-disk path per source index, inside the same inventory window as `GET /api/tasks/{id}/files`. Paths are ascending. When a sealed name and one or more `.open.e*` files share an index, the path is the highest-epoch open segment and keeps the `.open.e*` suffix. A small `limit` keeps the highest indexes, not the newest `sealed_at`. `source.flavor` `mysql` sets `client` to `mysqlbinlog` and `client_hint` to `MySQL mysqlbinlog`. `mariadb` sets both to `mariadb-binlog`. An empty flavor leaves the client empty. An empty directory returns `paths: []`. The files API is unchanged and still lists every name. The Console task detail shows that command and copies it. No new config key. No schema migration. Stop, resume, adopt, and kill-9 resume are unchanged.

## [v0.5.17] - 2026-10-04

### Fixed

- With `meta_dsn`, `GET /api/tasks/{id}/files` and the Console files table list catalog rows in the same order as the standalone disk scan: ascending source index, and for one index the sealed name before `.open.e*` epochs. `limit` keeps the highest indexes. The no-meta disk scan is unchanged. No new API. No new config key. No schema migration.

## [v0.5.16] - 2026-10-04

### Fixed

- Kill the process while a backup is `RUNNING`. This is not an API stop. The on-disk backup then has no task metadata. Adopt it without starting, then start it. The dump continues from the end position of the last complete event in the open segment. It does not jump to the current `SHOW MASTER STATUS`. It does not restart at position 4. When that open segment's last complete event already ends at the resume position, the segment is renamed onto the new epoch and new events are appended, so one source filename spans the crash. The file still starts with the format description, keeps the events written before the process died, and includes events the source committed while the process was down, once each. There is no gap and no duplicate event at the boundary. v0.5.15 already does this for a clean stop. The published v0.5.15 package does not do it after `kill -9`: adopt then start opened a new epoch whose first bytes were the magic plus events after the offset, so the format description and the pre-crash events stayed in the old segment and `mysqlbinlog` could not replay the new file. Direct start of that backup, before adopt, is still rejected because there is no task metadata. A clean stop and resume is unchanged. `delay_seconds` of 0, an omitted sample, a dump preamble that is not lag, and the 30-second real-lag threshold are unchanged from v0.5.15. No new API. No new config key. No schema migration.

## [v0.5.15] - 2026-10-04

### Fixed

- Stop a `RUNNING` backup, let the source keep writing, then start the same task. Standalone with no `meta_dsn`, and a catalog task, both resume at the end position of the last complete event in the highest open segment. The dump does not jump to the current `SHOW MASTER STATUS`. It does not rewind to position 4 and delete that segment. A dump that started mid-file does not resume at the file size. The stopped open segment is renamed into the new epoch and appended, so one source filename spans the stop, with no gap and no duplicate event at the boundary. A partial event past that end position is dropped before the append. A catalog takeover with no complete event on disk still starts at position 4. Standalone with no checkpoint and no complete event still starts at the current master status. `KeepLocalSegments` adopt still uses the saved file size and opens a new epoch. A task already at the source tip still reports at tip. `delay_seconds` of 0, an omitted sample, a dump preamble that is not lag, and the 30-second real-lag threshold are unchanged from v0.5.14. No new API. No new config key. No schema migration.

## [v0.5.14] - 2026-10-03

### Fixed

- A caught-up `FILE_POS` resume, including standalone adopt at the highest segment size, no longer reports `DELAY_EXCEEDS_THRESHOLD` from the dump preamble. MySQL sends the binlog format description before any new event, with `log_pos` 0 or the original end position (126 on MySQL 8). That header time is when the file was created. The preamble is not written and does not move the cursor. If the resume position is already at `SHOW MASTER STATUS`, `delay_seconds` is 0 / `NORMAL` as soon as StartSync succeeds. Catch-up that is still behind the tip still uses `now - last_event_at`.
- A caught-up RUNNING task includes `delay_seconds` as JSON `0` on `GET /api/tasks/{id}/replication` and the dashboard replication object. `omitempty` on `int64` was dropping that zero, so the Console showed `--` seconds while status stayed `NORMAL`. A task with no event-time sample still omits the field. The 30-second threshold, `DELAYED`, and the dump-preamble behavior are unchanged. No new config key. No schema migration.

## [v0.5.13] - 2026-10-03

### Added

- The Console is the surface on top of `POST /api/tasks/{id}/adopt` already in v0.5.11. The published v0.5.12 package still has no Console adopt button. Standalone with no `meta_dsn`: a leftover directory is listed. In the Chinese Console its detail actions are 认领, 启动, 停止, and 删除, and there is no 编辑. A task that already has source credentials shows 编辑, 启动, 停止, and 删除, and no 认领. 认领 submits `cluster_key` and source. Leaving the start mode at the default (highest segment end) does not send an override. The success toast is 任务已认领. The task stays 已停止, shows the source and cluster key, does not show the password, and does not start replication. The existing 启动 action then starts it. Dogfood on tip `39981ead` (`v0.0.0-20261003150852-39981ead3fd5`) saw `mysql-bin.000005.open.e2` in the same directory while the earlier segment sizes stayed unchanged. With `meta_dsn`, leftover directories are not listed (the Console table is empty; ids 99 and 42 do not appear) and there is no 认领. `POST /api/tasks/99/adopt` returns 404 with body `task not found`. `backup_tasks` stays 0. Adopt does not create a task from a directory. The API is unchanged. No schema migration. No new config key.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.13`.

## [v0.5.12] - 2026-10-03

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.12`.

### Docs

- This patch only corrects the shipped guide. Behavior is unchanged from v0.5.11. English `docs/guide/admin/deployment.md` now says that adopting an id that is not a catalog task returns 404 with body `task not found`. Chinese section 7.4 describes the same adopt path: the leftover directory is listed, adopt before start, 200 and `STOPPED`, the password is not returned, an omitted start is `FILE_POS` at the highest segment name and size, start is 204 in the same directory, and with `meta_dsn` a directory that is not a catalog task is 404 `task not found`. The published v0.5.11 tarball still has the old Chinese 7.4 and the English sentence that omits 404. The Console has no adopt button.

## [v0.5.11] - 2026-10-03

### Added

- Standalone with no `meta_dsn`: `POST /api/tasks/{id}/adopt` attaches `cluster_key` and source credentials to a leftover `{data_dir}/{id}/` directory and keeps that id. The response is 200, password is omitted, and the state stays `STOPPED`. When `start` is omitted, the saved position is `FILE_POS` at the size of the highest sealed or `.open.e*` segment. An explicit `start.mode` overrides that. `POST /api/tasks/{id}/start` then returns 204 and writes the next epoch in the same directory. Existing sealed and open segments stay. Before adopt, update and start still return 400 `on-disk backup has no task metadata`. A configured `meta_dsn` still does not discover directories from disk. No new config key. No schema migration.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.11`.

## [v0.5.10] - 2026-10-03

### Fixed

- Standalone with no `meta_dsn`, after the process exits and a new process starts on the same `data_dir`: leftover `{data_dir}/{task_id}/` directories that still contain sealed or `.open.e<epoch>` segments show up in `GET /api/tasks` and the Console without a known id. The id is the directory name. `GET /api/tasks/{id}/files` uses the existing disk-scan contract (`file_name` is the source name, an open `file_path` keeps `.open.e<epoch>`, `start_pos` and `end_pos` are 0, ascending source index). Checkpoint stays 404 `checkpoint not found`. The row has no source credentials. Start returns 400 with body `on-disk backup has no task metadata` and does not start replication. A configured `meta_dsn` does not discover directories from disk. A non-empty `binlog_files` catalog still wins, and extra files on disk are not listed. No new config key. No schema migration.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.10`.

### Docs

- `docs/guide/admin/configuration.md` no longer says that `GET /api/tasks/{id}/files` returns `[]` when `meta_dsn` is unset. This is a guide correction, not a behavior change. The published v0.5.9 tarball still has that sentence.

## [v0.5.9] - 2026-10-03

### Fixed

- Standalone with no `meta_dsn`, or a meta catalog with no `binlog_files` rows for that task: `GET /api/tasks/{id}/files` scans `{data_dir}/{task_id}/` and returns sealed files and `.open.e<epoch>` segments. `file_name` is the source file name. `file_path` is the on-disk path and includes `.open.e<epoch>` for an open segment. Order is ascending source index; the same index lists the sealed name, then open epochs. `limit` keeps the highest indexes. `start_pos` and `end_pos` are 0 because the disk scan has no offsets. Checkpoint stays 404 `checkpoint not found` when there is no checkpoint row. A non-empty `binlog_files` catalog is unchanged (`sealed_at` descending) and is not replaced by the disk scan. A store error does not fall back to disk. The Console task files table shows the on-disk name and `file_path`.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.9`.

### Docs

- The replay section says a MySQL source requires MySQL's own `mysqlbinlog`, and a MariaDB source requires `mariadb-binlog`. If `mysqlbinlog --version` prints MariaDB, do not use it against MySQL: the pipe fails at `check_constraint_checks` with error 1193 and no rows land. The files are not corrupt. The v0.5.9 archive includes `docs/guide`.

## [v0.5.8] - 2026-10-03

### Fixed

- With `api.auth.enabled=true` and a bearer token, `GET /ui/` and the Console assets load without an `Authorization` header. Settings collects the bearer token, and later API calls send `Authorization: Bearer`. `/api/*`, `/metrics`, and `/swagger/*` still require that credential. `/healthz` stays open. Config keys are unchanged.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.8`.

### Docs

- The admin guide explains replaying on-disk segments with `mysqlbinlog` or `mariadb-binlog`. The path is `{data_dir}/{task_id}/mysql-bin.NNNNNN`. A `.open.e<epoch>` name is a filename suffix; the tool reads that path as-is. Standalone without `meta_dsn` returns `[]` from the files API and 404 for checkpoint, so replay the disk. Object storage keeps only sealed successful uploads. The v0.5.7 archive omitted `docs/guide`. The v0.5.8 archive includes it, so the replay section is in the unpacked package.

## [v0.5.7] - 2026-10-02

### Fixed

- Task reads (`GetTask`, task lists, list counts, checkpoints) retry transient meta MySQL errors and recycle pooled connections, so a brief meta failover does not return 500. A missing task is still not found.
- A `flavor=mysql` probe that finds no `@@server_uuid` stays `SOURCE_IDENTITY_UNAVAILABLE` and tells the operator the source looks like MariaDB and to set `flavor=mariadb`. `flavor=mariadb` identity is unchanged.
- A heartbeats-less local process is not an offline Worker. Overview reports `worker_count=0` and `single_process=true`, `GET /api/workers` is empty, and the Console says this process pulls tasks.
- The embedded Console create-task and batch-create Flavor dropdown renders again. The hint no longer contains a raw `@@server_uuid` token, which vue-i18n@9 rejects while compiling production messages. CI compiles locale catalogs before the UI build.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.7`.
- Landing console screenshots ship as PNG assets. CI and the release workflow run `scripts/check-landing-assets.sh`.
- The landing page and production template describe the Day-1 install and first backup path.

## [v0.5.6] - 2026-10-02

### Added

- `scripts/failover-dogfood.sh` and the verify-binlog-server failover-lease skill prove dual-worker lease takeover. The lease state machine is unchanged.
- A project-local verify-binlog-server Cursor skill drives a released binary through the HTTP API and the embedded Console.

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.6`.
- `GET /api/dashboard` and `GET /api/summary` count states and sources with SQL `GROUP BY` and page rows with `LIMIT/OFFSET` instead of scanning every matching task row.
- When `api.auth.enabled=true`, `/ui/*` and `/swagger/*` use the API auth middleware. `/healthz` stays open.
- Go modules: validator `v10.30.5`.

### Fixed

- The embedded Console bundle parses again. `v0.5.5` shipped a stray `}` in the entry import, so Chrome threw `SyntaxError` and the shell never mounted. CI and the release workflow run `scripts/check-ui-bundle.sh` (`node --check --input-type=module`).

### Security

- `PRODUCTION=true` refuses to start when `--encryption-key` is empty.

## [v0.5.5] - 2026-09-21

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.5`.
- Task-observation host filters and source lookup share `SameSourceHost`; loopback spellings are one source.
- Cluster overview, worker task counters, and metrics task/owner views read the unfiltered store ownership copy.
- The console task page requires dashboard paging fields and no longer locally re-pages a legacy payload.
- Console list refresh uses the task owner/epoch copy for lease risk instead of per-row `/lease`.
- Go modules: validator `v10.30.4`, MySQL driver `v1.10.1`, migrate `v4.20.1`, `golang.org/x/time` `v0.16.0`.

### Fixed

- `GetTask` fails loud on store miss or store error instead of returning a stale in-memory ownership copy.
- Source lookup reads the store ownership copy instead of the boot-time memory list.
- Retry-upload E2E pulls MinIO from Quay so CI is not blocked on Docker Hub `minio/minio:latest`.

### Docs

- README includes Console, task detail, and Swagger screenshots.

## [v0.5.4] - 2026-09-07

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.4`.
- Standalone workers inject an in-process lease table and `worker_id=standalone`, so standalone and cluster share the same ownership door.
- Process boot and the claim tick both call `ClaimRunnableTasks` instead of a separate resume path.
- First sealed-file upload verifies the current lease, then shares `ApplySealedUpload` with retry-upload.
- Dashboard and summary load matching tasks with one filtered read (`Limit<=0` unbounded), then page in process.

### Fixed

- `FAILED` releases the lease immediately so another worker can start the task without waiting for TTL.
- Expired-lease and failed-upload lookups fail loud when the store does not implement the dedicated query, instead of silently scanning an empty result.

## [v0.5.3] - 2026-09-06

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.3`.
- Control-plane `listen_addr` that is not loopback (`:8080`, `0.0.0.0:8080`, and any non-`127.0.0.1`/`localhost`/`::1` bind) now requires `api.auth.enabled`, `protect_api`, and `protect_metrics` at `App.Run`. Loopback binds may stay unauthenticated for local demo. `PRODUCTION=true` still fail-closes independently and is not weakened.
- When `api.auth.enabled=true`, unset `protect_api` / `protect_metrics` now default to true. Explicit `false` is still honored at config load, but non-loopback listen and `PRODUCTION=true` still reject that combination. `listen_addr` default remains `:8080`.
- `GET /api/tasks` and dashboard task pages filter and page in SQL (`COUNT` + `LIMIT/OFFSET`). `GetTask` uses the primary key. Worker claim ticks query unowned `STARTING` instead of loading every task. Dashboard summary/sources still aggregate in-process delay. Public `{items,total,limit,offset}` shape is unchanged.
- OpenTelemetry Go API/SDK/trace/OTLP HTTP exporter moved from 1.45.0 to 1.46.0.

### Security

- Source passwords in `backup_tasks.source_json` are encrypted with AES-256-GCM (`enc:aes256:`) when `--encryption-key` is provided. Other source fields stay plaintext JSON. Without a key, plaintext persist is unchanged so existing deploys keep starting.

### Fixed

- Cluster workers claim `RUNNING`/`LEASE_DEGRADED`/`RETRY_BACKOFF` tasks whose lease has expired and resume dump after a successful Acquire, without going through `StopTask`.
- A newly started cluster worker no longer Stop+Starts another worker's live `RUNNING` task, which previously persisted `STOPPED` while dump continued on the original owner.
- FILE_POS/GTID catch-up no longer sticks `atTip` (and therefore `delay_seconds=0`) after a quiet 2s idle gap. Tip is confirmed against `SHOW MASTER STATUS` / `SHOW BINLOG STATUS`. Fresh LATEST start at tip is unchanged.
- Release tarballs include `config.example.yaml` and `config.production.example.yaml`, matching the documented production start path.

## [v0.5.2] - 2026-08-30

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.2`.

### Fixed

- LATEST start at the source tip no longer reports `DELAYED` from a days-old binlog event header. `delay_seconds` is 0 / `NORMAL` as soon as StartSync succeeds; FILE_POS/GTID catch-up that is still behind the tip keeps real lag. `last_event_at` may still show the last event header time.
- Task list and dashboard pages sort numeric string ids as integers, so page 1 is `1,2,3` rather than `1,10,100`.

## [v0.5.1] - 2026-08-30

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.1`. They still showed `v0.4.3` after the `v0.5.0` tag.

### Notes

- Runtime behavior is unchanged from `v0.5.0`. Isolated `make e2e-scale` (1000 control-plane tasks / 100 live streams) and production-template bearer auth were recorded locally; one MySQL fixture supplied the dump clients, so this is not independent-cluster capacity.

## [v0.5.0] - 2026-08-30

### Added

- `POST /api/tasks/batch` accepts 1..100 task-create requests and returns ordered per-item success or structured error results, so a valid batch can report partial success without stopping later items.
- Task and dashboard queries support bounded `limit`/`offset` paging and `state` filtering. Dashboard summary and source aggregates cover the complete filtered result, while task details are paged; the frontend uses server paging.
- Production deployments can start from `config.production.example.yaml`, which enables bearer authentication and protects both `/api/*` and `/metrics`; unresolved credential placeholders are rejected. An opt-in `make e2e-scale` harness writes a JSON evidence report for its isolated 1000-control-task / 100-live-stream scenario.

### Changed

- **API contract change:** `GET /api/tasks` now returns `{items,total,limit,offset}` instead of a raw JSON array `[...]`. Upgrade API clients to read `items` and use the returned page metadata before deploying `v0.5.0`.
- Summaries report `STARTING` separately from `RUNNING`.

### Fixed

- Unreachable replication sources now fail after bounded retries instead of retrying indefinitely; disconnected source streams follow the bounded retry path.

## [v0.4.3] - 2026-08-30

### Fixed

- Metadata/source isolation now treats `localhost`, IPv4 loopback literals in `127/8`, and IPv6 loopback literals such as `::1` as one same-port endpoint identity without DNS resolution; create, update, and start reject aliases, and source lookup returns the same matches.

## [v0.4.2] - 2026-08-21

### Fixed

- Create task now validates the full spec before persist. HTTP 400 no longer leaves a `LATEST` task behind.
- `flavor=mariadb` probes `server_id` + `gtid_domain_id` instead of MySQL-only `@@server_uuid`. `log_bin=off` fails as `SOURCE_LOG_BIN_OFF`.
- Access denied (ERROR 1045) is `SOURCE_ACCESS_DENIED` and enters `FAILED` instead of infinite retry. `POST /start` can restart a `FAILED` task after the operator fixes credentials.
- Silent masters no longer report multi-hour `DELAYED` lag: an idle dump wait treats lag as 0 / `NORMAL`. Heartbeat events are not written into backup files.
- Bind failures no longer dump cobra `Usage`.
- `GET /api/health` returns `{"status":"ok"}` (keep `/healthz`).

### Changed

- Quick Start is download/verify/extract/`./binlog-server`. `go run` moved to Development.
- Standalone without `meta_dsn` is documented as in-memory control plane. Restartable tasks need `meta_dsn`.

## [v0.1.2] - 2026-03-27

### Added

- Frontend development mock mode for Vite dev with shared scenario assets reused by Playwright E2E.
- New frontend mock scenarios for cluster/lease resilience coverage, including control-plane-down worker-running views.
- Workspace-C planning docs for ops console IA redesign in `docs/develop/plans/2026-03-27-ops-console-workspace-c-*.md`.

### Changed

- Reorganized the ops console into workspace-oriented views (`overview`, `tasks`, `sources`, `workers`, `alerts`) with deep-link routing semantics.
- Moved task filters to task/alert context and source lookup to source context to reduce cross-page cognitive load.
- Updated left navigation to full-height docked behavior with bottom-docked collapse control and compact icon alignment in collapsed mode.
- Normalized KPI navigation behavior so task-oriented metrics consistently route to task-focused workflows.
- Adjusted lease risk evaluation to use dashboard reference time in mock-driven views, avoiding false-risk inflation from local clock skew.

## [v0.1.1] - 2026-03-25

### Added

- Playwright-based frontend E2E acceptance coverage for empty-state, KPI filtering, task drawer, retry-upload, and auth-required flows.

### Changed

- Reworked the embedded web UI into a more operator-focused console with alert-first KPI hierarchy, reduced row-action noise, and a stronger detail drawer workflow.
- Localized frontend auth guidance and added in-app settings-driven recovery for `401` API responses.
- Added retry-upload affordance in the task detail drawer and refreshed embedded static UI assets.

## [v0.1.0] - 2026-03-09

### Added

- Root `SECURITY.md` security policy for vulnerability reporting.
- Root `.golangci.yml` baseline lint configuration.
- CI vulnerability scanning with `govulncheck`.

### Changed

- Foundation hardening work completed across auth, timeout governance, retry standardization, SQL access generation, API validation, Prometheus metrics, and OpenTelemetry tracing.

## [2026-03-08]

### Added

- Closure snapshot for the foundation hardening program in `docs/develop/plans/2026-03-08-foundation-hardening-closure.md`.
