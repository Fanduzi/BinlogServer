# Binlog Server v0.5.27

Release date: 2026-10-05

On Binlog Server `v0.5.27`, an existing backup task can be asked for the binlog window covering a UTC stop time, on top of `v0.5.26`. The published v0.5.26 package keeps the limit replay: `flavor`, `client`, `client_hint`, and `paths`, with no `command` field. Restore the full backup yourself, then run the printed command. Binlog Server does not restore that backup.

## Highlights

- `GET /api/tasks/{id}/replay` takes `stop_datetime` (UTC) and an optional `start_datetime`. The same query on `GET /api/tasks/{id}/replay/archive` downloads that selection. The window is half-open `[start, stop)`. `stop_datetime` is excluded. `start_datetime`, when set, is included. Times are UTC. `YYYY-MM-DD HH:MM:SS` and `YYYY-MM-DDTHH:MM:SS` are read as UTC. An RFC3339 value with a zone is converted to UTC. Each source index stays one path, using the same sealed-versus-open rule as the limit replay. `limit` is not applied on this path. The paths are the segments whose event-header times cover that window.
- The response includes `command` only when `stop_datetime` is present. `flavor`, `client`, `client_hint`, and `paths` stay. `source.flavor` `mysql` uses `mysqlbinlog`. `mariadb` uses `mariadb-binlog`. The command is prefixed `TZ=UTC`, so those clients compare the flags to the event-header timestamps. It includes `--stop-datetime`, and `--start-datetime` when a start time is set. The command names the on-server paths. Tar members are the basenames of that same selection, in that order, and the filename is `task-{id}-replay.tar`.
- Omitting both datetimes keeps the limit replay: `flavor`, `client`, `client_hint`, `paths`, and no `command` field. The archive then stays that same limit window.
- `start_datetime` equal to `stop_datetime` is an empty window: HTTP 200, empty `paths`, empty `command`. It is not a 400. Any empty window is HTTP 200 with empty `paths` and an empty `command`. A bad datetime or a start after the stop is plain-text 400 (`invalid stop_datetime`, `invalid start_datetime`, or `start_datetime is after stop_datetime`). `start_datetime` without `stop_datetime` is plain-text 400 `stop_datetime is required`. A missing task is 404 `task not found`. The archive of an empty window is HTTP 200 and an empty tar. A selected segment this process cannot open is 404 `segment not found on this process`, and that response is not a partial tar.
- Console task detail has the same window: a UTC stop time, an optional start time, generate, copy, and download of `task-{id}-replay.tar`. That Console is in the embedded bundle a plain `go build` serves.

## Upgrade Notes

- No schema migration is required for `v0.5.27`. Migrations are still `000001_init_schema` only.
- No new config key. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- Restore the full backup yourself, then use the printed command. The limit replay, background upload retry, retention, and lease takeover stay as they are in `v0.5.26`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.27.zh-CN.md
