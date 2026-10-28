// Package meta provides module-level functionality for meta.
// input: BINLOG_TEST_MYSQL_ROOT_DSNS, one scratch database per server, and migration 000011
// output: proof that the second task upsert bumps row_generation, a bare UPDATE is not rejected, a mismatched generation leaves the row, a restarted worker does not Acquire over an open refusal, a live older protocol refuses a claim, and an expired STOPPING lease is finalized by StopExpired, including pending_delete
// pos: ADR 0011 PR-4 scratch-meta cells; skipped when the DSN list is unset. Live SIGSTOP, directory copy, and kill -9 cells are named and skipped
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func TestClusterOwnershipPR4(t *testing.T) {
	roots := rootDSNs(t)
	for i, root := range roots {
		root := strings.TrimSpace(root)
		t.Run(fmt.Sprintf("server-%d", i), func(t *testing.T) {
			store := openOwnershipScratch(t, root)
			ctx := context.Background()
			t.Run("generation", func(t *testing.T) { ownershipGenerationFence(t, ctx, store) })
			t.Run("restart-after-yield", func(t *testing.T) { ownershipRestartAfterYield(t, ctx, store) })
			t.Run("mixed-version", func(t *testing.T) { ownershipMixedVersion(t, ctx, store) })
			t.Run("stop-expired", func(t *testing.T) { ownershipStopExpired(t, ctx, store, false) })
			t.Run("pending-delete", func(t *testing.T) { ownershipStopExpired(t, ctx, store, true) })
			t.Run("start", func(t *testing.T) { ownershipStart(t, ctx, store) })
		})
	}
}

func TestClusterOwnershipPR4UnrunCells(t *testing.T) {
	t.Run("per-node-sigstop", func(t *testing.T) {
		t.Skip("needs a live dump process to SIGSTOP and SIGCONT; this harness has no source server")
	})
	t.Run("self-recovery-directory-present", func(t *testing.T) {
		t.Skip("needs the same stopped dump and a directory that stays in place; not runnable without a source server")
	})
	t.Run("failed-heartbeat-only", func(t *testing.T) {
		t.Skip("needs a FAILED row whose only writer is heartbeat; not runnable without a source server")
	})
	t.Run("start-after-copy-directory", func(t *testing.T) {
		t.Skip("needs a copied task directory and a source the new node can dump; not runnable here")
	})
	t.Run("stop-delete-after-kill-9", func(t *testing.T) {
		t.Skip("needs kill -9 of a live holder; the expired-lease cell covers StopExpired without that process")
	})
}

func openOwnershipScratch(t *testing.T, root string) *MySQLTaskStore {
	t.Helper()
	name := fmt.Sprintf("binlog_own_%d", time.Now().UnixNano())
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + name) })
	dsn := strings.TrimSuffix(root, "/") + "/" + name + "?parseTime=true&multiStatements=true"
	m, err := migrate.New(migrationsURL(t), "mysql://"+dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	store, err := OpenClusterMeta(dsn, 15*time.Second, "", 15*time.Second, "off")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	return store
}

func sampleTask(id string, state tasks.State) tasks.Task {
	return tasks.Task{
		ID: id, Name: "n-" + id, ClusterKey: "k-" + id,
		State: state, DesiredRun: tasks.TaskDesiredRun,
		Source:    tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Flavor: "mysql"},
		Start:     tasks.StartConfig{Mode: tasks.StartModeLatest},
		UpdatedAt: time.Now(),
	}
}

func ownershipGenerationFence(t *testing.T, ctx context.Context, store *MySQLTaskStore) {
	t.Helper()
	task := sampleTask("trig", tasks.StateCreated)
	task.DesiredRun = tasks.TaskDesiredStop
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	var gen int64
	if err := store.db.QueryRowContext(ctx, `SELECT row_generation FROM backup_tasks WHERE id = 'trig'`).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	if gen != 0 {
		t.Fatalf("insert generation %d, want 0", gen)
	}
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT row_generation FROM backup_tasks WHERE id = 'trig'`).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("second upsert generation %d, want 1", gen)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE backup_tasks SET state = 'RUNNING' WHERE id = 'trig'`); err != nil {
		t.Fatalf("bare update err=%v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
VALUES ('trig', 'worker-a', 0, DATE_ADD(NOW(6), INTERVAL 60 SECOND), NOW(6))`); err != nil {
		t.Fatal(err)
	}
	n, err := store.CommitYield(ctx, tasks.YieldCommit{
		TaskID: "trig", State: string(tasks.StateRunning), Desired: tasks.TaskDesiredRun,
		ExpectEpoch: 0, ExpectGeneration: 99, UpdatedAt: time.Now(),
	})
	if err != nil || n != 0 {
		t.Fatalf("stale generation n=%d err=%v", n, err)
	}
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state, row_generation FROM backup_tasks WHERE id = 'trig'`).Scan(&state, &gen); err != nil {
		t.Fatal(err)
	}
	if state != string(tasks.StateRunning) || gen != 1 {
		t.Fatalf("stale yield wrote state=%s gen=%d", state, gen)
	}
	n, err = store.CommitYield(ctx, tasks.YieldCommit{
		TaskID: "trig", State: string(tasks.StateFailed), Desired: tasks.TaskDesiredRun,
		ExpectEpoch: 0, ExpectGeneration: 0, UpdatedAt: time.Now(),
	})
	if err != nil || n != 0 {
		t.Fatalf("generation 0 must not match a row at %d, n=%d err=%v", gen, n, err)
	}
	n, err = store.CommitYield(ctx, tasks.YieldCommit{
		TaskID: "trig", State: string(tasks.StateStopped), Desired: tasks.TaskDesiredStop,
		ExpectEpoch: 0, ExpectGeneration: 1, UpdatedAt: time.Now(),
	})
	if err != nil || n != 1 {
		t.Fatalf("matching generation n=%d err=%v", n, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT state, row_generation FROM backup_tasks WHERE id = 'trig'`).Scan(&state, &gen); err != nil {
		t.Fatal(err)
	}
	if state != string(tasks.StateStopped) || gen != 2 {
		t.Fatalf("fenced yield state=%s gen=%d", state, gen)
	}
}

func ownershipRestartAfterYield(t *testing.T, ctx context.Context, store *MySQLTaskStore) {
	t.Helper()
	task := sampleTask("yield", tasks.StateRunning)
	task.OwnerWorkerID = "worker-a"
	task.Epoch = 3
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
VALUES ('yield', 'worker-a', 3, DATE_ADD(NOW(6), INTERVAL 60 SECOND), NOW(6))`); err != nil {
		t.Fatal(err)
	}
	n, err := store.CommitYield(ctx, tasks.YieldCommit{
		TaskID: "yield", State: string(tasks.StateLeaseDegraded), Desired: tasks.TaskDesiredRun,
		Epoch: 3, ExpectEpoch: 3, UpdatedAt: time.Now(),
		RefusalNode: "worker-b", RefusalReason: "EPOCH_LOST", RefusalOpen: true,
		Release: true, ReleaseEpoch: 3,
	})
	if err != nil || n != 1 {
		t.Fatalf("CommitYield n=%d err=%v", n, err)
	}
	worker := ownershipWorker(t, store, "worker-b")
	claimed, err := worker.ClaimExpiredTasks()
	if err != nil {
		t.Fatal(err)
	}
	if claimed != 0 {
		t.Fatalf("claimed %d over an open refusal", claimed)
	}
	var epoch int64
	var owner string
	if err := store.db.QueryRowContext(ctx, `SELECT epoch, owner_worker_id FROM task_leases WHERE task_id = 'yield'`).Scan(&epoch, &owner); err != nil {
		t.Fatal(err)
	}
	if epoch != 3 || owner != "" {
		t.Fatalf("lease epoch=%d owner=%q, want 3 and empty", epoch, owner)
	}
}

func ownershipMixedVersion(t *testing.T, ctx context.Context, store *MySQLTaskStore) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO worker_registrations (worker_id, session_id, lease_expire_at, renewed_at, cluster_protocol_version)
VALUES ('old-proto', 'sess', DATE_ADD(NOW(6), INTERVAL 60 SECOND), NOW(6), 0)`); err != nil {
		t.Fatal(err)
	}
	worker := ownershipWorker(t, store, "worker-new")
	task, err := worker.CreateTask("mix", "mix-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatal(err)
	}
	err = worker.StartTask(task.ID)
	if err == nil || !strings.Contains(err.Error(), "mixed version") {
		t.Fatalf("StartTask err=%v, want mixed version", err)
	}
}

func ownershipStopExpired(t *testing.T, ctx context.Context, store *MySQLTaskStore, pending bool) {
	t.Helper()
	id := "stop-exp"
	if pending {
		id = "stop-del"
	}
	task := sampleTask(id, tasks.StateStopping)
	task.DesiredRun = tasks.TaskDesiredStop
	task.OwnerWorkerID = "worker-dead"
	task.Epoch = 4
	task.PendingDelete = pending
	if err := store.UpsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
VALUES (?, 'worker-dead', 4, DATE_ADD(NOW(6), INTERVAL -1 SECOND), NOW(6))`, id); err != nil {
		t.Fatal(err)
	}
	worker := ownershipWorker(t, store, "worker-b")
	if _, err := worker.ClaimExpiredTasks(); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTask(ctx, id)
	if pending {
		if !errors.Is(err, tasks.ErrTaskNotFound) {
			t.Fatalf("pending_delete GetTask err=%v state=%s", err, got.State)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.State != tasks.StateStopped || got.OwnerWorkerID != "" || got.Epoch != 0 || got.DesiredRun != tasks.TaskDesiredStop {
		t.Fatalf("expired stop = state %s owner %q epoch %d desired %s", got.State, got.OwnerWorkerID, got.Epoch, got.DesiredRun)
	}
}

type holdRunner struct{ release chan struct{} }

func (r *holdRunner) Run(context.Context, tasks.Task) error {
	<-r.release
	return nil
}

func ownershipStart(t *testing.T, ctx context.Context, store *MySQLTaskStore) {
	t.Helper()
	_ = ctx
	release := make(chan struct{})
	worker := ownershipWorker(t, store, "worker-start")
	worker.SetRunner(&holdRunner{release: release})
	task, err := worker.CreateTask("start-1", "start-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ConfigureSource(task.ID, tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"}); err != nil {
		t.Fatal(err)
	}
	if err := worker.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	before, err := worker.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.StartTask(task.ID); err != nil {
		t.Fatalf("idempotent Start: %v", err)
	}
	after, err := worker.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SpecRevision != before.SpecRevision || (after.State != tasks.StateStarting && after.State != tasks.StateRunning) {
		t.Fatalf("second Start spec %d->%d state %s", before.SpecRevision, after.SpecRevision, after.State)
	}
	if err := worker.StopTask(task.ID); err != nil {
		t.Fatal(err)
	}
	err = worker.StartTask(task.ID)
	var op *tasks.OperatorError
	if !errors.As(err, &op) || op.Status != 409 {
		t.Fatalf("Start on STOPPING err=%v, want 409", err)
	}
	close(release)
}

func ownershipWorker(t *testing.T, store *MySQLTaskStore, workerID string) *tasks.Scheduler {
	t.Helper()
	dir := t.TempDir()
	s := tasks.NewScheduler(
		tasks.WithStore(store),
		tasks.WithRunner(&holdRunner{release: make(chan struct{})}),
		tasks.WithClusterLeaseManager(NewLeaseStoreFromTaskStore(store)),
		tasks.WithClusterWorkerID(workerID),
		tasks.WithDataDir(dir),
		tasks.WithClusterGate(store),
		tasks.WithStorageMode("per_node"),
	)
	return s
}
