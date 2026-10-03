# Binlog Server v0.5.17

Release date: 2026-10-04

Binlog Server `v0.5.17` is an operator patch on `v0.5.16`. With `meta_dsn`, `GET /api/tasks/{id}/files` and the Console files table list catalog rows in the same order as the standalone disk scan. The published v0.5.16 package still lists those catalog rows by `sealed_at` descending.

## Highlights

- With `meta_dsn`, `GET /api/tasks/{id}/files` and the Console files table list catalog rows in the same order as the standalone disk scan: ascending source index, and for one index the sealed name before `.open.e*` epochs.
- `limit` keeps the highest indexes. The no-meta disk scan is unchanged.

## Upgrade Notes

- No schema migration is required for `v0.5.17`. Migrations are still `000001_init_schema` only.
- No new API and no new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.17.zh-CN.md
