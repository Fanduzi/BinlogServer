# Binlog Server v0.5.28

Release date: 2026-10-05

On Binlog Server `v0.5.28`, with object storage and a catalog (`meta_dsn`), age retention no longer deletes the only copy of an expired sealed binlog that has not been uploaded, on top of `v0.5.27`. The published v0.5.27 package still deletes that local file once it is older than `storage.retention_days`.

## Highlights

- With object storage and a catalog (`meta_dsn`), age retention no longer deletes an expired sealed binlog whose `upload_state` is `UPLOAD_FAILED` or `LOCAL_ONLY`. The local file and the catalog row stay. Replication keeps running. It does not set `last_error` and does not enter `RETRY_BACKOFF`. Age is still the local file modification time, compared with `storage.retention_days`.
- `GET /api/tasks/{id}/events` gets one `RETENTION_SKIPPED_NOT_UPLOADED` per kept file. The message names the file and its `upload_state`, for example `mysql-bin.000003 upload_state=LOCAL_ONLY`. Opening another binlog does not append that event again for the same file while this process stays up. After a restart, the next retention pass that still keeps the file appends that event once more.
- The process that runs retention exposes `binlog_server_retention_blocked_files{task_id}` on `GET /metrics`: how many of those expired files the latest successful pass is still keeping. The gauge drops when a later pass deletes them. After a restart it stays 0 until the next retention pass. Retention still runs when the replication loop opens a binlog file. A control-plane-only process does not run retention, so the gauge stays 0 there. The event remains on `GET /api/tasks/{id}/events`.
- Once the row is `UPLOADED`, the next retention pass deletes the object, then the catalog row, then the local file. A sealed `UPLOAD_FAILED` row is still picked up by the background upload retry, and by the existing manual retry `POST /api/tasks/{id}/files/retry-upload`. Either path can make that row `UPLOADED`. Already-`UPLOADED` rows, `OBJECT_PURGE_FAILED`, and local retention when upload is not configured stay as they are.
- `LOCAL_ONLY` is not picked up by the background upload retry. The manual retry selects only sealed `UPLOAD_FAILED` rows, so it does not upload a `LOCAL_ONLY` row. A sealed file that never had object storage configured stays on disk past retention, and the gauge keeps counting it. Configuring object storage later does not upload that row.
- Standalone without `meta_dsn` still deletes the local sealed file by age, including one that never reached the bucket. Do not use that mode as the only copy when the bucket can be down longer than `storage.retention_days`.

## Upgrade Notes

- No schema migration is required for `v0.5.28`. Migrations are still `000001_init_schema` only.
- No new config key. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.28.zh-CN.md
