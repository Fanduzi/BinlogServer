# Binlog Server v0.5.32

Release date: 2026-10-05

On Binlog Server `v0.5.32`, a sealed binlog is a durable uploaded copy only when the object matches the local file. A stored object that does not match those bytes, or a comparison that does not finish, is `UPLOAD_FAILED`. With object storage and a catalog, retention then keeps the local file and the catalog row. Up to `v0.5.31`, that row could stay `UPLOADED`, and retention could delete the local copy. The published v0.5.31 package still does that. Replication keeps running. No new state. No new config key. No schema migration.

## Highlights

- A sealed upload whose stored object does not match the local bytes is `UPLOAD_FAILED`. `upload_error` is `checksum mismatch`. `checksum` stays `mismatch`. Empty is not `match` and not `mismatch`.
- A comparison that could not finish is `UPLOAD_FAILED` with an empty `checksum`. That is an object HEAD error. `upload_error` begins with `checksum verify failed:`.
- With object storage and a catalog, retention does not delete that local file or its catalog row. `GET /api/tasks/{id}/events` gets one `RETENTION_SKIPPED_NOT_UPLOADED` event. Replication keeps running.
- The background upload retry and `POST /api/tasks/{id}/files/retry-upload` upload a checksum mismatch again. An unfinished comparison is checked again and is not uploaded again. When that later check matches, the row becomes `UPLOADED` with checksum `match`, and a later retention pass can delete it. If that later check finds different bytes, `checksum` becomes `mismatch` and a following retry uploads the file.
- Only an `UPLOADED` row whose checksum is `match` lets retention delete the local file. A checksum of `match` is otherwise unchanged: retention still deletes the object, the catalog row, and the local file, including the longer bucket window that deletes only the local file. PITR, download, and replay still read a sealed `UPLOADED` object when the local file is already gone. `LOCAL_ONLY` retention is unchanged. A bucket-only `UPLOADED` row, with the local file already gone, is still aged as before.
- An `UPLOADED` row already stored with checksum `mismatch` or an empty checksum, whose local file is still on disk, is kept. The next retention pass records it as `UPLOAD_FAILED`, so that same retry can upload a mismatch again or check an unfinished comparison again.
- A checksum is compared once, after the upload. An object that already matched, and is then changed in the bucket, is not compared again. Retention still treats checksum `match` as the copy whose local file may be deleted.
- No new upload state. No new config key. No schema migration. The Console already shows `upload_state` and `checksum`, and the existing retry button appears for `UPLOAD_FAILED`.

## Upgrade Notes

- No schema migration is required for `v0.5.32`. Migrations are still `000001_init_schema` only.
- No new state. No new config key. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- On `v0.5.31` and earlier, retention could delete the local copy of a sealed file whose object did not match the local bytes, or whose checksum was never verified. Check `GET /api/tasks/{id}/files` for `UPLOADED` rows whose `checksum` is `mismatch` or empty. If that local file is still on disk, the next retention pass on `v0.5.32` keeps it and records the row as `UPLOAD_FAILED`. If the local file is already gone, this release does not restore it. That object is still aged as before, and PITR, download, and replay still read it while the row stays `UPLOADED`.
- A checksum is compared once after upload. An object changed in the bucket after it matched is not re-checked.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.32.zh-CN.md
