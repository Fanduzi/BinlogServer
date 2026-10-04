# Binlog Server v0.5.25

Release date: 2026-10-04

Binlog Server `v0.5.25` continues a backup from the dead worker's segment directory after the lease moves, on top of `v0.5.24`. The published v0.5.24 package moves the lease and does not continue from that directory.

## Highlights

- The lease can move to a live worker. The segment directory stays at the `binlog_files.file_path` already recorded. It does not follow the lease.
- When this worker can read that directory, takeover continues from the last complete event there. The same source file is not sealed a second time. This worker does not open a fresh `{data_dir}/{task_id}/` and pull from position 4.
- When the open segment, or a sealed segment that is not yet `UPLOADED`, is not readable on this worker, and the checkpoint is not already inside a sealed `UPLOADED` object, the task becomes `FAILED`. `last_error` starts with `SEGMENT_NOT_ON_WORKER` and names the missing path. This worker does not create a new directory and keep pulling. Mount or copy that path onto this worker, then `POST /api/tasks/{id}/start`.
- When the checkpoint is already inside a sealed `UPLOADED` object, this worker reads the object back with the upload client already configured, even when the local file is absent, and continues from the last complete event in that object.
- Same-host `kill -9`, adopt, then start still uses this worker's own directory.

## Upgrade Notes

- No schema migration is required for `v0.5.25`. Migrations are still `000001_init_schema` only.
- No new config key. `cluster.failover_policy` is still not a switch for this path. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- A checkpoint already on the next file at position 4, with every sealed row `UPLOADED`, still starts on that next file. Retention from `v0.5.24` is unchanged: delete the object, then the catalog row, then the local file. `OBJECT_PURGE_FAILED` leaves the local file. The next open continues on the next file.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.25.zh-CN.md
