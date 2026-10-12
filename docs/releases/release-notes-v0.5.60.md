# Binlog Server v0.5.60

Release date: 2026-10-13

v0.5.60 fixes a P1 in `GET /api/tasks/{id}/replay` (#305, PR #306). Before, a plain `/replay` or `/replay/archive` returned only the newest 200 segment files with no warning, while `/window` still reported the whole chain as continuous. A restore built from that set failed with ERROR 1032, or silently left out older transactions when no later row depended on them. Now the plain replay set lists every file a restore needs. An explicit `limit` window still works, but it says how many earlier files it left out. The rest of the release is three internal refactors with no behavior change (PR #300, #301, #304), an e2e test fix, and a repository housekeeping move. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on `eaa100594b9bd2ebbaa4172a48d294b4dbe93857` (evidence-306) with no P0, P1, or P2. One P3 is filed as #307.

**History rewrite.** The BinlogServer history was rewritten on 2026-10-12. Commit hashes from before that date changed, so a hash quoted in older notes, issues, or QA evidence (for example 0a84917a in the v0.5.59 notes) may no longer resolve. Release tags now point at the rewritten commits; published release binaries did not change. Use the tag or the PR number to find a change.

## Highlights

- **`/replay` never truncates silently (#305, PR #306, P1).** `GET /api/tasks/{id}/replay` and `GET /api/tasks/{id}/replay/archive` without `limit` now return every file of the retained chain, starting at the first one, with the same damage cut as before. They read the same chain as `/window`, so the two always agree. The old 200-file list default no longer applies to replay.
- **An explicit `limit` window says what it left out.** With `?limit=N` you still get the newest N files. When the full replay set has earlier files, the JSON adds `omitted_files` and a `warning` naming the first left-out file and saying that this set alone does not restore from the start of the backup. The archive sets the headers `X-Replay-Omitted-Files` and `X-Replay-Warning`. A `limit` of 0, a negative number, or text is treated as no limit.
- **Console.** The replay command and 下载回放集 in the task drawer no longer send `limit=80`, so they hold every file. The files table still shows the newest 80 rows.
- **Refactors, no behavior change.** These were checked byte for byte against the previous main: API golden output on 14 real task directories, the e2e suites, the legacy failback upgrade script, and a reconnect/kill soak.
  - **Segment ledger (PR #300, ADR 0006).** One module answers "which GTIDs are on disk": segments, stored set, holes, damage, the resumable open tail.
  - **Storage health (PR #301, ADR 0007).** One verdict per task directory: damaged, legacy failback, or stored GTID hole, checked in that order. It drives `storage_alert`, `/replay`, `/window`, and the replay warning.
  - **Run phases (PR #304, ADR 0008).** The replication run is split into five named phases: resume position, connect, segment append/rotate/seal, event handling, and reconnect/error classification.
- **Tests.** `smoke-worker-crash-recovery` no longer compares the task's first file. A LATEST task starts in the middle of that binlog, so its backup never equals the source file. This was a test bug, not data corruption. The test now rotates once more and compares the newest two whole files, including the file that was open at the `kill -9`.
- **Repository.** The `verify-binlog-server` agent skill moved to `.agents/skills` (PR #303). Not shipped in the binary.

## Upgrade Notes

- Scripts or tools that call `/replay` or `/replay/archive` without `limit` now get every file instead of at most 200. On a long chain the JSON and the tar are larger. That is the intended result: the old answer was incomplete.
- Callers that pass `limit` get the same paths as before. Check `omitted_files` (or `X-Replay-Omitted-Files`) before you treat such a set as a full restore.
- If you restored from a plain `/replay` on v0.5.59 or earlier and the task had more than 200 segment files, that restore may be missing the oldest transactions. Compare the first file in the old `paths` with `/window` `earliest`, or rebuild the restore from the new full set.

No new migration. A database already on schema 6 stays on schema 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is still `000001` through `000006`. There is no `000007`. `minRequiredSchemaVersion` remains 6: v0.5.60 does not start on schema 5. It exits with code 1 before it listens, with `schema version too old` and `./migrate up` in the message. If the database is not on schema 6 yet, follow the [v0.5.56 upgrade](release-notes-v0.5.56.md) first. The floor stays v0.5.51. No new config key. The v0.5.59 legacy failback and v0.5.58 `storage_alert` rules still hold.

## Known behavior

- #307 (P3): `limit=0`, a negative number, or text is accepted and means "every file", so a typo goes unnoticed. With `stop_datetime`, `stop_gtid`, or `start_gtid_set`, `limit` is ignored: the point-in-time set is always complete, with no `omitted_files`. The `warning` text is English only.
- The v0.5.59 Known behavior items still apply.

## QA summary

BinlogServerQA, PR #306 head `eaa10059`, against main 199c8b7. PASS, no P0/P1/P2.

- Soak with `max_binlog_size` 128 KB, 354–357 segments per task (GTID through a proxy, LATEST through a proxy, GTID semi-sync). A plain `/replay` returned the whole chain, and `/replay/archive` returned 357 members. Restores of all three tasks returned rc 0, with table checksums equal to the source. On main the same sets had 200 files, and the restore failed with ERROR 1032.
- `limit` 1/5/199/200/201/356: N files, `omitted_files` = 357−N, the warning, and matching archive headers. `limit` 357/1000/0/−1/`abc`/empty gave the full chain with no warning.
- Point-in-time paths with `limit`: complete, the same as main.
- `smoke-worker-crash-recovery` logic: 3/3 PASS.
- Regression: storage-alert, legacy failback, and `/window` outputs were identical to main.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.60.zh-CN.md
