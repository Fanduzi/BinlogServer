# Binlog Server v0.5.19

Release date: 2026-10-04

Binlog Server `v0.5.19` adds one-segment download on `v0.5.18`. A DBA can download one binlog segment that already appears in `GET /api/tasks/{id}/files`, over the authenticated API and the Console, without SSH onto the backup host. The published v0.5.18 package has no download endpoint.

## Highlights

- `GET /api/tasks/{id}/files/{name}` streams the raw bytes of that on-disk basename (a sealed name or `*.open.e*`). `Content-Type` is `application/octet-stream`. `Content-Disposition` uses that basename. The body length is the file size at open. On commit `6c1b0ae` (self-built binary; version string `devel` because ldflags were unset) the downloads matched the on-disk sha256 for `mysql-bin.000003`, `mysql-bin.000002.open.e2`, and the sealed `mysql-bin.000002`. The sealed file is not the open segment.
- A slash, backslash, `..`, or other traversal form returns 400 `invalid segment name`. A missing task returns 404 `task not found`. A basename that is not under this process's task directory returns 404 `segment not found on this process`. A control plane with no local file does not invent bytes from the catalog path or object storage. An open segment is downloadable while the task is `RUNNING` or `STOPPED`. The Console files table has Download on each row.
- `GET /api/tasks/{id}/files` still lists every name. `GET /api/tasks/{id}/replay` and the Console copy command are unchanged: one path per source index, and when a sealed name and `.open.e*` share an index the path is the highest-epoch open segment and keeps the `.open.e*` suffix.

## Upgrade Notes

- No schema migration is required for `v0.5.19`. Migrations are still `000001_init_schema` only.
- `GET /api/tasks/{id}/files/{name}` is a new read API. No new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.19.zh-CN.md
