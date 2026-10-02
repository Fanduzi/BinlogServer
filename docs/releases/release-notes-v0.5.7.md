# Binlog Server v0.5.7

Release date: 2026-10-02

Binlog Server `v0.5.7` is an operator patch on `v0.5.6` for meta failover reads, MariaDB flavor guidance, single-process worker counts, and the embedded Console Flavor control.

## Highlights

- Control-plane task reads retry transient meta MySQL errors. `GetTask`, task list queries, list totals, and `LoadCheckpoint` use the same retry policy as lease writes. Pooled connections recycle (`ConnMaxLifetime` 30s) so a brief orchestrator takeover does not leave the process stuck on a demoted writer. A missing task stays not-found and is not retried.
- A `flavor=mysql` probe that finds no `@@server_uuid` (empty result or unknown system variable) stays `SOURCE_IDENTITY_UNAVAILABLE` and tells the operator the source looks like MariaDB and to set `flavor=mariadb`. `flavor=mariadb` identity is unchanged. Quick Start and the landing page show `"flavor":"mariadb"` for MariaDB sources. The Console Flavor control is a mysql/mariadb choice.
- Standalone and other single-process deploys own tasks as `worker_id=standalone` and do not write a heartbeat. Overview no longer counts that owner as an offline Worker: `worker_count=0`, `single_process=true`, and `GET /api/workers` is empty. The Console says “单机，任务由本进程拉取” (English: “Single process: this process pulls tasks”). A `standalone` heartbeat, when one exists, is still a worker row.
- The embedded Console create-task and batch-create Flavor dropdown renders again. The hint no longer contains a raw `@@server_uuid` token, which vue-i18n@9 rejects while compiling production messages. Locale catalogs are compiled before the UI build and in CI.
- Landing console screenshots are real PNG assets (`console-dashboard.png`, `task-detail.png`, `swagger.png`). `scripts/check-landing-assets.sh` runs in CI and in the release workflow.
- The landing page and production template describe the Day-1 install and first-backup path. Quick Start, landing, deployment, and config-template download pins point at `v0.5.7`.

## Upgrade Notes

- No schema migration is required for `v0.5.7`. Migrations are still `000001_init_schema` only.
- Task-read HTTP calls that previously returned 500 on a brief meta MySQL outage now retry inside the store. Not-found is still not-found.
- MariaDB sources must send `"flavor":"mariadb"`. Leaving `mysql` still fails at start with `SOURCE_IDENTITY_UNAVAILABLE`; the message now names MariaDB and `flavor=mariadb`.
- Single-process overview JSON includes `single_process`. When this process pulls and no worker heartbeat exists, `worker_count` is `0` and `workers` is empty. Cluster workers that heartbeat are unchanged.
- Config keys are unchanged. `PRODUCTION=true` still requires a non-empty `--encryption-key`.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.7.zh-CN.md
