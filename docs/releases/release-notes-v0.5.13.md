# Binlog Server v0.5.13

Release date: 2026-10-03

Binlog Server `v0.5.13` is the Console surface on top of the adopt API already in `v0.5.11`. `v0.5.11` already accepts `POST /api/tasks/{id}/adopt`. This release submits that same request from the Console. The published v0.5.12 package still has no Console adopt button. The API is unchanged. There is no schema migration and no new config key.

## Highlights

- Standalone with no `meta_dsn`: a leftover directory is listed. In the Chinese Console, that row's detail actions are 认领, 启动, 停止, and 删除. There is no 编辑. A task that already has source credentials shows 编辑, 启动, 停止, and 删除, and there is no 认领. 认领 submits `cluster_key` and source through the existing `POST /api/tasks/{id}/adopt`. Leaving the start mode at the default (highest segment end) does not send an override. The success toast is 任务已认领. The task stays 已停止, shows the source and cluster key, does not show the password, and does not start replication. The existing 启动 action then starts it. Dogfood on tip `39981ead` (version string `v0.0.0-20261003150852-39981ead3fd5`, not the published v0.5.12 package) saw the next epoch in the same directory, `mysql-bin.000005.open.e2`, while the earlier segment sizes stayed unchanged.
- With `meta_dsn` configured, leftover directories are not listed. The Console table is empty. Ids `99` and `42` do not appear, and there is no 认领. `POST /api/tasks/99/adopt` returns 404 with body `task not found`. `backup_tasks` stays 0. Adopt does not create a task from a directory.
- The API is unchanged. No schema migration. No new config key. Adopt in this release is the Console surface on top of the API already in `v0.5.11`. The published v0.5.12 package still has no Console adopt button.
- Quick Start, landing, deployment, config-template, and workers API example pins point at `v0.5.13`.

## Upgrade Notes

- No schema migration is required for `v0.5.13`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The API is unchanged from `v0.5.11`.
- Standalone with no `meta_dsn`: a leftover directory is listed. In the Chinese Console, that row's detail actions are 认领, 启动, 停止, and 删除, and there is no 编辑. A task that already has source credentials shows 编辑, 启动, 停止, and 删除, and there is no 认领. 认领 submits `cluster_key` and source through the existing `POST /api/tasks/{id}/adopt`. Leaving the start mode at the default (highest segment end) does not send an override. The success toast is 任务已认领. The task stays 已停止, shows the source and cluster key, does not show the password, and does not start replication. The existing 启动 action then starts it. Dogfood on tip `39981ead` (version string `v0.0.0-20261003150852-39981ead3fd5`, not the published v0.5.12 package) saw `mysql-bin.000005.open.e2` in the same directory while the earlier segment sizes stayed unchanged.
- With `meta_dsn` configured, leftover directories are not listed. The Console table is empty. Ids `99` and `42` do not appear, and there is no 认领. `POST /api/tasks/99/adopt` returns 404 with body `task not found`. `backup_tasks` stays 0. Adopt does not create a task from a directory.
- Adopt in this release is the Console surface on top of the API already in `v0.5.11`. The published v0.5.12 package still has no Console adopt button.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.13.zh-CN.md
