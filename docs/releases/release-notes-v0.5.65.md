# Binlog Server v0.5.65

Release date: 2026-10-14

v0.5.65 ships ADR 0009 phase 0 and phase 1 (backup assurance, #326): failures that used to end in a log line now leave persisted findings, the backup chain is checked against invariants I1–I7 in one place (`internal/assurance`), sealed segments carry seal facts, the binlog dump is fenced against the task lease, and every operator transition is persist-first with a stalled-transition watchdog. Two new migrations (`000007`, `000008`) are additive and optional: `minRequiredSchemaVersion` stays 6 and the floor stays v0.5.51. No config change. Tip dogfood by BinlogServerQA passed on main `b5c97c9fff2ecdcbdc531c5e3a14a72332c3d249`.

**History rewrite.** The BinlogServer history was rewritten on 2026-10-12, as described in the v0.5.60 notes. Commit hashes from before that date changed. Use the tag or the PR number to find a change.

## Highlights

- **Findings (ADR 0009 phase 0; PR #336, #337, #338, #339, #340).**
  - A finding sink: each finding is persisted (`task_findings`, migration `000007`; standalone uses `{data_dir}/.findings.json`), leaves a `FINDING_REPORTED` task event and a metric, and resolves when the condition clears.
  - Kinds include `TASK_PERSIST_FAILED`, `EVENT_PERSIST_FAILED`, `EVENT_READ_FAILED`, `DUMP_CLEANUP_PERSIST_FAILED`, `DISK_LIST_FAILED`, `RETRY_LOOP_FAILED`, `UPLOAD_RECORD_FAILED`, `UPLOAD_UNVERIFIED`, `RETENTION_RECORD_FAILED`, `OPEN_TAIL_CUT_FAILED`, `METRIC_SOURCE_FAILED`.
  - API: `GET /api/tasks/{id}/findings` (`all=1` adds resolved) and `GET /api/findings`. `open_findings` on `/api/tasks`, `/api/tasks/{id}` and `/api/dashboard`. The Console alert view lists rows with a `storage_alert` or open findings.
  - Metrics: `binlog_server_finding_open{task_id,kind,severity}`, `binlog_server_findings_reported_total{kind}`, `binlog_server_finding_sink_errors_total`, `binlog_server_finding_sink_pending`, `binlog_server_finding_store_active`, `binlog_server_metric_source_up{task_id,source}`, `binlog_server_upload_retry_pass_last_ts`. `/metrics` answers when the metadata store is down instead of HTTP 500.
  - `internal/lintguard` fails `go test ./...` on a new ignored error in the backup data path.
- **`DUMP_STALLED`.** The Binlog Dump connection has a 45s read timeout (three missed heartbeats). Silence opens `DUMP_STALLED` (WARNING), ends the run and reconnects after backoff; it resolves on the first event of the next session. All SQL connections to the source go through `connectSource` (5s dial, 5s read/write), so a blackholed source no longer hangs the dump loop. Semi-sync events reset the silence timer.
- **`METADATA_WRITE_BLOCKED`.** When the metadata store refuses writes (for example `super_read_only`), the dump stops and retries with backoff instead of writing binlog the catalog does not know about. A CRITICAL `METADATA_WRITE_BLOCKED` finding opens; the task shows `RETRY_BACKOFF`/`STARTING` (never `RUNNING`) and recovers on its own when the store accepts writes again. Only errors from the metadata store boundary count; source timeouts are not misreported.
- **Backup chain and invariants (ADR 0009 phase 1; PR #347, #348, #349, #350, #354).** `internal/assurance` builds one chain (`ChainOf`) and checks invariants I1–I7. `GET /window`, the storage-health verdict and the `/replay` cut are derived from it. Behaviour is frozen against goldens recorded on v0.5.64.
- **`SEGMENT_UNREADABLE` (PR #361, #358).** A sealed segment with a corrupt event size no longer makes the process allocate ~4 GiB. It is reported unreadable: `SEGMENT_UNREADABLE` finding, `storage_alert` `STORAGE_INCONSISTENT`, and the window stops before it.
- **Seal facts (PR #351).** Migration `000008_binlog_files_seal_facts` adds nullable `first_event_at`, `last_event_at` and `sha256` to `binlog_files`; each seal writes them in one streaming pass. Optional: on schema 6 or 7 the write is skipped until restart.
- **Lease fence (PR #355, #346).** Before a dump opens, Start verifies and renews the lease (owner + epoch CAS), so time spent killing a leftover dump counts against the lease. A worker on an expired lease cannot open a dump, and the lease-lost stop no longer overwrites the new owner's row. At most one dump per task.
- **Persist-first transitions and `TASK_TRANSITION_STALLED` (PR #364).** Create, Start, Stop, Delete, Update, Adopt and every Configure* write the store first; memory, events and the dump change only after the write lands. A refused action returns an error, records a `*_REJECTED` event and leaves the task, its row and its dump unchanged (a refused Start keeps the RETRY_BACKOFF loop alive; a refused Delete keeps the dump running). A task in STARTING or STOPPING for more than 60s raises `TASK_TRANSITION_STALLED`, which closes when the task converges.

## Fixes

- **MariaDB 11.4+ row events are stored (PR #362, #359, P1).** MariaDB 11.x backups on earlier releases stored only GTID and XID events, so restores came back empty with no error. 10.11 was not affected. **If you back up MariaDB 11.4 or later, take a new full backup and treat binlog backups from v0.5.64 and earlier for that source as unusable for row replay.**
- **No-meta adopt resumes at the last event end (PR #357, #356, P1).** After `kill -9` in no-meta mode, adopt resumed at the file size, which is wrong for a continuation segment or a torn tail.
- Stop records the intent before it cancels the dump (`TASK_STOP_REJECTED` when refused) instead of leaving a RUNNING row with no dump.

## Changes since v0.5.64

`git log --oneline --first-parent v0.5.64..b5c97c9`:

| Commit | Change |
|---|---|
| `b7df265` | PR #335: ADR 0009 backup assurance |
| `4b029d6` | PR #336: finding sink, migration 000007 |
| `c145fa1` | PR #337: upload and retention findings |
| `370ccbe` | PR #338: dump read timeout, `DUMP_STALLED`, `connectSource`, `METADATA_WRITE_BLOCKED`, open tail findings |
| `c8d217a` | PR #339: metric source, progress error and alert view findings |
| `e39b6a9` | PR #340: ignored-error guard |
| `650e13a` | PR #347: ADR 0009 phase 1 design note |
| `e159e38` | PR #348: `internal/assurance` chain and I1–I7 check |
| `f18a22f` | PR #349: `/window` from `assurance.WindowOf` |
| `6602753` | PR #350: storage health and replay cut from the chain |
| `44d5e36` | PR #351: seal facts, migration 000008 |
| `2213620` | PR #355: dump lease fence |
| `5ccf17b` | PR #357: no-meta adopt resume point |
| `1d6e78f` | PR #354: superseded open epoch and epoch continuation |
| `250d6b4` | PR #361: bounded event size, `SEGMENT_UNREADABLE` |
| `53a9b32` | PR #362: MariaDB 11.4+ row events |
| `b5c97c9` | PR #364: persist-first transitions, `TASK_TRANSITION_STALLED` |

## Upgrade Notes

- Replace the binary and restart. No config change.
- Migrations: `migrations/` now has `000007_task_findings` and `000008_binlog_files_seal_facts`. Both are additive and optional. Running `./migrate up` to 8 is recommended: on schema 6 findings stay in memory and `/metrics` only (`binlog_server_finding_store_active 0`), and without 8 seal facts are not written. `minRequiredSchemaVersion` stays 6; v0.5.65 does not start on schema 5 (`schema version too old`, `./migrate up`). The floor stays v0.5.51. The floor moves to 7 in v0.6.0.
- Alert on `binlog_server_finding_open{severity="CRITICAL"} > 0`, and in particular on `METADATA_WRITE_BLOCKED`, `TASK_TRANSITION_STALLED` and `SEGMENT_UNREADABLE`.
- Under a read-only metadata store, Start, Stop and Delete now return 400 and change nothing. Retry after the store accepts writes.
- MariaDB 11.4+: see the fix above; take a new full backup after upgrading.
- The v0.5.64 `/ui-classic/` rule and the v0.5.63 to v0.5.58 rules still hold. Coming from v0.5.61 or older, read the [v0.5.62 notes](release-notes-v0.5.62.md) first: bad credentials return 401 instead of 403.

## Known behavior

- A duplicate segment for the same source file in the same directory (for example `mysql-bin.000006` plus `mysql-bin.000006.sealed.e3`) is not detected at runtime: no finding, the window looks continuous, and `/replay` lists both. Same as v0.5.64; planned for ADR 0009 phase 2.
- #365 (P3): PUT and Create under read-only meta return 500 instead of 400 (state unchanged, `*_REJECTED` recorded).
- #366 (P3): `DUMP_STALLED` can stay open 40s+ on an already-STOPPED task after `kill -9` of the old worker in cluster mode.
- #367, #368 (P3): Console requests `/checkpoint` for a never-started task; the event timeline is ordered by write time.
- #312, #341–#345, #352, #353, #360, #363 (P3) are still open.

## QA summary

BinlogServerQA, tip dogfood of main `b5c97c9`, 2026-10-14. PASS, no regression.

- DBA path: restores match on MySQL 8.0, MariaDB 10.11 and 11.4.
- Upgrade from v0.5.64 (schema 6 to 8): restore matches.
- `/ui/` live checks 23/23; e2e 36/37 under heavy box load, the single flakes pass alone.
- Failback, no-meta `kill -9` + adopt, read-only meta recovery 3/3, blackhole `DUMP_STALLED`, the `TASK_TRANSITION_STALLED` watchdog, cluster mode and the lease fence (never more than one dump), seal facts 50/50, `SEGMENT_UNREADABLE`, `go vet` and `go test`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.65.zh-CN.md
