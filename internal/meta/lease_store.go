// Package meta provides module-level functionality for meta.
// input: MySQL connections, SQL schema/contracts, retry/lease timing policies
// output: persistent metadata operations for tasks, leases, runs, and checkpoints
// pos: metadata persistence layer between domain scheduler and MySQL storage engine
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"time"

	"binlog_server/internal/meta/sqlcgen"
)

const acquireLeaseSQL = `
-- name: AcquireTaskLease :exec
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
VALUES (
  ?,
  ?,
  1,
  DATE_ADD(NOW(6), INTERVAL ? MICROSECOND),
  NOW(6)
)
ON DUPLICATE KEY UPDATE
  owner_worker_id = IF(lease_expire_at <= NOW(6), ?, owner_worker_id),
  epoch = IF(lease_expire_at <= NOW(6), epoch + 1, epoch),
  lease_expire_at = IF(
    lease_expire_at <= NOW(6),
    DATE_ADD(NOW(6), INTERVAL ? MICROSECOND),
    lease_expire_at
  ),
  renewed_at = IF(lease_expire_at <= NOW(6), NOW(6), renewed_at)
`

const renewLeaseSQL = `
-- name: RenewTaskLease :execresult
UPDATE task_leases
SET lease_expire_at = DATE_ADD(NOW(6), INTERVAL ? MICROSECOND), renewed_at = NOW(6)
WHERE task_id = ?
  AND owner_worker_id = ?
  AND epoch = ?
`

const releaseLeaseSQL = `
-- name: ReleaseTaskLease :execresult
UPDATE task_leases
SET owner_worker_id = '', lease_expire_at = NOW(6), renewed_at = NOW(6)
WHERE task_id = ?
  AND owner_worker_id = ?
  AND epoch = ?
`

const getLeaseSQL = `
-- name: GetTaskLease :one
SELECT task_id, owner_worker_id, epoch, lease_expire_at, renewed_at
FROM task_leases
WHERE task_id = ?
`

const currentDBTimeSQL = `
-- name: GetCurrentDBTime :one
SELECT NOW(6)
`

// Lease 表示某任务当前 lease 的持有信息。
type Lease struct {
	// TaskID 是 lease 绑定的任务 ID。
	TaskID string
	// OwnerWorkerID 是当前持有该 lease 的 worker。
	OwnerWorkerID string
	// Epoch 是单调递增的 fencing token。
	Epoch int64
	// LeaseExpireAt 是 lease 过期时间（以 DB 时间为准）。
	LeaseExpireAt time.Time
	// RenewedAt 是最近续租时间。
	RenewedAt time.Time
}

// LeaseStore 负责基于 MySQL 表实现 lease acquire/renew/release。
type LeaseStore struct {
	db *sql.DB
}

// NewLeaseStore 基于 *sql.DB 构建 LeaseStore。
func NewLeaseStore(db *sql.DB) *LeaseStore {
	return &LeaseStore{db: db}
}

// NewLeaseStoreFromTaskStore 复用 MySQLTaskStore 的 DB 句柄创建 LeaseStore。
func NewLeaseStoreFromTaskStore(store *MySQLTaskStore) *LeaseStore {
	if store == nil {
		return nil
	}
	return &LeaseStore{db: store.db}
}

func (s *LeaseStore) queries() *sqlcgen.Queries {
	return sqlcgen.New(s.db)
}

// Acquire 尝试获取 lease，返回 epoch 和是否成功。
func (s *LeaseStore) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	ttlMicros := durationToMicroseconds(ttl)
	var (
		epoch int64
		ok    bool
	)
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		err := s.queries().AcquireTaskLease(ctx, sqlcgen.AcquireTaskLeaseParams{
			TaskID:    taskID,
			WorkerID:  workerID,
			TtlMicros: ttlMicros,
		})
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

// Renew 续租指定 epoch 的 lease，返回是否续租成功。
func (s *LeaseStore) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	_ = now
	var renewed bool
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		result, err := s.queries().RenewTaskLease(ctx, sqlcgen.RenewTaskLeaseParams{
			TtlMicros: durationToMicroseconds(ttl),
			TaskID:    taskID,
			WorkerID:  workerID,
			Epoch:     epoch,
		})
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

// Get 读取任务当前 lease 记录。
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

// getNoRetry 读取单任务 lease 记录（不带重试封装）。
func (s *LeaseStore) getNoRetry(ctx context.Context, taskID string) (Lease, bool, error) {
	row, err := s.queries().GetTaskLease(ctx, taskID)
	if err != nil {
		if err == sql.ErrNoRows {
			return Lease{}, false, nil
		}
		return Lease{}, false, err
	}
	return Lease{
		TaskID:        row.TaskID,
		OwnerWorkerID: row.OwnerWorkerID,
		Epoch:         row.Epoch,
		LeaseExpireAt: row.LeaseExpireAt,
		RenewedAt:     row.RenewedAt,
	}, true, nil
}

// Release 释放 lease（通过软过期而非删除行）。
func (s *LeaseStore) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	var released bool
	err := WithRetry(ctx, DefaultMySQLRetryPolicy(), func() error {
		result, err := s.queries().ReleaseTaskLease(ctx, sqlcgen.ReleaseTaskLeaseParams{
			TaskID:   taskID,
			WorkerID: workerID,
			Epoch:    epoch,
		})
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

// currentDBTime 获取数据库当前时间（带重试）。
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

// currentDBTimeNoRetry 获取数据库当前时间（不带重试）。
func (s *LeaseStore) currentDBTimeNoRetry(ctx context.Context) (time.Time, error) {
	return s.queries().GetCurrentDBTime(ctx)
}

// VerifyOwnership 校验给定 worker/epoch 当前是否仍拥有有效 lease。
func (s *LeaseStore) VerifyOwnership(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	lease, ok, err := s.Get(ctx, taskID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if lease.OwnerWorkerID != workerID || lease.Epoch != epoch {
		return false, nil
	}
	dbNow, err := s.currentDBTime(ctx)
	if err != nil {
		return false, err
	}
	return lease.LeaseExpireAt.After(dbNow), nil
}

// rowsAffectedGreaterThanZero 判断 SQL 执行是否影响至少一行。
func rowsAffectedGreaterThanZero(result sql.Result) (bool, error) {
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// durationToMicroseconds 将 TTL 转为 SQL INTERVAL 需要的微秒值。
func durationToMicroseconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	return d.Microseconds()
}
