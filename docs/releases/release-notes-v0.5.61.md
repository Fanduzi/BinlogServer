# Binlog Server v0.5.61

Release date: 2026-10-13

v0.5.61 makes `GET /api/tasks/{id}/replay` and `/replay/archive` reject a bad `limit` instead of quietly returning the full set (#307, PR #311). It also says so when `limit` is ignored on a point-in-time request. A typo like `limit=-1` or `limit=abc` used to give you a full replay set with HTTP 200, so you could not tell the value had been dropped. Now it is HTTP 400. Nothing else changes. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. The floor stays v0.5.51. Dogfood by BinlogServerQA passed on main `5c18b1e4608418eec8c6adaecacd5ae639e4b3eb` (evidence-311) with no P0, P1, or P2. One P3 is filed as #312.

**History rewrite.** The BinlogServer history was rewritten on 2026-10-12, as described in the v0.5.60 notes. Commit hashes from before that date changed. Use the tag or the PR number to find a change.

## Highlights

- **A bad `limit` is HTTP 400 (#307, PR #311).** `limit` must be a positive integer. `0`, a negative number, an empty value, a decimal (`1.5`, `1e3`), text, or a number too large for the server answers HTTP 400 with the body `invalid limit: must be a positive integer`. This applies to the JSON `/replay` and to `/replay/archive`, on the plain path and with `stop_datetime`, `stop_gtid` or `start_gtid_set`. Before, all of these returned 200 with the full set.
- **`limit` with a point-in-time request now warns.** With `stop_datetime`, `stop_gtid` or `start_gtid_set`, a valid `limit` is still ignored: the point-in-time set always lists every file the restore needs. The JSON `warning` now says `limit is ignored with stop_datetime, stop_gtid or start_gtid_set: a point-in-time set always lists every file it needs.` and the archive sends the same text in `X-Replay-Warning`. Without `limit` there is no warning and no header.
- **Unchanged.** A valid `limit` on the plain path returns the same paths, `omitted_files`, `warning` and archive headers as v0.5.60. A plain `/replay` without `limit` still returns every file. The Console does not send `limit`.
- **Docs.** The API reference (section 5.6 and the archive section) documents the 400 and the warning.

## Upgrade Notes

- Scripts that send `limit=0`, a negative value, an empty `limit=`, or a non-integer now get HTTP 400 instead of the full set. Drop the parameter to get every file, or pass a positive integer.
- Tools that send `limit` together with `stop_datetime`, `stop_gtid` or `start_gtid_set` get the same set as before, plus a `warning` (JSON) or `X-Replay-Warning` (archive). If your tooling treats any `warning` as a failure, stop sending `limit` on point-in-time requests. It never had an effect there.

No new migration. A database already on schema 6 stays on schema 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is still `000001` through `000006`. There is no `000007`. `minRequiredSchemaVersion` remains 6: v0.5.61 does not start on schema 5. It exits with code 1 before it listens, with `schema version too old` and `./migrate up` in the message. If the database is not on schema 6 yet, follow the [v0.5.56 upgrade](release-notes-v0.5.56.md) first. The floor stays v0.5.51. No new config key. The v0.5.60 `/replay` rules, the v0.5.59 legacy failback rules, and the v0.5.58 `storage_alert` rules still hold.

## Known behavior

- #312 (P3): when `limit` appears more than once, only the first value is used, so `limit=5&limit=abc` returns 200 with 5 files. `limit=abc&limit=5` is a 400. `+5`, ` 5` (leading space) and `9223372036854775807` (int64 max) are accepted as valid.
- Server messages, including the new 400 body and the `warning`, are English only.
- The v0.5.60 and v0.5.59 Known behavior items still apply, except #307, which this release fixes.

## QA summary

BinlogServerQA, main `5c18b1e` against baseline c7f3331 (v0.5.60 docs tip). PASS, no P0/P1/P2.

- Bad `limit`: `0`, `-1`, `abc`, empty, `1.5`, `1e3`, `99999999999999999999` and `abc&limit=5` gave 400 `invalid limit: must be a positive integer` on JSON and archive, on the plain path and all three point-in-time forms. The baseline returned 200 with the full set.
- Valid `limit` with a point-in-time request: paths and tar members were identical to the request without `limit` and to the baseline. The JSON `warning` and archive `X-Replay-Warning` were present. Without `limit`, neither appeared.
- Plain path with no `limit` and `limit` 1/200/201/366/367/100000: paths, `omitted_files`, `warning`, archive members and headers were identical to the baseline.
- Soak: a GTID task with `max_binlog_size` 128 KB had 366 replay files. Restores from the default `/replay` and from `/replay/archive` (366 members) matched the source checksums on both builds.
- Regression: the storage-alert and legacy-failback replay warnings were identical to the baseline. `go test` for api/tasks passed. The Console bundle was unchanged and sends no `limit`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.61.zh-CN.md
