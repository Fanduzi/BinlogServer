// Package meta provides module-level functionality for meta.
// input: BINLOG_TEST_MYSQL_ROOT_DSNS and QA_PRE_HOLD_MS (default 1500); one scratch database per server
// output: proof that a health-row pre-hold followed by a stalled lease holder ends as ErrLockWait, either server error 1205 or classified by elapsed time, and that a plain context deadline is not accepted; 40 health writes on a 32-connection pool all come back as ErrLockWait
// pos: ADR 0011 PR-2 deadline margin, the QA repro for one client deadline around the whole write
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestQA484DeadlineMargin(t *testing.T) {
	pre := qaPreHold(t)
	for i, root := range rootDSNs(t) {
		root := root
		t.Run(fmt.Sprintf("server-%d", i), func(t *testing.T) {
			qaDeadlineMargin(t, strings.TrimSpace(root), i, pre)
		})
	}
}

func TestQA484PoolPressure(t *testing.T) {
	for i, root := range rootDSNs(t) {
		root := root
		t.Run(fmt.Sprintf("server-%d", i), func(t *testing.T) {
			qaPoolPressure(t, strings.TrimSpace(root), i)
		})
	}
}

func qaPreHold(t *testing.T) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("QA_PRE_HOLD_MS"))
	if raw == "" {
		return 1500 * time.Millisecond
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		t.Fatalf("QA_PRE_HOLD_MS %q", raw)
	}
	return time.Duration(ms) * time.Millisecond
}

func qaDeadlineMargin(t *testing.T, root string, i int, pre time.Duration) {
	t.Helper()
	name := fmt.Sprintf("qa484m%d", i)
	db, m := scratchDB(t, root, name)
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	taskID := "t-qa-margin"
	if _, err := db.Exec(`INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
		VALUES (?, 'n1', 1, DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 1 HOUR), UTC_TIMESTAMP(6))`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_health (task_id, status, evaluated_at) VALUES (?, '', UTC_TIMESTAMP(6))`, taskID); err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimRight(root, "/") + "/" + name + "?parseTime=true"
	store, err := OpenClusterMeta(dsn, 15*time.Second, "", 15*time.Second, "off")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.db.Stats().MaxOpenConnections != 32 {
		t.Fatalf("pool max %d, want 32", store.db.Stats().MaxOpenConnections)
	}
	releaseLease := lockRow(t, dsn, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", taskID)
	defer releaseLease()
	releaseHealth := lockRowFor(t, dsn, "SELECT task_id FROM task_health WHERE task_id = ? FOR UPDATE", taskID, pre)
	defer releaseHealth()
	fence := health.Fence{Mode: health.FenceCluster, Node: "n1", Instance: "i", TaskEpoch: 1}
	start := time.Now()
	_, err = store.ApplyTransitions(context.Background(), taskID, fence, nil, health.TaskHealth{TaskID: taskID})
	elapsed := time.Since(start)
	var me *mysqlDriver.MySQLError
	server1205 := errors.As(err, &me) && me.Number == 1205
	classified := errors.Is(err, errClassifiedByElapsed)
	if !errors.Is(err, healthstore.ErrLockWait) || (!server1205 && !classified) {
		t.Fatalf("after %s (pre-hold %s): %v, want ErrLockWait from server 1205 or elapsed time", elapsed, pre, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait also matches a plain client deadline after %s: %v", elapsed, err)
	}
	if elapsed < 4*time.Second-500*time.Millisecond {
		t.Fatalf("finished in %s, the server lock timer did not run", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("finished in %s, the outer backstop fired", elapsed)
	}
}

func qaPoolPressure(t *testing.T, root string, i int) {
	t.Helper()
	name := fmt.Sprintf("qa484p%d", i)
	db, m := scratchDB(t, root, name)
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	taskID := "t-qa-pool"
	if _, err := db.Exec(`INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
		VALUES (?, 'n1', 1, DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 1 HOUR), UTC_TIMESTAMP(6))`, taskID); err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimRight(root, "/") + "/" + name + "?parseTime=true"
	store, err := OpenClusterMeta(dsn, 15*time.Second, "", 15*time.Second, "off")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.db.Stats().MaxOpenConnections != 32 {
		t.Fatalf("pool max %d, want 32", store.db.Stats().MaxOpenConnections)
	}
	release := lockRow(t, dsn, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", taskID)
	defer release()
	const n = 40
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	fence := health.Fence{Mode: health.FenceCluster, Node: "n1", Instance: "i", TaskEpoch: 1}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = store.ApplyTransitions(context.Background(), taskID, fence, nil, health.TaskHealth{TaskID: taskID})
		}(i)
	}
	close(start)
	wg.Wait()
	locked := 0
	for i, err := range errs {
		if !errors.Is(err, healthstore.ErrLockWait) {
			t.Fatalf("write %d: %v, want ErrLockWait", i, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("write %d is an unconfirmed deadline: %v", i, err)
		}
		var me *mysqlDriver.MySQLError
		if errors.As(err, &me) && me.Number == 1205 {
			locked++
		}
	}
	if locked == 0 {
		t.Fatal("no write returned server 1205")
	}
}

func lockRow(t *testing.T, dsn, query, taskID string) func() {
	t.Helper()
	return lockRowFor(t, dsn, query, taskID, 0)
}

func lockRowFor(t *testing.T, dsn, query, taskID string, hold time.Duration) func() {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := armLongSession(conn); err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	var sink string
	if err := tx.QueryRowContext(ctx, query, taskID).Scan(&sink); err != nil {
		_ = tx.Rollback()
		_ = conn.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	release := func() {
		_ = tx.Rollback()
		_ = conn.Close()
		_ = db.Close()
	}
	if hold <= 0 {
		return release
	}
	timer := time.AfterFunc(hold, release)
	return func() {
		if timer.Stop() {
			release()
		}
	}
}

func armLongSession(conn *sql.Conn) error {
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "SET SESSION wait_timeout = 600"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 50"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION idle_transaction_timeout = 600"); err != nil {
		var me *mysqlDriver.MySQLError
		if !errors.As(err, &me) || me.Number != 1193 {
			return err
		}
	}
	return nil
}
