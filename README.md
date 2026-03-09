# BinlogServer

[![Release](https://img.shields.io/github/v/release/Fanduzi/BinlogServer?display_name=tag)](https://github.com/Fanduzi/BinlogServer/releases)
![Platform](https://img.shields.io/badge/platform-darwin%20amd64%20%7C%20darwin%20arm64%20%7C%20linux%20amd64%20%7C%20linux%20arm64-blue)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

English | [中文](README_ZH.md) | [Changelog](CHANGELOG.md) | [Security](SECURITY.md)

Binlog Server is a service for pulling MySQL binlog into durable local files, persisting checkpoint state, and managing replication tasks through an API-driven control plane with UI, optional S3-compatible upload, and cluster coordination support.

If this is your first time visiting the repository, start with `Quick Start`. If you are evaluating whether the project fits your use case, read the positioning section below first.

## What Problem Does It Solve?

It turns binlog pulling, local persistence, checkpoint tracking, and task lifecycle management into an operational service instead of a collection of scripts, cron jobs, and ad hoc metadata.

Good fit for:

- Teams that want binlog pulling, local durability, and task state management as a service
- Environments that need `LATEST`, `FILE_POS`, or `GTID` start modes
- Workflows that want local persistence with optional S3-compatible upload
- Operators who want API, UI, and observability instead of a one-off script

Probably not a fit for:

- One-time exports instead of continuous replication
- Very small single-node scripts without task orchestration needs
- Teams looking for a full CDC platform replacement

## Why Use It?

- Clear checkpoint semantics: checkpoint advances only after local `fsync` succeeds
- Supports `LATEST`, `FILE_POS`, and `GTID` start modes
- Built-in API, UI, Swagger, metrics, and optional tracing
- Supports metadata-backed coordination, lease management, and S3-compatible upload
- Includes repository E2E scenarios to validate core regression paths

## Architecture

BinlogServer is organized as a control-plane-oriented service with clear boundaries between HTTP/API handling, task orchestration, replication execution, metadata persistence, upload integration, and UI delivery.

### Modules

| Module | Responsibility | Entry |
| --- | --- | --- |
| `cmd` | Top-level service startup and migration commands | [cmd/README.md](cmd/README.md) |
| `internal/api` | HTTP routes, request validation, Swagger, metrics, tracing hooks | [internal/api/README.md](internal/api/README.md) |
| `internal/app` | Runtime assembly and role lifecycle orchestration | [internal/app/README.md](internal/app/README.md) |
| `internal/binlog` | Local binlog file writing and checkpoint persistence helpers | [internal/binlog/README.md](internal/binlog/README.md) |
| `internal/config` | YAML and environment-based configuration loading | [internal/config/README.md](internal/config/README.md) |
| `internal/logging` | Logger setup and log output rotation | [internal/logging/README.md](internal/logging/README.md) |
| `internal/meta` | Metadata storage, schema checks, lease-backed coordination data | [internal/meta/README.md](internal/meta/README.md) |
| `internal/replication` | MySQL replication pull loop and durable local write path | [internal/replication/README.md](internal/replication/README.md) |
| `internal/tasks` | Task state machine, scheduling, and execution orchestration | [internal/tasks/README.md](internal/tasks/README.md) |
| `internal/ui` | Embedded UI asset serving | [internal/ui/README.md](internal/ui/README.md) |
| `internal/upload` | S3-compatible upload integration | [internal/upload/README.md](internal/upload/README.md) |
| `scripts` | Local build helpers, release asset packaging, and E2E entrypoints | [scripts/README.md](scripts/README.md) |
| `frontend` | Frontend source and build pipeline for the embedded UI | [frontend/README.md](frontend/README.md) |

## Install / Download

- For tagged public releases, download the archive that matches your platform from GitHub Releases:
  - `binlog-server_<version>_darwin_amd64.tar.gz`
  - `binlog-server_<version>_darwin_arm64.tar.gz`
  - `binlog-server_<version>_linux_amd64.tar.gz`
  - `binlog-server_<version>_linux_arm64.tar.gz`
- The release page also provides `checksums.txt`; verify it before extracting.
- UI assets are embedded into the binary, so you do not need to download a separate frontend package.

Source build remains the fallback path:

```bash
make ui-build
go build -o binlog-server ./cmd/binlog-server
```

To prepare a local set of release assets:

```bash
make release-assets VERSION=v0.1.0
```

## Quick Start

This section keeps the shortest path to a working service.

### Prerequisites

- Go `1.26.0+`
- A reachable MySQL instance with binlog enabled
- Docker available if you want to run E2E scenarios

### 1. Start the service

```bash
go run ./cmd/binlog-server
```

The default listen address is `:8080`.

To set it explicitly:

```bash
BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:18080 go run ./cmd/binlog-server
```

### 2. Verify `/healthz`

```bash
curl -fsS http://127.0.0.1:8080/healthz
```

Expected response:

```text
ok
```

### 3. Create your first task

Replace the MySQL connection details with your own instance:

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

Replace `<task-id>` with the `id` returned from the previous step:

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

For more operational and usage guidance, continue from [docs/guide/README.md](docs/guide/README.md).

### What You Should See

- `/healthz` returns `ok`
- `/api/tasks` lists the task you just created
- `/ui/` opens the management interface
- `/swagger/index.html` lets you inspect and try the API
- After the task starts, checkpoint and metrics begin reflecting runtime state

> ⚠️ **Security Warning**
>
> By default, API authentication is **DISABLED** for development convenience.
>
> **For production deployments, you MUST:**
> 1. Set `api.auth.enabled: true` (or `BINLOG_SERVER_API_AUTH_ENABLED=true`)
> 2. Configure your authentication method (Bearer Token or API Key)
> 3. Protect `/api/*` and `/metrics`
>
> See [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md) for security guidance.

## Minimal Production Notes

Development defaults are intentionally permissive. Do not carry them unchanged into production.

- Auth: disabled by default; production should protect at least `/api/*` and `/metrics`
- Meta DB: if `meta_dsn` is configured, run migrations first; the service does not auto-create or auto-upgrade schema
- Upload: S3-compatible upload is optional, but required fields must be complete when enabled
- Tracing: disabled by default; validate exporter configuration and sampling before rollout

Minimum production baseline:

```bash
export BINLOG_SERVER_API_AUTH_ENABLED=true
export BINLOG_SERVER_API_AUTH_MODE=bearer
export BINLOG_SERVER_API_AUTH_BEARER_TOKEN="$(openssl rand -hex 32)"
export BINLOG_SERVER_API_AUTH_PROTECT_API=true
export BINLOG_SERVER_API_AUTH_PROTECT_METRICS=true
```

If you use the metadata database:

```bash
export META_DSN='meta:replace_me@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true'
make migrate-up META_DSN="$META_DSN"
```

If you enable upload, provide at least:

- `BINLOG_SERVER_UPLOAD_ENDPOINT`
- `BINLOG_SERVER_UPLOAD_BUCKET`
- `BINLOG_SERVER_UPLOAD_ACCESS_KEY`
- `BINLOG_SERVER_UPLOAD_SECRET_KEY`

## FAQ / Common Pitfalls

### `make e2e-quick` fails locally

- Make sure Docker Desktop or another Docker daemon is running first.
- E2E scenarios start MySQL / Percona containers and depend on local Docker availability.
- See [scripts/e2e/README.md](scripts/e2e/README.md) for more details.

### Tasks fail after configuring `meta_dsn`

- A common cause is that the metadata schema has not been migrated yet.
- Run `make migrate-up META_DSN=...` before starting the service.
- Migration command details are documented in [cmd/migrate/README.md](cmd/migrate/README.md).

### Auth was left disabled in production

- This is the most important development default to override.
- Development keeps auth off for convenience; production should not.
- See [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md) for concrete guidance.

### Upload is configured but files do not upload

- `endpoint`, `bucket`, `access_key`, and `secret_key` must all be present.
- `region` and `prefix` are optional and not part of initialization minimums.
- The current implementation targets S3-compatible APIs.

### `/metrics` or tracing looks empty

- `/metrics` exposes core metric families even before tasks start; some values may still be placeholders.
- Tracing is off by default, so the absence of spans is often expected rather than a fault.

## Upgrade / Release Entry

Review [CHANGELOG.md](CHANGELOG.md) before upgrading.

Pay particular attention to these change types:

- schema / migration changes
- new, deprecated, or default-shifted config keys
- `sqlc` workflow changes
- observability contract changes that affect dashboards or alerts

This repository does not apply migrations or migrate configuration for you, so upgrades should be handled as an operator change, not just a binary swap.

## Repository Map

Once `Quick Start` works, continue from these entry points:

| Topic | Entry |
| --- | --- |
| Usage and operations guide | [docs/guide/README.md](docs/guide/README.md) |
| Security policy | [SECURITY.md](SECURITY.md) |
| Version history | [CHANGELOG.md](CHANGELOG.md) |
| Service startup command | [cmd/binlog-server/README.md](cmd/binlog-server/README.md) |
| Database migration | [cmd/migrate/README.md](cmd/migrate/README.md) |
| API module | [internal/api/README.md](internal/api/README.md) |
| Replication pipeline | [internal/replication/README.md](internal/replication/README.md) |
| Upload module | [internal/upload/README.md](internal/upload/README.md) |
| E2E suite | [scripts/e2e/README.md](scripts/e2e/README.md) |

## Development Validation

Common verification commands:

```bash
go test ./...
go vet ./...
make e2e-quick
```

If you want frontend-only development:

```bash
cd frontend
npm install
npm run dev
```
