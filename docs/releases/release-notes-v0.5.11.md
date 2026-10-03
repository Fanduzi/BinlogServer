# Binlog Server v0.5.11

Release date: 2026-10-03

Binlog Server `v0.5.11` is the adopt-and-resume step on `v0.5.10`. `v0.5.10` already lists leftover binlog directories. This release attaches `cluster_key` and source to one of those directories so a later start can open the next epoch in the same directory. There is no schema migration, no new config key, and the Console still cannot adopt.

## Highlights

- Standalone with no `meta_dsn`: `POST /api/tasks/{id}/adopt` attaches `cluster_key` and source to a leftover `{data_dir}/{id}/` directory already listed by `GET /api/tasks`. The id stays the directory name. Success is 200. The password is omitted. State stays `STOPPED`. Adopt does not start replication. Before adopt, `PUT /api/tasks/{id}` and `POST /api/tasks/{id}/start` still return 400 with body `on-disk backup has no task metadata` and do not start replication. If `start` is omitted, the saved position is `FILE_POS` at the source name and byte size of the highest sealed or `.open.e*` segment. An explicit `start.mode` overrides that. Dogfood on tip `9b4ded4`: `mysql-bin.000007` pos 477, equal to the size of that `.open.e1`. `POST /api/tasks/{id}/start` then returned 204, the task reached `STARTING`, epoch 2, and `.open.e2` was written in the same directory. Existing segments stayed.
- With `meta_dsn` configured, directories are not discovered from disk. Adopting a directory that is not a catalog task returns 404 `task not found`. Dogfood: directories `99` and `42`. Adopt does not create a task from disk in that mode.
- No schema migration. No new config key. The Console has no adopt button. Edit still uses `PUT` and stays blocked until adopt.
- Quick Start, landing, deployment, config-template, and workers API example pins point at `v0.5.11`.

## Upgrade Notes

- No schema migration is required for `v0.5.11`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`.
- Standalone with no `meta_dsn`: `POST /api/tasks/{id}/adopt` attaches `cluster_key` and source to a leftover directory already listed by `GET /api/tasks`. Success is 200, the password is omitted, state stays `STOPPED`, and adopt does not start replication. Before adopt, `PUT` and `POST` start still return 400 with body `on-disk backup has no task metadata` and do not start replication. If `start` is omitted, the saved position is `FILE_POS` at the source name and byte size of the highest sealed or `.open.e*` segment. An explicit `start.mode` overrides that. Dogfood on tip `9b4ded4`: `mysql-bin.000007` pos 477, equal to the size of that `.open.e1`. `POST` start then returned 204, the task reached `STARTING`, epoch 2, and `.open.e2` was written in the same directory. Existing segments stayed.
- With `meta_dsn` configured, directories are not discovered from disk. Adopting a directory that is not a catalog task returns 404 `task not found` (dogfood: directories `99` and `42`). Adopt does not create a task from disk in that mode.
- The Console has no adopt button. Edit still uses `PUT` and stays blocked until adopt. `v0.5.10` already shipped leftover-directory listing. This release is the adopt-and-resume step.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.11.zh-CN.md
