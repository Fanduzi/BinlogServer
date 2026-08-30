# Binlog Server v0.5.2

Release date: 2026-08-30

Binlog Server `v0.5.2` is an operator-facing bugfix patch on `v0.5.1`.

## Highlights

- Starting a `LATEST` task against a quiet source no longer flashes `DELAYED` from a days-old binlog event header. Once the dump is at the source file/pos, `delay_seconds` is 0 / `NORMAL`. Catch-up from `FILE_POS`/`GTID` that is still behind the tip still reports real lag.
- Task list and dashboard pages sort numeric string ids as integers. With hundreds of tasks, page 1 is `1,2,3,…` instead of `1,10,100,…`.
- Quick Start, landing-page, and deployment download examples now point at `v0.5.2`.

## Upgrade Notes

- No schema migration is required for `v0.5.2`.
- No API contract change versus `v0.5.1`. The `GET /api/tasks` paging object from `v0.5.0` still applies if you are upgrading from `v0.4.x`.
- List order of existing tasks changes from lexicographic id to numeric id.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.2.zh-CN.md
