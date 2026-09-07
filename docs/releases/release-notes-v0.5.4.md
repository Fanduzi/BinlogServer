# Binlog Server v0.5.4

Release date: 2026-09-07

Binlog Server `v0.5.4` is an operator-facing ownership and upload-path patch on `v0.5.3`.

## Highlights

- Standalone and cluster workers use the same task-ownership door. A standalone process injects an in-process lease table and `worker_id=standalone` instead of skipping lease checks.
- `FAILED` releases the lease immediately, so another worker can start the task without waiting for TTL.
- Process boot and the claim tick share `ClaimRunnableTasks`: owned idle tasks, unowned `STARTING`, and expired-lease `RUNNING` / `LEASE_DEGRADED` / `RETRY_BACKOFF`.
- The first sealed-file upload verifies the current lease, then uses the same `ApplySealedUpload` path as retry-upload.
- Dashboard and summary load matching tasks with one filtered read, then page in process.
- Quick Start, landing-page, and deployment download examples now point at `v0.5.4`.

## Upgrade Notes

- No schema migration is required for `v0.5.4`.
- No API contract change versus `v0.5.3`. `GET /api/tasks` remains `{items,total,limit,offset}`.
- Standalone still does not need `cluster.enabled` or a metadata DSN. The in-process lease table lives only in that process.
- Best-effort upload is unchanged: a failed upload does not stop dump; use `/api/tasks/{id}/files/retry-upload` to retry.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.5.4.zh-CN.md
