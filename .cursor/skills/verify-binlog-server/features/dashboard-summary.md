# Dashboard summary

The overview shows task and replication counters. The same numbers come from `GET /api/summary` and from `summary` inside `GET /api/dashboard`. `STARTING` is its own count, separate from `RUNNING`.

## Sub-features

- `summary-totals` returns `total`, `running`, `starting`, `failed`, `stopped`, `normal`, `delayed`, and `abnormal`.
- `summary-starting` counts `STARTING` only in `starting`, not in `running`.
- `dashboard-same-numbers` repeats that summary on `GET /api/dashboard` with the task rows and source rollup.
- `kpi-cards` expose the same counters on the overview as `kpi-abnormal`, `kpi-failed`, `kpi-delayed`, `kpi-starting`, `kpi-running`, `kpi-all`, and `kpi-normal`.

## How to get to it (user POV)

- Open `/ui/`. The landing view is `view-nav-overview`.
- Read the KPI row: `kpi-abnormal`, `kpi-failed`, `kpi-delayed`, `kpi-starting`, `kpi-running`, `kpi-all`, `kpi-normal`.
- `GET /api/summary` and `GET /api/dashboard`.

## Driving it with control-binlog-server

Preconditions:

- Doctor has passed for this run.
- The run's task set is known. A fresh standalone process with no tasks has `total` 0.
- For the starting-versus-running check, one task has been created and started and `GET /api/tasks/<id>` shows `STARTING` or `RUNNING`.

- **Empty summary.** Read counters before creating tasks. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /api/summary -o "$BINLOG_VERIFY_EVIDENCE/dashboard-summary/summary-empty.json"`. `total` is `0`.
- **After create.** Create one task with the lifecycle JSON and `cluster_key` `verify-summary`, then run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /api/summary -o "$BINLOG_VERIFY_EVIDENCE/dashboard-summary/summary-created.json"`. `total` is `1`. `running` and `starting` stay `0` while state is `CREATED`.
- **Dashboard agrees.** Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /api/dashboard -o "$BINLOG_VERIFY_EVIDENCE/dashboard-summary/dashboard.json"`. `summary.total` equals the summary document's `total`, and `tasks` includes the created id.
- **Starting is separate.** Start that task. While `GET /api/tasks/<id>` shows `STARTING`, run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /api/summary`. `starting` is at least `1` and `running` does not include that task. When the task later shows `RUNNING`, `running` increases and `starting` no longer includes it.
- **KPI row.** On `view-nav-overview`, `kpi-all` shows `total`, `kpi-starting` shows `starting`, and `kpi-running` shows `running`. Value handles that exist: `kpi-starting-value`, `kpi-failed-value`, `kpi-delayed-value`, `kpi-abnormal-value`.
- **Proof.** Save the empty summary, the post-create summary, and `dashboard.json` under `$BINLOG_VERIFY_EVIDENCE/dashboard-summary/`.

## Gotchas

- `GET /api/tasks` is a page. Overview numbers come from summary and dashboard, so a filtered task page can disagree with `kpi-all` without being a bug.
- `kpi-running`, `kpi-all`, and `kpi-normal` have no separate `*-value` test id. Read the card text for those three.
- `limit` above 500 on dashboard or summary returns 400 `invalid limit`.
- Host filters treat `localhost` and explicit loopback literals as one source. Other host strings match exactly, including case.
