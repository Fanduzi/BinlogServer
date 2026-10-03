# Binlog Server v0.5.9

Release date: 2026-10-03

Binlog Server `v0.5.9` is an operator patch on `v0.5.8` for listing on-disk binlog segments when the file catalog is empty, and for matching the replay client to the source.

## Highlights

- Standalone with no `meta_dsn`, or a meta catalog with no `binlog_files` rows for that task: `GET /api/tasks/{id}/files` scans `{data_dir}/{task_id}/` and returns sealed files and `.open.e<epoch>` segments. `file_name` is the source file name. `file_path` is the on-disk path and includes `.open.e<epoch>` for an open segment. Order is ascending source index; the same index lists the sealed name, then open epochs. `limit` keeps the highest indexes. `start_pos` and `end_pos` are 0 because the disk scan has no offsets. Checkpoint stays 404 `checkpoint not found` when there is no checkpoint row. A non-empty `binlog_files` catalog is unchanged (`sealed_at` descending) and is not replaced by the disk scan. A store error does not fall back to disk. The Console task files table shows the on-disk name and `file_path`. One response was HTTP 200 with `mysql-bin.000003` and `mysql-bin.000004.open.e1`, positions 0, checkpoint 404. After process restart the task id is 404 `task not found` and the directory remains. With meta and catalog rows, positions came from the catalog (sealed `end_pos` 845, open `start_pos` 4 `end_pos` 488) and an extra on-disk `mysql-bin.000099` was not listed.
- The replay client must match the source. A MySQL source needs official MySQL `mysqlbinlog`. A MariaDB source needs `mariadb-binlog`. `mysqlbinlog --version` that prints MariaDB (Debian `/usr/bin/mysqlbinlog` is often MariaDB, for example 11.8.6) exits 0 and injects `SET @@session.check_constraint_checks=1`, which is not in the file. MySQL 8.0 returns `ERROR 1193` and no rows land. The files are not corrupt. Use official `mysqlbinlog` (for example `Ver 8.0.46`). The guide already says this. The v0.5.9 archive includes `docs/guide`.
- Quick Start, landing, deployment, and config-template download pins point at `v0.5.9`.

## Upgrade Notes

- No schema migration is required for `v0.5.9`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`.
- Standalone with no `meta_dsn`, or a task with no `binlog_files` rows, lists sealed files and `.open.e<epoch>` segments from `GET /api/tasks/{id}/files`. `file_name` is the source file name. `file_path` is the on-disk path. `start_pos` and `end_pos` are 0. A non-empty catalog stays `sealed_at` descending and is not replaced by the disk scan. A store error does not fall back to disk. Checkpoint stays 404 `checkpoint not found` when there is no checkpoint row. After a standalone process restart the task id is 404 `task not found` and the directory remains.
- Replay a MySQL source with official MySQL `mysqlbinlog`. Replay a MariaDB source with `mariadb-binlog`. A `mysqlbinlog --version` that prints MariaDB exits 0 and injects `SET @@session.check_constraint_checks=1`. MySQL 8.0 returns `ERROR 1193` and no rows land. The files are not corrupt. The v0.5.9 archive includes `docs/guide`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.9.zh-CN.md
