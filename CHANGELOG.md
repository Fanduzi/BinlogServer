# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

Maintenance rules:

- Add user-visible or operator-visible changes to `Unreleased` as they land.
- Before a release, review `Unreleased` and make sure schema, config, `sqlc`, and observability changes are clearly called out.
- When cutting a release, move `Unreleased` entries into a dated release section and start a fresh `Unreleased` block.

## [Unreleased]

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
