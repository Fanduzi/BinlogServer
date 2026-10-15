# Binlog Server v0.5.67

Release date: 2026-10-15

v0.5.67 brings automatic task resume to standalone, using task records encrypted at rest (#385, ADR 0010). It also lands phase 2 of the ADR 0009 health evaluator (P2-1..P2-4), which **defaults to `off`**. It fixes truncated sealed segments (#381), a lost FAILED state (#389), and resume and checkpoint problems after a switchover or a large transaction (#391). It adds migration `000009`, which is additive and optional: `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51.

**Read the upgrade notes before upgrading a standalone server.** Configure the task key before the upgrade restart. Every task carried over from v0.5.66 must then be adopted once (bulk: `scripts/adopt-leftovers.sh`).

## Upgrade Notes

### `/api/health` can now return 503

`GET /api/health` returns **503 `{"status":"degraded","reasons":[...]}`** while any CRITICAL finding is open. Otherwise it returns 200 `{"status":"ok"}`. It is computed from the in-memory finding state, so it still degrades when `data_dir` is full and findings cannot be persisted (see `FINDINGS_PERSIST_FAILED` below).

**Liveness probes must use `GET /healthz`**, which is unchanged (200 `ok`). If a probe points at `/api/health`, any CRITICAL finding will get the process restarted. Use `/api/health` for alerting or readiness only.

### Standalone: configure the key BEFORE the upgrade restart, then adopt every task once

`standalone.task_records` defaults to `on`. A standalone server (no `meta_dsn`) with **no key configured opens CRITICAL `TASK_RECORDS_FAILED/key_missing`, and `/api/health` reports degraded.** Tasks keep running only until the next restart. Any restart without a key turns every task into a STOPPED leftover directory.

**Every standalone upgrade from v0.5.66 turns all its tasks into leftovers, key or not.** v0.5.66 kept no record. After the upgrade restart, each task lists as STOPPED with an empty source, replication for it is stopped, and it raises CRITICAL `TASK_UNADOPTED`, which degrades `/api/health` in every `health.evaluator` mode (#399). **Every task directory must be adopted once.** After that, restarts resume the tasks automatically.

Upgrade steps:

1. **Before stopping v0.5.66**, generate a key with the new binary (it works while the old server is still running). The command writes the file with mode `0600`, refuses to overwrite an existing path, and prints only the path and key id:
   ```bash
   ./binlog-server-v0.5.67 task-key generate --out /etc/binlog-server/task.key
   ```
   Keep the file outside `data_dir`, and back it up separately. Without the key, `.tasks.enc` cannot be read.
2. **Before the restart**, point the config at the key. Use **one** of:
   - **Key file (recommended):** `standalone.task_key_file: /etc/binlog-server/task.key` in the config, or `BINLOG_SERVER_STANDALONE_TASK_KEY_FILE=/etc/binlog-server/task.key`. The file must be a regular file with mode `0600` or `0400`. Wider permissions refuse the key with `TASK_RECORDS_FAILED/key_invalid`; the mode is never changed for you.
   - **Environment:** `BINLOG_SERVER_TASK_KEY=<base64 or 64 hex chars, 32 bytes>`, for container secrets. The process unsets the variable right after reading it. A file is still preferred, because the environment of a running process is visible to the same user and root.
   - A key in the YAML file itself is never accepted.
3. Replace the binary and restart. No `key_missing` opens, and every task directory raises `TASK_UNADOPTED`.
4. **Adopt every directory.** Adopt needs the source (including the password) **and `cluster_key`**: without it the API answers 400 `cluster_key is required`. `name` and `storage` are optional. To adopt them in bulk with `scripts/adopt-leftovers.sh`, which ships in the release tarball (needs `curl` and `jq`):
   ```bash
   scripts/adopt-leftovers.sh --api http://127.0.0.1:8080              # lists the leftovers and prints a map template
   # fill in name, cluster_key and source for each id; chmod 600 the file (it holds passwords)
   scripts/adopt-leftovers.sh --api http://127.0.0.1:8080 --map adopt-map.json --start
   ```
   The script adopts every task with an open `TASK_UNADOPTED` and starts it with `--start`. It exits non-zero while any directory is left unadopted. Auth: `BINLOG_SERVER_API_KEY` or `BINLOG_SERVER_BEARER_TOKEN`. A single directory can also be adopted by hand: `POST /api/tasks/{id}/adopt` with `{"name":"...","cluster_key":"...","source":{...}}`, then `POST /api/tasks/{id}/start`.
5. Check that `/api/findings` has no `TASK_UNADOPTED` and that `/api/health` is 200. From now on, every restart resumes the tasks automatically.

If the key was skipped and added later, every task created in the meantime is a leftover after the restart that adds the key. It raises `TASK_UNADOPTED`, so adopt it as in step 4.

To run without records, set `standalone.task_records: off`. Behaviour is then as in v0.5.66: tasks are kept in memory only and leftover directories need adopt after every restart. Leftovers still raise `TASK_UNADOPTED`.

Other standalone changes to know about:

- **One process per `data_dir`.** At startup, standalone takes an exclusive `flock` on `{data_dir}/.tasks.lock`. A second process on the same `data_dir` exits non-zero, naming the lock file and the holder's pid. `flock` does not protect a `data_dir` on NFS.
- **Refusals return 503.** When the record is unreadable (wrong key, corrupt file) or a save fails, Create/Adopt/Update/Start return **503 `TASK_RECORDS_UNAVAILABLE`** (they used to return 500, or 400 for Start), and the file is not overwritten. Stop and Delete are never refused: if the save fails, they apply in memory, open `TASK_RECORDS_FAILED/write`, and the save is retried every 30 s.
- **Key rotation:** set `standalone.task_key_previous_file` (or `BINLOG_SERVER_TASK_KEY_PREVIOUS`) to the old key and the new key as current, then restart. The file is re-encrypted on the new key at startup.

### New config keys

| Key | Default | Purpose |
|---|---|---|
| `standalone.task_records` | `on` | Encrypted standalone task records and auto-resume (`on` / `off`) |
| `standalone.task_key_file` | `""` | Key file, `0600`/`0400` (env `BINLOG_SERVER_STANDALONE_TASK_KEY_FILE`). Alternative: `BINLOG_SERVER_TASK_KEY` |
| `standalone.task_key_previous_file` | `""` | Old key during rotation. Alternative: `BINLOG_SERVER_TASK_KEY_PREVIOUS` |
| `health.evaluator` | `off` | Phase 2 health evaluator: `off` / `shadow` / `on` |
| `health.sweep_interval_sec` | `60` | Evaluator sweep interval |
| `health.rehash_bytes_per_sec` | `4194304` | Global budget for the sha256 re-check of sealed segments |
| `health.unreachable_after_sec` | `90` | `SOURCE_UNREACHABLE` threshold |
| `health.unreachable_after_task_sec` | — | Per-task override of the above (map of task id to seconds) |
| `health.signal_stale_after_sec` | `86400` | Age after which a signal with no news counts as stale |

### Migration and rollout

- Migration `000009` (health columns on `task_findings`, plus `task_health`, `finding_transitions`, `notify_cursor`, `rehash_cursor` and `task_mutes`) is additive and optional. It is needed only for `health.evaluator` `shadow`/`on` with a metadata DSN; standalone writes `{data_dir}/.health.json` instead. `./migrate up` is recommended.
- Cluster: keep `health.evaluator: off` until every node runs v0.5.67, then switch to `shadow` and later `on`. This release has no automatic fallback while older nodes are present, so do not switch during a rolling upgrade.
- **Rollback to v0.5.66:**
  - meta: works on schema 9.
  - standalone: v0.5.66 ignores `.tasks.enc` and `.tasks.lock`. That means no auto-resume (every task is a leftover again and needs adopt) and no single-writer guard: nothing stops two processes on one `data_dir`. Going forward to v0.5.67 again resumes from the record.
- The v0.5.66 notes still apply. Coming from v0.5.61 or older, read the [v0.5.62 notes](release-notes-v0.5.62.md) first.

## Highlights

- **Standalone auto-resume with encrypted task records (#385, ADR 0010; PRs #393, #395).**
  - What's stored: task spec (including the source password), desired state, terminal failure, id sequence and delete tombstones, in `{data_dir}/.tasks.enc`. Encryption is AES-256-GCM with a versioned envelope and key id, written atomically with mode `0600`.
  - What isn't: a position. The resume position always comes from the bytes on disk (the adopt rule). A LATEST task with no segment on disk resumes at LATEST and records `TASK_RESUMED_AT_LATEST`. A GTID task resumes with its start set ∪ the GTIDs of the sealed segments ∪ the open tail (MySQL).
  - New CRITICAL finding `TASK_RECORDS_FAILED` (`_cluster`; subjects `key_missing`, `key_invalid`, `wrong_key`, `corrupt`, `write`).
  - When the file cannot be read, nothing is restored, writes are refused, and the file is left untouched.
- **Findings survive a full disk.** When the health or findings file cannot be written (for example ENOSPC), findings are served from memory by `/api/findings` and count for `/api/health`. A synthesized CRITICAL `FINDINGS_PERSIST_FAILED` names the failing write; it is never stored and closes once writes land. Tested on a 30 MB tmpfs.
- **Health evaluator phase 2 (ADR 0009, P2-1..P2-4; PRs #375, #376, #377, #378, #380, #383). Default `off`.**
  - P2-1: pure health types, kind registry and reconcile, with GTID set handling for MySQL and MariaDB. #377 makes UPDATE/DOWNGRADE require every scope and adds hex-uuid and tagged GTIDs.
  - P2-2: migration `000009` and the health store: MySQL, plus the standalone file store, with fences.
  - P2-3: the evaluator and the rehasher. The rehasher re-checks sealed segments from their seal checksums, shares CRC-checked readability with `/window`, and reconciles catalog and disk.
  - P2-4: signal kinds. One source liveness clock, measured at the socket, drives `DUMP_STALLED` (silent ≥ 45 s) and the new `SOURCE_UNREACHABLE` (≥ `health.unreachable_after_sec`), including through STARTING and RETRY_BACKOFF.
  - With `on`, the evaluator owns the managed kinds and closes them when their condition clears. `shadow` evaluates and logs only.
- **Standalone leftover directory finding (#388, #399).** In every `health.evaluator` mode, a directory only `data_dir` knows opens CRITICAL `TASK_UNADOPTED`, with the adopt call in its detail, and `/api/health` degrades. With the evaluator off it goes through the legacy sink. Adopt closes it, and so does moving the directory out of `data_dir`. `scripts/adopt-leftovers.sh` adopts leftovers in bulk.

## Fixes

- **#381 (PR #384):** a sealed segment cut to 0 bytes, cut inside the magic or an event header, or shorter than its size at seal was read as a whole segment. It is now unreadable: `SEGMENT_UNREADABLE` opens, and `/window`, `/replay` and the chain check all stop before it and state the reason.
- **#389 (PR #390):** a `GET` that arrived between a run's FAILED write and its exit turned FAILED into a silent STOPPED. A task's own FAILED row is no longer taken for a remote Stop.
- **#391:**
  - PR #394: after a source switchover, the resume set covers the GTIDs of every sealed segment, not only the lagging checkpoint plus the open tail. Before this, those GTIDs were fetched again, stored twice and reported as `SEGMENT_OVERLAP`.
  - PR #396: checkpoints and OPEN catalog rows are written at transaction boundaries, with commits within 200 ms batched, instead of once per event. A 3 MB, ~20k-event transaction no longer costs ~28k meta writes and stalls.
- `/api/findings` in standalone lists the evaluator-managed `_cluster` findings.

## Changes since v0.5.66

`git log --oneline --first-parent v0.5.66..91d60d5`, plus the merge of this notes PR (#398), which v0.5.67 is tagged on:

| Commit | Change |
|---|---|
| `f6ccbd9` | PR #374: docs, point install docs at v0.5.66 |
| `e1fa178` | PR #375: design 0009 phase 2 health evaluator |
| `d476be5` | PR #376: P2-1 health types, kind registry, reconcile |
| `05ed3b5` | PR #377: UPDATE/DOWNGRADE scope gate, hex/tagged GTIDs |
| `f8cbbe0` | PR #378: P2-2 migration 000009 and health store |
| `e4e2620` | PR #380: P2-3 evaluator and rehasher |
| `c780b0e` | PR #383: P2-4 signal kinds, source liveness clock |
| `e01f620` | PR #384: truncated sealed segment is unreadable (#381) |
| `3a59221` | PR #388: standalone leftover directory opens `TASK_UNADOPTED` |
| `63df0b4` | PR #390: a run's own FAILED is not a remote Stop (#389) |
| `fd48fa1` | PR #394: resume from the durable stored GTID set (#391) |
| `ed36c91` | PR #396: checkpoint at transaction boundaries, batched (#391) |
| `69a45cc` | PR #393: ADR 0010 standalone encrypted task records |
| `b4c52f4` | PR #395: standalone task records, auto-resume, full-disk findings, `/api/health` degraded |
| `91d60d5` | PR #399: `TASK_UNADOPTED` in every evaluator mode, `scripts/adopt-leftovers.sh` |

## Known behavior

- A task directory removed or moved out of `data_dir` while its record says RUN is re-dumped from the operator start, with no finding. Delete the task first to retire a directory. A finding for this is tracked in #397.
- `flock` on `.tasks.lock` does not protect a `data_dir` on NFS.
- #365–#368, #312, #341–#345, #352, #353, #360, #363 (P3) are still open.

## QA summary

Tip QA b4c52f4 + #399 PASS (BinlogServerQA), 2026-10-15.

- Tip QA of main `b4c52f4` passed on the binary. `go vet`, `go test -race` and all CI checks are green.
  - Upgrades from the real v0.5.66 binary: meta (schema 8 then `migrate up` to 9) and standalone (adopt, then a restore match).
  - Standalone with no key and with a key: `key_invalid` on a 0644 key, auto-resume after `kill -9` with matching restores.
  - Rollbacks to v0.5.66 for meta and standalone.
  - Evaluator `on` regressions: `SEGMENT_UNREADABLE`, `SEGMENT_OVERLAP`, `DUMP_STALLED`/`SOURCE_UNREACHABLE`, and LATEST plus switchover under GET load.
  - A mixed v0.5.66 + v0.5.67 cluster with takeover.
- The same QA found a P1 that following the notes caused: with the default evaluator off, a leftover directory was silent. It was fixed in PR #399, which QA passed at `7aebcaf`: `TASK_UNADOPTED` is raised in every evaluator mode and `/api/health` degrades, and `scripts/adopt-leftovers.sh` adopts leftovers in bulk. These notes were corrected in the same round.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.67.zh-CN.md
