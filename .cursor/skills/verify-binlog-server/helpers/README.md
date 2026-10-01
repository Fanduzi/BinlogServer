# helpers

## Members

| File | Responsibility |
|------|----------------|
| `control-binlog-server.sh` | Launch, doctor, curl, cleanup, and env-print for one isolated verification run |

## Interface

- `control-binlog-server.sh launch` — create `$BINLOG_VERIFY_BASE` (`data/`, `logs/`), optional `migrate up` when `BINLOG_VERIFY_META_DSN` is set, write `start.env`, start `BINLOG_SERVER_BIN`, wait until `GET /healthz` is `ok`, write the pid file
- `control-binlog-server.sh doctor` — version, pid alive, `/healthz` `ok`, `/api/health` `status=ok`, `/ui/` 200, and (when `ss` can see owners) the listen port belongs to that pid
- `control-binlog-server.sh curl [--] METHOD PATH [curl-args...]` — curl `BASE_URL` + path; a leading `--` before METHOD is optional
- `control-binlog-server.sh cleanup` — signal only the pid in the pid file, remove `BINLOG_VERIFY_BASE`, leave `BINLOG_VERIFY_EVIDENCE` in place
- `control-binlog-server.sh env-print` — print the resolved isolation variables

The helper does not kill processes by name.

## Dependencies

- Upstream: a released `binlog-server` binary. When `BINLOG_VERIFY_META_DSN` is set, the sibling `migrate` binary and `migrations/` directory (or `BINLOG_VERIFY_MIGRATE_BIN` / `BINLOG_VERIFY_MIGRATIONS`).
- Downstream: the single process this run started. Proof files stay under `BINLOG_VERIFY_EVIDENCE`.
