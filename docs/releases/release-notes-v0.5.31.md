# Binlog Server v0.5.31

Release date: 2026-10-05

On Binlog Server `v0.5.31`, a point-in-time window no longer treats the format description or the previous-GTIDs event as copied data. Those two events record when the source binlog file was created. A backup that starts in the middle of a source file, which is what `LATEST` does, writes that format description in front of the first copied event. A `FILE_POS` start inside a file does the same. Up to `v0.5.30`, `GET /api/tasks/{id}/replay` with `stop_datetime` counted that creation time, so a stop time before any copied transaction still returned the file and a command. The published v0.5.30 package still does that.

## Highlights

- `GET /api/tasks/{id}/replay` with `stop_datetime`, and the same query on `GET /api/tasks/{id}/replay/archive`, skip the format description and the previous-GTIDs event. The timestamps that count are the copied events after those headers. A transaction's own GTID event still counts. Only the previous-GTIDs header is skipped.
- A UTC stop time before the first copied event returns an empty window: HTTP 200, `paths` is `[]`, and `command` is empty. The archive of that window is an empty tar. It is not a 400. The file is included once `stop_datetime` is after that first copied event, so the half-open window contains it. The window stays `[start, stop)`. The stop is excluded, the same rule as `mysqlbinlog --stop-datetime`. A stop time equal to the first copied event is still empty.
- A segment that has only the format description and the previous-GTIDs event, and no copied event yet, covers no stop time.
- A later file opened at position 4 by a real rotate uses the same rule. The format description and previous-GTIDs at the front of that file do not select it. Coverage starts at the first copied event after them.
- Omitting both datetimes keeps the limit replay. The `mysqlbinlog` and `mariadb-binlog` flags are unchanged. The command is still prefixed `TZ=UTC` and still prints `--stop-datetime`. Console task detail uses this same window. No new config key. No schema migration.

## Upgrade Notes

- No schema migration is required for `v0.5.31`. Migrations are still `000001_init_schema` only.
- No new config key. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- Segments already on disk are not rewritten. This release only changes which `stop_datetime` selects them.
- On the published v0.5.30 package, a stop time between the source file's creation and the first copied transaction can still return that first segment. Check a restore that used such a stop time: that file did not contain a copied transaction at the requested time.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.31.zh-CN.md
