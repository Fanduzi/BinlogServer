# Binlog Server v0.5.23

Release date: 2026-10-04

Binlog Server `v0.5.23` compares a sealed upload with the object in the bucket, on top of `v0.5.22`. After a sealed segment is uploaded, the server HEADs that object once and compares its ETag with the sealed file. The published v0.5.22 package does not make that comparison, and its Console file table has no 校验 column.

## Highlights

- After a sealed segment is uploaded, the server HEADs the object once and compares its ETag with the sealed file. At or under 16MiB that ETag is the whole-file MD5. Above that it is the same 16MiB multipart ETag as this upload, up to about 160GiB. There is no periodic re-check. An open segment is not checksummed.
- The result is stored in the existing `binlog_files.checksum` column as `match` or `mismatch`. `GET /api/tasks/{id}/files` returns that value. The Console file table shows it in 校验. `match` means this ETag comparison found the same bytes. `mismatch` means the comparison finished and the bytes differ.
- `mismatch` stays `UPLOADED`. It is not treated as verified, and it does not stop replication.
- A failed upload stays `UPLOAD_FAILED` and the checksum is cleared.
- If the object HEAD fails, checksum stays empty on an `UPLOADED` row. Empty is not `match` and not `mismatch`. `GET /api/tasks/{id}/files` omits `checksum` when it is empty. The Console shows that cell as `--`. An uploader that cannot read the object also leaves checksum empty. Empty is not verified. A blank 校验 cell is neither a pass nor a fail. An open segment, a cleared checksum, and an empty checksum all show that same `--`.

## Upgrade Notes

- No schema migration is required for `v0.5.23`. Migrations are still `000001_init_schema` only. `binlog_files.checksum` is the existing column.
- No new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.23.zh-CN.md
