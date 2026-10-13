# Binlog Server v0.5.62

Release date: 2026-10-13

v0.5.62 changes how authentication failures are reported, which is a **compatibility change**: every missing, malformed or wrong credential now returns HTTP 401 instead of 403 (PR #316). In bearer mode the 401 also carries `WWW-Authenticate: Bearer realm="binlog-server"`. The release also adds a public `GET /auth/config`, makes the built-in Vue console work in `api_key` mode, accepts the `Bearer` scheme in any letter case, and shows the server's error text when a task does not exist (PR #318). A new React console is being built in `console/` (PR #315, #317, #319). **It is not embedded yet**: `/ui/` still serves the Vue console. No new config key. No schema migration. Schema stays 6. `minRequiredSchemaVersion` stays 6. The floor stays v0.5.51. Tip dogfood by BinlogServerQA passed on main `ef6fc2ee7447f4dea4195fdd8a9fad4a6db4cfc8` (evidence-tip-ef6f) with no P0 or P1. One P2 that is not a regression (it is also on v0.5.61) is listed under Known behavior.

**History rewrite.** The BinlogServer history was rewritten on 2026-10-12, as described in the v0.5.60 notes. Commit hashes from before that date changed. Use the tag or the PR number to find a change.

## Compatibility change: 401 instead of 403

| Request | v0.5.61 | v0.5.62 |
|---|---|---|
| No credential | 401 | 401 |
| Bearer mode, header without the `Bearer ` prefix (e.g. a bare token, `Basic …`) | 403 | **401** |
| Bearer mode, wrong token | 403 | **401** |
| api_key mode, wrong key | 403 | **401** |
| Correct credential | 200 | 200 |

- The server has one shared credential and no per-user permissions, so a failed check always means "not authenticated". 403 is kept for a future "authenticated but not allowed" once users and roles exist.
- Bearer mode adds `WWW-Authenticate: Bearer realm="binlog-server"` to every 401. Browsers do not show a login prompt for a Bearer challenge.
- Both consoles reopen Settings on 401, so an operator with a stale or mistyped token now gets the token field back instead of a page of failed requests.

**Check before upgrading:** monitoring probes, scripts, or proxies that treat 403 as "wrong token" (for example an alert on `status == 403` against `/api/*` or `/metrics`) must look for 401 instead.

## Highlights

- **`GET /auth/config`, no credential needed (PR #318).** Returns `{"enabled", "mode", "header", "scheme"}`, for example `{"enabled":true,"mode":"bearer","header":"Authorization","scheme":"Bearer"}` or `{"enabled":true,"mode":"api_key","header":"X-BS-Key"}`. It never returns the token or key. The response has `Cache-Control: no-store`. If you put the server behind a proxy that only lets `/api/*` and `/ui/` through, also allow `/auth/config`.
- **Vue console in `api_key` mode (PR #318).** The console reads `/auth/config` once and sends the stored value in the configured header (`Authorization: Bearer …` or your `api_key_header`). Before, it always sent `Authorization: Bearer`, so in `api_key` mode every request failed. Against an older server without `/auth/config` it falls back to Bearer.
- **Bearer scheme is case-insensitive (PR #318).** `Bearer`, `bearer` and `BEARER` all work, with one or more spaces or tabs before the token (RFC 7235 / RFC 6750). A header with a scheme but no token is still 401.
- **Unknown task id shows the server's message (PR #318).** The task detail drawer shows the server's error text, for example `task not found`, instead of a generic failure.
- **New console in `console/`, not embedded (PR #315, #317, #319, part of #302).** A React + shadcn/ui rewrite with the shell, Tasks/Alerts lists, the task detail drawer at `#/tasks/:id`, and the create/edit/adopt dialog. It is built and tested in the repository but **not shipped at `/ui/`**. Nothing you use changes until a later cutover release.
- **Docs.** The troubleshooting table and the configuration guide describe the 401 behavior and `/auth/config`.

## Upgrade Notes

- Update anything that expects 403 for a bad credential (see the table above).
- If a reverse proxy filters paths, allow `GET /auth/config` so the console can pick its auth header. It needs no credential and returns none.
- `/healthz` stays open. `/ui/` still loads without a credential and asks for one on the first 401.

No new migration. A database already on schema 6 stays on schema 6. Confirm `SELECT version, dirty FROM schema_migrations` is `(6, 0)`. `migrations/` is still `000001` through `000006`. There is no `000007`. `minRequiredSchemaVersion` remains 6: v0.5.62 does not start on schema 5. It exits with code 1 before it listens, with `schema version too old` and `./migrate up` in the message. If the database is not on schema 6 yet, follow the [v0.5.56 upgrade](release-notes-v0.5.56.md) first. The floor stays v0.5.51. No new config key. The v0.5.61 `limit` rules, the v0.5.60 `/replay` rules, the v0.5.59 legacy failback rules and the v0.5.58 `storage_alert` rules still hold.

## Known behavior

- #322 (P2, also on v0.5.61): while the source is writing and binlogs rotate quickly, `GET /api/tasks/{id}/replay/archive` sometimes returns 500 `lstat …/mysql-bin.0000NN.open.eN: no such file or directory`. QA saw 7–25% of requests fail with a 256 KB `max_binlog_size`, and more on v0.5.61. A retry succeeds, and every tar that was returned restored correctly. A fix is in progress.
- #312 (P3): a repeated `limit` uses only the first value (`limit=5&limit=abc` returns 200).
- Server messages are English only.

## QA summary

BinlogServerQA, tip dogfood of main `ef6fc2e`, 2026-10-13 18:20–18:50 CST, MySQL 8.0.46. PASS, no P0/P1.

- Auth: 13 credential forms × `/api/tasks`, `/api/dashboard`, `/metrics` × both modes. Only a correct credential (Bearer/bearer/BEARER in bearer mode, the configured header in api_key mode) gave 200. Everything else gave 401, with `WWW-Authenticate` in bearer mode. `/auth/config` answered without a credential in both modes. `/healthz` and `/ui/` gave 200.
- 19 minutes of continuous writes (12,752 GTIDs, 58 files) with Stop/Start and `kill -9`, with and without a metadata database. Full-chain restores from `/replay` paths and from `/replay/archive` matched the source checksums, with `gtid_executed` 1-12752 and no gaps.
- Point-in-time restores with `stop_datetime` and `stop_gtid` matched the recorded point exactly.
- `limit` validation behaved as in v0.5.61.
- New console: `npm ci` and `npm run build` passed. Through a preview proxy it read `/auth/config` in both modes and got 401 without a credential and 200 with one.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.62.zh-CN.md
