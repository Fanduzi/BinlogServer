# Binlog Server v0.5.10

Release date: 2026-10-03

Binlog Server `v0.5.10` is an operator patch on `v0.5.9`. Standalone with no `meta_dsn` shows leftover binlog directories after a restart on the same `data_dir`.

## Highlights

- Standalone with no `meta_dsn`, after the process exits and a new process starts on the same `data_dir`: leftover `{data_dir}/{task_id}/` directories that still contain sealed or `.open.e<epoch>` segments show up in `GET /api/tasks` and the Console without a known id. The id is the directory name. `GET /api/tasks/{id}/files` uses the existing disk-scan contract. `file_name` is the source name. An open `file_path` keeps `.open.e<epoch>`. `start_pos` and `end_pos` are 0. Order is ascending source index. Checkpoint stays 404 `checkpoint not found`. The row has no source credentials. Start returns 400 with body `on-disk backup has no task metadata` and does not start replication. A configured `meta_dsn` does not discover directories from disk. A non-empty `binlog_files` catalog still wins, and extra files on disk are not listed. No new config key. No schema migration. Dogfood of `3703c98` used a self-built binary, not the published v0.5.9 package. With no meta, the files were `mysql-bin.000004` and `mysql-bin.000005.open.e1`. `GET /api/tasks` listed id `1`, positions 0, checkpoint 404, start 400. With meta, extra directory `99` was not listed. Catalog positions were `mysql-bin.000005` start 478 end 806 and `mysql-bin.000006` start 4 end 479. On-disk `mysql-bin.000099` was not listed.
- Guide correction: `docs/guide/admin/configuration.md` no longer says that `GET /api/tasks/{id}/files` returns `[]` when `meta_dsn` is unset. The published v0.5.9 tarball still has that sentence. This is a guide correction, not a behavior change.
- Quick Start, landing, deployment, config-template, and workers API example pins point at `v0.5.10`.

## Upgrade Notes

- No schema migration is required for `v0.5.10`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`.
- Standalone with no `meta_dsn`, after the process exits and a new process starts on the same `data_dir`, leftover `{data_dir}/{task_id}/` directories that still contain sealed or `.open.e<epoch>` segments show up in `GET /api/tasks` and the Console without a known id. `GET /api/tasks/{id}/files` uses the existing disk-scan contract (`file_name` is the source name, an open `file_path` keeps `.open.e<epoch>`, `start_pos` and `end_pos` are 0, ascending source index). Checkpoint stays 404 `checkpoint not found`. The row has no source credentials. Start returns 400 with body `on-disk backup has no task metadata` and does not start replication. A configured `meta_dsn` does not discover directories from disk. A non-empty `binlog_files` catalog still wins, and extra files on disk are not listed.
- `configuration.md` no longer says that `GET /api/tasks/{id}/files` returns `[]` when `meta_dsn` is unset. The published v0.5.9 tarball still has that sentence. This is a guide correction, not a behavior change.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.10.zh-CN.md
