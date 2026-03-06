-- name: LoadTaskRunState :one
SELECT run_id
FROM backup_tasks
WHERE id = sqlc.arg(task_id);

-- name: UpsertTaskRun :exec
INSERT INTO task_runs (run_id, task_id, worker_id, epoch, started_at)
VALUES (
  sqlc.arg(run_id),
  sqlc.arg(task_id),
  sqlc.arg(worker_id),
  sqlc.arg(epoch),
  sqlc.arg(started_at)
)
ON DUPLICATE KEY UPDATE
  task_id = VALUES(task_id),
  worker_id = VALUES(worker_id),
  epoch = VALUES(epoch),
  started_at = VALUES(started_at);

-- name: FinishTaskRun :exec
UPDATE task_runs
SET ended_at = sqlc.arg(ended_at), end_reason = sqlc.arg(end_reason)
WHERE run_id = sqlc.arg(run_id)
  AND ended_at IS NULL;

-- name: ListTaskRuns :many
SELECT run_id, task_id, worker_id, epoch, started_at, ended_at, end_reason
FROM task_runs
WHERE task_id = sqlc.arg(task_id)
ORDER BY started_at DESC
LIMIT ?;

-- name: UpsertWorkerHeartbeat :exec
INSERT INTO worker_heartbeats (worker_id, host, version, last_seen_at, status)
VALUES (
  sqlc.arg(worker_id),
  sqlc.arg(host),
  sqlc.arg(version),
  sqlc.arg(last_seen_at),
  sqlc.arg(status)
)
ON DUPLICATE KEY UPDATE
  host = VALUES(host),
  version = VALUES(version),
  last_seen_at = VALUES(last_seen_at),
  status = VALUES(status);

-- name: ListWorkerHeartbeats :many
SELECT worker_id, host, version, last_seen_at, status
FROM worker_heartbeats
ORDER BY worker_id ASC
LIMIT ?;
