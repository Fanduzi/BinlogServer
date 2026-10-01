# Task lifecycle

An operator creates a replication task, sees it in the task list, starts it, stops it, and deletes it. The Console task row and the task JSON stay on the same id and state.

## Sub-features

- `task-create` accepts the create JSON and returns the new task without `source.password`.
- `task-list` shows that task from `GET /api/tasks` and as `task-row-<id>` on the Tasks view.
- `task-start` moves a `CREATED` task to `STARTING` and returns HTTP 204.
- `task-stop` moves a started task to `STOPPED` and returns HTTP 204.
- `task-delete` removes the task and returns HTTP 204. A later GET is 404.

## How to get to it (user POV)

- `POST /api/tasks` with `name`, `cluster_key`, `source`, `start.mode` (`LATEST`, `FILE_POS`, or `GTID`), and `storage.retention_days`.
- `GET /api/tasks` for the paged list.
- Open `/ui/`, choose `view-nav-tasks`, then the row `task-row-<id>` or `task-detail-trigger-<id>`.
- In the drawer (`task-drawer`), use `task-action-start`, `task-action-stop`, and `task-action-delete`. The state tag is `task-drawer-status`.
- `POST /api/tasks/<id>/start`, `POST /api/tasks/<id>/stop`, and `DELETE /api/tasks/<id>`.

## Driving it with control-binlog-server

Preconditions:

- Doctor has passed for this run.
- `BINLOG_VERIFY_EVIDENCE` exists.
- No other run is using `cluster_key` `verify-lifecycle` on this server.

- **Create.** Submit a `LATEST` task. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl POST /api/tasks -H 'Content-Type: application/json' -d '{"name":"verify-lifecycle","cluster_key":"verify-lifecycle","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"},"start":{"mode":"LATEST"},"storage":{"retention_days":7}}' -o "$BINLOG_VERIFY_EVIDENCE/task-lifecycle/create.json" -w '%{http_code}'`. Status is `201`. `create.json` has `state` `CREATED`, `cluster_key` `verify-lifecycle`, `start.mode` `LATEST`, and no `password` field value.
- **List.** Read the task back. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl GET /api/tasks -o "$BINLOG_VERIFY_EVIDENCE/task-lifecycle/list.json"`. `items` includes the id from `create.json`.
- **Open in Console.** Choose Tasks, then that id. The row handle is `task-row-<id>` and the detail trigger is `task-detail-trigger-<id>`. `task-drawer-status` reads the same state as `GET /api/tasks/<id>`.
- **Start.** Start the created task. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl POST /api/tasks/<id>/start -o "$BINLOG_VERIFY_EVIDENCE/task-lifecycle/start.body" -w '%{http_code}'`. Status is `204`. A follow-up `GET /api/tasks/<id>` shows `STARTING` or a later state, not `CREATED`.
- **Stop.** Stop the same task. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl POST /api/tasks/<id>/stop -w '%{http_code}'`. Status is `204`. A follow-up GET shows `STOPPED` once the runner has finished stopping.
- **Delete.** Delete the stopped task. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl DELETE /api/tasks/<id> -w '%{http_code}'`. Status is `204`. `GET /api/tasks/<id>` then returns 404.
- **Proof.** Keep `create.json`, `list.json`, and the start status under `$BINLOG_VERIFY_EVIDENCE/task-lifecycle/`.

## Gotchas

- `FILE_POS` without `file` and `pos`, or `GTID` without `gtid_set` (or the `gtid` alias), returns 400 and creates nothing.
- `cluster_key` rejects slashes and `..`. A duplicate `cluster_key` on this server returns 400.
- Start returns 204 with an empty body. Read state with a second GET.
- Starting a task that is already `RUNNING` returns 400. Stop it first.
- The list route is paged (`limit` default 100, max 500). A missing id on page 1 is not proof of deletion until GET by id returns 404.
