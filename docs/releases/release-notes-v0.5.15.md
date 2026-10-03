# Binlog Server v0.5.15

Release date: 2026-10-04

Binlog Server `v0.5.15` is an operator patch on `v0.5.14`. Stop a running backup, let the source keep writing, then start the same task: it continues from the last complete event. The published v0.5.14 package still skips that range, or opens a new epoch and dumps the file again from position 4.

## Highlights

- Stop a `RUNNING` backup, let the source keep writing, then start the same task. Standalone with no `meta_dsn`, and a catalog task, both resume at the end position of the last complete event in the highest open segment. The dump does not jump to the current `SHOW MASTER STATUS`. That was the standalone hole. It does not restart at position 4. That was the catalog path: the next start opened a new epoch and deleted the open segment.
- When the dump started mid-file, that end position is not the file size. The stopped open segment is renamed into the new epoch and appended, so one source filename spans the stop. Events the source commits while the task is stopped land in that file. There is no gap and no duplicate event at the boundary. A partial event past that end position is dropped before the append.
- A catalog takeover with an empty directory, or with no complete event, still starts at position 4. Standalone with no checkpoint and no complete event still starts at the current master status. `KeepLocalSegments` adopt still uses the saved file size and opens a new epoch. A task that was already at the source tip still reports at tip.
- `delay_seconds` of `0`, an omitted sample, a dump preamble that is not lag, and the 30-second real-lag threshold are unchanged from `v0.5.14`. Catch-up that is still behind the tip still uses event time.

## Upgrade Notes

- No schema migration is required for `v0.5.15`. Migrations are still `000001_init_schema` only.
- No new API and no new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.15.zh-CN.md
