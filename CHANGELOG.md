# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

Maintenance rules:

- Add user-visible or operator-visible changes to `Unreleased` as they land.
- Before a release, review `Unreleased` and make sure schema, config, `sqlc`, and observability changes are clearly called out.
- When cutting a release, move `Unreleased` entries into a dated release section and start a fresh `Unreleased` block.

## [Unreleased]

## [v0.5.3] - 2026-09-06

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.3`.
- Control-plane `listen_addr` that is not loopback (`:8080`, `0.0.0.0:8080`, and any non-`127.0.0.1`/`localhost`/`::1` bind) now requires `api.auth.enabled`, `protect_api`, and `protect_metrics` at `App.Run`. Loopback binds may stay unauthenticated for local demo. `PRODUCTION=true` still fail-closes independently and is not weakened.
- When `api.auth.enabled=true`, unset `protect_api` / `protect_metrics` now default to true. Explicit `false` is still honored at config load, but non-loopback listen and `PRODUCTION=true` still reject that combination. `listen_addr` default remains `:8080`.
- `GET /api/tasks` and dashboard task pages filter and page in SQL (`COUNT` + `LIMIT/OFFSET`). `GetTask` uses the primary key. Worker claim ticks query unowned `STARTING` instead of loading every task. Dashboard summary/sources still aggregate in-process delay. Public `{items,total,limit,offset}` shape is unchanged.
- OpenTelemetry Go API/SDK/trace/OTLP HTTP exporter moved from 1.45.0 to 1.46.0.

### Security

- Source passwords in `backup_tasks.source_json` are encrypted with AES-256-GCM (`enc:aes256:`) when `--encryption-key` is provided. Other source fields stay plaintext JSON. Without a key, plaintext persist is unchanged so existing deploys keep starting.

### Fixed

- Cluster workers claim `RUNNING`/`LEASE_DEGRADED`/`RETRY_BACKOFF` tasks whose lease has expired and resume dump after a successful Acquire, without going through `StopTask`.
- A newly started cluster worker no longer Stop+Starts another worker's live `RUNNING` task, which previously persisted `STOPPED` while dump continued on the original owner.
- FILE_POS/GTID catch-up no longer sticks `atTip` (and therefore `delay_seconds=0`) after a quiet 2s idle gap. Tip is confirmed against `SHOW MASTER STATUS` / `SHOW BINLOG STATUS`. Fresh LATEST start at tip is unchanged.
- Release tarballs include `config.example.yaml` and `config.production.example.yaml`, matching the documented production start path.

## [v0.5.2] - 2026-08-30

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.2`.

### Fixed

- LATEST start at the source tip no longer reports `DELAYED` from a days-old binlog event header. `delay_seconds` is 0 / `NORMAL` as soon as StartSync succeeds; FILE_POS/GTID catch-up that is still behind the tip keeps real lag. `last_event_at` may still show the last event header time.
- Task list and dashboard pages sort numeric string ids as integers, so page 1 is `1,2,3` rather than `1,10,100`.

## [v0.5.1] - 2026-08-30

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.1`. They still showed `v0.4.3` after the `v0.5.0` tag.

### Notes

- Runtime behavior is unchanged from `v0.5.0`. Isolated `make e2e-scale` (1000 control-plane tasks / 100 live streams) and production-template bearer auth were recorded locally; one MySQL fixture supplied the dump clients, so this is not independent-cluster capacity.

## [v0.5.0] - 2026-08-30

### Added

- `POST /api/tasks/batch` accepts 1..100 task-create requests and returns ordered per-item success or structured error results, so a valid batch can report partial success without stopping later items.
- Task and dashboard queries support bounded `limit`/`offset` paging and `state` filtering. Dashboard summary and source aggregates cover the complete filtered result, while task details are paged; the frontend uses server paging.
- Production deployments can start from `config.production.example.yaml`, which enables bearer authentication and protects both `/api/*` and `/metrics`; unresolved credential placeholders are rejected. An opt-in `make e2e-scale` harness writes a JSON evidence report for its isolated 1000-control-task / 100-live-stream scenario.

### Changed

- **API contract change:** `GET /api/tasks` now returns `{items,total,limit,offset}` instead of a raw JSON array `[...]`. Upgrade API clients to read `items` and use the returned page metadata before deploying `v0.5.0`.
- Summaries report `STARTING` separately from `RUNNING`.

### Fixed

- Unreachable replication sources now fail after bounded retries instead of retrying indefinitely; disconnected source streams follow the bounded retry path.

## [v0.4.3] - 2026-08-30

### Fixed

- Metadata/source isolation now treats `localhost`, IPv4 loopback literals in `127/8`, and IPv6 loopback literals such as `::1` as one same-port endpoint identity without DNS resolution; create, update, and start reject aliases, and source lookup returns the same matches.

## [v0.4.2] - 2026-08-21

### Fixed

- Create task now validates the full spec before persist. HTTP 400 no longer leaves a `LATEST` task behind.
- `flavor=mariadb` probes `server_id` + `gtid_domain_id` instead of MySQL-only `@@server_uuid`. `log_bin=off` fails as `SOURCE_LOG_BIN_OFF`.
- Access denied (ERROR 1045) is `SOURCE_ACCESS_DENIED` and enters `FAILED` instead of infinite retry. `POST /start` can restart a `FAILED` task after the operator fixes credentials.
- Silent masters no longer report multi-hour `DELAYED` lag: an idle dump wait treats lag as 0 / `NORMAL`. Heartbeat events are not written into backup files.
- Bind failures no longer dump cobra `Usage`.
- `GET /api/health` returns `{"status":"ok"}` (keep `/healthz`).

### Changed

- Quick Start is download/verify/extract/`./binlog-server`. `go run` moved to Development.
- Standalone without `meta_dsn` is documented as in-memory control plane. Restartable tasks need `meta_dsn`.

## [v0.1.2] - 2026-03-27

### Added

- Frontend development mock mode for Vite dev with shared scenario assets reused by Playwright E2E.
- New frontend mock scenarios for cluster/lease resilience coverage, including control-plane-down worker-running views.
- Workspace-C planning docs for ops console IA redesign in `docs/develop/plans/2026-03-27-ops-console-workspace-c-*.md`.

### Changed

- Reorganized the ops console into workspace-oriented views (`overview`, `tasks`, `sources`, `workers`, `alerts`) with deep-link routing semantics.
- Moved task filters to task/alert context and source lookup to source context to reduce cross-page cognitive load.
- Updated left navigation to full-height docked behavior with bottom-docked collapse control and compact icon alignment in collapsed mode.
- Normalized KPI navigation behavior so task-oriented metrics consistently route to task-focused workflows.
- Adjusted lease risk evaluation to use dashboard reference time in mock-driven views, avoiding false-risk inflation from local clock skew.

## [v0.1.1] - 2026-03-25

### Added

- Playwright-based frontend E2E acceptance coverage for empty-state, KPI filtering, task drawer, retry-upload, and auth-required flows.

### Changed

- Reworked the embedded web UI into a more operator-focused console with alert-first KPI hierarchy, reduced row-action noise, and a stronger detail drawer workflow.
- Localized frontend auth guidance and added in-app settings-driven recovery for `401` API responses.
- Added retry-upload affordance in the task detail drawer and refreshed embedded static UI assets.

## [v0.1.0] - 2026-03-09

### Added

- Root `SECURITY.md` security policy for vulnerability reporting.
- Root `.golangci.yml` baseline lint configuration.
- CI vulnerability scanning with `govulncheck`.

### Changed

- Foundation hardening work completed across auth, timeout governance, retry standardization, SQL access generation, API validation, Prometheus metrics, and OpenTelemetry tracing.

## [2026-03-08]

### Added

- Closure snapshot for the foundation hardening program in `docs/develop/plans/2026-03-08-foundation-hardening-closure.md`.
