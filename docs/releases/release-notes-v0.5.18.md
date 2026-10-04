# Binlog Server v0.5.18

Release date: 2026-10-04

Binlog Server `v0.5.18` adds the replay set on `v0.5.17`. `GET /api/tasks/{id}/replay` returns one on-disk path per source index, inside the same inventory window as `GET /api/tasks/{id}/files`. The published v0.5.17 package has no replay endpoint.

## Highlights

- `GET /api/tasks/{id}/replay` returns one on-disk path per source index, inside the same inventory window as `GET /api/tasks/{id}/files`. Paths are ascending. When a sealed name and one or more `.open.e*` files share an index, the path is the highest-epoch open segment and keeps the `.open.e*` suffix. A small `limit` keeps the highest indexes, not the newest `sealed_at`.
- `source.flavor` `mysql` sets `client` to `mysqlbinlog` and `client_hint` to `MySQL mysqlbinlog`. `mariadb` sets both to `mariadb-binlog`. An empty flavor leaves the client empty. An empty directory returns `paths: []`.
- The files API is unchanged and still lists every name. The Console task detail shows that command and copies it. Stop, resume, adopt, and kill-9 resume are unchanged.

## Upgrade Notes

- No schema migration is required for `v0.5.18`. Migrations are still `000001_init_schema` only.
- `GET /api/tasks/{id}/replay` is a new read API. No new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.18.zh-CN.md
