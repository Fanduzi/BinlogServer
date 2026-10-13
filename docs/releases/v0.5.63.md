# Binlog Server v0.5.63

Release date: 2026-10-13

v0.5.63 fixes #322: while a RUNNING task sealed its open segment during binlog rotation, `GET /api/tasks/{id}/replay/archive` (and its `stop_datetime` / `stop_gtid` / `start_gtid_set` variants) and the JSON `GET /api/tasks/{id}/replay` routes could return 500 (`lstat …/mysql-bin.0000NN.open.eN: no such file or directory`, or `tar: write too long`). They no longer do (PR #325). The embedded Vue console at `/ui/` also gets the console fixes from PR #320. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. The floor stays v0.5.51. No API status code changes other than the 500s going away. Tip dogfood by BinlogServerQA passed on main `0ef91872a31d868eb076a3ba43f827105c08ff66` with no findings.

**History rewrite.** The BinlogServer history was rewritten on 2026-10-12, as described in the v0.5.60 notes. Commit hashes from before that date changed. Use the tag or the PR number to find a change.

## Fixed

- **Replay archive and replay during rotation (#322, PR #325).**
  - The disk listing reads the task directory again until it is stable. A name that disappears between the directory read and `stat` (an `.open.eN` renamed on seal, or an old sealed file removed by retention) is dropped, and new names are read, so the listing no longer fails with `lstat … no such file`.
  - Archive members are opened against the same inventory they were selected from. If a selected `.open.eN` was sealed in between, the server reads the file it became (`name.sealed.eN`, or `name` when it is not shorter or older than the listed open file). Otherwise it selects again from a fresh listing, up to 5 times.
  - `PITRReplay`, `GTIDReplay` and `ExecutedGTIDReplay` open segments against that one inventory instead of re-listing the directory per segment, with the same rule.
  - An OPEN member that is still not on this process after the retries (for example an OPEN catalog row owned by another worker) returns the existing 404 `segment not found on this process`, not 500.
  - The size cap now also holds under `io.Copy`, so an open segment that grows after it was opened no longer produces `tar: write too long`.
- **Vue console at `/ui/` (PR #320).**
  - `/auth/config`: only a 404 (an older server without the route) caches the Bearer fallback. A timeout, network error or 5xx falls back to Bearer for that request, and the next request asks again. Before, one failed lookup in `api_key` mode left the page on 401 until a reload.
  - The "leave empty to keep unchanged" password placeholder shows only when editing a task, not when creating one. Adopt keeps its required hint.

## Changes since v0.5.62

`git log --oneline v0.5.62..0ef9187`:

| Commit | Change | Shipped in the binary |
|---|---|---|
| `062107b` / `0ef9187` | fix(tasks): replay archive survives open-segment seal during rotation (#322, PR #325) | Yes |
| `28eb69c` / `5881038` | fix(console): QA P3s from #318/#319 (PR #320) | Yes, in the rebuilt Vue bundle under `internal/ui/static`; the rest is in `console/` |
| `6dfebc2` / `fd4c4dc` | feat(console): batch create dialog with preview (PR #321, part of #302) | No, `console/` only |
| `375431a` | docs: point install docs at v0.5.62 (PR #324) | No, docs |

The new React console in `console/` (PR #320's console part and PR #321's batch create dialog) is **still not embedded**: `/ui/` serves the Vue console.

## Upgrade Notes

- Replace the binary and restart. No migration, no config change.
- A database already on schema 6 stays on schema 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is still `000001` through `000006`. There is no `000007`. v0.5.63 does not start on schema 5: it exits with code 1 before it listens, with `schema version too old` and `./migrate up` in the message. If the database is not on schema 6 yet, follow the [v0.5.56 upgrade](release-notes-v0.5.56.md) first. The floor stays v0.5.51.
- The v0.5.62 401 rules and `/auth/config`, the v0.5.61 `limit` rules, the v0.5.60 `/replay` rules, the v0.5.59 legacy failback rules and the v0.5.58 `storage_alert` rules still hold.
- If you are coming from v0.5.61 or older, read the [v0.5.62 notes](release-notes-v0.5.62.md) first: bad credentials return 401 instead of 403.

## Known behavior

- #312 (P3): a repeated `limit` uses only the first value (`limit=5&limit=abc` returns 200).
- Server messages are English only.

## QA summary

BinlogServerQA, PR #325 and tip dogfood of main `0ef9187`, 2026-10-13, MySQL 8.0.46. PASS, no findings.

- #322 under fast rotation (128 KB `max_binlog_size`): 177 rounds × 4 request kinds. PR #325 returned 0 errors in 2120 pulls. The previous main `fd4c4dc` had 36 errors (archive 18/176, archive with `stop_datetime` 12/176, JSON with `stop_datetime` 6/176): `500 tar write too long`, `500 lstat … open.e1`, and two 404s. A further 960 pulls on tip `0ef9187` gave 0 errors.
- Restores: the PITR restore, 20 archives pulled while writes were running (10 full, 10 PITR), and the final 57-file chain all matched the source checksums. On tip, 4 PITR restores and a 10.5-minute soak (GTIDs 1-6404, no gaps) on two tasks matched the source checksums.
- 404s: with the OPEN catalog row deleted, `/files` still returns 404 and replay/archive leave that file out. A sealed file missing from disk gives a clean 404.
- Auth: 66 requests in both modes. The 12 with the right credential got 200 (Bearer in any case); the other 54 got 401.
- Stop/Start passed. `kill -9` with a metadata database: the task came back RUNNING. Without one: it came back STOPPED, and adopt resumed from the right point.
- `limit` validation: 16 bad values, all 400.
- `go test` passed for `tasks`, `api` and `binlog`; the new race tests passed with `-race -count=3`.
- PR #320: in the browser, `api_key` mode recovered in both consoles once `/auth/config` failures stopped; the password hint showed only on edit.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.63.zh-CN.md
