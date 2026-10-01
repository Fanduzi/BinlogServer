# Failure paths

A task that cannot read the source ends `FAILED`. A bad password fails immediately with `SOURCE_ACCESS_DENIED`. An unreachable source retries, then fails with `SOURCE_UNREACHABLE`. The task JSON, the replication view, and the failed KPI all show that outcome.

## Sub-features

- `fail-bad-password` reaches `FAILED` with `last_error` beginning `SOURCE_ACCESS_DENIED` when the source rejects the password.
- `fail-unreachable` reaches `FAILED` with `last_error` beginning `SOURCE_UNREACHABLE` when the source cannot be contacted.
- `fail-visible` shows the same `FAILED` state and `last_error` on `GET /api/tasks/<id>`, `GET /api/tasks/<id>/replication`, and `GET /api/dashboard`.
- `fail-kpi` increments `failed` and the overview card `kpi-failed`.

## How to get to it (user POV)

- Create a task whose `source.password` is wrong for a MySQL that is up, then start it.
- Create a task whose `source.host` and `source.port` accept no connection, then start it.
- Read the task, the replication panel, and the overview card `kpi-failed` (`kpi-failed-value`).
- Open `task-drawer-status` for that id. It shows the failed state.

## Driving it with control-binlog-server

Preconditions:

- Doctor has passed for this run.
- Bad-password proof uses a MySQL that answers on the configured host and port and returns access denied for the password in the JSON. An unreachable port is a different sub-feature.
- Unreachable proof uses a closed port on `127.0.0.1` (the recipe uses port `1`) so the dial fails before authentication.
- Each sub-feature uses its own `cluster_key`.

- **Bad password.** Create and start a task against the live source with a wrong password. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- POST /api/tasks -H 'Content-Type: application/json' -d '{"name":"verify-bad-password","cluster_key":"verify-bad-password","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"wrong-password","flavor":"mysql"},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}' -o "$BINLOG_VERIFY_EVIDENCE/failure-paths/bad-password-create.json" -w '%{http_code}'`, then `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- POST /api/tasks/<id>/start -w '%{http_code}'`. Create status is `201` and start status is `204`. Poll `GET /api/tasks/<id>` until `state` is `FAILED`. `last_error` begins with `SOURCE_ACCESS_DENIED`.
- **Unreachable.** Create and start a task aimed at port `1`. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- POST /api/tasks -H 'Content-Type: application/json' -d '{"name":"verify-unreachable","cluster_key":"verify-unreachable","source":{"host":"127.0.0.1","port":1,"user":"repl","password":"secret","flavor":"mysql"},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}' -o "$BINLOG_VERIFY_EVIDENCE/failure-paths/unreachable-create.json" -w '%{http_code}'`, then `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- POST /api/tasks/<id>/start -w '%{http_code}'`. Poll `GET /api/tasks/<id>` until `state` is `FAILED`. `last_error` begins with `SOURCE_UNREACHABLE`.
- **Visible on read APIs.** For each failed id, run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /api/tasks/<id>/replication` and `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /api/dashboard`. Replication `state` is `FAILED` and `last_error` matches the task. Dashboard `tasks` includes that id with the same `task.state` and `task.last_error`.
- **KPI.** `GET /api/summary` `failed` counts those tasks. On `view-nav-overview`, `kpi-failed` and `kpi-failed-value` show that count.
- **Proof.** Save the create bodies and the final task JSON under `$BINLOG_VERIFY_EVIDENCE/failure-paths/`.

## Gotchas

- Start still returns 204 when the source is bad. `FAILED` shows up on a later GET, not in the start response.
- Unreachable is retried before `FAILED` (ten consecutive source failures, with backoff from 1s up to 30s). A single immediate GET that still says `STARTING` or `RETRY_BACKOFF` is not a failure of the path. Keep polling.
- Bad password is permanent: `SOURCE_ACCESS_DENIED` does not spend the unreachable retry budget. A closed port will not produce `SOURCE_ACCESS_DENIED`.
- The create response strips `source.password`. Do not look for the password in the saved JSON.
- `last_error` on a failed task stays visible to dashboard and replication. Filtering `GET /api/tasks?state=FAILED` is a list page, not a substitute for the dashboard row.
