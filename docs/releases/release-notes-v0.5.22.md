# Binlog Server v0.5.22

Release date: 2026-10-04

Binlog Server `v0.5.22` adds the resume identity on top of `v0.5.21`. A stopped task's Console detail shows the configured start and the position the next Start continues from. The published v0.5.21 package shows the start as the mode name alone. `GET /api/tasks/{id}/checkpoint` there returns the stored checkpoint row, which can differ from the position Start will use.

## Highlights

- A stopped task's Console detail shows two identities. Start (起点) is the configured start: `LATEST`, or `FILE_POS` as `file:pos`, or `GTID` as `gtid_set`. Resume (续传) is the position the next Start continues from, shown as `file:pos`, and as `file:pos GTID gtid_set` when that resume includes a GTID. With no resume, the line is `None. The next Start uses the configured start.`
- A stopped `LATEST` task still shows `LATEST` as its start. It does not get rewritten to `FILE_POS`. The resume line is the file and position of the last complete event in the highest local open segment.
- `GET /api/tasks/{id}/checkpoint` returns that same resume the runner uses on Start: `file` and `pos`. `gtid_set` is included only when the stored checkpoint is that same file and position. With neither a checkpoint row nor a complete local event, the response stays 404 `checkpoint not found`. The configured start stays on the task's `start` object, not inside the checkpoint JSON.
- A stop that races the run exit no longer leaves the task stuck in `STOPPING`. A later write of an older `STOPPING` snapshot does not overwrite `STOPPED`.

## Upgrade Notes

- No schema migration is required for `v0.5.22`. Migrations are still `000001_init_schema` only.
- No new config key. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.22.zh-CN.md
