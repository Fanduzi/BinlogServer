-- name: AcquireTaskLease :exec
-- Epoch increases when a different worker takes an expired lease, and when
-- the same worker_id re-acquires a released row whose owner is empty.
-- A non-empty same owner keeps the epoch, including after restart once the
-- lease has expired. A missing row inserts epoch 1.
-- @bs_lease_now is this statement's single NOW(6). Every comparison and every
-- written timestamp uses that pin, so a clock tick inside the statement cannot
-- split the expire check from the new deadline.
-- Assign epoch before owner_worker_id so the comparison sees the previous owner.
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
SELECT
  sqlc.arg(task_id),
  sqlc.arg(worker_id),
  1,
  DATE_ADD(@bs_lease_now, INTERVAL sqlc.arg(ttl_micros) MICROSECOND),
  @bs_lease_now
FROM (SELECT @bs_lease_now := NOW(6)) AS clk
ON DUPLICATE KEY UPDATE
  epoch = IF(
    lease_expire_at <= @bs_lease_now
      AND (owner_worker_id = '' OR owner_worker_id <> sqlc.arg(worker_id)),
    epoch + 1,
    epoch
  ),
  owner_worker_id = IF(lease_expire_at <= @bs_lease_now, sqlc.arg(worker_id), owner_worker_id),
  lease_expire_at = IF(
    lease_expire_at <= @bs_lease_now,
    DATE_ADD(@bs_lease_now, INTERVAL sqlc.arg(ttl_micros) MICROSECOND),
    lease_expire_at
  ),
  renewed_at = IF(lease_expire_at <= @bs_lease_now, @bs_lease_now, renewed_at);

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
