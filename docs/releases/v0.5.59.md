# Binlog Server v0.5.59

Release date: 2026-10-09

v0.5.59 makes failback (A→B→A) a first-class source chain and repairs, or refuses, the task directories that v0.5.57 and unreleased main 0a84917a wrote across a real failback (PR #297). A server's n-th stint now writes `{identity}~{n}.{binlog file}`, so A's files after a failback never reuse the names A wrote before the switch. On upgrade, a legacy failback directory is flagged with `storage_alert` and `/replay` and `/window` stop before the first affected file. The first Start repairs it when every file provably belongs to exactly one stint, and otherwise refuses and changes nothing. When the first stint's file is only in object storage, Start fetches it back and checks it before repairing. A stored GTID hole after a server switch now sets `storage_alert` and cuts `/replay` and `/window`, so a restore cannot apply past a gap with rc 0. `/window`, `source_chain.outcome`, and `binlog_server_source_switchovers` no longer report false holes or a stop the task has already run past. A task stopped by a source switch mid-dump can be started again. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on `53db97574b8e77a519639df20b26978b00c84744` (evidence-297) with no P0, P1, or P2. Three P3s are listed under Known behavior.

## Highlights

- **Failback stints (PR #297, #292 QA P1).** Each `.source-chain` line is one stint: A→B→A is three lines. The n-th stint of a server seen before writes `{identity}~{n}.{binlog file}` (the third stint is `A~2.`). `source_chain.servers[]` adds `prefix`, `current` is the server now being written, `/replay` lists A, then B, then `A~2`, and a restart on A records no phantom B→A switch. `GET /api/tasks/{id}/files` adds optional `source_server` (1-based stint). The switch event's `file` is the source binlog name without the disk prefix.
- **Legacy failback directories are repaired or refused (QA 6c88ad5b P1).** v0.5.57 and 0a84917a wrote A's third stint under A's first-stint names and pointed the catalog row at the later file, so `/files` and `/replay` silently left out the first-stint file (a restore failed with ERROR 1032 or lost rows) and Start wrote a third copy of the name. Now reads set `storage_alert` (`STORAGE_INCONSISTENT`, `detail` `legacy failback: <files>`), and `/replay` and `/window` stop before the first affected file. Start checks each file's transactions against the GTID sets recorded at each switch. See Upgrade Notes for what it does.
- **First-stint file only in object storage (QA bbc2c88b P2).** `detail` adds `not on disk: <file>`. Start fetches `{prefix}/{cluster}/{identity}/<file>` back, checks that its transactions belong to that stint, and only then repairs. When the object is missing, holds other transactions, or no object storage is configured, Start refuses and changes nothing. Before, Start repaired the names, `/window` said `continuous: true`, and a restore applied with rc 0 while the first stint's last transactions were missing.
- **Stored GTID hole safety net.** On any task that switched servers, transactions that a continued switch recorded as stored but no local segment holds, between stored ones, set `storage_alert` (`detail` `stored gtid hole: ...`, `missing_gtids`). `/replay` and `/window` stop before the first segment after them. `/window` is never `continuous` when one source's stored GTIDs have an interior gap the start set does not cover.
- **Upload retry waits for the repair.** The background upload retry no longer uploads a legacy row under its old (wrong-server) key before the repair, so a refusal changes neither the catalog nor object storage.
- **`/window` without false holes.** No `gtid hole` at a failback boundary when the start set covers transactions inside the first file (a mysqldump seed), and none where the next server's first file header lacks transactions already stored from the previous server. A real hole is still reported.
- **Switch outcome and metric.** Starting a task that is stopped on a source switch again adds no second switch. When the task runs again (the VIP pointed back), `source_chain.outcome` is `resumed`, the stop carries `resolved: true`, the Console drops the stop banner, and `binlog_server_source_switchovers{outcome="stopped"}` returns to 0. It stays resolved after a normal Stop or a process restart, once a `TASK_RUNNING` event follows the stop.
- **Restart after a mid-dump switch stop.** Before, the stop sealed the open segment while the checkpoint still named it, so every later Start failed with `SEGMENT_NOT_ON_WORKER`. The open segment now stays open. When a later Start continues on another server, the previous server's open segment is sealed under that server first; before, a switch found at connect time left it open and the new epoch's cleanup deleted it, losing that server's last transactions.
- **Tests.** `smoke-source-switchover` covers A→B→A failback. `scripts/e2e/upgrade-legacy-failback.sh` builds a legacy directory with real v0.5.57 and 0a84917a binaries and checks the repair (`FBUP_CASE` plain, `object-only`) and the refusal (`object-gone`: catalog and object storage unchanged).

## Upgrade Notes

**Legacy failback directories.** Only tasks that went through a real failback (A→B→A, A caught up from B first) on v0.5.57 or 0a84917a are affected. Recognize them by several files for one binlog index (no suffix plus `.sealed.eN` / `.open.eN`), a `.source-chain` with only A and B, and continued B→A switch events. After the upgrade:

1. Reads show `storage_alert` (`STORAGE_INCONSISTENT`, `detail` `legacy failback: ...`), and `/replay` and `/window` stop before the first affected file. Do not restore past that point before the repair.
2. **Repair.** The first Start, including the automatic resume when the process starts, repairs the directory when every file fits exactly one stint. It renames the later stint to `{identity}~{n}.`, lists the first-stint file in the catalog again for re-upload and checksum verification, fixes the checkpoint file name, writes `.source-chain` as A,B,A, and records `STORAGE_REPAIRED`. It deletes nothing. A `.legacy-failback-repair` plan lets the next Start finish a repair that was interrupted; QA killed the process at every step and each restart finished the repair with replays equal to the source.
3. **Refuse.** When a file's transactions belong to no stint or to several, the rename target exists, a file cannot be read, or the first-stint file cannot be fetched back from object storage, Start fails with `STORAGE_INCONSISTENT`, names the files (and the object key), and changes nothing on disk, in the catalog, or in object storage. Then: do not restore past `storage_alert.segment` (`valid_segments` before it are still good), create a new task against the current primary from its `@@gtid_executed`, keep the old task and its objects for evidence, and if you need that period, assemble it by GTID with `mysqlbinlog --include-gtids` / `--exclude-gtids`. To repair instead, put the first-stint file back by hand (troubleshooting 5.4, "manual fetch", and the note under Known behavior) and Start again.
4. **Orphaned objects.** The old builds uploaded some files under keys that the repaired catalog no longer references. `STORAGE_REPAIRED` lists them after `orphaned_objects=`. Some of those keys may not exist (an upload the old build never completed). The service never deletes them. Delete them by hand only after you have confirmed a restore from the repaired task.
5. Detection relies on the switch events in the metadata database. A metadata restore from a backup older than those events loses them, and the directory is then not recognized as a legacy failback.

Healthy directories are not read beyond the files from each re-entered server's switch point onward. Only files on local disk are checked, apart from the first-stint fetch above.

The v0.5.58 rule still holds: after upgrading from v0.5.57, a GTID task that shows `storage_alert` for a stray Rotate is damaged; do not Start it, and recover from `restart_gtid_set`.

No new migration. A database already on schema 6 stays on schema 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is still `000001` through `000006`. There is no `000007`. `minRequiredSchemaVersion` remains 6: v0.5.59 does not start on schema 5 and exits with code 1 before it listens, with `schema version too old` and `./migrate up` in the message. If the database is not on schema 6 yet, follow the [v0.5.56 upgrade](release-notes-v0.5.56.md) first. The floor stays v0.5.51. No new config key. This release is not an ADR 0005 step. ADR 0005 is unchanged.

## Known behavior

These P3s were found in the QA of PR #297. None of them loses data silently.

- After Start refuses because the first-stint object cannot be read, `storage_alert.detail` still ends with `(Start repairs)` and `message` still says Start fetches it back. The alert is computed from the directory and does not know the object is gone. Trust `last_error`, which names the file and the object key and says nothing was changed. Starting again repeats the same refusal.
- Manual fetch from the old primary: the same-named binlog on A also holds the transactions A wrote after the failback, so as it is, it never matches the first stint and Start refuses it (`holds transactions that no stint ... stored`). Cut it at the position of the A→B `SOURCE_SWITCHOVER` event first (`at <file>:<pos>`, then `head -c <pos> <file>`), check its GTIDs, put it back, and Start. QA repaired a directory this way with replays equal to the source.
- When a sealed segment in the middle of the chain has lost its bytes (local file and object both deleted, or a local-only file deleted), `/window` reports `segment is not readable` and is not continuous, but `/replay` still lists that segment (`location` `bucket` or empty) with no `warning`. Downloading it returns 404, and `mysqlbinlog` stops with `Could not open log file`, so later files are not applied. Check `/window` `breaks` before you restore.
- `stop_datetime` still cuts on the event-header second. Use `stop_gtid` when the bad GTID is known. A manual `GTID_NEXT` hole on the source is still reported as a `gtid hole` by `/window`.

## QA summary

BinlogServerQA, PR #297 head `53db9757`, MySQL 8.0.46, real v0.5.57 → 0a84917a legacy directories. PASS, no P0/P1/P2.

- Legacy directory with the first stint only in object storage (mixed and insert-only): repaired, the fetched file is byte-identical, `/window` continuous, replay checksum and GTID set equal to the source.
- Object also gone: refused; disk, catalog, and object storage byte-identical before and after Start.
- Repair interrupted by `kill -9` at seven points (fetch start, mid-fetch, after fetch, journal, rename, catalog, upload): every restart finished the repair once, with replays equal to the source.
- Regressions: 32-minute reconnect soak (74,238 transactions, 42 KILL rounds, 29 FLUSH, 15 proxy drops) with 0 `FAILED`, 0 `STREAM_REGRESSION`, 0 `storage_alert`; #293 redump, MTA replica with a `GTID_NEXT` hole, live A→B→A failback, and kill-cut runs all replay equal to the source. Schema stays 6.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.59.zh-CN.md
