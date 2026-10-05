# Binlog Server v0.5.30

Release date: 2026-10-05

On Binlog Server `v0.5.30`, a backup that starts in the middle of a source binlog writes the format description MySQL sends first, once, in front of the first copied event. `LATEST` starts in the middle of the current source file. A `FILE_POS` start inside a file does the same. Up to `v0.5.29`, that description was dropped. `mysqlbinlog` then refused the first segment with `does not contain any Format_description_log_event`, and it could not decode the row events in that segment. The published v0.5.29 package still drops the description.

## Highlights

- MySQL sends a format description before the first copied event. On a mid-file start its `end_log_pos` is 0, or the original header position (126 on MySQL 8), which is behind the dump cursor. `v0.5.30` keeps the bytes MySQL sent and writes them once, after the 4-byte magic header and in front of the first copied event. `mysqlbinlog --verify-binlog-checksum` can read that file.
- The checkpoint stays on the copied event. The description's own end position (126 on MySQL 8, or 0) is not the checkpoint, and its timestamp is not lag.
- A quiet source that has not sent a later event still leaves the segment as the 4-byte magic header. Stop then start does not rewind to position 126.
- A segment that already has events does not gain a second description on the next start.
- No new config key. No schema migration.

## Upgrade Notes

- No schema migration is required for `v0.5.30`. Migrations are still `000001_init_schema` only.
- No new config key. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- Segments written by a `LATEST` start, or by a `FILE_POS` start inside a file, on `v0.5.29` or earlier still lack the format description. This release does not backfill them and does not repair them. The next start sees that the segment already has events and does not insert a description.
- To tell: run `mysqlbinlog` on that first segment. It reports `does not contain any Format_description_log_event`.
- A later file opened by a real rotate, starting at position 4, was not affected. On that file the format description ends after position 4 (126 on MySQL 8), so it is ahead of the cursor. `v0.5.29` and earlier copied that event with the rest of the file. The description was dropped only when its end position was behind the dump cursor, which is the first segment of a mid-file start. Sealing that first segment renames it and leaves the bytes, so `mysqlbinlog` still reports the missing `Format_description_log_event` on the sealed name.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.30.zh-CN.md
