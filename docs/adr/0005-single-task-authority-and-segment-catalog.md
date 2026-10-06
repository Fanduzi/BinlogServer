# One authority for task state, one key for a segment

Status: Accepted (2026-10-06). Steps 1–6 of the ordered rollout are on main. Step 5, fail-and-alert, landed in v0.5.47 (PR #250). Step 6 landed `000005_binlog_source_epoch_key` and the catalog writer that identifies a segment by `(task_id, source_file, epoch)`. `minRequiredSchemaVersion` is 5. Schema is `000001` through `000005_binlog_source_epoch_key`. The next new migration is `000006`. Steps 7–9 are not done. #189 is fixed in v0.5.49 (PR #264). #224 stays open. #205 is fixed in v0.5.48 (PR #261). v0.5.48 and v0.5.49 are not step 6, 7, 8, or 9. Step 6 has no release tag.

The problem section cites `main` at `5373057` (includes the #234 fix for #203). That snapshot is why this record was written. It is not the code after steps 2–5.

Related records: [0001](0001-task-ownership.md) (one ownership door; the control plane does not take the lease), [0003](0003-do-not-split-scheduler-first.md) (do not split the scheduler as the first step), [0004](0004-two-observations.md) (a page of tasks is not the cluster inventory). Those decisions stay. This record says what those doors write, and what a binlog segment's name is.

## Decision

Metadata MySQL is the only authority for desired task state and for observed task state. A worker runs an idempotent control loop that reads that row and acts. Start, Stop, and a config change are writes. They do not launch or cancel a dump by themselves.

The lease epoch changes only when ownership changes: the lease row is missing or expired, and `Acquire` takes it. A retry, a password change, and a claim of a task this worker already holds do not call `Acquire`.

The retry budget is two columns on `backup_tasks`, updated in the same write as `RETRY_BACKOFF` or `FAILED`. The default for an error that is not on the allowlist is fail and alert: observed `FAILED`, lease released, `last_error` set. The operator Starts the task again. `SOURCE_UNREACHABLE` stays retryable, with that persisted budget.

`binlog_files` identifies a segment by `(task_id, source_file, epoch)`. One function, `binlog.ClassifySegment`, is the only parser for a file name. Resume, takeover, retention, replay, `GET /api/tasks/{id}/files`, and the Console all use it. `end_pos` is written only from a cursor the writer actually has. The value `0` is not a stand-in for "unknown".

Standalone (no metadata DSN) has one process. The in-memory task is the record. The same loop runs against that struct. There is no second copy synced from a database that is not there.

## Context

An architecture review of v0.5.30 listed eight problems. The fixes landed across v0.5.31–v0.5.40 as separate issues: #172, #173, #174, #175, #176, #177, #178, #179, and the follow-ups #204, #209, #213, #214, #220, #222, #223. Each fix made one symptom match the operator's expectation. The two structures below are still in the code, and the issues that are still open sit on them.

#203 is closed. #234 stopped the claim loop from cancelling a live owned run, and a same-owner claim with no local run now rebuilds the `SOURCE_UNREACHABLE` streak from task events. The streak is still not a column. That issue is the example of structure A, not an open bug to re-fix.

## Problem

The bullets in this section describe `main` at `5373057`. Steps 2–5 have since changed the runtime. Migration and rollout is the status after v0.5.49. #189 is fixed in that release. #205 is fixed in v0.5.48. Steps 6–9 are still next work.

### A. Task state has more than one writer

With a metadata store, each process keeps `s.tasks`, `s.cancels`, `s.runs`, and the `runTask` goroutine. `backup_tasks` stores the same task. Both are written as if they were the authority.

Writers and readers on `main`:

| What | Where | What it treats as true |
| --- | --- | --- |
| Memory map | `Scheduler.tasks`, `cancels`, `runs` in `internal/tasks/scheduler.go` | The process-local task, the cancel handle, and the live goroutine |
| Store overlay | `syncTasksFromStore` (`internal/tasks/scheduler.go`) | Upserts store rows into `s.tasks`. It does not rebuild the map. A row read across a publish is skipped (`staleStoreReadLocked`). A `STOPPING`/`STOPPED` row for a live run cancels the dump and does not replace owner/epoch (`noteRemoteStopLocked`) |
| Read overlay | `GetTask` (`internal/tasks/scheduler_task_ops.go`) | Reads `backup_tasks` by primary key and, unless the read is stale or the row is a remote stop, assigns `s.tasks[id] = item`. The value returned to the API is the store row even when memory was left alone |
| Transition | `persistTaskLocked` (`internal/tasks/scheduler.go`), called from `internal/tasks/scheduler_transitions.go` | Copies the task into `s.tasks`, then drops `s.mu` for `UpsertTask`. A newer transition can land while the old write is in flight. The generation counter repairs a lost update; it does not make the row the only writer |
| Claim | `claimOwnedIdleTasks` (`internal/tasks/scheduler_lifecycle.go`), called from the 2s loop in `startWorkerClaimLoop` (`internal/app/app.go`) | Decides "this process should start the task" from the memory snapshot |
| Retry budget | locals `attempt` and `consecutiveSourceFailures` in `runTask`; `carriedSourceFailures` reads at most 200 `TASK_RUNNER_ERROR` events | The cap of 10 for `SOURCE_UNREACHABLE` (`maxConsecutiveRetryableSourceFailures`) |
| Lease | `AcquireTaskLease` in `internal/meta/sql/lease.sql`, called from `startTask` | `epoch = epoch + 1` only when `lease_expire_at <= NOW(6)`. `startTask` calls `Acquire` on every start, including a same-owner claim that finds no local run |
| Dump | `MySQLRunner.run` (`internal/replication/mysql_runner.go`) | Holds the `Task` value from when the run started, and one `BinlogSyncer` closed by `defer` when `Run` returns |

`StopTask` writes `STOPPING` into memory and the store, and cancels `s.cancels[id]` when this process has the run. If another worker owns the lease, the control plane leaves the row at `STOPPING` and waits for that worker to notice. That path is what #174 added. The dump is still gone only when the goroutine returns and `syncer.Close` runs. Nothing in `backup_tasks` records "the source connection has closed".

`dumpConfigLocked` treats `RUNNING`, `STARTING`, `LEASE_DEGRADED`, and `RETRY_BACKOFF` as frozen. `STOPPING` is not in that list, so `UpdateTask` accepts a new source password while a dump may still be connected. The running `Run` keeps the old `Task` value. `startTask` replaces the single `s.cancels[id]` slot when it starts another goroutine.

#### #203, closed, as the example

Before #234, `claimOwnedIdleTasks` called `StartTask` for an owned `RETRY_BACKOFF` every claim tick. `StartTask` always allows that state, cancels the previous context, and starts a new `runTask` whose counters begin at 0. Cancelling the context also stops `renewLeaseLoop`. That loop waits `leaseRenewInterval` (default 5s) before the first `Renew`, and the claim tick is 2s, so the 15s lease expired and the next `Acquire` incremented `epoch`. The task stayed in `RETRY_BACKOFF` past 10 failures, and the epoch climbed (the report was 1→29).

On this `main`, `claimOwnedIdleTasks` skips the id when `s.runs[id]` is still open, and a claim with no local run passes `carrySourceFailures` so `runTask` seeds the counter from events (`carriedSourceFailures`). That removes the live-run reset. It leaves the structure:

- The budget is still not on `backup_tasks`. After a restart it is derived from the event log. `ListEvents` is limited to 200 rows. A read error logs and falls back to the in-memory slice, which a new process does not have, so the streak comes back as 0.
- `startTask` still calls `Acquire` whenever it starts a run. A retry inside a live `runTask` does not. A same-owner start after the renew loop has stopped still takes a new epoch once the lease is expired.
- Unknown errors still enter `RETRY_BACKOFF` with no cap. `classifyRunError` (`internal/tasks/errors.go`) turns a sealed-file conflict into `SEALED_FILE_EXISTS` and otherwise returns the error unchanged. `runTask` fails the task only for `IsPermanent` and for ten `SOURCE_UNREACHABLE`. #178 closed the "retry forever" report for those permanent codes. The tasks module README still says a transient metadata error, `OBJECT_PURGE_FAILED`, MySQL 1236 with no stored GTID, and a local append/flush error stay in `RETRY_BACKOFF` with no new limit. Transient metadata is `meta.IsTransientMySQLError`: the error text contains `deadlock`, `lock wait timeout`, `connection reset`, `connection refused`, `broken pipe`, `server has gone away`, `invalid connection`, `bad connection`, `read-only`, `read only`, `timeout`, or `eof`.

#### #193, open

Reproduction on the #192 tip: Stop reaches `STOPPING`, the source password is changed with `ALTER USER` and `PUT`, the row reaches `STOPPED`, Start runs again. `SHOW PROCESSLIST` showed two `Binlog Dump` threads, and a later Stop left one dump on the source after `owner_worker_id` was empty. Ordinary Stop on that tip did not leave a dump. #174's `STOPPING` → `STOPPED` path passed.

The row can say `STOPPED` with an empty owner while a connection the process no longer tracks is still a dump thread. Causes in the current code:

- The source connection is the `BinlogSyncer` inside `MySQLRunner.run`. It closes when that `Run` returns. `backup_tasks` has no generation for "this dump".
- `s.cancels` holds one function. A second `startTask` cancels the previous context and stores a new one. The previous `Run` closes its syncer only if that return actually happens.
- `UpdateTask` will persist a new password in `STOPPING`, because `dumpConfigLocked` does not include `STOPPING`. The in-flight `Run` does not read that write. The next start opens another syncer with the new password.

### B. A segment's name is decided in more than one place

Schema v2 (migration `000002_binlog_file_epoch_key`) already replaced `uk_task_file (task_id, file_name)` with `uk_task_file_epoch (task_id, file_name, epoch)`. `minRequiredSchemaVersion` is 2. `source_file` exists and is nullable. `UpsertBinlogFile` binds `meta.FileName` for both `file_name` and `source_file`. Callers set `FileName` to the source name (`mysql-bin.000009`), not the basename.

Disk layout, from `binlog.ClassifySegment` (`internal/binlog/segment.go`):

| Basename | Meaning |
| --- | --- |
| `mysql-bin.000009` | Plain sealed name. `ClassifySegment` reports epoch `-1`. Migration 000002 left those rows at epoch 0. That plain name is the historical object key. |
| `mysql-bin.000009.open.eN` | Open segment for epoch N |
| `mysql-bin.000009.sealed.eN` | A later seal of a source name that already has a sealed file |

`catalogEpoch` (`internal/replication/mysql_runner.go`) stores N for a `.open.eN` or `.sealed.eN` name. A plain name sealed while the task epoch is positive stores the task epoch, so it does not replace the epoch-0 row. Open `.open.eN` and sealed `.sealed.eN` are the same unique key `(task_id, file_name, epoch)`. They are one row, updated in place. `ON DUPLICATE KEY UPDATE` assigns `end_pos = VALUES(end_pos)` on every upsert.

Parsers that still disagree:

| Parser | File | Rule |
| --- | --- | --- |
| `ClassifySegment` | `internal/binlog/segment.go` | The shared parser. Used by durable resume (`internal/binlog/durable.go`), the runner's directory walks, and `classifyBinlogSegment` in `internal/tasks/disk_files.go` |
| `openEpoch`, `catalogOpen`, `readablePlainOpen` | `internal/tasks/resume.go` | `openEpoch` parses `.open.e` itself and returns 0 on failure. `catalogOpen` is true when `state` is `OPEN` or the name contains `.open.e`. `readablePlainOpen` treats a basename that contains `.open.e` or `.sealed.e` as not a plain open file |
| `isOpenSegmentName` | `internal/replication/mysql_runner.go` | `strings.Contains(name, ".open.e")`. Retention skips those names and then treats every other directory entry as a sealed file subject to the age cutoff |
| `catalogSegmentName` | `internal/replication/mysql_runner.go` | Basename of `file_path`, or `file_name#epoch` when the path is empty. Enroll and retention join rows to disk with this string |
| Failed-upload SQL | `listFailedSealedBinlogFilesSQL` in `internal/meta/mysql_store.go` | `file_name NOT LIKE '%.open.e%' AND file_path NOT LIKE '%.open.e%'` |
| `SelectReplayFiles` / `WindowBinlogFilesForReplay` | `internal/tasks/disk_files.go` | Replay order and which sealed rows are duplicates. `sealedPointCovered` ignores a row whose `end_pos` is 0 |
| Disk listing | `listTaskBinlogFilesOnDisk` | Uses `ClassifySegment`, and leaves `start_pos` and `end_pos` at 0 because it does not read the file |

#### #189, fixed

After failover, `mysql-bin.000009.sealed.e1` was `SEALED`, `UPLOADED`, `checksum=match`, `size_bytes=543`, `start_pos=4`, `end_pos=0`. Download still returned the bytes. A later epoch whose open file already held the events stored `start_pos` and `end_pos` as the resume cursor, while the file held events from position 4 through the last event.

On this `main`, seal and enroll set `start_pos` and `end_pos` from the contiguous event chain in the sealed file. A newly sealed row has `start_pos` equal to the first event position and `end_pos` equal to the last event end log position. That includes epoch 1, a third epoch of the same name, and a segment sealed after a file/pos resume gets MySQL 1236 and continues with GTID. A same-file artificial rotate with position 0 does not clear the position being copied. An already `UPLOADED` row is not rewritten, so a row an older binary stored with `end_pos` 0 stays that way. This shipped in v0.5.49 (PR #264). It is not step 7.

The migration cannot repair a row that is already stored. The bytes are on the worker disk, or in the object, not in a column MySQL can recompute.

#### #224, open

`materializeUploaded` copies an `UPLOADED` object through `os.CreateTemp(dir, ".takeover-*")` and removes that file in a `defer` when the process is still alive. `kill -9` skips the defer. Nothing on startup, and nothing on the next materialize, globs `.takeover-*`.

`ClassifySegment` rejects a name that starts with `.`, so the files API and the disk scan do not list it. Retention does not use that parser. `purgeExpiredAt` skips the active file and any name for which `isOpenSegmentName` is true, then deletes other entries whose mtime is past local retention. A fresh `.takeover-*` (the #224 report was about 98MB of a 266MB object) is younger than retention, so it stays. An old one is deleted only because it is an unrecognized file that aged out, not because a segment policy owns it. The cap in the report is the segment size, up to 1GB.

#### #205, fixed

Reported on `9c350c07`: a sealed segment ended with a manual Rotate (flags `0x20`, timestamp 0, about 43 bytes, no CRC). `mysqlbinlog --verify-binlog-checksum` failed at that offset. GTID start also sealed and uploaded a `task-N.binlog` of about 47 bytes (magic plus that Rotate).

On this `main`, `handleEvent` does not append a rotate whose `LogPos` is 0. A rotate that names the current file only updates the position. A rotate that names the next file seals the current file and does not write the artificial event. That is the #214 rule, and it is in the tree. This record does not propose writing that event again.

A GTID start does not create `task-{id}.binlog`. `run` waits until the dump names a source file, then opens that name. A rotate that arrives before any copied event does not seal a magic-only file and does not upload one. The same discard applies when a file/pos resume gets MySQL 1236 before any event and the GTID dump then names a different file: the magic-only file opened for the purged name is removed, not sealed. An already-uploaded segment whose tail is that no-CRC Rotate is not rewritten, including when `checksum` is `match`. This shipped in v0.5.48 (PR #261). It is not step 8.

## Target design

### Desired state and observed state

`backup_tasks.state` stays the observed state (`CREATED`, `STARTING`, `RUNNING`, `LEASE_DEGRADED`, `REBUILDING_FILE`, `RETRY_BACKOFF`, `FAILED`, `STOPPING`, `STOPPED`).

New columns, all with defaults so a binary that does not list them in `upsertTaskSQL` still inserts:

| Column | Meaning |
| --- | --- |
| `desired_run` | `RUN` or `STOP`. Operator intent. |
| `spec_revision` | Integer. Increases by 1 on operator Start, operator Stop, and any `UpdateTask` that changes source, start, storage, or `cluster_key`. |
| `applied_spec_revision` | The revision the owning worker has opened a dump for. `0` until the first run. |
| `failed_spec_revision` | The revision that reached `FAILED`. `0` if the task has not failed on the current ask. |
| `retry_attempt` | Exponential backoff step, persisted. |
| `consecutive_source_failures` | `SOURCE_UNREACHABLE` streak, persisted. Cap remains 10. |

Operator Start writes `desired_run=RUN`, increments `spec_revision`, and sets both counters to 0. It does not call `Acquire` and it does not start a goroutine.

Operator Stop writes `desired_run=STOP` and increments `spec_revision`. It does not cancel a dump in the API process.

`UpdateTask` writes the new JSON and increments `spec_revision` in `STOPPING` as well as in `RUNNING`. The password change is a fact the owner must observe. The API does not open a second connection to apply it.

Create leaves `desired_run=STOP` and `state=CREATED`.

A worker that decides the task has failed writes `state=FAILED`, `desired_run=STOP`, `failed_spec_revision=spec_revision`, `last_error`, and releases the lease. The operator's next Start is a new `spec_revision` with `desired_run=RUN` and zero counters. The loop does not restart a `FAILED` task by itself. That matches today's claim set: `FAILED` is not an `isClaimableActiveState`.

`applied_spec_revision` is how the worker tells "I am already running this spec" from "the row moved". Comparing it to `spec_revision` is the signal to close the current syncer and open one new one.

### Control loop

One loop, the existing claim tick, becomes the only thing that starts and stops a dump. `ClaimRunnableTasks` today mixes three policies (unowned `STARTING`, expired lease, owned idle) and then `startTask` both acquires and launches. The target loop, for each task this worker might own:

1. Read `backup_tasks` and `task_leases` for that id. Memory is the cache filled by that read.
2. `desired_run=STOP`: if this process has a syncer for the task, cancel it and wait until `syncer.Close` has returned. Then, if this worker still holds the lease, write observed `STOPPED`, clear owner and `run_id`, and `Release` this epoch. Do not write `STOPPED` first. A control-plane process with no syncer only writes `desired_run`. It does not write `STOPPED` for a lease it does not hold. That keeps the #174 rule.
3. Observed `FAILED`: do not start while `spec_revision` equals `failed_spec_revision`. Operator Start increments `spec_revision`, zeroes the counters, and, while intent is still mirrored into `state`, writes `STARTING`. The loop starts that `STARTING` row. The same `UPDATE` that sets `FAILED` also sets `desired_run=STOP` and `failed_spec_revision=spec_revision`, including in the budget PR, so a worker that has not picked up the control loop yet cannot leave `desired_run=RUN` on a failed task.
4. `desired_run=RUN` and this worker holds an unexpired lease and a local run is already on `applied_spec_revision`: do nothing. Do not call `Acquire`. Leave the counters alone.
5. `desired_run=RUN` and this worker holds an unexpired lease and no local run (or the run is on an older `applied_spec_revision`): start one run at the current epoch. Load the two counters from the row.
6. `desired_run=RUN` and the lease is missing or expired: `Acquire`. This is the epoch change. Then start one run.

A retryable error updates the two counter columns and `state=RETRY_BACKOFF` in one `UPDATE`, then sleeps inside the same goroutine, same epoch, same renew loop. The tick must not start a second goroutine for that id.

A config change (`spec_revision` greater than `applied_spec_revision`) closes the current syncer and, after `Close` returns, opens one syncer with the new source. Same lease, same epoch, when this worker still holds it. The worker keeps a single active syncer per task. It does not call the next `StartSync` until the previous `Close` has returned.

`GetTask` for the API reads the row and returns it. It does not assign that row back onto a live run's memory copy. `syncTasksFromStore` stops being a second writer of owner, epoch, and state. The loop's read is the writer of the cache.

`persistTaskLocked` stays the function that writes the row, and it still drops the scheduler lock during the round trip. The row is the authority, so a write that loses uses a conditional update: the `UPDATE` matches `spec_revision` and the epoch the worker holds. A stale snapshot does not cover a newer revision. The in-memory generation counter can stay as a local optimization; it is not what another process sees.

All-in-one uses the same loop. The API process and the worker are one process, and the API handlers still only write the row.

### Lease and epoch

`AcquireTaskLease` already increments `epoch` only when the lease is expired. The change is to stop calling it on any path that is not step 6 above. `Renew` keeps the lease through `RETRY_BACKOFF`. `FAILED` and a finished Stop `Release` immediately, which is the 0001 rule.

A same `worker_id` restart before lease expiry keeps the epoch. A crash alone does not bump it. The epoch changes only when the lease row is missing or `lease_expire_at` has passed and `Acquire` takes it, including when the new process uses the same `worker_id`. The loop does not `Release` first in order to start clean. `Acquire` already returns the current epoch when the same worker still holds an unexpired lease.

`MemoryLease.Acquire` has the same rule (new epoch only when the row is missing, empty, or expired). Standalone uses it, as 0001 requires.

### Failure policy

Default: an error that is not on the allowlist writes `FAILED`, sets `last_error`, appends `TASK_FAILED`, releases the lease, and sets `desired_run=STOP` with `failed_spec_revision=spec_revision`. The alert is those three facts: observed `FAILED`, `last_error`, and the `TASK_FAILED` event. The Console already shows the task state and `last_error`. This series adds no notifier and no new metric.

Allowlist, retried inside the same ownership:

| Error | Budget | Why it stays retryable |
| --- | --- | --- |
| `SOURCE_UNREACHABLE` (`net.OpError`, `io.EOF`, `io.ErrUnexpectedEOF`, as `classifySourceError` already marks) | `consecutive_source_failures` reaches 10, then `FAILED` and the lease is released | A source network blip should not page on the first packet loss. The cap is the one #203 was trying to enforce. |
| Transient metadata errors. The substring list lives in `tasks.IsTransientMetadataError`. `meta.IsTransientMySQLError` delegates to it. Same substrings: deadlock, lock wait timeout, connection reset, connection refused, broken pipe, server has gone away, invalid connection, bad connection, read-only, read only, timeout, eof | No task-level cap. The metadata client already retries inside one call. The outer loop retries so a metadata failover returns the task to `RUNNING` without an operator Start and without `FAILED` | Failing the backup because the catalog blipped is the wrong page. The dump's lease stays held. |
| `OBJECT_PURGE_FAILED` | Not a task failure. The next file open retries the object delete, which is the current retention rule | Stopping the dump because the bucket rejected a delete leaves the source unconsumed until an operator notices. |

Not on the allowlist, fail on the first occurrence: `SOURCE_ACCESS_DENIED`, `SOURCE_LOG_BIN_OFF`, `SOURCE_IDENTITY_UNAVAILABLE`, `SEALED_FILE_EXISTS`, non-transient `CHECKPOINT_WRITE_FAILED`, `SEGMENT_NOT_ON_WORKER`, `EPOCH_NOT_ACQUIRED`, MySQL 1236 when no GTID is stored, a lease handoff (this is not a failure of the task: the worker stops and does not write `FAILED`), and any error `classifyRunError` does not recognize. That last set includes a local append or flush error. Step 5 fails it once. It does not stay in `RETRY_BACKOFF`.

MySQL 1236 with no stored GTID is not on the allowlist and does not use the budget of 10. Step 5 (v0.5.47) writes `FAILED` on the first occurrence, releases the lease, and sets `last_error` so the text names 1236 and a purged binlog. One `TASK_FAILED` event is appended. The task does not sit in `RETRY_BACKOFF`. Before step 5, v0.5.36 left that 1236 in `RETRY_BACKOFF` with no cap.

A successful dump ready (`onReady`) sets both counters to 0 in the same write as `RUNNING`.

### Segment identity

One row per `(task_id, source_file, epoch)`.

- `source_file` is the source basename (`mysql-bin.000009`), `NOT NULL`.
- `epoch` is `0` for a historical plain sealed name, and `N` for `.open.eN` / `.sealed.eN`.
- `state` is `OPEN` or `SEALED` for that generation. Open and sealed of epoch N are the same row, not two keys.
- `file_name` stays equal to `source_file` so the existing unique key and the API field keep working through the rollout.
- `file_path` is where the bytes are on this worker. It is not part of the key.
- The object key stays `prefix/cluster_key/server_uuid/` plus the basename that was uploaded. Existing plain-name objects stay. A later epoch keeps `name.sealed.eN`.

`end_pos` rules:

- The live writer sets `start_pos` and `end_pos` from the cursor it flushed.
- Any other writer, including enroll, that must insert a missing row sets them from `DurableCursor` on the local file. If the cursor is not ok, the positions stay `NULL` (column becomes nullable). An upsert that does not know `end_pos` does not assign `end_pos`.
- `end_pos = 0` is never written. A historical `0` is repaired when a worker that can read the bytes opens the task directory. Rows that exist only as objects stay `NULL` or `0` until that happens. SQL cannot see the file.

`binlog.ClassifySegment` is the only name parser. `openEpoch`, the `.open.e` string checks, `isOpenSegmentName`, and the `NOT LIKE '%.open.e%'` SQL are replaced by a predicate on the parse result (or on `state` for a catalog row). Retention deletes a file only when the parse says it is a sealed segment and the age rule says so. A name the parser rejects is not a binlog. `.takeover-*` is in that set.

Startup of a worker, and `materializeUploaded` before `CreateTemp`, removes `filepath.Glob(dir, ".takeover-*")` for that task directory. The temp file from a `kill -9` is gone before the next copy. Retention is not the cleaner.

GTID start does not create `task-{id}.binlog`. The runner waits until the dump names a source file, then opens `{source}.open.e{epoch}`. A rotate that arrives before that name does not seal a placeholder and does not upload one. New seals follow #214: a rotate whose end log_pos is 0 is not appended. An object already uploaded with that no-CRC tail is left as stored. This series does not rewrite it and does not truncate it on read.

The HTTP file object keeps `file_name` (the source name) through this series. `source_file` in SQL equals it. Clients keep reading `file_name`. The Console shows the `epoch` field the file object already returns. This series does not add an HTTP field and does not rename `file_name`. The files API and the Console do not parse names again. Standalone disk listing uses the same parser and `DurableCursor`, so a leftover directory no longer returns positions `0` when the file has a complete event.

### Standalone

No metadata DSN means `s.store == nil`. `persistTaskLocked` returns without a write. `GetTask` reads the memory task, then a leftover `{data_dir}/{id}` that still has segments.

There is one process, so the memory task is the authority, not a cache. The control loop reads and writes that struct. Do not add a second in-memory shadow, and do not add a local database to stand in for metadata MySQL.

`MemoryLease` remains the ownership door. Epoch changes only when that in-process lease is free or expired.

The two counters live on the struct. They disappear when the process exits. A standalone restart starts them at 0. That is the standalone rule. Cluster mode is the mode that persists them.

A leftover directory with segments and no task record stays read-only: list and download work, Start returns `on-disk backup has no task metadata`. Adopt is still the operator's explicit write of source identity. The classifier change applies to those listings: positions come from `DurableCursor` when the bytes are readable.

## Migration and rollout

Schema on this tree is through `000005_binlog_source_epoch_key` (`migrations/000001` through `000005`). `minRequiredSchemaVersion` is 5. A process built from this tree does not start on schema 4. The message contains `schema version too old` and `./migrate up`. Schema 5 includes `000004`, so cluster mode still has `pending_dump_cleanup`. The next new migration is `000006`. `000003_task_desired_and_retry_budget` and `000004_pending_dump_cleanup` were already on main before step 6. `000004` adds `backup_tasks.pending_dump_cleanup`. It is not the segment-key migration. `000005` is.

`ensureSchemaVersion` refuses a version below `minRequiredSchemaVersion`, a dirty `schema_migrations`, or a missing required index. It allows a newer version. Required indexes include `uk_task_file_epoch` and `uk_task_source_epoch`. A binary that still lists `uk_task_file_epoch` refuses to start if that index is dropped. Step 6 keeps the index, so a v0.5.49 process still starts on schema 5. Step 9 drops it no earlier than the release after steps 6 and 7. Steps 7–9 are not done.

`upsertTaskSQL` lists columns. A new column with a default is invisible to an old binary. An old binary keeps writing `state` and does not clear `desired_run`.

Operator order for every schema step: `migrate up`, confirm `schema_migrations` version and `dirty=0`, then restart processes onto the binary that requires that version. Production `migrate down` stays blocked unless `ALLOW_DESTRUCTIVE_MIGRATE=1`. Down scripts in this series do not delete `binlog_files` rows. The 000002 down deletes newer rows that share `(task_id, file_name)`; do not copy that.

During a rolling restart the new API writes both the old `state` transitions and the new columns, until every process is on the binary that reads `desired_run`. An old worker still stops when it sees `STOPPING`. A new worker treats `desired_run` as intent and still understands a `STOPPING` row written by an old control plane. After the floor version is the control-loop binary, a later change can stop mirroring intent into `state`. That later change is not one of the PRs below.

### Ordered PRs

Each PR is releasable on its own. Schema that a binary reads is migrated before that binary starts.

Steps 1–6 are on main. Steps 7–9 are not done. #189 is fixed in v0.5.49 (PR #264). #224 stays open. #205 is fixed in v0.5.48 (PR #261). Those releases are not ADR steps. Steps 7–9 remain next work.

1. **This record.** Documentation only. Landed when this file was accepted. Acceptance: the file is in `docs/adr/` and a reviewer can point at a sentence that does not match `main`.

2. **Migration `000003_task_desired_and_retry_budget`.** Landed in v0.5.44. Additive columns from the table above. No reader in that PR. `minRequiredSchemaVersion` stayed 2 in that release. It is 3 from step 3.
   - Up backfill: `RUNNING`, `STARTING`, `RETRY_BACKOFF`, `LEASE_DEGRADED`, `REBUILDING_FILE` → `desired_run=RUN`. `CREATED`, `STOPPING`, `STOPPED`, `FAILED` → `desired_run=STOP`. Counters and both revisions are 0. `FAILED` rows set `failed_spec_revision=0`; the next operator Start is what arms them, which is the same as today.
   - Down: drop the new columns. `backup_tasks` and `binlog_files` row counts are unchanged.
   - Acceptance a DBA can run: `migrate up`; `SELECT version, dirty FROM schema_migrations` is `(3, 0)`; `SHOW COLUMNS FROM backup_tasks` lists the new columns; `SELECT COUNT(*) FROM backup_tasks WHERE desired_run IS NULL` is 0; a process built from the previous release still passes its health check; `migrate down --steps 1` on a scratch database returns to version 2 and the task count matches the count taken before up.

3. **Persisted budget, and `Acquire` only on a real ownership change.** Landed in v0.5.45. `runTask` reads and writes the columns. The `UPDATE` that sets `FAILED` also sets `desired_run=STOP` and `failed_spec_revision=spec_revision`. Delete `carriedSourceFailures` once the column is populated; on first start after upgrade, if the column is 0 and the event streak is non-zero, copy the streak into the column once. `claimOwnedIdleTasks` does not call `startTask` for a lease this worker already holds. `minRequiredSchemaVersion` becomes 3. Migrate before restart.
   - Acceptance: point a task at a closed port. After each failure, `SELECT consecutive_source_failures, retry_attempt, state, epoch FROM backup_tasks WHERE id=?` shows the counter increasing, `state=RETRY_BACKOFF`, and `epoch` unchanged. `SELECT epoch, owner_worker_id, lease_expire_at FROM task_leases WHERE task_id=?` stays on the same epoch. The 10th failure is `state=FAILED`, `last_error` beginning with `SOURCE_UNREACHABLE:`, and the lease row is released (`owner_worker_id` empty). Kill the worker after failure 4 and start it: the next failure stores 5, not 1. Operator Start stores 0. Epoch increases only if the lease had been released.

4. **Control loop.** Landed in v0.5.46. API Start, Stop, and config update only write the row (and still mirror the old `state` value so a mixed-version worker observes Stop). The worker loop is the only starter. One syncer; `Close` returns before `STOPPED` and before the next open. Migration `000004_pending_dump_cleanup` landed with this step. It adds one column. It is online. A single process can start without it. Cluster mode needs it. It is not step 6.
   - Acceptance: two processes. Stop on the control plane. `SHOW PROCESSLIST` on the source has no `Binlog Dump` for this task's `server_id` at the moment `state` becomes `STOPPED`, and `owner_worker_id` is empty only then. While the row is `STOPPING`, `PUT` a new password and Start. `spec_revision` increases. `SHOW PROCESSLIST` shows one dump, then after the final Stop shows zero. `epoch` is unchanged when the same worker kept the lease. `GET /api/tasks/{id}` returns the new password's user and that same epoch.

5. **Fail-and-alert default.** Landed in v0.5.47 (PR #250). The allowlist above is the one that shipped. MySQL 1236 with no stored GTID is not on it. A runner error that is not on the allowlist fails once, while the task is still supposed to be running: `FAILED`, one `TASK_FAILED`, lease cleared (`owner_worker_id` empty), `desired_run=STOP`, `failed_spec_revision=spec_revision`. `SOURCE_UNREACHABLE` still takes 10 on `consecutive_source_failures`, then `FAILED` and the lease is released. Transient metadata (`tasks.IsTransientMetadataError`; `meta.IsTransientMySQLError` delegates to it, same substrings) has no task-level cap and returns to `RUNNING` without an operator Start and without `FAILED`. `OBJECT_PURGE_FAILED` is not a task failure. A runner error while the task is already `STOPPING` or `STOPPED`, or while the stored row is a newer Stop, stays `STOPPED` and does not append `TASK_FAILED`. `pending_dump_cleanup` is unchanged. No new migration and no new config key. `minRequiredSchemaVersion` stays 3.
   - Acceptance: an error that is not on the allowlist (the e2e can use the sealed-file conflict, which is already permanent, plus one unclassified runner error in the existing scheduler test) stores `FAILED` on the first occurrence, one `TASK_FAILED` event, and a released lease. `SOURCE_UNREACHABLE` still takes 10, with the column from step 3. Kill the metadata database connection once: the task returns to `RUNNING` without an operator Start, and `state` does not pass through `FAILED`.
   - A file/pos resume that gets MySQL 1236 and has no stored GTID: `state=FAILED` on that first error, not `RETRY_BACKOFF`. `last_error` names 1236 and a purged binlog. `task_events` has one `TASK_FAILED` row. The lease is released (`owner_worker_id` empty). There is no new metric and no notifier. The operator signal is `state`, `last_error`, and that event.

6. **Migration `000005_binlog_source_epoch_key`.** Landed. `000004` was already `pending_dump_cleanup`, so this step is migration `000005`. `source_file` is `NOT NULL` after backfill `source_file = file_name` where it was null or empty. `UNIQUE KEY uk_task_source_epoch (task_id, source_file, epoch)` is added. `uk_task_file_epoch` stays. `start_pos` and `end_pos` are nullable. Existing positions are not `UPDATE`d. The catalog writer landed with the key: seal, enroll, and the open-segment upsert identify one segment by `(task_id, source_file, epoch)`. `file_name` stays that same source basename, so both unique keys match one row and a v0.5.49 upsert still updates that row. `minRequiredSchemaVersion` is 5. Enroll still writes `end_pos`; omitting an unknown end is step 7.
   - Down: drop `uk_task_source_epoch`, restore the previous nullability. Do not delete rows. Do not fill a NULL position with 0. If a later writer stored NULL, restoring `NOT NULL` fails.
   - Acceptance: `SELECT COUNT(*) FROM binlog_files WHERE source_file IS NULL OR source_file=''` is 0. `SELECT COUNT(*) FROM binlog_files WHERE source_file <> file_name` is 0. `SHOW INDEX FROM binlog_files` shows both unique keys. A second insert of the same `(task_id, source_file, epoch)` returns duplicate key. A v0.5.49 process still starts, because `uk_task_file_epoch` is still there. This binary refuses schema 4. Scratch-database down leaves the `binlog_files` count unchanged.

7. **One classifier, and no invented `end_pos`.** Not done. #189 is fixed in v0.5.49 (PR #264). That release is not this step. Newly sealed rows take `start_pos` and `end_pos` from the events in the file. An already `UPLOADED` row is not rewritten. Still to do: enroll, retention, replay, resume, the files API, and the Console call `ClassifySegment`. Enroll fills positions from `DurableCursor` and its upsert omits `end_pos` when it does not know it. The disk scan does the same for standalone listings. Step 6 made `start_pos` and `end_pos` nullable. Enroll still writes a number and does not omit `end_pos`.
   - Acceptance: fail over so the same source file seals as `mysql-bin.00000N.sealed.e1`. `SELECT start_pos, end_pos, size_bytes, state, upload_state FROM binlog_files WHERE task_id=? AND source_file=? AND epoch=?` shows `end_pos` equal to the last complete event (the same number `DurableCursor` returns), not 0. `GET /api/tasks/{id}/files` and the Console end-position column show that number. Replay for that source index does not list two copies of the same transactions.

8. **Temp files and the placeholder name.** Not done. #224 stays open. #205 is fixed in v0.5.48 (PR #261). That release is not this step. Startup and `materializeUploaded` still do not delete `.takeover-*`. Retention still does not skip every name `ClassifySegment` rejects. Objects already uploaded with a no-CRC artificial Rotate tail are not rewritten.
   - Acceptance: throttle the object GET, `kill -9` the worker while `.takeover-*` is non-empty, start the worker. The task directory has no `.takeover-*`. The catalog has no row whose `file_name` is that temp name. A GTID-mode task after one rotate: `SELECT COUNT(*) FROM binlog_files WHERE file_name LIKE 'task-%.binlog' AND task_id=?` is 0, and the bucket has no such object. `mysqlbinlog --verify-binlog-checksum` on each newly sealed segment exits 0. An object that already had the no-CRC tail is unchanged (`checksum` and object bytes the same as before the upgrade).

9. **Drop `uk_task_file_epoch`.** Not done. The floor is the first release that ships steps 6 and 7 (the new unique key and the writer that uses it). PR 9 ships no earlier than the release after that. The release note tells operators to confirm no older binary is running before they migrate. `file_name` stays as a column so old `SELECT` lists keep working; dropping the column is a later decision.
   - Acceptance: the release note contains that confirmation step. Operators check the running inventory and proceed only when every process is the floor release or newer. `SHOW INDEX FROM binlog_files` has `uk_task_source_epoch` and does not have `uk_task_file_epoch`. The new process starts. An older binary, if started on purpose, refuses with missing index `uk_task_file_epoch`. That refusal means an older binary is still in the inventory; do not migrate while it is deployed. Do not ship PR 9 in the same release as PR 6 and PR 7.

## Alternatives

**Keep fixing one symptom per release.** That is what v0.5.31–v0.5.40 and #234 did. #234 is the right patch for the live-run reset, and it is already on `main`. Steps 2–5 then landed the columns, the persisted budget, the control loop, and fail-and-alert. Step 6 landed the segment key (`000005`) and the writer that uses `(task_id, source_file, epoch)`. What is still open is the classifier and nullable-aware enroll (step 7) and a `.takeover-*` file left after `kill -9` (#224, step 8). A newly sealed row no longer stores `end_pos` 0 (#189, v0.5.49, PR #264). That release is not step 7. An already `UPLOADED` row is not rewritten. The no-CRC Rotate and stray `task-N.binlog` (#205) are fixed in v0.5.48 (PR #261). That release is not step 8. Steps 7–9 are not done.

**Split the scheduler into lifecycle, lease, and upload packages first.** [0003](0003-do-not-split-scheduler-first.md) rejected this. The control loop is a change in who writes the row, inside the scheduler that already owns the claim tick. A package split does not remove the second writer.

**Make the memory map the authority and treat MySQL as a write-behind copy.** Two processes do not share the map. #174 was a Stop that updated the process that received the HTTP call and left the worker's dump running. 0004 already refuses to use the memory list as the cluster inventory.

**Unique-key the basename only.** The historical object is the plain source name, and epoch 0 is that object. Open and sealed of one epoch are one generation: two basenames for one key, with `state` moving from `OPEN` to `SEALED`. A basename key would store both, and retention would have to know they are the same span.

**Leave the default as "retry unless the error is on the permanent list".** That was the code after #178, before step 5. An error nobody had classified held the lease and sat in `RETRY_BACKOFF`. The claim loop used to refresh that run every 2 seconds; #234 stopped the refresh, and the uncapped wait remained. Step 5 rejected this. An unclassified error now fails once. The allowlist keeps the blips that operators already decided should not page.

**Fail `SOURCE_UNREACHABLE` on the first error.** A dropped route would page and stop the backup. The cap of 10 stays. It has to be the column, not a goroutine local and not the last 200 events.

**Add a local database for standalone so the budget survives restart.** Standalone is one process. A second store reintroduces structure A for the mode that does not have a cluster. The counters reset on process exit.

## Risks

- Step 5 shipped in v0.5.47. Tasks that used to sit in `RETRY_BACKOFF` on an unclassified error, and a file/pos task that hits MySQL 1236 with no stored GTID, go `FAILED` on the first occurrence and release the lease. `last_error` for that 1236 names 1236 and a purged binlog. The v0.5.47 release note lists the allowlist and this 1236 change. The alert is `FAILED`, `last_error`, and `TASK_FAILED`.
- Mixed versions: a new API with an old worker is safe only while the new API still writes `state` the way the old worker reads it (`STARTING`, `STOPPING`). PR 4 does that. A new worker with an old API is safe because the old API's `state` writes are still what the compatibility branch of the loop honors.
- Newly sealed rows no longer store `end_pos` 0 (#189, v0.5.49, PR #264). That release is not step 7. An already `UPLOADED` row is not rewritten, so a row an older binary stored with `end_pos` 0 stays wrong until a later repair. `000004_pending_dump_cleanup` does not fix those rows. An object-only row stays wrong until a worker materializes it or the operator accepts the old number. The files API should show `NULL` rather than 0 once the column is nullable, including for rows not yet repaired, so the Console stops displaying a fake end position. Step 7 is not done.
- `000002` down deletes rows. A down written the same way for `000005` would drop epoch segments. The down in step 6 only drops the new index and the nullability change. `000004_pending_dump_cleanup` down drops only that column.
- Conditional updates in `persistTaskLocked` can return "lost update" under load. The loop treats that as "read the row again", not as a task failure.
- Standalone positions changing from 0 to a real cursor is a files-API change for leftover directories. Checkpoints stay absent. Start of a leftover directory stays refused.

## Resolved questions

Accepted with this record on 2026-10-06. The decisions are in the sections named here.

1. MySQL 1236 with no stored GTID is not on the allowlist. The first occurrence is `FAILED`, and `last_error` names 1236 / purged binlog. Failure policy, PR 5.
2. The alert is observed `FAILED`, `last_error`, and the `TASK_FAILED` event. No notifier and no new metric in this series. Failure policy.
3. Already-uploaded segments with a no-CRC artificial Rotate tail are not rewritten. New seals follow #214 and never create `task-{id}.binlog`. Segment identity, PR 8.
4. A same `worker_id` restart before lease expiry keeps the epoch. A crash alone does not bump it. Lease and epoch.
5. The floor for dropping `uk_task_file_epoch` is the first release that ships PR 6 and PR 7. PR 9 ships no earlier than the release after that. That release note tells operators to confirm no older binary is running. PR 9.
6. HTTP keeps `file_name`. The Console shows the existing `epoch` field. Segment identity.
