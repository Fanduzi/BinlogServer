# BinlogServer verification map

This directory is the maintained source for verifying user-facing BinlogServer behavior. Read the index before driving the app, then use the matching feature file as the recipe.

## Baseline preconditions

- Launch a released `binlog-server` binary with `helpers/control-binlog-server.sh launch`.
- Default listen is `127.0.0.1:18080`. Default base is `/tmp/binlog-server-verify-$BINLOG_VERIFY_RUN_ID`.
- Set `BINLOG_VERIFY_EVIDENCE` outside the base directory (default `/tmp/binlog-server-verify-evidence-$BINLOG_VERIFY_RUN_ID`).
- Give every concurrent run its own `BINLOG_VERIFY_LISTEN`, `BINLOG_VERIFY_BASE`, and, when metadata is enabled, its own database name in `BINLOG_VERIFY_META_DSN`.
- Run `control-binlog-server.sh doctor` and require version, a live pid, `/healthz` `ok`, `/api/health` `status=ok`, `/ui/` 200, and port ownership when `ss` can show it.
- Drive only the instance this run started.

## Driving conventions

- Start every recipe from the baseline unless its preconditions say otherwise.
- Prefer the HTTP helper for API proof. When a browser is used, drive the embedded Console at `/ui/` and the `data-testid` values named in the feature file.
- Treat every command as literal. Keep JSON fields, modes, and flags unchanged.
- Run HTTP through `control-binlog-server.sh curl`.
- Leave `BINLOG_VERIFY_EVIDENCE` in place during cleanup.

## Proof and skip reporting

- Capture the user action and the resulting state, not only the final screen.
- HTTP proof includes the command, status, and body.
- Console shell proof includes `GET /ui/` 200 and the document title `Binlog Server Console` from that same process.
- Mutation proof includes a follow-up read of the stored task, checkpoint, or summary.
- Record the feature id and entry point with every artifact.
- Report an unreachable path with the attempted command and the unmet precondition.
- Do not report a skipped entry point as verified through a different path.
- A Vite Playwright run against the mock API is not a result for this map.

## Feature entry contract

Each feature file starts with an H1 title and one paragraph describing the user-visible behavior. It then uses exactly four H2 sections in this order.

1. `Sub-features` lists short IDs with one line for each behavior.
2. `How to get to it (user POV)` lists every user entry point.
3. `Driving it with control-binlog-server` starts with `Preconditions:` and uses labeled bullets that pair each user action with an exact command and observable result.
4. `Gotchas` lists traps that can waste or invalidate a verification run.

Keep implementation details out of the map. Name only user paths, stable handles, required state, commands, and observable proof.

## Features

- [Health and Console](./health-console.md) covers `/healthz`, `/api/health`, and the embedded Console shell.
- [Task lifecycle](./task-lifecycle.md) covers create, list, start, stop, and delete.
- [Checkpoint resume](./checkpoint-resume.md) covers reading a saved position and continuing from it.
- [Dashboard summary](./dashboard-summary.md) covers summary counters and the overview KPI cards.
- [Failure paths](./failure-paths.md) covers bad source password and unreachable source ending in `FAILED`.
