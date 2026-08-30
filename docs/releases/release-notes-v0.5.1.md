# Binlog Server v0.5.1

Release date: 2026-08-30

Binlog Server `v0.5.1` is a documentation and evidence patch on `v0.5.0`. Replication, API, and scheduler behavior are unchanged.

## Highlights

- Quick Start, landing-page, and deployment download examples now point at `v0.5.1`. After `v0.5.0` they still told operators to fetch `v0.4.3`.
- Isolated `make e2e-scale` was run: 1000 control-plane tasks and 100 live streams all advanced unique local files. Process RSS was about 112 MB; task-list p95 was about 2 ms. One MySQL 5.7 fixture supplied the dump clients.
- Production-template bearer auth was verified on a local process using `config.production.example.yaml`: missing credentials return 401 on `/api/tasks` and `/metrics`; `/healthz` stays 200; unresolved token placeholders refuse to start.

## Upgrade Notes

- No schema migration is required for `v0.5.1`.
- No API contract change versus `v0.5.0`. The `GET /api/tasks` paging change from `v0.5.0` still applies if you are upgrading from `v0.4.x`.
- The scale run is isolated-harness evidence, not a production-capacity claim and not 100 independent source clusters.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.1.zh-CN.md
