# Binlog Server v0.5.21

Release date: 2026-10-04

Binlog Server `v0.5.21` downloads the replay window from `v0.5.20` as one tar. A DBA can fetch the same selection `GET /api/tasks/{id}/replay` already returns, including a sealed segment that is only `UPLOADED` and no longer on this process, over the same authenticated API and the Console. The published v0.5.20 package has no replay archive. Single-segment download, the files list, and the JSON replay command are unchanged.

## Highlights

- `GET /api/tasks/{id}/replay/archive` uses the same `limit` and the same replay-file window as `GET /api/tasks/{id}/replay`. Each tar member is that window's basename, in that window's order, not a host path. One source index is still one member: when a sealed name and `.open.e*` share an index, the member is the highest-epoch open segment and keeps the `.open.e*` suffix. Bytes follow the existing single-segment download. The local file wins, including an open segment while the task is `RUNNING` or `STOPPED`. A missing sealed file is read from object storage only when its catalog row is `UPLOADED` and `object_key` is non-empty. `Content-Type` is `application/x-tar`. `Content-Disposition` is `attachment` with filename `task-{id}-replay.tar`.
- An empty window is HTTP 200 and an empty ustar (1024 zero bytes). A missing task is 404 `task not found`. Every selected segment is opened before the tar is written. If one cannot be opened or read, the response is an error and is not a partial archive. `LOCAL_ONLY`, `UPLOAD_FAILED`, an empty `object_key`, an open segment with no local file, and a process with no object store configured stay 404 `segment not found on this process`.
- Console task detail has 下载回放集 next to the existing copy-replay action. That download uses the same `limit=80` as the task drawer. `GET /api/tasks/{id}/files`, `GET /api/tasks/{id}/files/{name}`, and `GET /api/tasks/{id}/replay` are unchanged.

## Upgrade Notes

- No schema migration is required for `v0.5.21`. Migrations are still `000001_init_schema` only.
- `GET /api/tasks/{id}/replay/archive` is a new read API. No new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.21.zh-CN.md
