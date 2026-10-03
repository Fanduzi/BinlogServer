# Binlog Server v0.5.16

Release date: 2026-10-04

Binlog Server `v0.5.16` is an operator patch on `v0.5.15`. Kill the process while a backup is `RUNNING`, adopt the on-disk backup without starting, then start it: it continues from the last complete event in the open segment. The published v0.5.15 package, after `kill -9`, still opens a new epoch that `mysqlbinlog` cannot replay.

## Highlights

- Kill the process while a backup is `RUNNING`. This is not an API stop. The on-disk backup then has no task metadata. Adopt it without starting, then start it. The dump continues from the end position of the last complete event in the open segment. It does not jump to the current `SHOW MASTER STATUS`. It does not restart at position 4.
- When that open segment's last complete event already ends at the resume position, the segment is renamed onto the new epoch and new events are appended, so one source filename spans the crash. The file still starts with the format description and keeps the events written before the process died. Events the source commits while the process is down land in that file, once each. There is no gap and no duplicate event at the boundary.
- `v0.5.15` already does this for a clean stop. The published v0.5.15 package does not do it after `kill -9`: adopt then start opened a new epoch whose first bytes were the magic plus events after the offset, so the format description and the pre-crash events stayed in the old segment and `mysqlbinlog` could not replay the new file.
- Direct start of that backup, before adopt, is still rejected because there is no task metadata. A clean stop and resume is unchanged. `delay_seconds` of `0`, an omitted sample, a dump preamble that is not lag, and the 30-second real-lag threshold are unchanged from `v0.5.15`.

## Upgrade Notes

- No schema migration is required for `v0.5.16`. Migrations are still `000001_init_schema` only.
- No new API and no new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.16.zh-CN.md
