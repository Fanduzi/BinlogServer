# Binlog Server v0.5.45

Release date: 2026-10-06

On all-in-one and cluster, `retry_attempt` and `consecutive_source_failures` are stored on `backup_tasks` and written with the task row. A restart during one `SOURCE_UNREACHABLE` streak continues that streak and the backoff delay. Two restarts, `SIGTERM` and `kill -9` in either order, still reach `FAILED` on the 10th consecutive failure. `last_error` begins with `SOURCE_UNREACHABLE:`, and the lease is released. `minRequiredSchemaVersion` is 3. A process on schema 2 exits with code 1. The message tells the operator to run `./migrate up`. It is printed to stderr only, not the log file. This is ADR 0005 step 3 of 9. No new config key. Tracked as #239, PR #244. QA passed on `ec1e4f1d` for all-in-one and a 2-worker cluster.

## Highlights

- With a metadata database, each `SOURCE_UNREACHABLE` failure writes `retry_attempt` and `consecutive_source_failures` on `backup_tasks` in the same write as the task row. A successful dump connect stores 0. Operator Start stores 0. A claim does not.
- Two restarts inside one streak, `SIGTERM` and `kill -9` in either order, still reach `FAILED` on the 10th consecutive failure. `GET /api/tasks/{id}` shows `last_error` beginning with `SOURCE_UNREACHABLE:`, and the lease is released. The consecutive count stays at most 10.
- A restart tries the source once immediately. The delays after that continue from the stored `retry_attempt` (8s, then 16s, then 30s, and so on). They do not start again at 1s. The delay cap stays 30s.
- The same worker renewing its lease, or reclaiming its own lease after that lease has expired, keeps the epoch. A different worker taking an expired lease increases the epoch by 1.
- `FAILED` sets `desired_run` to `STOP` and `failed_spec_revision` to `spec_revision`. Lease renewal no longer rewrites that row as `STOPPING`.
- The first start after upgrading from a binary that only had the event streak copies that streak into the columns once, when both columns are still 0. That one start begins the backoff delay at 1s again. Later restarts use the stored attempt.
- Standalone, with no metadata database, keeps the counters in memory. They reset when the process exits. That path is unchanged.

## Upgrade Notes

`minRequiredSchemaVersion` is 3. v0.5.45 does not start on schema 2. If this database is not already schema 3 from v0.5.44, run `./migrate up` while those processes stay up. Schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. A restart is not required for that migration. Confirm `SELECT version, dirty FROM schema_migrations` is `(3, 0)`. Then replace binaries. A v0.5.45 process started on schema 2 exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`. That message is printed to stderr only. It is not written to the log file.

Schema 1 to schema 2 is the v0.5.34 migration. That step still needs every binlog-server on that database stopped. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every process, run `./migrate up`, then start only v0.5.34 or newer. The online step is schema 2 to schema 3. v0.5.45 needs schema 3 before start.

Replacing the binary with v0.5.44 works on schema 3. Do not run `./migrate down` to schema 2 while v0.5.45 is running. v0.5.45 will not start on schema 2.

No new config key. This release does not add a migration. `migrations/` is still `000001`, `000002`, and `000003`.

## Known behavior

The items below are still open. #189, #193, #205, and #224 were already present before this release. #239 is fixed in this release.

- After failover, a sealed segment of the same source name can have `end_pos` written as 0. Tracked as #189.
- After the source password is changed during Stop, then Start, the source may keep an extra Binlog Dump connection. Tracked as #193.
- A GTID start, or the fallback after MySQL 1236, writes a manual Rotate with no CRC, so checksum verification fails and a stray `task-N.binlog` is left behind. Tracked as #205.
- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.45.zh-CN.md
