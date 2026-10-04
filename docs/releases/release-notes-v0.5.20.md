# Binlog Server v0.5.20

Release date: 2026-10-04

Binlog Server `v0.5.20` extends one-segment download on `v0.5.19`. When `{data_dir}/{id}/{name}` is not on this process, a DBA can still download a sealed segment that is already `UPLOADED`, over the same authenticated API and Console Download. The published v0.5.19 package returns 404 `segment not found on this process` for that missing local file and does not read the object store.

## Highlights

- `GET /api/tasks/{id}/files/{name}` still opens the local file under `{data_dir}/{id}/{name}` first. That includes an open segment named `*.open.e*` while the task is `RUNNING` or `STOPPED`. When that local file is missing, a sealed catalog row whose `upload_state` is `UPLOADED` and whose `object_key` is non-empty is streamed from the object store already configured for upload. `{name}` is that basename, not the object key. `Content-Type` is `application/octet-stream`. `Content-Disposition` uses that basename. For the object, the body length is the object size at open. A local file's body length is still the file size at open. A local file still wins over the object. Console Download uses this same route, so the browser never opens the bucket.
- A `LOCAL_ONLY` row, an `UPLOAD_FAILED` row, an empty `object_key`, an open segment that has no local file, and a process with no object store configured stay 404 `segment not found on this process`. A name containing `/`, `\`, or `..` stays 400 `invalid segment name`. A missing task stays 404 `task not found`. The response does not follow catalog `file_path` and does not invent open-segment bytes from object storage.
- `GET /api/tasks/{id}/files` and `GET /api/tasks/{id}/replay` are unchanged. The files list still lists every name. Replay is still one path per source index, and when a sealed name and `.open.e*` share an index the path is the highest-epoch open segment and keeps the `.open.e*` suffix.

## Upgrade Notes

- No schema migration is required for `v0.5.20`. Migrations are still `000001_init_schema` only.
- No new config key. The download route is the same read API as `v0.5.19`. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.20.zh-CN.md
