// Package meta provides module-level functionality for meta.
// input: BINLOG_TEST_MYSQL_ROOT_DSNS, one scratch database per server; BINLOG_TEST_NOPROCESS_DSN for an account granted only that database
// output: proof that a newly opened pool connection carries the session-kill timeouts and that a sleeping FOR UPDATE holder is dropped inside the bound; proof that META_LOCK_STALLED still opens from server error 1205 when that account has no PROCESS privilege
// pos: ADR 0011 PR-2 e2e cell for MySQL 8 and MariaDB; skipped when the DSN list is unset
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/ownership"
	"binlog_server/internal/tasks"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func TestSessionKillDropsSleepingForUpdate(t *testing.T) {
	roots := rootDSNs(t)
	for i, root := range roots {
		root := strings.TrimSpace(root)
		t.Run(fmt.Sprintf("server-%d", i), func(t *testing.T) {
			name := fmt.Sprintf("binlog_sk_%d", time.Now().UnixNano())
			admin, err := sql.Open("mysql", root)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name)
			})
			dsn := strings.TrimSuffix(root, "/") + "/" + name + "?parseTime=true&multiStatements=true"
			m, err := migrate.New(migrationsURL(t), "mysql://"+dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Up(); err != nil {
				t.Fatal(err)
			}
			store, err := OpenClusterMeta(dsn, 15*time.Second, "", 4*time.Second, "off")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.db.Close() })
			store.db.SetMaxIdleConns(0)
			ctx := context.Background()
			var version string
			if err := store.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
				t.Fatal(err)
			}
			assertSessionTimeouts(t, store.db, strings.Contains(strings.ToLower(version), "mariadb"))
			ls := NewLeaseStoreFromTaskStore(store)
			if _, _, err := ls.Acquire(ctx, "t-sk", "w1", 4*time.Second); err != nil {
				t.Fatal(err)
			}
			if degraded, detail := store.SessionDegraded(); degraded {
				t.Fatalf("direct connection degraded: %s", detail)
			}
			hold, err := store.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Close()
			tx, err := hold.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			var owner string
			if err := tx.QueryRowContext(ctx, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", "t-sk").Scan(&owner); err != nil {
				t.Fatal(err)
			}
			time.Sleep(1500 * time.Millisecond)
			waitCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			peer, err := store.db.Conn(waitCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			// The pool's lock wait is under the client deadline. This peer has to
			// outlast the holder's session kill, so this one connection waits longer.
			if _, err := peer.ExecContext(waitCtx, "SET SESSION innodb_lock_wait_timeout = 8"); err != nil {
				t.Fatal(err)
			}
			ptx, err := peer.BeginTx(waitCtx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ptx.Rollback() }()
			if err := ptx.QueryRowContext(waitCtx, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", "t-sk").Scan(&owner); err != nil {
				t.Fatalf("peer did not take the row inside the bound: %v", err)
			}
			_ = tx.Rollback()
		})
	}
}

func assertSessionTimeouts(t *testing.T, db *sql.DB, mariadb bool) {
	t.Helper()
	ctx := context.Background()
	want := map[string]int{
		"wait_timeout":             2,
		"net_read_timeout":         2,
		"net_write_timeout":        2,
		"innodb_lock_wait_timeout": 1,
	}
	if mariadb {
		want["idle_transaction_timeout"] = 2
	}
	for name, expect := range want {
		var got int
		if err := db.QueryRowContext(ctx, "SELECT @@session."+name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != expect {
			t.Fatalf("@@session.%s=%d, want %d", name, got, expect)
		}
	}
}

// TestMetaLockStallWithoutProcess is the least-privilege cell. The smoke
// script creates the account with privileges on the meta database only.
// The child holds task_leases and is SIGSTOPped. Two health writes must
// come back as server 1205, and the second observation opens META_LOCK_STALLED.
func TestMetaLockStallWithoutProcess(t *testing.T) {
	if os.Getenv("BINLOG_TEST_HOLD_CHILD") == "1" {
		if err := holdStoppedLease(os.Getenv("BINLOG_TEST_NOPROCESS_DSN"), os.Getenv("BINLOG_TEST_HOLD_READY")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	dsn := strings.TrimSpace(os.Getenv("BINLOG_TEST_NOPROCESS_DSN"))
	if dsn == "" {
		t.Skip("BINLOG_TEST_NOPROCESS_DSN is not set")
	}
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	store, err := OpenClusterMeta(dsn, 15*time.Second, "", 4*time.Second, "off")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	if store.LockDetail() != "reduced" {
		t.Fatalf("lock detail %q, want reduced", store.LockDetail())
	}
	if ok, detail := store.SessionDegraded(); ok {
		t.Fatalf("startup degraded: %s", detail)
	}
	if !strings.Contains(logs.String(), "lock holder name is omitted") {
		t.Fatalf("startup warning missing: %s", logs.String())
	}
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at)
		VALUES ('t-noproc', 'w-hold', 1, DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 1 HOUR), UTC_TIMESTAMP(6))`); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "holding")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMetaLockStallWithoutProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "BINLOG_TEST_HOLD_CHILD=1", "BINLOG_TEST_HOLD_READY="+ready)
	childLog, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer childLog.Close()
	cmd.Stdout = childLog
	cmd.Stderr = childLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGCONT)
			_ = cmd.Process.Kill()
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-done:
			body, _ := os.ReadFile(childLog.Name())
			t.Fatalf("holder exited: %s", body)
		default:
		}
		if time.Now().After(deadline) {
			body, _ := os.ReadFile(childLog.Name())
			t.Fatalf("holder did not lock the row: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	fence := health.Fence{Mode: health.FenceCluster, Node: "n1", Instance: "i", TaskEpoch: 1}
	for i := 0; i < 2; i++ {
		_, err := store.ApplyTransitions(context.Background(), "t-noproc", fence, nil, health.TaskHealth{TaskID: "t-noproc"})
		if !errors.Is(err, healthstore.ErrLockWait) {
			t.Fatalf("observation %d: %v, want ErrLockWait", i+1, err)
		}
		var me *mysqlDriver.MySQLError
		if !errors.As(err, &me) || me.Number != 1205 {
			t.Fatalf("observation %d: %v, want server 1205", i+1, err)
		}
	}
	sched := tasks.NewScheduler()
	now := time.Now()
	p := ownership.DefaultParams()
	p.LockWait = time.Second
	sched.NoteMetaLockTimeout("t-noproc", "n1", now, p)
	if opened := sched.NoteMetaLockTimeout("t-noproc", "n1", now.Add(time.Millisecond), p); !opened {
		t.Fatal("second observation did not open META_LOCK_STALLED")
	}
	sum := sched.HealthSummary(context.Background())
	if sum.Status != tasks.HealthDegraded {
		t.Fatalf("status %s, want degraded", sum.Status)
	}
	found := false
	for _, reason := range sum.Reasons {
		if strings.Contains(reason, "META_LOCK_STALLED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons %v, want META_LOCK_STALLED", sum.Reasons)
	}
}

func holdStoppedLease(dsn, ready string) error {
	store, err := OpenClusterMeta(dsn, 15*time.Second, "", 4*time.Second, "off")
	if err != nil {
		return err
	}
	defer store.db.Close()
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Two observer lock waits outlast the armed idle kill. This connection
	// stays up so both 1205s still see the row.
	if _, err := conn.ExecContext(ctx, "SET SESSION wait_timeout = 60"); err != nil {
		return err
	}
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(version), "mariadb") {
		if _, err := conn.ExecContext(ctx, "SET SESSION idle_transaction_timeout = 60"); err != nil {
			return err
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", "t-noproc").Scan(&owner); err != nil {
		return err
	}
	if err := os.WriteFile(ready, []byte("HOLDING"), 0o644); err != nil {
		return err
	}
	for {
		time.Sleep(time.Hour)
	}
}
