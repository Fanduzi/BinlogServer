# Binlog Server v0.5.46

Release date: 2026-10-06

Metadata mode drives each dump from `desired_run` and `spec_revision`. `STOPPED` is written only after the dump connection `Close()` returns. A task keeps at most one Binlog Dump. `KILL` uses the current source password. `minRequiredSchemaVersion` stays 3. Migration `000004_pending_dump_cleanup` adds one column. It is online. A single process can start without it. Cluster mode needs it. This is ADR 0005 step 4 of 9. No new config key.

## Highlights

- `backup_tasks.desired_run` is `RUN` or `STOP`. `spec_revision` is the operator revision. Start writes `RUN` and increments `spec_revision`. Stop writes `STOP` and increments `spec_revision`. A change to source, start position, storage, or `cluster_key` increments `spec_revision`. The worker control loop is the only starter and stopper. It starts a dump, stops one, restarts one dump after a spec change, and leaves a row alone when the desired state and the open dump already match. `Acquire` runs only when ownership changes. The same worker keeping its lease does not increase the epoch.
- `STOPPED` is written only after `Close()` returns. `owner_worker_id` is cleared then. A Stop that can reach the source leaves no Binlog Dump thread for that task. Standalone with no metadata database waits for `Close()` the same way.
- `KILL` uses the task's current password (#193). Stop, change the source password, then Start leaves one Binlog Dump thread. A later Stop leaves zero.
- A startup refusal because the metadata schema is below 3 is written to the configured log file. The process then exits. The message contains `schema version too old` and `./migrate up` (#245).
- An operator Stop that cannot reach the source still writes `STOPPED` (#249). `GET /api/tasks/{id}` includes `pending_dump_cleanup` (`connection_id`, `host`, `port`, and `warning`). The warning is `source Binlog Dump connection <id> may still be open; will KILL when source is reachable`. The Console task detail shows it. The warning stays after `STOPPED`. One `DUMP_CLEANUP_PENDING` event is written when the marker is set, and one `DUMP_CLEANUP_CLEARED` when it clears. The worker retries `KILL` with the current password once the source is reachable, first after 5s, then doubling up to 30s, until the thread is gone or the task is deleted. `ER_NO_SUCH_THREAD`, or a process-list row that is no longer that Binlog Dump, clears the marker. `STOPPED` is not held for that `KILL`. A retry that is still writing the snapshot it opened does not cover a newer operator Stop.
- On schema 3 the marker stays in the process that held the dump, including after that runner is gone (#252). `GET /api/tasks/{id}` sets `process_local` to true. Another process does not show it. A restart clears it. After migration `000004`, the same marker is on `backup_tasks` and survives a restart.
- A clean close, where FIN or RST reaches the source, is about one `master_heartbeat_period` (15s). A FIN close was observed around 26–29s. A half-open path, where packets are dropped and no FIN or RST arrives, does not block the small heartbeat write, so `net_write_timeout` does not drop the thread. How long the source keeps it depends on that host's TCP retransmission (Linux `tcp_retries2`, about 15 minutes by default; one half-open close was measured at about 957s). This process cannot shorten that wait while the path stays blackholed. The warning and the `KILL` retry stay until the thread is confirmed gone. A DBA can `KILL` that `connection_id` on the source (#253).
- A task has at most one Binlog Dump (#255). Start `KILL`s the old connection and opens a new dump only after the process list, or `ER_NO_SUCH_THREAD`, confirms the old one is gone. If that `KILL` is not confirmed, the task stays in `RETRY_BACKOFF` with the warning and does not dump. The worker that holds the dump renews its lease until `Close()` returns, and stops its own dump when it loses the lease. Another process does not write `STOPPED` for a dump it did not close. While the dump is open, schema 4 stores the connection id as `held`, and the API does not show that as a warning.

## Upgrade Notes

Stop every v0.5.44 and v0.5.45 worker that uses this metadata database before starting v0.5.46. Do not run those versions together with v0.5.46. Those binaries do not maintain `desired_run`. A mixed cluster starts a task on one side and stops it on the other.

`minRequiredSchemaVersion` stays 3. v0.5.46 does not start on schema 2. It exits with code 1 before it listens. The message contains `schema version too old` and `./migrate up`, and that message is written to the configured log file (#245).

If this database is not already schema 3, schema 2 to schema 3 (`000003_task_desired_and_retry_budget`) is online. Run `./migrate up` while the old processes stay up. A restart is not required for that migration. Confirm `SELECT version, dirty FROM schema_migrations` is `(3, 0)`. Schema 1 to schema 2 still follows the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server on that database, run `./migrate up`, then start only v0.5.34 or newer.

Migration `000004_pending_dump_cleanup` only adds `backup_tasks.pending_dump_cleanup VARCHAR(512) NOT NULL DEFAULT ''`. It is online. A single process may skip it and keep the pending marker in memory (`process_local` true). Cluster mode requires it. A cluster still on schema 3 logs `cluster mode needs migration 000004 (pending_dump_cleanup) before another worker can finish a dump it did not close; run ./migrate up` and does not take over another worker's dump. After `./migrate up`, `schema_migrations` is `(4, 0)`. Restart a process that was already up so it reads the column. `migrations/` is `000001`, `000002`, `000003`, and `000004`.

On startup, and on each claim pass, this process rewrites `desired_run` only for rows where `spec_revision=0` and `applied_spec_revision=0`. `RUNNING`, `STARTING`, `RETRY_BACKOFF`, `LEASE_DEGRADED`, and `REBUILDING_FILE` become `RUN`. Every other state becomes `STOP`. The revision columns stay 0. A row that already matches is not changed again. A row this process has started, stopped, or edited has `spec_revision>0` and is left alone. A task an older binary left as `RUNNING` with `desired_run=STOP` keeps running after the upgrade.

Do not run `./migrate down` to schema 2 while v0.5.46 is running. `./migrate down --steps 1` from schema 4 drops only `pending_dump_cleanup` and returns to version 3. Stop the cluster workers before that down migration. A single process can keep running on schema 3.

No new config key. Standalone with no metadata database does not run these migrations.

## Known behavior

The items below are still open. #189, #205, and #224 were already present before this release. #193 is fixed in this release.

- After failover, a sealed segment of the same source name can have `end_pos` written as 0. Tracked as #189.
- A GTID start, or the fallback after MySQL 1236, writes a manual Rotate with no CRC, so checksum verification fails and a stray `task-N.binlog` is left behind. Tracked as #205.
- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.46.zh-CN.md
