# Binlog Server v0.5.3

Release date: 2026-09-06

Binlog Server `v0.5.3` is an operator-facing reliability and security patch on `v0.5.2`.

## Highlights

- Cluster workers take over `RUNNING`/`LEASE_DEGRADED`/`RETRY_BACKOFF` tasks after a lease expires, instead of leaving dump stopped while the dashboard still shows `RUNNING`. A new worker no longer writes another worker's live task to `STOPPED`.
- Catch-up from `FILE_POS`/`GTID` no longer reports fake `delay_seconds=0` after a quiet idle gap. Tip is confirmed against `SHOW MASTER STATUS` / `SHOW BINLOG STATUS`. Fresh `LATEST` start at tip is unchanged.
- Task list and dashboard pages page in SQL. `GetTask` uses the primary key. Worker claim ticks no longer `SELECT` the full task table.
- Non-loopback listen (including default `:8080`) requires API auth and protected `/api/*` plus `/metrics`. Loopback (`127.0.0.1`/`localhost`/`::1`) may stay unauthenticated for local demo.
- Release tarballs now include `config.example.yaml` and `config.production.example.yaml`.
- Quick Start, landing-page, and deployment download examples now point at `v0.5.3`.

## Upgrade Notes

- No schema migration is required for `v0.5.3`.
- No API contract change versus `v0.5.2`. `GET /api/tasks` remains `{items,total,limit,offset}`.
- Copy-paste `:8080` with auth disabled no longer starts. Bind loopback for local demo, or enable `api.auth` with `protect_api` and `protect_metrics`. `config.example.yaml` binds `127.0.0.1:8080`.
- Source passwords in `source_json` stay plaintext unless you pass `--encryption-key`. Encrypted rows cannot be loaded later without the same key.
- Every cluster process that reads encrypted `source_json` must use the same `--encryption-key`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.3.zh-CN.md
