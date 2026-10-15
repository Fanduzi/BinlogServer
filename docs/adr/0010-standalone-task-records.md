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
| `BINLOG_SERVER_TASK_KEY` env var | base64 (std or URL, padded or not) or 64 hex chars that decode to exactly 32 bytes | For systemd `EnvironmentFile=` / container secrets |
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

- Create, Update/Configure*, Start, Stop, Adopt, Delete: save the projected snapshot first. If the save fails, the action is refused with the existing `ErrTransitionNotRecorded` (API 503/500 as today for a refused metadata write), memory is unchanged, a `*_REJECTED` event is recorded, and `TASK_RECORDS_FAILED` (subject `write`) opens. The next successful save closes it.
- A worker-side terminal failure (`FAILED`, which sets desired STOP and `failed_spec_revision`) changes the record projection; it is saved best effort from `persistTaskLocked`. A failed save there opens `TASK_RECORDS_FAILED/write` and is retried on the next save; the run itself is not failed because of it.
- Observed state, owner, epoch, retry counters, `last_error` of a retryable error, liveness: never written. They are process-local in standalone (0005: the two counters reset on restart; that stays).

## Resume position on restart

The record never carries a position. At startup, for each record:

1. **The task directory has segments**: the task is restored the way adopt restores a leftover directory today (#388 QA: adopt + Start resumed and restores matched on MySQL 8.0 and MariaDB 11.4): `KeepLocalSegments`, and the runtime start is computed **fresh from disk** by the existing adopt rule `diskResumeStart` — the end log_pos of the last complete event (`DurableResume`), else a sealed segment's Rotate target, never a file size (#356). The epoch is raised above every leftover `.open.eN` (`prepareDiskResumeEpochLocked`), so a new segment sorts after them. This disk start lives in memory only.
2. **No segments, the task never ran** (desired STOP, never started): restored as `CREATED`/`STOPPED` with the operator start. Nothing is started.
3. **No segments but desired RUN**: the operator start is safe only when it is not `LATEST` (`FILE_POS`/`GTID` re-read from the operator's point; nothing was stored, so nothing is duplicated, and a purged point fails loudly with 1236 as today). `LATEST` would silently skip everything written while the process was down, so the task is restored `FAILED` with `last_error` `RESUME_POSITION_UNKNOWN: ...` (TASK_FAILED, CRITICAL). The operator's Start accepts the new tip explicitly.
4. **Segments exist but no resume point can be read** (`diskResumeStart` fails): if every segment is only magic or a format-description header (`binlog.PreambleOnly`: nothing was ever stored) and the operator start is not `LATEST`, rule 3 applies and the operator start is used. Otherwise the task is restored `FAILED` with `RESUME_POSITION_UNKNOWN` naming the directory. Never a guessed position.

After restore, every desired-RUN task is started through the normal Start path (the one-shot boot claim in standalone), so it gets the same validation, lease and alerts as an operator Start. A start that cannot even be attempted (for example an invalid source in the record) becomes `FAILED` with that error, never `STOPPED`.

Known limit, unchanged from adopt: the restored run is `FILE_POS`, so a GTID task resumes by position after a restart; GTID tracking for a later source switch starts again from that run. Restoring the executed set from the segment ledger is a follow-up that belongs with the resume-position work of #391 (that code is not touched here).

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
| `write` | a save failed (disk full, permissions, fsync) | the action was refused | the next save succeeds |

"Writes refused" uses the same `ErrTransitionNotRecorded` path: Create/Adopt/Update/Start/Stop/Delete return an error saying the records are locked and why; read paths keep working.

## Key rotation

`BINLOG_SERVER_TASK_KEY_PREVIOUS` / `standalone.task_key_previous_file` (same forms and permission rule). At startup, a file whose `key_id` matches the previous key is decrypted with it and immediately re-saved with the current key (atomic write), and the log line names both key ids, never the keys. Procedure: generate new key → configure it as current and the old one as previous → restart → confirm the log line → remove the previous key. A file already on the current key ignores the previous key.

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
- **Rollback**: an attacker (or an operator restoring an old `data_dir` copy) can replace the file with an older valid one; that restores older intent (for example a deleted task, or an old password). Mitigation is the startup log line with `saved_at` and task count; a monotonic counter outside `data_dir` is out of scope.
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
3. Release note: new default `on`; standalone users must configure `BINLOG_SERVER_TASK_KEY` or `standalone.task_key_file` (until then `TASK_RECORDS_FAILED/key_missing` is open), and re-adopt existing leftovers once.
