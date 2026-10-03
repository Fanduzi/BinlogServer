# Binlog Server v0.5.14

Release date: 2026-10-03

Binlog Server `v0.5.14` is an operator patch on `v0.5.13`. A dump preamble is not replication lag. A caught-up `RUNNING` task returns `delay_seconds` as JSON `0`. There is no schema migration and no new config key. The 30-second threshold is unchanged. The published v0.5.13 package still stores the format-description timestamp as `last_event_at` and raises `DELAY_EXCEEDS_THRESHOLD` after adopt and start at the highest segment end, with the source already there and no new writes.

## Highlights

- After adopt, start from the end of the highest segment, with the source already at that position and no new writes, used to store the format-description timestamp as `last_event_at` and raise `DELAY_EXCEEDS_THRESHOLD`. That false alarm is gone. A real catch-up lag still uses event time. The 30-second threshold is unchanged.
- A caught-up `RUNNING` task now returns `delay_seconds` as JSON `0`, and the Console shows 「0 秒」. Status stays `NORMAL` at the tip. A task with no event-time sample still omits `delay_seconds`, so the Console can still show 「-- 秒」. No new config key. No schema migration.
- Dogfood on self-built tip `d833072d` (not the published v0.5.13 package): no `meta_dsn`, adopt then start at the highest segment end, source stopped at `mysql-bin.000005:490`. Console delay 「0 秒」, replication status 正常, no `DELAY_EXCEEDS_THRESHOLD`. `GET /api/tasks/{id}/replication` has `delay_seconds` 0. A second task with no sample still shows 「-- 秒」. The new open segment was 4 bytes, not a 126-byte format description, and the source position stayed 490.

## Upgrade Notes

- No schema migration is required for `v0.5.14`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- After adopt, start from the end of the highest segment, with the source already at that position and no new writes, no longer stores the format-description timestamp as `last_event_at` and no longer raises `DELAY_EXCEEDS_THRESHOLD`. A real catch-up lag still uses event time. The published v0.5.13 package still stores that timestamp and raises `DELAY_EXCEEDS_THRESHOLD`.
- A caught-up `RUNNING` task returns `delay_seconds` as JSON `0`, and the Console shows 「0 秒」. Status stays `NORMAL` at the tip. A task with no event-time sample still omits `delay_seconds`, so the Console can still show 「-- 秒」.
- Dogfood on self-built tip `d833072d` (not the published v0.5.13 package): no `meta_dsn`, adopt then start at the highest segment end, source stopped at `mysql-bin.000005:490`. Console delay 「0 秒」, replication status 正常, no `DELAY_EXCEEDS_THRESHOLD`. `GET /api/tasks/{id}/replication` has `delay_seconds` 0. A second task with no sample still shows 「-- 秒」. The new open segment was 4 bytes, not a 126-byte format description, and the source position stayed 490.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.14.zh-CN.md
