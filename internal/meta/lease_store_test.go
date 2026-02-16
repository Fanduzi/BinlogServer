package meta

import (
	"context"
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

	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-a", now.Add(ttl), now, now, "worker-a", now, now, now.Add(ttl), now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-a", int64(7), now.Add(ttl), now))

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
		WithArgs(now.Add(ttl), now, "task-1", "worker-a", int64(7)).
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

	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WithArgs("task-1", "worker-a", now.Add(ttl), now, now, "worker-a", now, now, now.Add(ttl), now).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-b", int64(9), now.Add(ttl), now))

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
		WithArgs(now.Add(ttl), now, "task-1", "worker-a", int64(8)).
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
