# Binlog Server v0.5.44

Release date: 2026-10-06

Migration `000003_task_desired_and_retry_budget` adds six `NOT NULL` columns with defaults to `backup_tasks`: `desired_run`, `spec_revision`, `applied_spec_revision`, `failed_spec_revision`, `retry_attempt`, `consecutive_source_failures`. Existing rows are backfilled. `RUNNING`, `STARTING`, `RETRY_BACKOFF`, `LEASE_DEGRADED`, and `REBUILDING_FILE` become `desired_run=RUN`. `CREATED`, `STOPPING`, `STOPPED`, and `FAILED` become `desired_run=STOP`. Revisions and counters are 0. The running process does not read or write these columns. This is ADR 0005 step 2 of 9. `minRequiredSchemaVersion` stays 2. v0.5.44 runs on schema 2 or schema 3. v0.5.43 keeps running on schema 3. No new config key. No runtime behavior change. Tracked as PR #236.

## Highlights

- Migration `000003_task_desired_and_retry_budget` adds six `NOT NULL` columns with defaults on `backup_tasks`: `desired_run`, `spec_revision`, `applied_spec_revision`, `failed_spec_revision`, `retry_attempt`, and `consecutive_source_failures`. An older binary's insert does not list these columns. Each column has a default, so that insert still succeeds.
- Backfill sets `desired_run=RUN` for `RUNNING`, `STARTING`, `RETRY_BACKOFF`, `LEASE_DEGRADED`, and `REBUILDING_FILE`. It sets `desired_run=STOP` for `CREATED`, `STOPPING`, `STOPPED`, and `FAILED`. `spec_revision`, `applied_spec_revision`, `failed_spec_revision`, `retry_attempt`, and `consecutive_source_failures` are 0. A `FAILED` row keeps `failed_spec_revision` at 0.
- The runtime does not read or write these columns. ADR 0005 (`docs/adr/0005-single-task-authority-and-segment-catalog.md`) step 2 of 9 is this migration. `minRequiredSchemaVersion` stays 2. A v0.5.44 process starts on schema 2 or schema 3. A v0.5.43 process keeps running after the database is schema 3.
- No new config key. Task state, retry, lease, and dump behavior are the same as v0.5.43.

## Upgrade Notes

Run `./migrate up` while the processes stay up. A restart is not required for this migration. That was checked with v0.5.43 still running. Confirm `SELECT version, dirty FROM schema_migrations` is `(3, 0)`. `SHOW COLUMNS FROM backup_tasks` lists `desired_run`, `spec_revision`, `applied_spec_revision`, `failed_spec_revision`, `retry_attempt`, and `consecutive_source_failures`. Then replace binaries in any order. v0.5.44 runs on schema 2 or schema 3. v0.5.43 keeps running on schema 3. `minRequiredSchemaVersion` stays 2. `migrations/` is `000001`, `000002`, and `000003`.

Run this migration now. The next ADR 0005 step, persisted retry budget (step 3), raises `minRequiredSchemaVersion` to 3. A binary that requires schema 3 will not start on schema 2.

A metadata database still on schema 1 must be migrated before a current binary will start. Follow the [v0.5.34 upgrade](release-notes-v0.5.34.md): stop every binlog-server process on that database, run `./migrate up`, then start only v0.5.34 or newer. The online step in this release is schema 2 to schema 3.

Rollback is `./migrate down --steps 1`. That returns `schema_migrations` to version 2 and drops only those six columns. `backup_tasks` and `binlog_files` row counts stay the same.

No new config key. No runtime behavior change.

## Known behavior

The items below are still open. #239, #189, #193, #205, and #224 were already present before this release.

- Two restarts inside one failure streak can take up to 14 failures before `FAILED`. A claim-written `TASK_STARTED` is indistinguishable from an operator Start, so the second restart keeps only the failures after the previous claim. The backoff delay starts again at 1s after a restart. The structural fix is ADR 0005 step 3, a persisted retry budget column. Tracked as #239.
- After failover, a sealed segment of the same source name can have `end_pos` written as 0. Tracked as #189.
- After the source password is changed during Stop, then Start, the source may keep an extra Binlog Dump connection. Tracked as #193.
- A GTID start, or the fallback after MySQL 1236, writes a manual Rotate with no CRC, so checksum verification fails and a stray `task-N.binlog` is left behind. Tracked as #205.
- A process crash while a takeover is downloading an `UPLOADED` object leaves a `.takeover-*` temp file on disk. A restart and a successful retry do not remove it. Tracked as #224.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.44.zh-CN.md
