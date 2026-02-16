package meta

import (
	"context"
	"database/sql"
	"time"
)

const acquireLeaseSQL = `
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
VALUES (?, ?, 1, DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), NOW(6))
ON DUPLICATE KEY UPDATE
  owner_worker_id = IF(lease_expire_at <= NOW(6), ?, owner_worker_id),
  epoch = IF(lease_expire_at <= NOW(6), epoch + 1, epoch),
  lease_expire_at = IF(lease_expire_at <= NOW(6), DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), lease_expire_at),
  renewed_at = IF(lease_expire_at <= NOW(6), NOW(6), renewed_at);
`

const renewLeaseSQL = `
UPDATE task_leases
SET lease_expire_at = DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), renewed_at = NOW(6)
WHERE task_id = ? AND owner_worker_id = ? AND epoch = ?;
`

const releaseLeaseSQL = `
UPDATE task_leases
SET owner_worker_id = '', lease_expire_at = NOW(6), renewed_at = NOW(6)
WHERE task_id = ? AND owner_worker_id = ? AND epoch = ?;
`

const getLeaseSQL = `
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = ?;
`

const currentDBTimeSQL = `
SELECT NOW(6);
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
	_ = now
	ttlMicros := durationToMicroseconds(ttl)
	var (
		epoch int64
		ok    bool
	)
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		_, err := s.db.ExecContext(
			ctx,
			acquireLeaseSQL,
			taskID,
			workerID,
			ttlMicros,
			workerID,
			ttlMicros,
		)
		if err != nil {
			return err
		}

		lease, exists, err := s.getNoRetry(ctx, taskID)
		if err != nil {
			return err
		}
		if !exists {
			epoch = 0
			ok = false
			return nil
		}

		dbNow, err := s.currentDBTimeNoRetry(ctx)
		if err != nil {
			return err
		}
		epoch = lease.Epoch
		ok = lease.OwnerWorkerID == workerID && lease.LeaseExpireAt.After(dbNow)
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return epoch, ok, nil
}

func (s *LeaseStore) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	_ = now
	var renewed bool
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		result, err := s.db.ExecContext(
			ctx,
			renewLeaseSQL,
			durationToMicroseconds(ttl),
			taskID,
			workerID,
			epoch,
		)
		if err != nil {
			return err
		}
		renewed, err = rowsAffectedGreaterThanZero(result)
		return err
	})
	if err != nil {
		return false, err
	}
	return renewed, nil
}

func (s *LeaseStore) Get(ctx context.Context, taskID string) (Lease, bool, error) {
	var (
		lease Lease
		ok    bool
	)
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		var err error
		lease, ok, err = s.getNoRetry(ctx, taskID)
		return err
	})
	if err != nil {
		return Lease{}, false, err
	}
	return lease, ok, nil
}

func (s *LeaseStore) getNoRetry(ctx context.Context, taskID string) (Lease, bool, error) {
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
	var released bool
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		result, err := s.db.ExecContext(ctx, releaseLeaseSQL, taskID, workerID, epoch)
		if err != nil {
			return err
		}
		released, err = rowsAffectedGreaterThanZero(result)
		return err
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

func (s *LeaseStore) currentDBTime(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		var err error
		now, err = s.currentDBTimeNoRetry(ctx)
		return err
	})
	if err != nil {
		return time.Time{}, err
	}
	return now, nil
}

func (s *LeaseStore) currentDBTimeNoRetry(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := s.db.QueryRowContext(ctx, currentDBTimeSQL).Scan(&now); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

func rowsAffectedGreaterThanZero(result sql.Result) (bool, error) {
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func durationToMicroseconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	return d.Microseconds()
}
