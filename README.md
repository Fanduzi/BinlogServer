# binlog_server

Binlog Server is a Go service for pulling MySQL binlog, writing local sealed files, persisting checkpoints, and coordinating task execution with lease-aware scheduling.

It is designed for operators who want a control plane API, resumable replication tasks, local durability, and optional S3-compatible upload.

## Quick Start

Use this path if you want to get the service up and verify the basic API as quickly as possible.

### Prerequisites

- Go `1.24+`
- A reachable MySQL instance with binlog enabled
- Optional: Docker, if you want to run the E2E suite

### 1. Start the server

```bash
go run ./cmd/binlog-server
```

Default listen address: `:8080`

If you want an explicit address:

```bash
BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080 go run ./cmd/binlog-server
```

### 2. Verify health

```bash
curl -fsS http://127.0.0.1:8080/healthz
```

Expected response:

```text
ok
```

### 3. Create your first task

Replace the source connection values with a MySQL instance you control.

```bash
curl -fsS -X POST http://127.0.0.1:8080/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "quickstart-task",
    "cluster_key": "quickstart-task",
    "source": {
      "host": "127.0.0.1",
      "port": 3306,
      "user": "repl",
      "password": "secret",
      "flavor": "mysql"
    },
    "start": {
      "mode": "LATEST"
    },
    "storage": {
      "retention_days": 7
    }
  }'
```

### 4. Start the task

Replace `<task-id>` with the `id` returned by the previous command.

```bash
curl -i -X POST http://127.0.0.1:8080/api/tasks/<task-id>/start
```

### 5. Check task state

```bash
curl -fsS http://127.0.0.1:8080/api/tasks
```

### 6. Open the UI or Swagger

- UI: `http://127.0.0.1:8080/ui/`
- Swagger: `http://127.0.0.1:8080/swagger/index.html`

If you want the longer operational walkthrough, start from [docs/guide/README.md](docs/guide/README.md).

## Minimal Production Notes

Development defaults are intentionally loose. Production should not use the default posture unchanged.

- Auth: API auth is disabled by default for developer convenience. In production, enable auth and protect both `/api/*` and `/metrics`. See [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md).
- Meta DB: if you use `meta_dsn`, run migrations before starting the service. The service does not auto-create or auto-upgrade schema.
- Upload: S3-compatible upload is optional. If you enable it, provide a complete upload config set; partial config will fail uploader initialization.
- Tracing: OpenTelemetry is designed to be low-risk and default-off. Enable it explicitly and validate the exporter configuration before production rollout.

Minimum production checklist:

```bash
export BINLOG_SERVER_API_AUTH_ENABLED=true
export BINLOG_SERVER_API_AUTH_MODE=bearer
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="$(openssl rand -hex 32)"
export BINLOG_SERVER_API_AUTH_PROTECT_API=true
export BINLOG_SERVER_API_AUTH_PROTECT_METRICS=true
```

If you use a metadata database:

```bash
export META_DSN='meta:replace_me@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true'
make migrate-up META_DSN="$META_DSN"
```

## Common Pitfalls

### `make e2e-quick` fails locally

- Check that Docker Desktop or another Docker daemon is running.
- The E2E suite pulls MySQL and Percona images and starts local containers.
- Detailed E2E usage lives in [scripts/e2e/README.md](scripts/e2e/README.md).

### The service starts but meta-backed tasks fail

- If `meta_dsn` is configured, the schema must already exist.
- Run `make migrate-up META_DSN=...` before starting the server.
- Migration command details are in [cmd/migrate/README.md](cmd/migrate/README.md).

### Auth is still open in production

- This is the most important default to override.
- Development keeps auth disabled; production should not.
- Review [SECURITY.md](SECURITY.md) before exposing the service.

### Upload does not work

- `endpoint`, `bucket`, `access_key`, and `secret_key` must all be present together.
- Region and prefix are optional; partial required config is not.
- Current upload implementation targets S3-compatible APIs.

### Metrics or tracing look empty

- `/metrics` exists even before tasks are running; some metrics are placeholders until real activity appears.
- Tracing is default-off, so seeing no spans is expected unless you enabled it explicitly.

## Upgrade And Release

Before upgrading, read [CHANGELOG.md](CHANGELOG.md).

Pay attention to these classes of changes:

- Schema or migration requirements
- Config key additions or deprecations
- SQL generation workflow changes such as `sqlc`
- Observability contract changes that affect dashboards or alerts

This repository does not auto-apply migrations or auto-rewrite config for you, so upgrades should be treated as an operator task, not just a binary swap.

## Repository Map

Use these docs when you need more than the quick start:

| Topic | Entry |
| --- | --- |
| Operator and usage guides | [docs/guide/README.md](docs/guide/README.md) |
| Security policy | [SECURITY.md](SECURITY.md) |
| Changelog | [CHANGELOG.md](CHANGELOG.md) |
| Server command | [cmd/binlog-server/README.md](cmd/binlog-server/README.md) |
| Database migrations | [cmd/migrate/README.md](cmd/migrate/README.md) |
| API module | [internal/api/README.md](internal/api/README.md) |
| Replication runtime | [internal/replication/README.md](internal/replication/README.md) |
| Upload module | [internal/upload/README.md](internal/upload/README.md) |
| E2E suite | [scripts/e2e/README.md](scripts/e2e/README.md) |

## Development

Run the main verification gates:

```bash
go test ./...
go vet ./...
make e2e-quick
```

If you want the frontend in separate dev mode:

```bash
cd frontend
npm install
npm run dev
```
