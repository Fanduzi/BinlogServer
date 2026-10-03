<div align="center">
  <div style="display:inline-block; text-align:left;">
    <pre>
 ____                  ___                       ____
/\  _`\    __         /\_ \                     /\  _`\
\ \ \L\ \ /\_\    ___ \//\ \     ___      __    \ \,\L\_\     __   _ __   __  __     __   _ __
 \ \  _ <'\/\ \ /' _ `\ \ \ \   / __`\  /'_ `\   \/_\__ \   /'__`\/\`'__\/\ \/\ \  /'__`\/\`'__\
  \ \ \L\ \\ \ \/\ \/\ \ \_\ \_/\ \L\ \/\ \L\ \    /\ \L\ \/\  __/\ \ \/ \ \ \_/ |/\  __/\ \ \/
   \ \____/ \ \_\ \_\ \_\/\____\ \____/\ \____ \   \ `\____\ \____\\ \_\  \ \___/ \ \____\\ \_\
    \/___/   \/_/\/_/\/_/\/____/\/___/  \/___L\ \   \/_____/\/____/ \/_/   \/__/   \/____/ \/_/
                                          /\____/
                                          \_/__/
    </pre>
  </div>
</div>

<div align="center">

# BinlogServer

[![Release](https://img.shields.io/github/v/release/Fanduzi/BinlogServer?display_name=tag)](https://github.com/Fanduzi/BinlogServer/releases)
![Platform](https://img.shields.io/badge/platform-darwin%20amd64%20%7C%20darwin%20arm64%20%7C%20linux%20amd64%20%7C%20linux%20arm64-blue)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

[![English](https://img.shields.io/badge/docs-English-blue.svg)](README.md)
[![中文](https://img.shields.io/badge/docs-中文-inactive.svg)](README_ZH.md)
[![Changelog](https://img.shields.io/badge/docs-Changelog-informational.svg)](CHANGELOG.md)
[![Security](https://img.shields.io/badge/docs-Security-critical.svg)](SECURITY.md)
</div>

BinlogServer is a centralized MySQL binlog backup control plane engineered for database reliability teams. It pulls binlog streams from MySQL/MariaDB instances into durable local files, commits checkpoint offsets only after disk `fsync`, coordinates multi-node worker failover via distributed leases, and provides decoupled S3-compatible archiving with an embedded web console and REST API.

If you are evaluating BinlogServer, start with the **Architectural Guarantees** and **Topologies** below. If you are deploying immediately, jump directly to **Quick Start**.

---

## What Problem Does It Solve?

In most production environments, MySQL binlog backup still relies on brittle `mysqlbinlog` shell scripts wrapped in cron jobs, custom daemon loops with untracked checkpoint drift, and synchronous scripts that halt replication whenever cloud storage hiccups.

BinlogServer replaces shell history with an authoritative, observable control plane:
- **Zero phantom progress:** Checkpoints advance strictly after local disk `fsync`.
- **Decoupled archiving:** Remote S3/MinIO upload network hiccups or outages never stall the local replication capture loop.
- **Automated cluster failover:** Multi-worker lease ownership backed by metadata MySQL guarantees exactly one active dumper per task, with automatic timeout takeover upon worker crash.
- **Operational visibility:** Embedded Web Console, interactive Swagger API, and Prometheus `/metrics` evaluate replication lag directly against `SHOW MASTER STATUS` / `SHOW BINLOG STATUS`.

### Live Console Preview

![Console overview](docs/images/console-dashboard.png)
*Console Dashboard: Real-time replication lag, worker assignment, and task state classification.*

![Task detail](docs/images/task-detail.png)
*Task Inspection: Precise GTID / file-pos coordinates, local segment rotation, and one-click manual upload retry.*

![Swagger API](docs/images/swagger.png)
*Swagger Explorer: Full OpenAPI specification for CMDB automation and programmatic orchestration.*

---

## Architectural Guarantees (DBA First)

| Guarantee | Engineering Implementation | Why DBAs Trust It |
|---|---|---|
| **Durable Checkpoints** | Checkpoint offset persists only after local `fsync` completes on the sealed binlog segment. | No optimistic in-memory progress. On process restart or crash recovery, replication resumes cleanly from the exact durable byte. |
| **Decoupled S3 Archiving** | Upload to S3/MinIO operates asynchronously. Upload failures trigger backoff and can be retried via API or Console drawer. | Object storage rate limits or network partitions never stall binlog capture from primary databases. |
| **Cluster HA &amp; Lease Ownership** | Distributed leases backed by metadata MySQL enforce mutually exclusive task ownership per worker. | Eliminates split-brain dumper conflicts on the same source. When a worker fails heartbeat renewal, healthy workers claim and resume the task. |
| **Strict Fail-Closed Security** | Non-loopback binds mandate auth. In `PRODUCTION=true`, startup is refused without a 32-byte AES key (`--encryption-key`) and protected credentials. | Protects replication credentials with AES-256-GCM (`enc:aes256:`) in metadata; prevents accidental public exposure of unauthenticated control planes. |
| **True Master Lag Tracking** | Lag is evaluated against source binlog coordinates (`SHOW MASTER STATUS` / `SHOW BINLOG STATUS`), not arbitrary sleep timers. | Accurate lag alerting during quiet nighttime periods; no misleading zero-lag reports on stalled streams. |

---

## Deployment Topologies

Choose the topology matching your availability and infrastructure requirements:

1. **Standalone Mode (Single Process):**
   - *Without `meta_dsn`:* Tasks and checkpoints run in-memory; binlog files land under `{data_dir}/{task_id}/`. Ideal for quick local testing and evaluation.
   - *With `meta_dsn`:* Task metadata, checkpoints, and file catalogs persist in MySQL. On process restart, active tasks resume automatically from their last checkpoint.
2. **Control Plane + Worker Pool (Cluster HA - Recommended):**
   - *Control Plane nodes (`cluster.role: control-plane`):* Serve the REST API, embedded Console UI, and manage task lifecycle states without dumping binlogs.
   - *Worker nodes (`cluster.role: worker`):* Headless processes that register heartbeats, compete for task leases, and execute continuous binlog replication. Automatic failover upon node failure.
3. **All-in-one Cluster Node (`cluster.role: all-in-one`):**
   - Single process running both the API server and worker replication loop, backed by a shared metadata MySQL. Suitable for compact 2-node or 3-node HA clusters.

---

## Quick Start (Release Operators)

Deploy official precompiled binaries without needing Go installed.

### Prerequisites

- A release archive for your OS/architecture from [GitHub Releases](https://github.com/Fanduzi/BinlogServer/releases).
- A reachable MySQL or MariaDB source with `log_bin=ON`, `binlog_format=ROW`, and a replication account (`GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.*`). MariaDB must send `"flavor":"mariadb"`. The `mysql` flavor reads `@@server_uuid`, which MariaDB does not have.

> ⚠️ **Metadata Isolation Rule:** When `meta_dsn` is configured, its MySQL instance must be dedicated and NEVER added to the backup task set. The server strictly rejects identical TCP `host:port` targets and loopback aliases (`localhost`, `127/8`, `::1`).

### 1. Download, verify, and unpack v0.5.11

```bash
VER=0.5.11
OS=linux          # linux | darwin
ARCH=amd64        # amd64 | arm64

curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing

tar -xzf "binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
cd "binlog-server_${VER}_${OS}_${ARCH}"
```

The release tarball contains everything required for operation:

```text
binlog-server_0.5.11_linux_amd64/
  binlog-server                  # Main application executable
  migrate                        # Schema migration utility
  migrations/                    # SQL migrations
  README.md                      # English documentation
  README_ZH.md                   # Chinese documentation
  CHANGELOG.md                   # Release history
  LICENSE                        # Apache 2.0 license
  config.example.yaml            # Annotated configuration reference
  config.production.example.yaml # Hardened production baseline
  docs/guide/                    # Operator guide, including on-disk replay
```

### 2. Start local evaluation (Loopback)

```bash
# Loopback bind (127.0.0.1) allows unauthenticated local evaluation
export BINLOG_SERVER_LISTEN_ADDR=127.0.0.1:8080
export BINLOG_SERVER_DATA_DIR=./data
./binlog-server
```

*Note:* Default listen address is `:8080`. Any non-loopback bind (`0.0.0.0:8080` or `:8080`) strictly fail-closes at startup unless `api.auth.enabled: true` is configured.

### 3. Verify health endpoint

```bash
curl -fsS http://127.0.0.1:8080/healthz
# Expected output: ok
```

### 4. Create your first replication task

Submit task parameters via `POST /api/tasks`:

```bash
curl -fsS -X POST http://127.0.0.1:8080/api/tasks \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "prod-mysql-01",
    "cluster_key": "prod-cluster-main",
    "source": {
      "host": "192.168.1.50",
      "port": 3306,
      "user": "repl",
      "password": "your_repl_password",
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

*Parameters:*
- `cluster_key`: Identifier used for partitioning and object storage path routing (`[A-Za-z0-9._-]`).
- `source.flavor`: `mysql` or `mariadb`. A MariaDB source must use `"flavor":"mariadb"`. Leaving `mysql` fails at start with `SOURCE_IDENTITY_UNAVAILABLE` because MariaDB has no `@@server_uuid`.
- `start.mode`: `LATEST` (replicate from source tip), `FILE_POS` (requires `file` and `pos`), or `GTID` (requires `gtid_set`).
- `storage.retention_days`: Local retention boundary (1..3650 days).

### 5. Start the replication task

Replace `<task-id>` with the ID returned by step 4:

```bash
curl -i -X POST http://127.0.0.1:8080/api/tasks/<task-id>/start
```

### 6. Inspect state &amp; open the Web Console

- **Web Console:** Open `http://127.0.0.1:8080/ui/` in your browser.
- **Interactive Swagger:** Open `http://127.0.0.1:8080/swagger/index.html`.
- **API Task List:** `curl -fsS http://127.0.0.1:8080/api/tasks`

---

## Production Deployment Checklist

Do not use permissive development defaults in production. Follow these mandatory rules:

### 1. Fail-Closed Security &amp; Encryption Key
- **Non-loopback listen:** Any non-loopback bind requires `api.auth.enabled: true`, `protect_api: true`, and `protect_metrics: true`.
- **`PRODUCTION=true` enforcement:** In production mode, BinlogServer refuses to start if `--encryption-key` is empty.
- **AES-256 Key generation:** Supply a 32-byte key via the `--encryption-key` flag:
  ```bash
  export BINLOG_SERVER_ENCRYPTION_KEY="$(openssl rand -hex 16)" # 32 hex chars = 32 bytes
  ./binlog-server --config config.production.example.yaml --encryption-key "$BINLOG_SERVER_ENCRYPTION_KEY"
  ```
- **Password encryption:** When `--encryption-key` is provided, source database passwords in `backup_tasks.source_json` are automatically encrypted at rest using AES-256-GCM (`enc:aes256:`).
- **Console:** Open `/ui/` in a browser. The page and its assets load without an `Authorization` header. A 401 from `/api/*` opens Settings; paste the same bearer token configured in `api.auth.bearer_token`. Later Console calls send `Authorization: Bearer`. `/api/*`, `/metrics`, and `/swagger/*` still reject missing credentials. `/healthz` stays unauthenticated for load balancers. Swagger has no token field; use curl or a reverse proxy that injects the header. The Settings field is a bearer token. `api_key` mode is for clients that send the configured header.

### 2. Metadata Database Setup
- Prepare an isolated MySQL database and execute schema migrations before starting the service:
  ```bash
  export META_DSN='binlog_meta:secure_pass@tcp(10.0.0.15:3306)/binlog_server_meta?parseTime=true'
  ./migrate up --dsn "$META_DSN" --path ./migrations
  export BINLOG_SERVER_META_DSN="$META_DSN"
  ```

### 3. Local Retention &amp; Object Storage Upload
- Local binlogs are stored at `{data_dir}/{task_id}/`.
- Expired sealed segments older than `storage.retention_days` are automatically purged by the replication loop. The active `OPEN` segment is never deleted.
- S3 upload is configured via environment variables or YAML:
  ```bash
  export BINLOG_SERVER_UPLOAD_ENDPOINT="s3.us-east-1.amazonaws.com"
  export BINLOG_SERVER_UPLOAD_BUCKET="my-mysql-binlogs"
  export BINLOG_SERVER_UPLOAD_ACCESS_KEY="AKIA..."
  export BINLOG_SERVER_UPLOAD_SECRET_KEY="..."
  ```
- If an upload fails, replication continues uninterrupted. Trigger manual retry anytime:
  ```bash
  curl -X POST http://localhost:8080/api/tasks/<task-id>/files/retry-upload?limit=100
  ```

Start production instances from [`config.production.example.yaml`](config.production.example.yaml).

---

## Upgrade Notes (v0.5.11)

Before upgrading existing deployments, review the v0.5.11 operator contract:

- **Zero Schema Migrations:** `v0.5.11` requires no database migrations (`000001_init_schema` unchanged). Config keys are unchanged.
- **Adopt and Resume:** Standalone with no `meta_dsn`: `POST /api/tasks/{id}/adopt` attaches `cluster_key` and source to a leftover directory already listed by `GET /api/tasks`. Success is 200, the password is omitted, state stays `STOPPED`, and adopt does not start replication. Before adopt, `PUT` and `POST` start still return 400 with body `on-disk backup has no task metadata` and do not start replication. If `start` is omitted, the saved position is `FILE_POS` at the source name and byte size of the highest sealed or `.open.e*` segment. An explicit `start.mode` overrides that. Dogfood on tip `9b4ded4`: `mysql-bin.000007` pos 477, equal to the size of that `.open.e1`. `POST` start then returned 204, the task reached `STARTING`, epoch 2, and `.open.e2` was written in the same directory. Existing segments stayed. `v0.5.10` already shipped leftover-directory listing. This release is the adopt-and-resume step.
- **Meta and Console:** With `meta_dsn` configured, directories are not discovered from disk. Adopting a directory that is not a catalog task returns 404 `task not found` (dogfood: directories `99` and `42`). Adopt does not create a task from disk in that mode. The Console has no adopt button. Edit still uses `PUT` and stays blocked until adopt.

Full release notes: [docs/releases/release-notes-v0.5.11.md](docs/releases/release-notes-v0.5.11.md) | [docs/releases/v0.5.11.zh-CN.md](docs/releases/v0.5.11.zh-CN.md)

---

## Architecture

BinlogServer is structured as a modular control plane with clear separation between API ingestion, state scheduling, replication execution, metadata coordination, and UI delivery.

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

---

## Repository Map

| Topic | Documentation Entry |
| --- | --- |
| Full Deployment Guide | [docs/guide/admin/deployment.md](docs/guide/admin/deployment.md) |
| Configuration Reference | [docs/guide/admin/configuration.md](docs/guide/admin/configuration.md) |
| Troubleshooting Runbook | [docs/guide/admin/troubleshooting.md](docs/guide/admin/troubleshooting.md) |
| Observability &amp; Metrics | [docs/guide/admin/observability.md](docs/guide/admin/observability.md) |
| Security Policy | [SECURITY.md](SECURITY.md) |
| Release History | [CHANGELOG.md](CHANGELOG.md) |

---

## Source Build &amp; Verification

Building from source requires Go `1.26.7+`. Docker is required only for automated E2E tests.

```bash
# Compile local binaries
make build

# Compile Linux static binaries (CGO_ENABLED=0)
make build-linux

# Run tests and linting
go test ./...
go vet ./...

# Run fast automated E2E suite (requires Docker)
make e2e-quick
```
