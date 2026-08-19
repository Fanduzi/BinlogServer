# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

Maintenance rules:

- Add user-visible or operator-visible changes to `Unreleased` as they land.
- Before a release, review `Unreleased` and make sure schema, config, `sqlc`, and observability changes are clearly called out.
- When cutting a release, move `Unreleased` entries into a dated release section and start a fresh `Unreleased` block.

## [Unreleased]

### Fixed

- Quick Start is now download/verify/extract/`./binlog-server`; `go run` / `make build` moved to Development (#9).
- `flavor=mariadb` probes `server_id` + `gtid_domain_id` and fails with `log_bin is off` instead of `empty server_uuid` (#10).
- `POST /api/tasks` validates the full spec before persist; HTTP 400 no longer leaves a LATEST task (#11).
- Standalone (no `meta_dsn`) persists tasks and fsynced checkpoints under `BINLOG_SERVER_DATA_DIR` and resumes after crash (#12).
- Bind failures no longer dump cobra Usage; access denied maps to `SOURCE_ACCESS_DENIED` without infinite retry; quiet-master lag is `IDLE`/`0`; `GET /api/health` works (#13).

### Changed

- Canonical GTID field is `gtid_set`; request alias `gtid` is accepted. `storage.dir` is rejected; files live at `{data_dir}/{task_id}/`.
- `POST /api/tasks/{id}/start` returns JSON `{"id","state"}` instead of 204.

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
