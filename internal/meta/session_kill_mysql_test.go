// Package meta provides module-level functionality for meta.
// input: BINLOG_TEST_MYSQL_ROOT_DSNS, one scratch database per server
// output: proof that a newly opened pool connection carries the session-kill timeouts and that a sleeping FOR UPDATE holder is dropped inside the bound
// pos: ADR 0011 PR-2 e2e cell for MySQL 8 and MariaDB; skipped when the DSN list is unset
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

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
	vars := []string{"wait_timeout", "net_read_timeout", "net_write_timeout", "innodb_lock_wait_timeout"}
	if mariadb {
		vars = append(vars, "idle_transaction_timeout")
	}
	for _, name := range vars {
		var got int
		if err := db.QueryRowContext(ctx, "SELECT @@session."+name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 2 {
			t.Fatalf("@@session.%s=%d, want 2", name, got)
		}
	}
}
