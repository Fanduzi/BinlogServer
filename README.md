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

### 1. Download, verify, and unpack v0.5.29

```bash
VER=0.5.29
OS=linux          # linux | darwin
ARCH=amd64        # amd64 | arm64

curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
curl -fsSL -O "https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing

tar -xzf "binlog-server_${VER}_${OS}_${ARCH}.tar.gz"
cd "binlog-server_${VER}_${OS}_${ARCH}"
```

Published `v0.5.29` `checksums.txt`:

```text
802c92e91d8f19a082b2d638fb21cfe7df8a0e94357ae2b4fcd14a8158ab72f4  binlog-server_0.5.29_darwin_amd64.tar.gz
796c550c53c9e18750795f41e7d1c1eec55b061d5afbe25e381edbb94e4045e6  binlog-server_0.5.29_darwin_arm64.tar.gz
fd988a3ff588133e618745c2917eb4f1b975bb761cc10e585fe240e0df8d5875  binlog-server_0.5.29_linux_amd64.tar.gz
54f4255acf5f2b845d0369659ee075f996ffdf91973d7a4b6eec9ee1ddeb6b4c  binlog-server_0.5.29_linux_arm64.tar.gz
```

The release tarball contains everything required for operation:

```text
binlog-server_0.5.29_linux_amd64/
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
- `storage.retention_days`: Retention boundary (1..3650 days). When `local_retention_days` and `bucket_retention_days` are omitted, this one number is both the local-disk retention and the bucket retention.
- `storage.local_retention_days`: Optional. Days a sealed file stays on local disk. `0` or omitted uses `retention_days`.
- `storage.bucket_retention_days`: Optional. Days an uploaded object and its catalog row stay. `0` or omitted uses `retention_days`. Must be greater than or equal to the local retention. Applied only when upload and `meta_dsn` are both configured.

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
- Expired sealed segments older than `storage.retention_days` are purged when the replication loop opens a file. The active `OPEN` segment is not deleted. Retention does not delete an open segment or its object. A sealed segment that was uploaded is deleted from the bucket in that same purge, and its catalog row is removed. A segment still inside retention stays in the bucket. With object storage and a catalog (`meta_dsn`), an expired sealed file in `UPLOAD_FAILED` or `LOCAL_ONLY` stays on disk with its catalog row. Replication keeps running, and one `RETENTION_SKIPPED_NOT_UPLOADED` event names the file and its `upload_state`. After that row is `UPLOADED`, the next pass deletes the object, the catalog row, and the local file. `binlog_server_retention_blocked_files{task_id}` counts the expired files that pass is still keeping, and drops when they are purged. Standalone without `meta_dsn` still deletes the local sealed file by age, including one that never reached the bucket. If the object delete fails, the local file stays, the task retries with `last_error` beginning `OBJECT_PURGE_FAILED`, and the next file open tries the delete again. When that delete succeeds, replication continues on the next binlog file. It does not stay in retry with `sealed file already exists` for the file just sealed. That failure does not change `checksum`: `match`, `mismatch`, and an empty checksum stay as they were. Empty is not `match` and not `mismatch`. A task that sets only `storage.retention_days` keeps that single cutoff. `storage.local_retention_days` and `storage.bucket_retention_days` are optional and default to `retention_days`. Create and update reject a bucket retention shorter than the local retention. With upload and a catalog, a sealed `UPLOADED` file older than the local retention and younger than the bucket retention is removed from local disk only. The object and the catalog row stay. `GET /api/tasks/{id}/files` sets `location` to `bucket` (`local` or `both` when the file is still on disk). `file_path` stays the catalog path and is not on this process. `GET /api/tasks/{id}/files/{name}`, `GET /api/tasks/{id}/replay` (limit and `stop_datetime`), and `GET /api/tasks/{id}/replay/archive` still read the object. Replay `locations` is `bucket` for that path: do not pass it to `mysqlbinlog` until you download the file. While the file is on disk, both cutoffs use its modification time. After the local file is gone, bucket age is `sealed_at`, or `uploaded_at` when `sealed_at` is empty. A row with neither timestamp is kept. Once past the bucket retention, the object, the catalog row, and any remaining local file are purged together. `UPLOAD_FAILED` and `LOCAL_ONLY` are still never deleted locally. The same `RETENTION_SKIPPED_NOT_UPLOADED` event and `binlog_server_retention_blocked_files` gauge use the local retention as the age cutoff. Standalone without upload still deletes the local sealed file by that local age. Without a catalog, a longer bucket retention is not applied: upload still deletes the object with the local file at the local retention. No schema migration.
- S3 upload is configured via environment variables or YAML:
  ```bash
  export BINLOG_SERVER_UPLOAD_ENDPOINT="s3.us-east-1.amazonaws.com"
  export BINLOG_SERVER_UPLOAD_BUCKET="my-mysql-binlogs"
  export BINLOG_SERVER_UPLOAD_ACCESS_KEY="AKIA..."
  export BINLOG_SERVER_UPLOAD_SECRET_KEY="..."
  ```
- If an upload fails, replication continues uninterrupted. The worker retries sealed `UPLOAD_FAILED` segments in the background; after the bucket is healthy again they become `UPLOADED` without calling the retry API. Open segments are not uploaded. Manual retry still works:
  ```bash
  curl -X POST http://localhost:8080/api/tasks/<task-id>/files/retry-upload?limit=100
  ```
- Download one listed segment without SSH. `name` is the basename from `GET /api/tasks/<task-id>/files` (a sealed name or `mysql-bin.000004.open.e1`). The response is the raw bytes, named as that file. Replay on the backup host still uses `GET /api/tasks/<task-id>/replay`.
  ```bash
  curl -fL -OJ -H "Authorization: Bearer $TOKEN" \
    "http://localhost:8080/api/tasks/<task-id>/files/mysql-bin.000004.open.e1"
  ```
- Download that replay selection as one tar. `GET /api/tasks/<task-id>/replay/archive` uses the same `limit` as `GET /api/tasks/<task-id>/replay`. The body is `application/x-tar`. Each member is the basename, not a host path. A local file still wins. A missing local sealed file is read from object storage only when that catalog row is still `UPLOADED` with a non-empty `object_key`. Retention removes that row and the object together, so a purged segment is not in the archive. An empty selection is an empty tar. If one selected segment cannot be opened, the response is an error and not a partial tar. Extract it and pass those basenames to `mysqlbinlog` or `mariadb-binlog`. The JSON replay command is unchanged.
- Ask for a point-in-time window after you have restored your own full backup. `GET /api/tasks/<task-id>/replay?stop_datetime=2024-01-01%2001:30:00` returns one path per source index whose event times cover that UTC instant, plus a `command` that runs `TZ=UTC mysqlbinlog` or `TZ=UTC mariadb-binlog` with `--stop-datetime`. Add `start_datetime` when the backup's consistent time should be the inclusive start. The same query on `/replay/archive` downloads those basenames. An empty window is HTTP 200 with `paths: []` and an empty `command`. An unparseable datetime is HTTP 400.
  ```bash
  curl -fL -OJ -H "Authorization: Bearer $TOKEN" \
    "http://localhost:8080/api/tasks/<task-id>/replay/archive?limit=200"
  tar -xf "task-<task-id>-replay.tar"
  ```

Start production instances from [`config.production.example.yaml`](config.production.example.yaml).

---

## Upgrade Notes (v0.5.29)

Before upgrading existing deployments to `v0.5.29`, review the operator contract from `v0.5.27`, `v0.5.28`, and `v0.5.29`:

- **Zero Schema Migrations:** `v0.5.27`, `v0.5.28`, and `v0.5.29` require no database migrations (`000001_init_schema` unchanged). `v0.5.29` stores `storage.local_retention_days` and `storage.bucket_retention_days` in the existing task storage JSON. `cluster.failover_policy` is still not a switch. `PRODUCTION=true` still requires a non-empty `--encryption-key`. The 30-second threshold is unchanged.
- **Point-in-time window (v0.5.27):** An existing backup task can be asked for the binlog window covering a UTC stop time. `GET /api/tasks/{id}/replay` takes `stop_datetime` and an optional `start_datetime`. The window is half-open `[start, stop)`: the stop is excluded, and a start, when set, is included. The same query on `GET /api/tasks/{id}/replay/archive` downloads that selection. The response includes `command` only when `stop_datetime` is present. `mysql` uses `mysqlbinlog`, `mariadb` uses `mariadb-binlog`, and the command is prefixed `TZ=UTC`. Restore the full backup yourself, then run the printed command. Binlog Server does not restore that backup. Omitting both datetimes keeps the limit replay: `flavor`, `client`, `client_hint`, `paths`, and no `command` field. `start_datetime` equal to `stop_datetime` is an empty window: HTTP 200, empty `paths`, empty `command`. It is not a 400. A bad datetime or a start after the stop is plain-text 400. No new config key. The limit replay, background upload retry, retention, and lease takeover stay as they are in `v0.5.26`.
- **Un-uploaded segments stay (v0.5.28):** With object storage and a catalog (`meta_dsn`), age retention no longer deletes the only copy of an expired sealed binlog whose `upload_state` is `UPLOAD_FAILED` or `LOCAL_ONLY`. The published v0.5.27 package still deletes that local file once it is older than `storage.retention_days`. The local file and the catalog row stay. Replication keeps running. It does not set `last_error` and does not enter `RETRY_BACKOFF`. Age is the local file modification time, compared with `storage.retention_days`. `GET /api/tasks/{id}/events` gets one `RETENTION_SKIPPED_NOT_UPLOADED` per kept file. The message names the file and its `upload_state`. Opening another binlog does not append that event again for the same file while this process stays up. After a restart, the next retention pass that still keeps the file appends that event once more. The process that runs retention exposes `binlog_server_retention_blocked_files{task_id}` on `GET /metrics`. The gauge drops when a later pass deletes those files. Once the row is `UPLOADED`, the next retention pass deletes the object, then the catalog row, then the local file. A sealed `UPLOAD_FAILED` row is still picked up by the background upload retry and by `POST /api/tasks/{id}/files/retry-upload`. `LOCAL_ONLY` is not. Standalone without `meta_dsn` still deletes the local sealed file by age, including one that never reached the bucket. No new config key.
- **Separate disk and bucket retention (v0.5.29):** `storage.retention_days` stays required. The range is still 1..3650. Two optional keys are `storage.local_retention_days` and `storage.bucket_retention_days`. Omitted or 0 means the same as `retention_days`. When the effective local days and the effective bucket days are equal, that shared number is the one cutoff. A task that sets only `retention_days` keeps the v0.5.28 purge. `POST /api/tasks` and `PUT /api/tasks/{id}` reject a bucket window shorter than the local window. The comparison uses the effective days, so `local_retention_days` above `retention_days` with `bucket_retention_days` omitted is rejected too. The longer bucket window is applied only when object storage and `meta_dsn` are both configured. A sealed `UPLOADED` segment older than the local window and still inside the bucket window loses only the local file. The object and the catalog row stay. `GET /api/tasks/{id}/files` reports `location` as `local`, `bucket`, or `both`. A `bucket` value means that path is the catalog `file_path` and is not on this process. Download the segment or the replay archive before passing that path to `mysqlbinlog`. After the local file is gone, and only when the bucket window is longer, bucket age is `sealed_at`, or `uploaded_at` when `sealed_at` is empty. A row with neither timestamp is kept. `UPLOAD_FAILED` and `LOCAL_ONLY` older than the local window still stay on disk. The same skip event and gauge use the local retention as the age cutoff. Without a catalog, a longer `bucket_retention_days` is not applied: upload still deletes the object together with the local file at the local window. Standalone without `meta_dsn` still deletes the local sealed file by the local age.

Full release notes: [docs/releases/release-notes-v0.5.29.md](docs/releases/release-notes-v0.5.29.md) | [docs/releases/v0.5.29.zh-CN.md](docs/releases/v0.5.29.zh-CN.md)

Notes for the versions in this upgrade: [docs/releases/release-notes-v0.5.28.md](docs/releases/release-notes-v0.5.28.md) | [docs/releases/v0.5.28.zh-CN.md](docs/releases/v0.5.28.zh-CN.md) and [docs/releases/release-notes-v0.5.27.md](docs/releases/release-notes-v0.5.27.md) | [docs/releases/v0.5.27.zh-CN.md](docs/releases/v0.5.27.zh-CN.md)

---

## Architecture

BinlogServer is structured as a modular control plane with clear separation between API ingestion, state scheduling, replication execution, metadata coordination, and UI delivery.

### Modules

| Module | Responsibility | Entry |
| --- | --- | --- |
| `cmd` | Top-level service startup and migration commands | [cmd/README.md](cmd/README.md) |
| `internal/api` | HTTP routes, request validation, Swagger, metrics, tracing hooks | [internal/api/README.md](internal/api/README.md) |
| `internal/app` | Runtime assembly and role lifecycle orchestration | [internal/app/README.md](internal/app/README.md) |
| `internal/binlog` | Local binlog file writing, the durable resume file:pos, and checkpoint persistence helpers | [internal/binlog/README.md](internal/binlog/README.md) |
| `internal/config` | YAML and environment-based configuration loading | [internal/config/README.md](internal/config/README.md) |
| `internal/logging` | Logger setup and log output rotation | [internal/logging/README.md](internal/logging/README.md) |
| `internal/meta` | Metadata storage, schema checks, lease-backed coordination data | [internal/meta/README.md](internal/meta/README.md) |
| `internal/replication` | MySQL replication pull loop and durable local write path | [internal/replication/README.md](internal/replication/README.md) |
| `internal/tasks` | Task state machine, scheduling, and execution orchestration | [internal/tasks/README.md](internal/tasks/README.md) |
| `internal/ui` | Embedded UI asset serving | [internal/ui/README.md](internal/ui/README.md) |
| `internal/upload` | S3-compatible upload, checksum, and retention object delete | [internal/upload/README.md](internal/upload/README.md) |
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
