# Binlog Server v0.5.14

Release date: 2026-10-03

Binlog Server `v0.5.14` is an operator patch on `v0.5.13`. A dump preamble is not replication lag. The published v0.5.13 package still has the false alarm.

## Highlights

- After adopt, start from the end of the highest segment, with the source already at that position and no new writes, no longer stores the format-description timestamp as `last_event_at` and no longer raises `DELAY_EXCEEDS_THRESHOLD`. A real catch-up lag still uses event time.
- A caught-up `RUNNING` task returns `delay_seconds` as JSON `0`, and the Console shows 「0 秒」. A task with no event-time sample still omits `delay_seconds`, so the Console can still show 「-- 秒」.

## Upgrade Notes

- No schema migration is required for `v0.5.14`. Migrations are still `000001_init_schema` only.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.14.zh-CN.md
