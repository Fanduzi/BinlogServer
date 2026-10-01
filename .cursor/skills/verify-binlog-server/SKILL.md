---
name: verify-binlog-server
description: "Drive a released BinlogServer binary through its HTTP API and embedded Console to prove health, task lifecycle, checkpoint resume, dashboard summary, and failure paths."
---

# Verify BinlogServer

Prove BinlogServer by launching a released `binlog-server` binary and driving its HTTP API plus the Console embedded in that same binary at `/ui/`. Read `features/README.md` first, then the feature file for the behavior under test. A passing frontend Vite Playwright spec that mocks the API, and a `go run` dev process, are outside this skill.

Keep `/maintain-verification-skill` as the upkeep loop when the map drifts from the app.

## Launch

Export one isolation set for the whole run, then launch. Concurrent runs use different ports, data directories, and meta database names.

```bash
export BINLOG_VERIFY_RUN_ID="${BINLOG_VERIFY_RUN_ID:-$(date +%Y%m%d%H%M%S)-$$}"
export BINLOG_VERIFY_LISTEN="${BINLOG_VERIFY_LISTEN:-127.0.0.1:18080}"
export BINLOG_VERIFY_BASE="${BINLOG_VERIFY_BASE:-/tmp/binlog-server-verify-$BINLOG_VERIFY_RUN_ID}"
export BINLOG_VERIFY_EVIDENCE="${BINLOG_VERIFY_EVIDENCE:-/tmp/binlog-server-verify-evidence-$BINLOG_VERIFY_RUN_ID}"
export BINLOG_SERVER_BIN="/absolute/path/to/release/binlog-server"
.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh launch
```

Ready means launch exits 0: the pid file exists and `GET /healthz` returned `ok`. The helper creates `$BINLOG_VERIFY_BASE`, `$BINLOG_VERIFY_BASE/data`, and `$BINLOG_VERIFY_BASE/logs`, writes `$BINLOG_VERIFY_BASE/start.env`, starts the binary with `nohup` from that base directory, and records `$BINLOG_VERIFY_BASE/binlog-server.pid`.

Unset `BINLOG_VERIFY_META_DSN` for a standalone memory control plane. When it is set, launch runs `migrate up` against that DSN before the server starts, and the server receives it as `BINLOG_SERVER_META_DSN`. Point the DSN at a database name reserved for this `BINLOG_VERIFY_RUN_ID`. Include `parseTime=true` on the DSN (the helper appends it when missing); without it, task list/get returns HTTP 500 on DATETIME scan. The task `source` host:port must not be the same metadata endpoint. Checkpoint read/resume (`features/checkpoint-resume.md`) needs this meta DSN — without it `GET /api/tasks/<id>/checkpoint` always returns 404.

Loopback listen (`127.0.0.1`) matches the quick start: API auth stays off. The child process gets a clean environment (`env -i`) so a leftover `BINLOG_SERVER_*` value from the parent shell cannot attach this run to another data dir or meta database.

## Doctor

Run doctor before driving, and again when a call looks wrong. It is read-only.

```bash
.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh doctor
```

Doctor passes only when all of these hold:

- `BINLOG_SERVER_BIN --version` prints a version line
- the pid in the pid file is alive
- `GET /healthz` body is `ok`
- `GET /api/health` JSON has `"status":"ok"`
- `GET /ui/` returns HTTP 200
- when `ss` can see socket owners, the listen port is owned by that pid

A passing doctor prints `PASS` on its own line and exits 0. `ss` is preferred for the ownership check. If `ss` is missing or cannot show pids, doctor still requires the health checks above and says the ownership check was skipped.

## Drive

The helper's HTTP surface is the drive harness. Browser proof, when a browser is available, uses the embedded Console at `$BASE_URL/ui/` and these stable handles:

- `view-nav-overview`, `view-nav-tasks`, `view-nav-sources`, `view-nav-workers`, `view-nav-alerts`, `view-nav-collapse`
- `kpi-abnormal`, `kpi-failed`, `kpi-delayed`, `kpi-starting`, `kpi-running`, `kpi-all`, `kpi-normal` (value nodes: `kpi-abnormal-value`, `kpi-failed-value`, `kpi-delayed-value`, `kpi-starting-value`)
- `task-row-<id>`, `task-detail-trigger-<id>`, `task-drawer`, `task-drawer-status`, `task-drawer-checkpoint`, `task-action-start`, `task-action-stop`, `task-action-delete`
- `auth-required-banner` when the Console is told the API requires a token

Create-task JSON:

```json
{
  "name": "verify-task",
  "cluster_key": "verify-task",
  "source": {
    "host": "127.0.0.1",
    "port": 3306,
    "user": "repl",
    "password": "secret",
    "flavor": "mysql"
  },
  "start": { "mode": "LATEST" },
  "storage": { "retention_days": 7 }
}
```

`start.mode` is `LATEST`, `FILE_POS` (requires `file` and `pos`), or `GTID` (requires `gtid_set`, or the `gtid` alias). `cluster_key` is `[A-Za-z0-9._-]`. `storage.retention_days` is 1..3650. Responses omit `source.password`.

```bash
HELPER=.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh
"$HELPER" curl GET /healthz
"$HELPER" curl POST /api/tasks -H 'Content-Type: application/json' -d @/path/to/task.json
"$HELPER" curl POST /api/tasks/<id>/start
```

Arguments after PATH are passed to curl. The helper drops one leading `--` before METHOD so that marker is not taken as the method or the path. Follow the feature file for the entry under test. One convenient route does not cover the other entries in that file.

## Evidence

Write proof under `BINLOG_VERIFY_EVIDENCE`. Capture the request and the resulting state. For HTTP, save the command, status, and body. For the Console shell, save the `/ui/` status and body and confirm the title `Binlog Server Console`. For a mutation, read it back with a second GET. Record the feature id and the entry point in the artifact name.

```bash
mkdir -p "$BINLOG_VERIFY_EVIDENCE/health-console"
.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh doctor \
  | tee "$BINLOG_VERIFY_EVIDENCE/health-console/doctor.txt"
.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl GET /healthz \
  | tee "$BINLOG_VERIFY_EVIDENCE/health-console/healthz.txt"
```

Cleanup deletes the instance directory and leaves this evidence directory in place.

## Cleanup

```bash
.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh cleanup
test -d "$BINLOG_VERIFY_EVIDENCE"
```

Cleanup signals only the pid stored in `$BINLOG_VERIFY_BASE/binlog-server.pid`, then removes `BINLOG_VERIFY_BASE`. It does not signal by process name. `BINLOG_VERIFY_EVIDENCE` remains, including when that directory was placed inside the base directory. Run cleanup after a failed launch too, so a half-started pid does not keep the port.

## Helpers

`helpers/control-binlog-server.sh` is the only helper. Invoke it as shown above. `env-print` shows the resolved run id, listen address, base, evidence, binary, meta DSN, base URL, and pid file:

```bash
.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh env-print
```

Subcommands: `launch`, `doctor`, `curl`, `cleanup`, `env-print`.
