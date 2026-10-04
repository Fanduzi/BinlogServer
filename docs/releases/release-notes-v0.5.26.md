# Binlog Server v0.5.26

Release date: 2026-10-04

Binlog Server `v0.5.26` retries sealed `UPLOAD_FAILED` segments in the background when this process is a worker and object storage is configured, on top of `v0.5.25`. The published v0.5.25 package leaves those rows at `UPLOAD_FAILED` until `POST /api/tasks/{id}/files/retry-upload`.

## Highlights

- A worker that has object storage configured retries sealed `UPLOAD_FAILED` segments in the background. Standalone and all-in-one processes are workers, so they run this loop. A cluster worker runs it too. A control-plane-only process does not. A process with no object storage configured does not run this loop.
- The loop uses the same path as the existing manual retry. A successful row becomes `UPLOADED`, including checksum, the same way a manual retry does.
- Open segments stay unuploaded. A sealed file that is not on this machine is skipped, and its catalog row is left unchanged.
- Upload failure still does not stop binlog pull and does not change task state.
- `POST /api/tasks/{id}/files/retry-upload` and `RetryFailedUploads(taskID string, limit int)` stay as they are. While a task's background pass is running, a manual retry of that task still returns the existing in-progress error, `upload retry already in progress`.
- The default cadence is every 15 seconds, up to 100 sealed `UPLOAD_FAILED` rows per task per pass.

## Upgrade Notes

- No schema migration is required for `v0.5.26`. Migrations are still `000001_init_schema` only.
- No new config key. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- Retention, lease takeover, and the v0.5.25 object-read behavior are unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.26.zh-CN.md
