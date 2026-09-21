# Binlog Server v0.5.5

Release date: 2026-09-21

Binlog Server `v0.5.5` is a control-plane observation patch on `v0.5.4`.

## Highlights

- `GetTask` no longer returns a stale in-memory ownership copy when the store fails. Store miss is 404; other store errors are 5xx.
- Source lookup and task-observation host filters share `SameSourceHost`. Loopback spellings are one source.
- Cluster overview, worker task counters, and metrics task/owner views read the unfiltered store ownership copy. They are not the filtered dashboard page and not the boot-time memory list.
- The console task page trusts one dashboard materialization for page, summary, and source counts. It does not treat a missing paging payload as a legacy local page.
- Task-list refresh no longer fetches `/lease` per row. Lease risk uses the owner/epoch copy on the task. `GET /api/tasks/{id}/lease` remains for a single task.
- Source lookup reads the same store ownership copy as cluster observation.
- Retry-upload E2E pulls MinIO from Quay instead of Docker Hub `minio/minio:latest`.
- Go modules: validator `v10.30.4`, MySQL driver `v1.10.1`, migrate `v4.20.1`, `golang.org/x/time` `v0.16.0`.
- README now includes Console, task detail, and Swagger screenshots.
- Quick Start, landing-page, and deployment download examples now point at `v0.5.5`.

## Upgrade Notes

- No schema migration is required for `v0.5.5`.
- `GET /api/tasks` remains `{items,total,limit,offset}`. Dashboard and summary still materialize the filtered set once, then page in process. That is not a second SQL `COUNT`.
- Store read failures on a single task, cluster overview, and workers now fail instead of serving a stale memory snapshot. Clients that treated those 200 bodies as authoritative should handle 5xx.
- Best-effort upload is unchanged: a failed upload does not stop dump; use `/api/tasks/{id}/files/retry-upload` to retry.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.5.zh-CN.md
