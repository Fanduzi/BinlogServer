# Standalone keeps an encrypted task record so a restart resumes replication

Status: Proposed (2026-10-15). Amends [0005](0005-single-task-authority-and-segment-catalog.md) §Standalone and its rejected alternative "Add a local database for standalone". Decision by Adrian on #385: standalone (no `meta_dsn`) must resume replication by itself after a restart, with the task configuration stored encrypted under `data_dir`. [0009](0009-backup-assurance.md) design §19 (`TASK_UNADOPTED`, #388) stays and keeps its meaning for a directory that has no record.

## Problem

0005 made the in-memory task the only record in standalone. #385 showed what that costs: every restart (deploy, crash, host reboot) turns each task into a leftover `{data_dir}/{id}` directory. It is `STOPPED`, its source is empty, Start returns 400, and the backup silently falls behind the source until someone adopts it and types the source password again. #388 made it loud (CRITICAL `TASK_UNADOPTED`) but replication still stops on every restart. For a backup tool, "a restart stops the backup until a human acts" is the defect, not the alert.

## Decision

Standalone writes one encrypted **task record file**, `{data_dir}/.tasks.enc`, and reads it once at startup.

1. The file is a **write-through journal of the one in-memory authority**, not a second authority. There is one process. Memory stays the record at runtime (0005 still holds); the file is written in the same commit as the memory change and is read only at startup. Nothing ever merges file and memory while running, so structure A of 0005 (two writers of one task) does not come back.
2. It stores **operator intent and identity only**: the task spec, the source password, desired state, and a terminal failure. It never stores a binlog position. The resume position after a restart always comes from the bytes on disk (§Resume position).
3. The file is **authenticated-encrypted** (AES-256-GCM) with a key that never lives in `data_dir` or in YAML (§Key).
4. Every way the file cannot be used — no key, wrong key, corrupt, unknown version, write failure — is **loud**: a CRITICAL finding, plus the leftover directories still raise `TASK_UNADOPTED`. None of them is a silent `STOPPED`, and none of them overwrites the file.

Cluster mode and any process with `meta_dsn` are unchanged: metadata MySQL is the only authority there, and this file is neither read nor written.

## Key

Source of the 32-byte key, first match wins:

| Source | Form | Notes |
| --- | --- | --- |
| `BINLOG_SERVER_TASK_KEY` env var | base64 (std or URL, padded or not) or 64 hex chars that decode to exactly 32 bytes | For container secrets. **The key file is the recommended source**: an environment variable can be read from `/proc/<pid>/environ` by the same user and root. The process unsets `BINLOG_SERVER_TASK_KEY` and `BINLOG_SERVER_TASK_KEY_PREVIOUS` right after loading them (#393 QA P3), which narrows but does not remove that exposure (the initial environment block of the process is not rewritten) |
| `standalone.task_key_file` (config, or `BINLOG_SERVER_STANDALONE_TASK_KEY_FILE`) | a file holding the same text | Must be a regular file with no group/other permission bits (`0600` or `0400`). Wider permissions refuse the key (loud), they are not "fixed" silently |
| KMS | — | Not in this change. The loader is one function (`LoadKey`) returning `[]byte`; a KMS source is a third case there and needs no format change |

The key is never accepted from the YAML file itself (a key next to the data it protects is not a key), never logged, never returned by the API, and never written under `data_dir`. A key that decodes to anything but 32 bytes is refused. `binlog-server task-key generate --out <path>` writes a new random key file with mode `0600` (refuses an existing path) and prints only the path and key id.

The **key id** is the first 8 bytes of `SHA-256("binlog-server task records v1" || key)`, hex. It is stored in the file header so that "wrong key" and "corrupt file" are told apart without trial decryption; it reveals nothing usable about the key.

There is deliberately no fallback to `--encryption-key` (the metadata source-password key): different purpose, different lifecycle, and that flag is only required in PRODUCTION cluster deployments.

## File format

```
{data_dir}/.tasks.enc   (mode 0600; the leading dot keeps it out of the on-disk task listing)

{
  "format": "binlog-server/task-records",
  "version": 1,
  "key_id": "<16 hex>",
  "nonce": "<base64 12 bytes>",
  "ciphertext": "<base64 AES-256-GCM(plaintext) incl. 16-byte tag>"
}
```

- AAD = `format || "\n" || version || "\n" || key_id`, so the header cannot be swapped or downgraded without failing authentication.
- A fresh random nonce per write (crypto/rand). Writes are rare (operator actions and terminal failures), so the 2^32 random-nonce bound per key is not approached.
- The plaintext is JSON:

```json
{
  "seq": 7,
  "saved_at": "2026-10-15T10:00:00Z",
  "tasks": [{
    "id": "3", "name": "orders", "cluster_key": "orders-prod",
    "source": {"host": "...", "port": 3306, "user": "repl", "password": "...", "flavor": "mysql", "server_id": 1001},
    "start": {"mode": "gtid", "gtid_set": "..."},
    "storage": {"retention_days": 7},
    "desired_run": "RUN",
    "spec_revision": 4,
    "adopted": false,
    "failed": {"spec_revision": 4, "last_error": "SOURCE_ACCESS_DENIED: ..."},
    "created_at": "...", "updated_at": "..."
  }],
  "deleted": [{"id": "2", "deleted_at": "..."}]
}
```

- Whole-file encryption, not just the password: hosts, users and names are also not readable from a copied `data_dir`.
- `start` is the **operator's** start (what Create/Update/Adopt-with-explicit-start asked for). It is the first-run start only; see §Resume position. A start computed from disk (adopt, or a previous restore) is never written here (`start` is then omitted).
- `seq` keeps ids monotonic across deletes, so a deleted id is not reused even if its directory is moved away.
- Unknown `version` → refuse to read (loud), never best-effort parse. A newer binary may add fields; an older binary refuses a newer version rather than dropping fields on its next write.
- **Atomic write**: marshal → encrypt → write `{data_dir}/.tasks.enc.tmp-<rand>` with `O_EXCL`, mode `0600` → `fsync` → `rename` over `.tasks.enc` → `fsync` the directory. A crash leaves either the old or the new file, never a torn one. Startup removes stale `.tasks.enc.tmp-*`.
- Size is a few KiB per hundred tasks; the whole file is rewritten on every save.

## What is written, and when

The file is saved inside the two commit points that already are "the only way an operator action changes a task" (`commitTransitionLocked`, `commitDeleteLocked`, #364), **before** memory changes — the same persist-first rule cluster mode follows with MySQL:

- Create, Update/Configure*, Start, Adopt: save the projected snapshot first. If the save fails, the action is refused with `ErrTransitionNotRecorded` (HTTP **503**, see §API status), memory is unchanged, a `*_REJECTED` event is recorded, and `TASK_RECORDS_FAILED` (subject `write`) opens.
- **Stop and Delete are never refused because of the record** (#393 QA P2-1). The usual cause of a failed save is a full `data_dir`, which is exactly when an operator wants to stop a task. Stop and Delete try the save first; if it fails they are applied **in memory anyway**, an event records that the record was not saved, and `TASK_RECORDS_FAILED/write` opens with a detail that names the task and says what a restart would do with the stale file: "task N was stopped in memory but the record still has desired RUN: a restart now resumes it" (Delete: "a restart now brings task N back"). The same holds while the record is locked (§New finding kind): a Stop is applied in memory and the detail says the file was not changed.
- A worker-side terminal failure (`FAILED`, which sets desired STOP and `failed_spec_revision`) changes the record projection; it is saved best effort from `persistTaskLocked`. A failed save there opens `TASK_RECORDS_FAILED/write`; the run itself is not failed because of it.
- **Retry**: while `write` is open, the scheduler retries the save of the current memory projection every 30 s (and on the next action). The snapshot is always projected from memory, so the first successful save carries every change made meanwhile (a Stop, a Delete, a terminal FAILED) and closes `write`. Until then, a restart resumes from the older file; the detail says so (#393 QA P3: a FAILED task whose save failed would otherwise run again after a restart).
- The `write` detail also carries the `seq` and `saved_at` of the last save that did land, so an operator sees how stale the file is without reading logs.
- Observed state, owner, epoch, retry counters, `last_error` of a retryable error, liveness: never written. They are process-local in standalone (0005: the two counters reset on restart; that stays).

## Resume position on restart

The record never carries a position. At startup, for each record:

1. **The task directory has segments**: the task is restored the way adopt restores a leftover directory today (#388 QA: adopt + Start resumed and restores matched on MySQL 8.0 and MariaDB 11.4): `KeepLocalSegments`, and the runtime start is computed **fresh from disk** by the existing adopt rule `diskResumeStart` — the end log_pos of the last complete event (`DurableResume`), else a sealed segment's Rotate target, never a file size (#356). The epoch is raised above every leftover `.open.eN` (`prepareDiskResumeEpochLocked`), so a new segment sorts after them. This disk start lives in memory only.
2. **No segments, the task never ran** (desired STOP, never started): restored as `CREATED`/`STOPPED` with the operator start. Nothing is started.
3. **No segments but desired RUN**: `FILE_POS`/`GTID` re-read from the operator's point (nothing was stored, so nothing is duplicated, and a purged point fails loudly with 1236 as today). **`LATEST` resumes as `LATEST`** (changed after #393 QA gap (b); the first draft restored it `FAILED`). Decision and why it is safe: "no segment" means the backup holds no transaction of this task, so there is no stored history for a new tip to be discontinuous with; replay and `/window` only ever cover what is on disk, so no restore can silently span the downtime. What is lost is the same thing a `LATEST` Start always gives up: transactions committed before the tip it resolves. The cost of `FAILED` was concrete and recurring: every idle `LATEST` task (no event yet) failed after every deploy and needed a manual Start that does exactly this. The resume is not silent: the event log records `TASK_RESUMED_AT_LATEST` with the time of the last record save, and the startup log names the task. A directory that holds segments is never in this case (rule 1 or 4 applies), so a task with history can never jump to a new tip.
4. **Segments exist but no resume point can be read** (`diskResumeStart` fails): if every segment is only magic or a format-description header (`binlog.PreambleOnly`: nothing was ever stored), rule 3 applies to the operator start (including `LATEST`). Otherwise the task is restored `FAILED` with `RESUME_POSITION_UNKNOWN` naming the directory. Never a guessed position.

**GTID tasks keep their executed set** (#393 QA P2-3, closes gap (a)). A restored run is `FILE_POS` from disk, and in the first draft its executed GTID set was empty until the next switch, so a source switchover during or after the downtime failed with `SOURCE_SWITCHOVER` "no GTID set" where a running task would have continued. Now, when the operator start is `GTID`, the restored start carries a GTID set built from the stored bytes with #394's ledger: the operator's seed ∪ `Ledger.SealedStored` (every sealed segment's transactions) ∪ the open tail's complete transactions (`Ledger.ResumeGTID`). The runner seeds its executed set from it, so checkpoints keep a GTID set, the 1236 fallback works, and a switchover plans from the stored set. The position still comes from disk; the set comes from the same bytes, never from the record. MySQL only: the ledger reads MySQL GTID events (`SealedText` is empty for MariaDB), so a MariaDB GTID task keeps the seed plus whatever the open tail's header lists, as before.

After restore, every desired-RUN task is started through the normal Start path (the one-shot boot claim in standalone), so it gets the same validation, lease and alerts as an operator Start. A start that cannot even be attempted (for example an invalid source in the record) becomes `FAILED` with that error, never `STOPPED`.

Known limit: a record with desired RUN whose directory was moved out of `data_dir` (how 0009 §19 retires a directory) falls into rule 3 and re-dumps from the operator start into a new directory. The record does not know that segments once existed. The operator who retires a directory should Delete the task first (a tombstone keeps it retired); a "directory missing" finding is tracked in #397.

## Deleted tasks

Delete in standalone removes the task from the record and adds a **tombstone** (`deleted[]`). The directory is **kept**: deleting backup bytes as a side effect of an API call is never safe, and the bytes may be the only copy. A tombstoned directory:

- still lists read-only (files, download, replay work as for any leftover);
- does **not** raise `TASK_UNADOPTED` (the operator said they are done with it; 0009 §19's "a delete also opens it" no longer applies when records are on);
- can be adopted again (adopt clears the tombstone);
- loses its tombstone when the directory no longer exists at startup.

Removing the directory stays an explicit operator step (`rm`/move out of `data_dir`).

## Migration of existing leftover directories

A directory left by a binary without records (or created while records were off) has no record. It keeps raising `TASK_UNADOPTED`. The operator migrates it with the existing `POST /api/tasks/{id}/adopt` (source including password, optional name/cluster_key/storage). With records on, adopt is persisted, so this is a one-time step: the next restart resumes it automatically. No separate import CLI is added: adopt already validates the source and is the one door for "attach identity to a directory"; a CLI would be a second writer of the same file. A batch can be scripted against the API.

## Interaction with TASK_UNADOPTED

- A directory with a record is never unadopted (it is a task again).
- A directory with no record and no tombstone raises `TASK_UNADOPTED` as in #388, including when the record file cannot be read (no key / wrong key / corrupt): then every directory is unadopted **and** `TASK_RECORDS_FAILED` names the cause. The detail of both says what to fix.
- Records `off`: #388 behaviour exactly.

## New finding kind

`TASK_RECORDS_FAILED` — CRITICAL, task `_cluster`, signal kind (works with the evaluator off, shadow and on; Managed like the other P2-4 signal kinds). Subject:

| Subject | When | Effect | Clears when |
| --- | --- | --- | --- |
| `key_missing` | records on, no key configured | No record file exists: tasks still work but will not survive a restart (detail says so). A record file exists: nothing is restored; record-changing actions are refused so the file is not overwritten | a key is configured and the process restarts |
| `key_invalid` | the key source exists but is unusable (bad encoding, not 32 bytes, key file mode wider than 0600) | as `key_missing` | fixed key, restart |
| `wrong_key` | `key_id` matches neither the key nor the previous key | nothing restored; writes refused; file untouched | the right key, restart |
| `corrupt` | not JSON, unknown format/version, or GCM authentication fails with a matching key id | nothing restored; writes refused; file untouched. Detail names the path and that a copy should be taken before repair | file restored from backup (or moved away to start empty), restart |
| `write` | a save failed (disk full, permissions, fsync) | Create/Adopt/Update/Start refused; Stop/Delete applied in memory, the detail says what a restart would do | the next save (action or 30 s retry) succeeds |

Startup subjects (`key_missing`, `key_invalid`, `wrong_key`, `corrupt`) hold one at a time: when a start finds one, the others an earlier start left open in `.health.json` are closed (#395 QA P3).

"Writes refused" uses the same `ErrTransitionNotRecorded` path: Create/Adopt/Update/Start return an error saying the records are locked and why; Stop and Delete are applied in memory (§What is written); read paths keep working. Error texts and finding details never contain the password or key (tested).

## API status

A refusal caused by the record (locked, or a save that failed) returns **503 Service Unavailable** with code `TASK_RECORDS_UNAVAILABLE` and the reason, not a generic 500: the request was valid, the server cannot persist it right now, and it can be retried once the cause (key, file, disk) is fixed. 409 was considered and rejected: nothing about the request conflicts with the task's state. This includes Start, which earlier answered 400 for every error (#395 QA P3). Other refused metadata writes keep their existing status.

## Findings when data_dir is full

*Follow-up from the #395 QA review (P2).* The health store (`.health.json`), the legacy findings file (`.findings.json`) and `.tasks.enc` share `data_dir`. When the disk is full, the store write that would open `TASK_RECORDS_FAILED/write` fails with ENOSPC too. Before this follow-up, `GET /api/findings` read only the store, so it stayed empty, and `GET /api/health` returned a static `ok`. A full disk is exactly the case `write` exists for.

Decision: **the in-memory finding state is authoritative; persistence is best-effort.**
- When the evaluator's store write fails (any error except fenced), it keeps the open set that write would have produced, per task, and keeps evaluating on top of it. `GET /api/findings` and the task findings route serve that set until a write lands; the store's copy is used again after a write succeeds. The legacy book already kept its open findings in memory.
- A synthesized CRITICAL finding `FINDINGS_PERSIST_FAILED` (task `_cluster`, never stored) is listed while either the health store or the findings file is failing. Its detail names the failing write and its error (ENOSPC), and the tasks whose health is unsaved, and says the state is lost on a restart until a write lands. It closes on its own once writes succeed. An unsaved entry for a task that no longer exists (a refused create still marks its id) is dropped.
- `GET /api/health` is **503 `{"status":"degraded","reasons":[...]}`** while any CRITICAL finding is open, read from the same in-memory view; otherwise 200 `{"status":"ok"}`. `/healthz` stays the liveness probe and is unaffected, so a full disk does not get the process restarted in a loop.
- Reserving space (a preallocated ballast file in `data_dir`, released on ENOSPC) was considered and not done. It only postpones the failure by its size, a binlog dump would consume the freed space within seconds, and it adds a file the operator must know not to delete. Serving from memory covers the case without it; the operator frees space, and the record retry (30 s) and the next sweep persist the state.

Tested with a fake store returning ENOSPC (unit) and a 30 MB tmpfs `data_dir` (`smoke-standalone-full-disk.sh`). With the disk full, a create gets 503, the findings list `TASK_RECORDS_FAILED/write` and `FINDINGS_PERSIST_FAILED`, and health is 503 degraded. After space is freed, both findings close and health is ok.

## Single writer of data_dir

Two standalone processes on one `data_dir` (a double start, or an old unit left running) would each rewrite `.tasks.enc` (last writer wins, memory and file drift silently) and both dump into the same task directories (#393 QA P2-2). At startup a standalone process takes an exclusive, non-blocking `flock` on `{data_dir}/.tasks.lock` (created `0600`, holding the pid for diagnosis) and keeps it for its lifetime. If the lock is held, startup **fails and the process exits non-zero** with a message naming the lock file and the pid written in it; nothing is restored or started. The lock is taken whether records are on or off, because the dump collision does not depend on records; it is not taken with `meta_dsn` (cluster workers already fence by lease). The kernel drops the lock when the process dies, including `kill -9`, so a crash never leaves a stale lock. `flock` is advisory and local; a `data_dir` on NFS is not protected (documented, as before).

## Key rotation

`BINLOG_SERVER_TASK_KEY_PREVIOUS` / `standalone.task_key_previous_file` (same forms and permission rule). At startup, a file whose `key_id` matches the previous key is decrypted with it and immediately re-saved with the current key (atomic write), and the log line names both key ids, never the keys. Procedure: generate new key → configure it as current and the old one as previous → restart → confirm the log line → remove the previous key. A file already on the current key ignores the previous key.

If that re-save fails (#393 QA P3), the old file stays as it was (the atomic write never touches it), the tasks are restored and run from it, `TASK_RECORDS_FAILED/write` opens saying the file is still on the previous key id, and the 30 s retry saves it on the current key as soon as it can. Do not remove the previous key until the rotation log line appears.

## Off and on

`standalone.task_records`: `on` (default) | `off`.

- `on`, standalone: everything above.
- `off`: exactly today's behaviour (memory only, #388 `TASK_UNADOPTED` after restart). An existing `.tasks.enc` is neither read nor written nor deleted; turning it back `on` resumes from what it held then (it may be stale — the startup log says the file's `saved_at`).
- `meta_dsn` set: the option is ignored. If a `.tasks.enc` exists, a startup log line says it is not used (moving standalone → cluster is a separate, explicit migration; out of scope).

## Security review

- **Permissions**: `.tasks.enc` and temp files are created `0600`; the key file must be `0600`/`0400` (checked, not chmod-ed). `data_dir` permissions are not changed by this feature.
- **Secrets in logs**: the key, the plaintext and passwords are never logged or put in events/findings. Logs and findings carry the path, key id, `saved_at`, task count and error class. `Task` JSON already omits the password; a test asserts no password appears in the logs of a restore, a failed restore and a rotation.
- **Backups of `data_dir`**: a copy of `data_dir` carries the encrypted file but not the key. Restoring a backup on a new host needs the key from the secret store; without it the host comes up loud (`key_missing`), with leftovers unadopted, and can still serve the segments read-only. Operators must back up the key separately; losing it means re-adopting each directory (the bytes are not encrypted, only the task record is).
- **Tampering**: GCM authenticates header and body; a flipped byte is `corrupt`, never a partially applied record.
- **Rollback**: an attacker (or an operator restoring an old `data_dir` copy) can replace the file with an older valid one; that restores older intent (for example a deleted task, or an old password). Mitigation is the startup log line with `saved_at`, `seq` and task count, plus `seq`/`saved_at` in the `write` detail; a monotonic counter outside `data_dir` is out of scope.
- **Concurrent writers**: the `.tasks.lock` flock (§Single writer) keeps a second process from rewriting the file.
- **Memory**: the password is in process memory as today; nothing new.
- **Scope creep**: the file never stores checkpoints, positions, findings or events, so it can never become the "local database for metadata" 0005 rejected.

## Amendment to 0005

0005 §Standalone "do not add a local database to stand in for metadata MySQL" and the alternative "Add a local database for standalone so the budget survives restart" are amended: standalone keeps one encrypted, write-through record of operator intent (spec, password, desired state, terminal failure). Still not added: a local database, a second runtime authority, persisted retry counters, checkpoints or catalog rows. The in-memory task remains the authority at runtime.

## Alternatives

- **Keep #388 only (manual adopt after every restart).** Rejected by Adrian: a restart must not stop the backup.
- **Store only the password encrypted, the rest plaintext.** Leaks topology from a `data_dir` copy, and still needs the same key handling; no simpler.
- **SQLite / bbolt in `data_dir`.** A database invites more state into it (exactly what 0005 warned against) and adds a dependency for a file written a few times a day.
- **One file per task.** Delete becomes unlink + directory fsync, ids need their own counter file, and a key change is N writes that can half-complete. One file is one atomic rename.
- **Store the resume position in the record.** Rejected by the #385 decision and by #356: the bytes on disk are the truth; a stored position goes stale the moment the next event is written.
- **Refuse to start without a key.** Would turn a missing secret into a total outage, including read-only restore access to the segments. Loud finding plus refused writes keeps the data reachable.

## Rollout

1. This record (docs PR).
2. Implementation PR: `internal/taskrecords` (key, format, atomic store, rotation), scheduler wiring at the two commit points and `Restore`, the `TASK_RECORDS_FAILED` kind, `standalone.task_records`/key config, `task-key generate`, tombstones in the `TASK_UNADOPTED` fact. Tests first: unit tests for format/key/rotation/atomicity, scheduler tests for restore cases 1–4 and refused writes, and e2e on MySQL 8.0 and MariaDB 11.4: `kill -9` → restart → auto-resume with restore checksums matching, wrong key, corrupt file, missing key.
3. Release note, **at the top** (#393 QA P3): new default `on`, so every existing standalone install opens a CRITICAL `TASK_RECORDS_FAILED/key_missing` at upgrade until a key is configured. Run `binlog-server task-key generate --out <path>`, set `standalone.task_key_file`, restart, and re-adopt existing leftovers once. A second standalone process on the same `data_dir` now refuses to start (§Single writer).

QA review of this record (#393): PASS with changes. P2-1 (Stop/Delete never refused), P2-2 (flock), P2-3 (GTID set from the ledger) are in the sections above; the P3s on rotation failure, FAILED-save retry, env key, release note, rollback visibility and the wider no-secrets test are folded in; the moved-directory P3 is recorded as a known limit.
