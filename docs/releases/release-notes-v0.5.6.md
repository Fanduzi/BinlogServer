# Binlog Server v0.5.6

Release date: 2026-10-02

Binlog Server `v0.5.6` is an operator-facing console, production fail-closed, and dashboard-scale patch on `v0.5.5`.

## Highlights

- The embedded Console shipped in `v0.5.5` did not mount. The entry bundle had a stray `}` in its import list, so Chrome threw `SyntaxError: Unexpected token ','` and never mounted the page shell. A clean UI rebuild restores the shell. `scripts/check-ui-bundle.sh` runs `node --check --input-type=module` on the JS referenced by `index.html` in CI and in the release workflow. Plain `node --check` on a `.js` path parses as CommonJS and misses this error.
- `PRODUCTION=true` with an empty `--encryption-key` returns before the process listens. When `api.auth.enabled=true`, `/ui/*` and `/swagger/*` use the same auth middleware. `/healthz` stays open. The console lock follows `api.auth.enabled`, not `protect_api`.
- `GET /api/dashboard` and `GET /api/summary` count states and source rollups with SQL `GROUP BY` and page dashboard rows with `LIMIT/OFFSET`. Delay classification reads RUNNING task ids. They no longer set `Limit=0` and scan every matching task row. `total`, `summary.total`, and per-source `task_count` stay the filtered set.
- `scripts/failover-dogfood.sh` and the verify-binlog-server failover-lease skill prove dual-worker lease takeover against one meta MySQL. The lease state machine is unchanged.
- A project-local verify-binlog-server Cursor skill drives a released binary through the HTTP API and the embedded Console.
- Go modules: validator `v10.30.5`.
- Quick Start, landing-page, and deployment download examples now point at `v0.5.6`.

## Upgrade Notes

- No schema migration is required for `v0.5.6`. Migrations are still `000001_init_schema` only.
- `PRODUCTION=true` requires a non-empty `--encryption-key` or the process does not listen. Outside production, an empty key still loads existing plaintext rows.
- With `api.auth.enabled=true`, a browser visit to `/ui/` or `/swagger/` returns 401 until a proxy or client sends the same bearer token or API key. Loopback demos with auth disabled keep an open console. `/healthz` stays unauthenticated.
- `GET /api/tasks` remains `{items,total,limit,offset}`. Dashboard and summary counters are SQL aggregates over that filtered set, not an in-process scan of every task row.
- Lease ownership, takeover events, and the claim path are unchanged from `v0.5.5`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.6.zh-CN.md
