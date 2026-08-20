# Binlog Server v0.4.2

Binlog Server `v0.4.2` is a patch release for operators who tried `v0.4.1` in the field. It fixes MariaDB startup, create-task validation that persisted a `LATEST` task after HTTP 400, unfriendly failure paths, and the first-mile install docs.

## Highlights

- `POST /api/tasks` now validates the full spec before persist. HTTP 400 returns JSON `{"error","code"}` and does not leave a `LATEST` task behind.
- `flavor=mariadb` no longer fails with `empty server_uuid`. Identity uses `server_id` + `gtid_domain_id`. `log_bin=off` fails as `SOURCE_LOG_BIN_OFF`.
- ERROR 1045 maps to `SOURCE_ACCESS_DENIED` and enters `FAILED` instead of infinite `RETRY_BACKOFF`. `POST /start` can restart a `FAILED` task after credentials are fixed.
- A silent source at tip is no longer reported as multi-hour `DELAYED`. Idle dump waits report lag `0` / `NORMAL`. Heartbeat events are not written into backup files.
- Listen bind failures no longer dump cobra `Usage`. `GET /api/health` returns `{"status":"ok"}`; `GET /healthz` is unchanged.
- Quick Start is download / checksum / extract / `./binlog-server`. `go run` is documented under Development.

## Upgrade Notes

- No database migration is required for `v0.4.2`.
- `POST /api/tasks` now requires `source.password`. Clients that omitted it will get HTTP 400 instead of 201.
- `POST /api/tasks/{id}/start` is still `204 No Content`.
- Standalone without `meta_dsn` is still an in-memory control plane. Restartable tasks and durable `GET /checkpoint` / `GET /files` need `meta_dsn`. `storage.dir` remains ignored; files are stored at `{data_dir}/{task_id}/`.

## Operator Impact

- Release operators can install from GitHub Releases without installing Go.
- MariaDB sources with binlog enabled can start instead of looping on `empty server_uuid`.
- Invalid GTID/FILE_POS creates no longer leave a task that would pull from `LATEST` if started.
- Wrong passwords fail closed and are restartable after a fix; quiet masters no longer look hours behind.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.4.2.zh-CN.md
