# Health and Console

An operator confirms the released process is up, the JSON health endpoint agrees, and the Console shell embedded in that binary opens at `/ui/`.

## Sub-features

- `healthz-ok` returns the text `ok` from `GET /healthz`.
- `api-health-ok` returns `{"status":"ok"}` from `GET /api/health`.
- `console-shell` serves the embedded Console at `GET /ui/` with HTTP 200 and title `Binlog Server Console`.
- `doctor-owns-port` reports the listen port owned by this run's pid when `ss` can see owners.

## How to get to it (user POV)

- Request `GET /healthz` on the listen address.
- Request `GET /api/health` on the listen address.
- Open `/ui/` in a browser. The overview nav item is `view-nav-overview`.
- With API auth enabled, a rejected call shows `auth-required-banner`. The default loopback verification instance leaves auth off, so that banner stays hidden.

## Driving it with control-binlog-server

Preconditions:

- `BINLOG_VERIFY_RUN_ID`, `BINLOG_VERIFY_LISTEN`, `BINLOG_VERIFY_BASE`, and `BINLOG_VERIFY_EVIDENCE` are exported for this run.
- `BINLOG_SERVER_BIN` is the released binary.
- `control-binlog-server.sh launch` has exited 0.
- `control-binlog-server.sh doctor` exits 0 before the curls below.

- **Doctor.** Confirm the instance is the one this run started. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh doctor`. Stdout contains `version:`, `pid:` with `alive`, `healthz: ok`, `api-health: ok`, and `ui: 200`.
- **Health text.** Request the liveness body. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /healthz`. The body is `ok`.
- **Health JSON.** Request the API health object. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /api/health`. The body contains `"status":"ok"`.
- **Console shell.** Open the embedded UI document. Run `.cursor/skills/verify-binlog-server/helpers/control-binlog-server.sh curl -- GET /ui/ -o "$BINLOG_VERIFY_EVIDENCE/health-console/ui.html" -w '%{http_code}'`. The status is `200` and `ui.html` contains `Binlog Server Console`.
- **Proof.** Save the doctor transcript and both health bodies under `$BINLOG_VERIFY_EVIDENCE/health-console/`. The files name this feature and still exist after cleanup.

## Gotchas

- `GET /ui` without the trailing slash is not the shell check. Use `/ui/`.
- Doctor's port-owner line is required when `ss` prints `pid=`. A skipped line means `ss` could not see owners; the health lines still have to pass.
- `auth-required-banner` appears only after the Console hits a protected API. Its absence on the default loopback instance is expected.
- Frontend Playwright against the Vite mock API never hits this process.
