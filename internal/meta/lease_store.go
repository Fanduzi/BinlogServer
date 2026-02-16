package meta

import (
	"context"
	"database/sql"
	"time"
)

const acquireLeaseSQL = `
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
VALUES (?, ?, 1, ?, ?)
ON DUPLICATE KEY UPDATE
  owner_worker_id = IF(lease_expire_at <= ?, ?, owner_worker_id),
  epoch = IF(lease_expire_at <= ?, epoch + 1, epoch),
  lease_expire_at = IF(lease_expire_at <= ?, ?, lease_expire_at),
  renewed_at = IF(lease_expire_at <= ?, VALUES(renewed_at), renewed_at);
`

const renewLeaseSQL = `
UPDATE task_leases
SET lease_expire_at = ?, renewed_at = ?
WHERE task_id = ? AND owner_worker_id = ? AND epoch = ?;
`

const releaseLeaseSQL = `
DELETE FROM task_leases
WHERE task_id = ? AND owner_worker_id = ? AND epoch = ?;
`

const getLeaseSQL = `
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = ?;
`

type Lease struct {
	TaskID        string
	OwnerWorkerID string
	Epoch         int64
	LeaseExpireAt time.Time
	RenewedAt     time.Time
}

type LeaseStore struct {
	db *sql.DB
}

func NewLeaseStore(db *sql.DB) *LeaseStore {
	return &LeaseStore{db: db}
}

func (s *LeaseStore) Acquire(ctx context.Context, taskID, workerID string, now time.Time, ttl time.Duration) (int64, bool, error) {
	leaseExpireAt := now.Add(ttl)
	_, err := s.db.ExecContext(
		ctx,
		acquireLeaseSQL,
		taskID,
		workerID,
		leaseExpireAt,
		now,
		now,
		workerID,
		now,
		now,
		leaseExpireAt,
		now,
	)
	if err != nil {
		return 0, false, err
	}

	lease, ok, err := s.Get(ctx, taskID)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	if lease.OwnerWorkerID != workerID || !lease.LeaseExpireAt.After(now) {
		return lease.Epoch, false, nil
	}
	return lease.Epoch, true, nil
}

func (s *LeaseStore) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	result, err := s.db.ExecContext(
		ctx,
		renewLeaseSQL,
		now.Add(ttl),
		now,
		taskID,
		workerID,
		epoch,
	)
	if err != nil {
		return false, err
	}
	return rowsAffectedGreaterThanZero(result)
}

func (s *LeaseStore) Get(ctx context.Context, taskID string) (Lease, bool, error) {
	row := s.db.QueryRowContext(ctx, getLeaseSQL, taskID)
	var lease Lease
	if err := row.Scan(
		&lease.TaskID,
		&lease.OwnerWorkerID,
		&lease.Epoch,
		&lease.LeaseExpireAt,
		&lease.RenewedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return Lease{}, false, nil
		}
		return Lease{}, false, err
	}
	return lease, true, nil
}

func (s *LeaseStore) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, releaseLeaseSQL, taskID, workerID, epoch)
	if err != nil {
		return false, err
	}
	return rowsAffectedGreaterThanZero(result)
}

func rowsAffectedGreaterThanZero(result sql.Result) (bool, error) {
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}
