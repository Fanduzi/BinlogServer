# Binlog Server v0.5.64

Release date: 2026-10-13

v0.5.64 serves the new Console at `/ui/` (#302): a React + shadcn/ui rewrite of the management console. The previous Vue console stays at `/ui-classic/` for this one release and will be removed in a later release. Bookmarks and deep links under `/ui/` keep working. No API change. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on main `56b221e65d7e047a154a00ae215b646cb8bd6031` with no findings.

**History rewrite.** The BinlogServer history was rewritten on 2026-10-12, as described in the v0.5.60 notes. Commit hashes from before that date changed. Use the tag or the PR number to find a change.

## Highlights

- **New Console at `/ui/` (PR #315, #317, #319, #320, #321, #327, #328, #329, #332; #302).**
  - Tasks and Alerts lists with server-side paging, filters and KPIs.
  - Task detail drawer at `#/tasks/:id`: status, banners, files, events, lease and runs, PITR command builder, Start/Stop/Delete. The drawer refreshes itself after Start or Stop.
  - Create, edit and adopt task form, and batch create with a preview.
  - Overview, Sources (with host/port lookup) and Workers pages.
  - `api_key` mode works from the UI: the Console reads `GET /auth/config` and sends the credential in the configured header.
  - English and Chinese.
- **Classic Vue console at `/ui-classic/`, for one release (PR #332).** Same behavior as the v0.5.63 `/ui/`. It loads without a credential, like `/ui/`, and asks for one on the first 401. Both consoles are served from the same origin and use the same browser storage key (`binlog_server_token`), so a token saved in one is used by the other.
- **Console fixes in the cutover (PR #332).** A deep link opened without a token shows only the auth dialog; a missing worker count shows 0; the worker badge counts registered workers; the batch preview has its `totalRows` / `allPassed` / `hasFailed` texts.

## Changes since v0.5.63

`git log --oneline --first-parent v0.5.63..56b221e`:

| Commit | Change |
|---|---|
| `4dc350c` (`68afa51`, `1514275`) | PR #327: one prettier config and vitest; shared data/error/poll/table modules, shell metric grid, Overview page, helper unit tests |
| `529a6e3` (`40728e7`) | PR #328: Sources page with host/port lookup |
| `614d93d` (`88ab228`) | PR #329: Workers page; deep-merge console-only i18n; drop the pending placeholder |
| `56b221e` (`06570fe`) | PR #332: serve the new Console at `/ui/`, keep Vue at `/ui-classic/`; locales move to `console/src/locales`; `build-ui.sh` and `check-ui-bundle.sh` build and check both consoles; the Playwright suite moves to `console/tests/e2e` |
| `86fb636` | PR #331: docs, point install docs at v0.5.63 |

Server code changes are limited to `internal/ui` (a second embedded handler) and the `/ui-classic/*path` route in `internal/api/server.go`.

## Upgrade Notes

- Replace the binary and restart. No migration, no config change.
- Bookmarks and deep links under `/ui/` (for example `/ui/#/tasks/12`) still work and open the new Console.
- Anyone relying on the Vue UI should use `/ui-classic/`. It will be removed in a later release.
- If a reverse proxy filters paths, allow `/ui-classic/` as well as `/ui/` and `/auth/config`. Both need no credential.
- A database already on schema 6 stays on schema 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is still `000001` through `000006`. There is no `000007`. v0.5.64 does not start on schema 5: it exits with code 1 before it listens, with `schema version too old` and `./migrate up` in the message. If the database is not on schema 6 yet, follow the [v0.5.56 upgrade](release-notes-v0.5.56.md) first. The floor stays v0.5.51.
- The v0.5.63 replay-during-rotation fix, the v0.5.62 401 rules and `/auth/config`, the v0.5.61 `limit` rules, the v0.5.60 `/replay` rules, the v0.5.59 legacy failback rules and the v0.5.58 `storage_alert` rules still hold. Coming from v0.5.61 or older, read the [v0.5.62 notes](release-notes-v0.5.62.md) first: bad credentials return 401 instead of 403.

## Known behavior

- #312 (P3): a repeated `limit` uses only the first value (`limit=5&limit=abc` returns 200).
- Server messages are English only.

## QA summary

BinlogServerQA, tip dogfood of main `56b221e`, 2026-10-13. PASS, no findings.

- Every Console page passed 22/22 checks with 0 browser console errors.
- e2e: 37/37 passed.
- An 11-minute soak through 3 `kill -9`s and 2 adopts: restores matched the source checksums.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.64.zh-CN.md
