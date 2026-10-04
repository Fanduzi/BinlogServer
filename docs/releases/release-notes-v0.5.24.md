# Binlog Server v0.5.24

Release date: 2026-10-04

Binlog Server `v0.5.24` removes an expired sealed segment from the bucket when local retention removes it, on top of `v0.5.23`. The published v0.5.23 package deletes that local file and leaves the object in the bucket. If the object delete fails, the task retries. A later success continues on the next binlog file.

## Highlights

- When the replication loop opens a file, a sealed segment older than `storage.retention_days` is removed from the bucket, then its catalog row is removed, then the local file is removed. Age is the local file modification time, not `sealed_at`. The row must be `UPLOADED` and must have an object key. A segment still inside the retention window stays. An open segment is not deleted, and retention does not delete an object for one.
- Without upload configured, local retention is unchanged.
- If deleting the object fails, the local file stays. `checksum` is left as it was: `match`, `mismatch`, or empty. Empty is not `match` and not `mismatch`. The row stays `UPLOADED`. The task retries with `last_error` beginning `OBJECT_PURGE_FAILED`. An object that is already gone (HTTP 204 or 404) counts as success, so that retry can finish.
- After a segment is sealed, the checkpoint moves to the next file before retention runs. If opening that next file then fails, the retry continues on the next file. It does not try to seal the file that was just sealed, so it does not stop on `sealed file already exists`.

## Upgrade Notes

- No schema migration is required for `v0.5.24`. Migrations are still `000001_init_schema` only.
- No new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- A task that is already stopped on `sealed file already exists`, with its checkpoint still on that sealed file, is not repaired by this release. Move that sealed file aside once so the next retry can continue.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.24.zh-CN.md
