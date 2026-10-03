# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

Maintenance rules:

- Add user-visible or operator-visible changes to `Unreleased` as they land.
- Before a release, review `Unreleased` and make sure schema, config, `sqlc`, and observability changes are clearly called out.
- When cutting a release, move `Unreleased` entries into a dated release section and start a fresh `Unreleased` block.

## [Unreleased]

## [v0.5.15] - 2026-10-04

### Fixed

- Stop a `RUNNING` backup, let the source keep writing, then start the same task. Standalone with no `meta_dsn`, and a catalog task, both resume at the end position of the last complete event in the highest open segment. The dump does not jump to the current `SHOW MASTER STATUS`. It does not rewind to position 4 and delete that segment. A dump that started mid-file does not resume at the file size. The stopped open segment is renamed into the new epoch and appended, so one source filename spans the stop, with no gap and no duplicate event at the boundary. A partial event past that end position is dropped before the append. A catalog takeover with no complete event on disk still starts at position 4. Standalone with no checkpoint and no complete event still starts at the current master status. `KeepLocalSegments` adopt still uses the saved file size and opens a new epoch. A task already at the source tip still reports at tip. `delay_seconds` of 0, an omitted sample, a dump preamble that is not lag, and the 30-second real-lag threshold are unchanged from v0.5.14. No new API. No new config key. No schema migration.

## [v0.5.14] - 2026-10-03

### Fixed

- A caught-up `FILE_POS` resume, including standalone adopt at the highest segment size, no longer reports `DELAY_EXCEEDS_THRESHOLD` from the dump preamble. MySQL sends the binlog format description before any new event, with `log_pos` 0 or the original end position (126 on MySQL 8). That header time is when the file was created. The preamble is not written and does not move the cursor. If the resume position is already at `SHOW MASTER STATUS`, `delay_seconds` is 0 / `NORMAL` as soon as StartSync succeeds. Catch-up that is still behind the tip still uses `now - last_event_at`.
- A caught-up RUNNING task includes `delay_seconds` as JSON `0` on `GET /api/tasks/{id}/replication` and the dashboard replication object. `omitempty` on `int64` was dropping that zero, so the Console showed `--` seconds while status stayed `NORMAL`. A task with no event-time sample still omits the field. The 30-second threshold, `DELAYED`, and the dump-preamble behavior are unchanged. No new config key. No schema migration.

## [v0.5.13] - 2026-10-03

### Added

- The Console is the surface on top of `POST /api/tasks/{id}/adopt` already in v0.5.11. The published v0.5.12 package still has no Console adopt button. Standalone with no `meta_dsn`: a leftover directory is listed. In the Chinese Console its detail actions are 认领, 启动, 停止, and 删除, and there is no 编辑. A task that already has source credentials shows 编辑, 启动, 停止, and 删除, and no 认领. 认领 submits `cluster_key` and source. Leaving the start mode at the default (highest segment end) does not send an override. The success toast is 任务已认领. The task stays 已停止, shows the source and cluster key, does not show the password, and does not start replication. The existing 启动 action then starts it. Dogfood on tip `39981ead` (`v0.0.0-20261003150852-39981ead3fd5`) saw `mysql-bin.000005.open.e2` in the same directory while the earlier segment sizes stayed unchanged. With `meta_dsn`, leftover directories are not listed (the Console table is empty; ids 99 and 42 do not appear) and there is no 认领. `POST /api/tasks/99/adopt` returns 404 with body `task not found`. `backup_tasks` stays 0. Adopt does not create a task from a directory. The API is unchanged. No schema migration. No new config key.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.13`.

## [v0.5.12] - 2026-10-03

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.12`.

### Docs

- This patch only corrects the shipped guide. Behavior is unchanged from v0.5.11. English `docs/guide/admin/deployment.md` now says that adopting an id that is not a catalog task returns 404 with body `task not found`. Chinese section 7.4 describes the same adopt path: the leftover directory is listed, adopt before start, 200 and `STOPPED`, the password is not returned, an omitted start is `FILE_POS` at the highest segment name and size, start is 204 in the same directory, and with `meta_dsn` a directory that is not a catalog task is 404 `task not found`. The published v0.5.11 tarball still has the old Chinese 7.4 and the English sentence that omits 404. The Console has no adopt button.

## [v0.5.11] - 2026-10-03

### Added

- Standalone with no `meta_dsn`: `POST /api/tasks/{id}/adopt` attaches `cluster_key` and source credentials to a leftover `{data_dir}/{id}/` directory and keeps that id. The response is 200, password is omitted, and the state stays `STOPPED`. When `start` is omitted, the saved position is `FILE_POS` at the size of the highest sealed or `.open.e*` segment. An explicit `start.mode` overrides that. `POST /api/tasks/{id}/start` then returns 204 and writes the next epoch in the same directory. Existing sealed and open segments stay. Before adopt, update and start still return 400 `on-disk backup has no task metadata`. A configured `meta_dsn` still does not discover directories from disk. No new config key. No schema migration.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.11`.

## [v0.5.10] - 2026-10-03

### Fixed

- Standalone with no `meta_dsn`, after the process exits and a new process starts on the same `data_dir`: leftover `{data_dir}/{task_id}/` directories that still contain sealed or `.open.e<epoch>` segments show up in `GET /api/tasks` and the Console without a known id. The id is the directory name. `GET /api/tasks/{id}/files` uses the existing disk-scan contract (`file_name` is the source name, an open `file_path` keeps `.open.e<epoch>`, `start_pos` and `end_pos` are 0, ascending source index). Checkpoint stays 404 `checkpoint not found`. The row has no source credentials. Start returns 400 with body `on-disk backup has no task metadata` and does not start replication. A configured `meta_dsn` does not discover directories from disk. A non-empty `binlog_files` catalog still wins, and extra files on disk are not listed. No new config key. No schema migration.

### Changed

- Operator download examples in README, the landing page, the deployment guide, the config templates, and the workers API example now pin `v0.5.10`.

### Docs

- `docs/guide/admin/configuration.md` no longer says that `GET /api/tasks/{id}/files` returns `[]` when `meta_dsn` is unset. This is a guide correction, not a behavior change. The published v0.5.9 tarball still has that sentence.

## [v0.5.9] - 2026-10-03

### Fixed

- Standalone with no `meta_dsn`, or a meta catalog with no `binlog_files` rows for that task: `GET /api/tasks/{id}/files` scans `{data_dir}/{task_id}/` and returns sealed files and `.open.e<epoch>` segments. `file_name` is the source file name. `file_path` is the on-disk path and includes `.open.e<epoch>` for an open segment. Order is ascending source index; the same index lists the sealed name, then open epochs. `limit` keeps the highest indexes. `start_pos` and `end_pos` are 0 because the disk scan has no offsets. Checkpoint stays 404 `checkpoint not found` when there is no checkpoint row. A non-empty `binlog_files` catalog is unchanged (`sealed_at` descending) and is not replaced by the disk scan. A store error does not fall back to disk. The Console task files table shows the on-disk name and `file_path`.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.9`.

### Docs

- The replay section says a MySQL source requires MySQL's own `mysqlbinlog`, and a MariaDB source requires `mariadb-binlog`. If `mysqlbinlog --version` prints MariaDB, do not use it against MySQL: the pipe fails at `check_constraint_checks` with error 1193 and no rows land. The files are not corrupt. The v0.5.9 archive includes `docs/guide`.

## [v0.5.8] - 2026-10-03

### Fixed

- With `api.auth.enabled=true` and a bearer token, `GET /ui/` and the Console assets load without an `Authorization` header. Settings collects the bearer token, and later API calls send `Authorization: Bearer`. `/api/*`, `/metrics`, and `/swagger/*` still require that credential. `/healthz` stays open. Config keys are unchanged.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.8`.

### Docs

- The admin guide explains replaying on-disk segments with `mysqlbinlog` or `mariadb-binlog`. The path is `{data_dir}/{task_id}/mysql-bin.NNNNNN`. A `.open.e<epoch>` name is a filename suffix; the tool reads that path as-is. Standalone without `meta_dsn` returns `[]` from the files API and 404 for checkpoint, so replay the disk. Object storage keeps only sealed successful uploads. The v0.5.7 archive omitted `docs/guide`. The v0.5.8 archive includes it, so the replay section is in the unpacked package.

## [v0.5.7] - 2026-10-02

### Fixed

- Task reads (`GetTask`, task lists, list counts, checkpoints) retry transient meta MySQL errors and recycle pooled connections, so a brief meta failover does not return 500. A missing task is still not found.
- A `flavor=mysql` probe that finds no `@@server_uuid` stays `SOURCE_IDENTITY_UNAVAILABLE` and tells the operator the source looks like MariaDB and to set `flavor=mariadb`. `flavor=mariadb` identity is unchanged.
- A heartbeats-less local process is not an offline Worker. Overview reports `worker_count=0` and `single_process=true`, `GET /api/workers` is empty, and the Console says this process pulls tasks.
- The embedded Console create-task and batch-create Flavor dropdown renders again. The hint no longer contains a raw `@@server_uuid` token, which vue-i18n@9 rejects while compiling production messages. CI compiles locale catalogs before the UI build.

### Changed

- Operator download examples in README, the landing page, the deployment guide, and the config templates now pin `v0.5.7`.
- Landing console screenshots ship as PNG assets. CI and the release workflow run `scripts/check-landing-assets.sh`.
- The landing page and production template describe the Day-1 install and first backup path.

## [v0.5.6] - 2026-10-02

### Added

- `scripts/failover-dogfood.sh` and the verify-binlog-server failover-lease skill prove dual-worker lease takeover. The lease state machine is unchanged.
- A project-local verify-binlog-server Cursor skill drives a released binary through the HTTP API and the embedded Console.

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.6`.
- `GET /api/dashboard` and `GET /api/summary` count states and sources with SQL `GROUP BY` and page rows with `LIMIT/OFFSET` instead of scanning every matching task row.
- When `api.auth.enabled=true`, `/ui/*` and `/swagger/*` use the API auth middleware. `/healthz` stays open.
- Go modules: validator `v10.30.5`.

### Fixed

- The embedded Console bundle parses again. `v0.5.5` shipped a stray `}` in the entry import, so Chrome threw `SyntaxError` and the shell never mounted. CI and the release workflow run `scripts/check-ui-bundle.sh` (`node --check --input-type=module`).

### Security

- `PRODUCTION=true` refuses to start when `--encryption-key` is empty.

## [v0.5.5] - 2026-09-21

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.5`.
- Task-observation host filters and source lookup share `SameSourceHost`; loopback spellings are one source.
- Cluster overview, worker task counters, and metrics task/owner views read the unfiltered store ownership copy.
- The console task page requires dashboard paging fields and no longer locally re-pages a legacy payload.
- Console list refresh uses the task owner/epoch copy for lease risk instead of per-row `/lease`.
- Go modules: validator `v10.30.4`, MySQL driver `v1.10.1`, migrate `v4.20.1`, `golang.org/x/time` `v0.16.0`.

### Fixed

- `GetTask` fails loud on store miss or store error instead of returning a stale in-memory ownership copy.
- Source lookup reads the store ownership copy instead of the boot-time memory list.
- Retry-upload E2E pulls MinIO from Quay so CI is not blocked on Docker Hub `minio/minio:latest`.

### Docs

- README includes Console, task detail, and Swagger screenshots.

## [v0.5.4] - 2026-09-07

### Changed

- Operator download examples in README, the landing page, and the deployment guide now pin `v0.5.4`.
- Standalone workers inject an in-process lease table and `worker_id=standalone`, so standalone and cluster share the same ownership door.
- Process boot and the claim tick both call `ClaimRunnableTasks` instead of a separate resume path.
- First sealed-file upload verifies the current lease, then shares `ApplySealedUpload` with retry-upload.
- Dashboard and summary load matching tasks with one filtered read (`Limit<=0` unbounded), then page in process.

### Fixed

- `FAILED` releases the lease immediately so another worker can start the task without waiting for TTL.
- Expired-lease and failed-upload lookups fail loud when the store does not implement the dedicated query, instead of silently scanning an empty result.

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
