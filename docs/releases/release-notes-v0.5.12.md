# Binlog Server v0.5.12

Release date: 2026-10-03

This patch only corrects the shipped guide. Behavior is unchanged from `v0.5.11`. The published v0.5.11 tarball still has the old Chinese 7.4 and the English sentence that omits 404. There is no schema migration, no new config key, and the Console still cannot adopt.

## Highlights

- English `docs/guide/admin/deployment.md` section 8.4 already described `POST /api/tasks/{id}/adopt`, 200, password omitted, `STOPPED`, adopt does not start replication, the 400 before adopt, `FILE_POS` at the highest sealed or `.open.e*` segment, and start 204. It now also says that adopting an id that is not a catalog task returns 404 with body `task not found`. Dogfood on the published v0.5.11 linux amd64 package: `POST /api/tasks/99/adopt` was 404 `task not found` and `backup_tasks` stayed 0.
- Chinese section 7.4 now describes the same adopt path. The leftover directory is listed. Adopt comes before start. Success is 200 and `STOPPED`. The password is not returned. An omitted start is `FILE_POS` at the highest segment name and size. Start is 204 in the same directory. With `meta_dsn`, a directory that is not a catalog task is 404 `task not found`. The old sentence that the row has no source account and cannot be started is no longer the only guidance.
- No schema migration. No new config key. The Console has no adopt button. Behavior is unchanged from `v0.5.11`.
- Quick Start, landing, deployment, config-template, and workers API example pins point at `v0.5.12`.

## Upgrade Notes

- No schema migration is required for `v0.5.12`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`.
- This patch only corrects the shipped guide. Behavior is unchanged from `v0.5.11`. The published v0.5.11 tarball still has the old Chinese 7.4 and the English sentence that omits 404.
- The Console has no adopt button.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.12.zh-CN.md
