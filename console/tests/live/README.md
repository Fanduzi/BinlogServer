# console/tests/live

Live checks for the embedded Console, run with Playwright against a real
binlog-server. Each script compares the Console at `/ui/` with the classic
console at `/ui-classic/` (the reference until `frontend/` is deleted), or
asserts Console behaviour directly.

| Script | What it checks |
| --- | --- |
| `rc.mjs` | 401 → settings, token storage, Tasks/Alerts paging, filters, KPI jumps, deep links, locale, mobile |
| `ov.mjs` | Overview cluster panel and source coverage match classic; KPI jump |
| `src.mjs` | Sources lookup: same API calls as classic, validation, clamp, Clear, page reset |
| `wk.mjs` | Workers rows (mocked list) match classic text and order; empty state |
| `fixes.mjs` | Deep link without a token, missing worker counts, worker-count badge, batch preview strings |
| `auth-inputs.mjs` | 401 on every route shows only the auth dialog; KPI parity; raw unknown values; number clamps |
| `rd.mjs` | Task drawer: replay, PITR, start/stop, delete (deletes task 6), unknown id, mobile |

## Run

The checks expect the ui302 test env: a server with bearer token
`ui302-test-token` and 26 seeded tasks, freshly seeded (rd.mjs deletes a task).

```bash
cd console
SERVER=http://127.0.0.1:18302 npm run test:live        # all, in order
SERVER=http://127.0.0.1:18302 node tests/live/run.mjs wk fixes   # some
```

`BASE` / `CLASSIC` override the two console URLs (e.g. `BASE=http://127.0.0.1:5174`
for `npm run dev`). Screenshots go to `OUT` (default `/tmp/console-live`).
