# Binlog Server v0.5.8

Release date: 2026-10-03

Binlog Server `v0.5.8` is an operator patch on `v0.5.7` for opening the embedded Console in a browser when bearer auth is on, and for replaying on-disk segments from the admin guide.

## Highlights

- With `api.auth.enabled` and a bearer token, a browser can open `/ui/` with no `Authorization` header. The Console loads. Settings asks for the bearer token, and later API calls send `Authorization: Bearer`. `/ui/*` static files are anonymous. `/api/*`, `/metrics`, and `/swagger/*` still return 401 without the credential. `/healthz` stays 200. The Settings field is a bearer token. Config keys are unchanged.
- The admin guide explains replaying on-disk segments with `mysqlbinlog` or `mariadb-binlog`. The path is `{data_dir}/{task_id}/mysql-bin.NNNNNN`. A `.open.e<epoch>` name is a filename suffix on a normal binlog; the tool reads that path as-is, with no rename. Standalone without `meta_dsn`: `GET /api/tasks/{id}/files` is `[]` and checkpoint is 404, so replay the disk. Object storage has only sealed successful uploads. The v0.5.7 archive omitted `docs/guide`. The v0.5.8 archive includes it, so the replay section is in the unpacked package.
- Quick Start, landing, deployment, and config-template download pins point at `v0.5.8`.

## Upgrade Notes

- No schema migration is required for `v0.5.8`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`.
- With bearer auth enabled, `/ui/` and its assets load without a credential. Paste the bearer token in Console Settings. `/api/*`, `/metrics`, and `/swagger/*` still return 401 without it. `/healthz` stays 200. Swagger still has no token field.
- When the source is gone, replay the files under `{data_dir}/{task_id}/`. Standalone without `meta_dsn` has no file index and no checkpoint row. The open segment is not in object storage.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.8.zh-CN.md
