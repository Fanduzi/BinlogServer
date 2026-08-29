# Binlog Server v0.5.0

Release date: 2026-08-30

Binlog Server `v0.5.0` improves control-plane scale handling, batch task creation, source-failure handling, and production authentication defaults.

## Highlights

- Unreachable replication sources and disconnected source streams now use bounded retries and enter failure instead of retrying forever.
- `STARTING` is reported separately from `RUNNING` in summary and dashboard views.
- `GET /api/tasks` and `GET /api/dashboard` support `state` filtering and bounded `limit`/`offset` paging. Dashboard summary and source aggregation cover all filtered tasks; only task details are paged. The frontend follows the server paging contract.
- `POST /api/tasks/batch` accepts 1..100 create requests. A malformed envelope fails as HTTP 400 without creating tasks; a valid envelope returns ordered per-item results, allowing later items to succeed when another item fails.
- `config.production.example.yaml` provides an authenticated control-plane starting point. In `PRODUCTION=true`, control-plane API and metrics protection are required and unresolved auth-secret placeholders are rejected.
- `make e2e-scale` is an opt-in isolated evidence harness for 1000 control-plane tasks and 100 live streams. It produces a JSON report; it has not been run or used to claim production-capacity validation for this release.

## Upgrade Notes

- **Breaking API response change:** `GET /api/tasks` now returns `{ "items": [...], "total": N, "limit": N, "offset": N }` instead of a raw JSON array `[...]`. Update clients to read `items`, honor the returned page metadata, and request additional pages as needed.
- No schema migration is required for `v0.5.0`.
- For production control-plane deployments, begin with `config.production.example.yaml`, set `BINLOG_SERVER_API_AUTH_BEARER_TOKEN`, and keep API and metrics protection enabled. The scale harness is opt-in and requires its generated JSON report as evidence; do not infer scale or production-capacity results from its presence.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.0.zh-CN.md
