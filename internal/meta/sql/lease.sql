-- name: AcquireTaskLease :exec
-- Epoch increases only when a different worker takes an expired lease.
-- The same worker_id keeps the epoch, including after restart once the lease
-- has expired. A released row stores an empty owner, so the next acquire
-- is a different owner and the epoch increases. A missing row inserts epoch 1.
-- Assign epoch before owner_worker_id so the comparison sees the previous owner.
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
VALUES (
  sqlc.arg(task_id),
  sqlc.arg(worker_id),
  1,
  DATE_ADD(NOW(6), INTERVAL sqlc.arg(ttl_micros) MICROSECOND),
  NOW(6)
)
ON DUPLICATE KEY UPDATE
  epoch = IF(
    lease_expire_at <= NOW(6) AND owner_worker_id <> sqlc.arg(worker_id),
    epoch + 1,
    epoch
  ),
  owner_worker_id = IF(lease_expire_at <= NOW(6), sqlc.arg(worker_id), owner_worker_id),
  lease_expire_at = IF(
    lease_expire_at <= NOW(6),
    DATE_ADD(NOW(6), INTERVAL sqlc.arg(ttl_micros) MICROSECOND),
    lease_expire_at
  ),
  renewed_at = IF(lease_expire_at <= NOW(6), NOW(6), renewed_at);

-- name: RenewTaskLease :execresult
UPDATE task_leases
SET lease_expire_at = DATE_ADD(NOW(6), INTERVAL sqlc.arg(ttl_micros) MICROSECOND), renewed_at = NOW(6)
WHERE task_id = sqlc.arg(task_id)
  AND owner_worker_id = sqlc.arg(worker_id)
  AND epoch = sqlc.arg(epoch);

-- name: ReleaseTaskLease :execresult
UPDATE task_leases
SET owner_worker_id = '', lease_expire_at = NOW(6), renewed_at = NOW(6)
WHERE task_id = sqlc.arg(task_id)
  AND owner_worker_id = sqlc.arg(worker_id)
  AND epoch = sqlc.arg(epoch);

-- name: GetTaskLease :one
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = sqlc.arg(task_id);

-- name: GetCurrentDBTime :one
SELECT NOW(6);
