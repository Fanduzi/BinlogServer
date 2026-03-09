# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

Maintenance rules:

- Add user-visible or operator-visible changes to `Unreleased` as they land.
- Before a release, review `Unreleased` and make sure schema, config, `sqlc`, and observability changes are clearly called out.
- When cutting a release, move `Unreleased` entries into a dated release section and start a fresh `Unreleased` block.

## [Unreleased]

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
