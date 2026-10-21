// Package meta provides module-level functionality for meta.
// input: sqlmock for one health apply with a cluster fence, with and without Fence.ChainVerified
// output: proof that task_health.chain_owner is assigned only when the holder read the directory, and is left as chain_owner = chain_owner otherwise
// pos: ADR 0011 PR-3 call site in applyOnce (the in-memory lease store uses the same Fence.RecordChainOwner predicate)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"binlog_server/internal/health"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestApplyTransitionsChainOwnerRequiresVerifiedRead(t *testing.T) {
	const updateKeep = "UPDATE task_health SET status = ?, reasons = ?, evaluated_at = UTC_TIMESTAMP(6), generation = ?, owner = ?, owner_epoch = ?, fence_mode = ?, fence_instance = ?, fence_epoch = ?, fence_node = ?, chain_owner = chain_owner WHERE task_id = ?"
	const updateSet = "UPDATE task_health SET status = ?, reasons = ?, evaluated_at = UTC_TIMESTAMP(6), generation = ?, owner = ?, owner_epoch = ?, fence_mode = ?, fence_instance = ?, fence_epoch = ?, fence_node = ?, chain_owner = ? WHERE task_id = ?"
	keep := []any{"HEALTHY", "null", int64(0), "n1", int64(1), "cluster", "i", int64(1), "n1", "t1"}
	set := []any{"HEALTHY", "null", int64(0), "n1", int64(1), "cluster", "i", int64(1), "n1", "n1", "t1"}

	t.Run("unverified", func(t *testing.T) {
		applyChainOwner(t, health.Fence{Mode: health.FenceCluster, Node: "n1", Instance: "i", TaskEpoch: 1}, updateKeep, keep)
	})
	t.Run("verified", func(t *testing.T) {
		applyChainOwner(t, health.Fence{Mode: health.FenceCluster, Node: "n1", Instance: "i", TaskEpoch: 1, ChainVerified: true}, updateSet, set)
	})
}

func applyChainOwner(t *testing.T, fence health.Fence, update string, args []any) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("8.0.36"))
	mock.ExpectExec(regexp.QuoteMeta(insertHealthRowSQL)).WithArgs("t1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_worker_id, epoch, lease_expire_at <= NOW(6) FROM task_leases WHERE task_id = ? FOR UPDATE")).
		WithArgs("t1").WillReturnRows(sqlmock.NewRows([]string{"owner_worker_id", "epoch", "expired"}).AddRow("n1", int64(1), false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fence_mode, fence_epoch, fence_node, fence_instance, generation, chain_owner FROM task_health WHERE task_id = ? FOR UPDATE")).
		WithArgs("t1").WillReturnRows(sqlmock.NewRows([]string{"fence_mode", "fence_epoch", "fence_node", "fence_instance", "generation", "chain_owner"}).AddRow("cluster", int64(1), "n1", "i", int64(0), "n1"))
	vals := make([]driver.Value, len(args))
	for i, arg := range args {
		vals[i] = arg
	}
	mock.ExpectExec(regexp.QuoteMeta(update)).WithArgs(vals...).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	_, err = store.ApplyTransitions(context.Background(), "t1", fence, nil, health.TaskHealth{TaskID: "t1", Status: health.StatusHealthy})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
