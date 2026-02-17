package meta

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestLeaseStore_AcquireRenewRelease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()
	ttl := 15 * time.Second
	ttlMicros := ttl.Microseconds()

	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-a", ttlMicros, "worker-a", ttlMicros).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-a", int64(7), now.Add(ttl), now))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	epoch, ok, err := store.Acquire(context.Background(), "task-1", "worker-a", now, ttl)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected acquire success")
	}
	if epoch != 7 {
		t.Fatalf("expected epoch 7, got %d", epoch)
	}

	mock.ExpectExec(regexp.QuoteMeta(renewLeaseSQL)).
		WithArgs(ttlMicros, "task-1", "worker-a", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	renewed, err := store.Renew(context.Background(), "task-1", "worker-a", 7, now, ttl)
	if err != nil {
		t.Fatalf("Renew returned error: %v", err)
	}
	if !renewed {
		t.Fatal("expected renew success")
	}

	mock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WithArgs("task-1", "worker-a", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	released, err := store.Release(context.Background(), "task-1", "worker-a", 7)
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if !released {
		t.Fatal("expected release success")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_FencingByEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()
	ttl := 15 * time.Second
	ttlMicros := ttl.Microseconds()

	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-a", ttlMicros, "worker-a", ttlMicros).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-b", int64(9), now.Add(ttl), now))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	epoch, ok, err := store.Acquire(context.Background(), "task-1", "worker-a", now, ttl)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if ok {
		t.Fatal("expected acquire rejected by fencing")
	}
	if epoch != 9 {
		t.Fatalf("expected current epoch 9, got %d", epoch)
	}

	mock.ExpectExec(regexp.QuoteMeta(renewLeaseSQL)).
		WithArgs(ttlMicros, "task-1", "worker-a", int64(8)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	renewed, err := store.Renew(context.Background(), "task-1", "worker-a", 8, now, ttl)
	if err != nil {
		t.Fatalf("Renew returned error: %v", err)
	}
	if renewed {
		t.Fatal("expected renew rejected by epoch mismatch")
	}

	mock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WithArgs("task-1", "worker-a", int64(8)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	released, err := store.Release(context.Background(), "task-1", "worker-a", 8)
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if released {
		t.Fatal("expected release rejected by epoch mismatch")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_ReleaseThenAcquireKeepsEpochMonotonic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()
	ttl := 15 * time.Second
	ttlMicros := ttl.Microseconds()

	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-a", ttlMicros, "worker-a", ttlMicros).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-a", int64(7), now.Add(ttl), now))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	firstEpoch, ok, err := store.Acquire(context.Background(), "task-1", "worker-a", now, ttl)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected first acquire success")
	}
	if firstEpoch != 7 {
		t.Fatalf("expected first epoch 7, got %d", firstEpoch)
	}

	mock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WithArgs("task-1", "worker-a", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	released, err := store.Release(context.Background(), "task-1", "worker-a", firstEpoch)
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if !released {
		t.Fatal("expected release success")
	}

	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-b", ttlMicros, "worker-b", ttlMicros).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-b", int64(8), now.Add(ttl), now))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	secondEpoch, ok, err := store.Acquire(context.Background(), "task-1", "worker-b", now, ttl)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected second acquire success")
	}
	if secondEpoch != 8 {
		t.Fatalf("expected second epoch 8, got %d", secondEpoch)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_ReleaseUsesSoftExpireInsteadOfDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	mock.ExpectExec(`(?s)UPDATE\s+task_leases\s+SET\s+owner_worker_id\s*=\s*''\s*,\s*lease_expire_at\s*=\s*NOW\(6\)\s*,\s*renewed_at\s*=\s*NOW\(6\)`).
		WithArgs("task-1", "worker-a", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	released, err := store.Release(context.Background(), "task-1", "worker-a", 7)
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if !released {
		t.Fatal("expected release success")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_AcquireRetriesTransientError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()
	ttl := 15 * time.Second
	ttlMicros := ttl.Microseconds()
	transientErr := errors.New("connection reset by peer")

	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-a", ttlMicros, "worker-a", ttlMicros).
		WillReturnError(transientErr)
	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-a", ttlMicros, "worker-a", ttlMicros).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-a", int64(11), now.Add(ttl), now))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	epoch, ok, err := store.Acquire(context.Background(), "task-1", "worker-a", now, ttl)
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected acquire success")
	}
	if epoch != 11 {
		t.Fatalf("expected epoch 11, got %d", epoch)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_RenewRetriesTransientError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()
	ttl := 15 * time.Second
	ttlMicros := ttl.Microseconds()
	transientErr := errors.New("connection reset by peer")

	mock.ExpectExec(regexp.QuoteMeta(renewLeaseSQL)).
		WithArgs(ttlMicros, "task-1", "worker-a", int64(7)).
		WillReturnError(transientErr)
	mock.ExpectExec(regexp.QuoteMeta(renewLeaseSQL)).
		WithArgs(ttlMicros, "task-1", "worker-a", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := store.Renew(context.Background(), "task-1", "worker-a", 7, now, ttl)
	if err != nil {
		t.Fatalf("Renew returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected renew success")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_ReleaseRetriesTransientError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	transientErr := errors.New("connection reset by peer")

	mock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WithArgs("task-1", "worker-a", int64(7)).
		WillReturnError(transientErr)
	mock.ExpectExec(regexp.QuoteMeta(releaseLeaseSQL)).
		WithArgs("task-1", "worker-a", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := store.Release(context.Background(), "task-1", "worker-a", 7)
	if err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected release success")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_GetRetriesTransientError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()
	transientErr := errors.New("connection reset by peer")

	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnError(transientErr)
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-a", int64(7), now.Add(15*time.Second), now))

	lease, ok, err := store.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected lease exists")
	}
	if lease.Epoch != 7 {
		t.Fatalf("expected epoch 7, got %d", lease.Epoch)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_VerifyOwnership(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()

	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-a", int64(7), now.Add(15*time.Second), now))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))

	ok, err := store.VerifyOwnership(context.Background(), "task-1", "worker-a", 7)
	if err != nil {
		t.Fatalf("VerifyOwnership returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected ownership verification success")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestLeaseStore_VerifyOwnershipMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := NewLeaseStore(db)
	now := time.Unix(1735689600, 0).UTC()

	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-b", int64(9), now.Add(15*time.Second), now))

	ok, err := store.VerifyOwnership(context.Background(), "task-1", "worker-a", 7)
	if err != nil {
		t.Fatalf("VerifyOwnership returned error: %v", err)
	}
	if ok {
		t.Fatal("expected ownership verification rejected")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
