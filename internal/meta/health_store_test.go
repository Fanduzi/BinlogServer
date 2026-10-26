// Package meta provides module-level functionality for meta.
// input: sqlmock for the health statements; optionally BINLOG_TEST_MYSQL_ROOT_DSNS (comma-separated root DSNs ending in "/") for real MySQL 8.0, 5.7 and MariaDB
// output: assertions that a missing 000009 maps to healthstore.ErrSchemaAbsent without being remembered, the upsert dialect per server version, the healthstore contract on real servers, and 6/7/8 → latest migrations (through 000009) with down to 8 and up again
// pos: persistence coverage for ADR 0009 phase 2 (P2-2)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/healthstore/storetest"
	"binlog_server/internal/tasks"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func TestHealthStoreSchemaAbsentIsNotRemembered(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	for _, code := range []uint16{1146, 1054} {
		mock.ExpectQuery("FROM task_health").WillReturnError(&mysqlDriver.MySQLError{Number: code, Message: "absent"})
		if _, err := store.ListHealth(context.Background()); !errors.Is(err, healthstore.ErrSchemaAbsent) {
			t.Fatalf("%d: %v, want ErrSchemaAbsent", code, err)
		}
	}
	mock.ExpectQuery("FROM task_health").WillReturnRows(sqlmock.NewRows([]string{"task_id", "status", "reasons", "evaluated_at"}).
		AddRow("1", "HEALTHY", nil, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)))
	hs, err := store.ListHealth(context.Background())
	if err != nil || len(hs) != 1 || hs[0].Status != health.StatusHealthy {
		t.Fatalf("after the table appears the next call must work: %+v %v", hs, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertDialect(t *testing.T) {
	for v, want := range map[string]bool{"8.0.46": true, "8.0.20": true, "8.0.19": false, "8.4.3": true, "9.1.0": true,
		"5.7.44-log": false, "5.7.44-47": false, "11.4.4-MariaDB-ubu2404": false, "10.11.10-MariaDB": false} {
		if got := upsertWithAlias(v); got != want {
			t.Errorf("%s: alias=%v want %v", v, got, want)
		}
	}
}

func rootDSNs(t *testing.T) []string {
	raw := strings.TrimSpace(os.Getenv("BINLOG_TEST_MYSQL_ROOT_DSNS"))
	if raw == "" {
		t.Skip("BINLOG_TEST_MYSQL_ROOT_DSNS is not set")
	}
	return strings.Split(raw, ",")
}

func migrationsURL(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return "file://" + filepath.Join(filepath.Dir(file), "..", "..", "migrations")
}

// latestMigration returns the highest version among migrations/*.up.sql, so
// upgrade tests follow new migrations without editing a constant.
func latestMigration(t *testing.T) uint {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	ups, err := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "migrations", "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var latest uint
	for _, up := range ups {
		var v uint
		if _, err := fmt.Sscanf(filepath.Base(up), "%d_", &v); err != nil {
			t.Fatalf("migration %s: %v", up, err)
		}
		if v > latest {
			latest = v
		}
	}
	if latest < 9 {
		t.Fatalf("latest migration %d, want at least 000009", latest)
	}
	return latest
}

// scratchDB creates an empty database and returns its DSN and a migrator.
func scratchDB(t *testing.T, root, name string) (*sql.DB, *migrate.Migrate) {
	t.Helper()
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
	dsn := strings.TrimSuffix(root, "/") + "/" + name + "?parseTime=true&multiStatements=true"
	m, err := migrate.New(migrationsURL(t), "mysql://"+dsn)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", strings.TrimSuffix(root, "/")+"/"+name+"?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(64)
	t.Cleanup(func() { db.Close(); m.Close() })
	return db, m
}

func setLease(db *sql.DB) func(t *testing.T, taskID, owner string, epoch int64, expired bool) {
	return func(t *testing.T, taskID, owner string, epoch int64, expired bool) {
		t.Helper()
		if epoch == 0 {
			if _, err := db.Exec("DELETE FROM task_leases WHERE task_id = ?", taskID); err != nil {
				t.Fatal(err)
			}
			return
		}
		offset := "INTERVAL 60 SECOND"
		if expired {
			offset = "INTERVAL -1 SECOND"
		}
		_, err := db.Exec("INSERT INTO task_leases (task_id, owner_worker_id, epoch, lease_expire_at, renewed_at) VALUES (?, ?, ?, DATE_ADD(NOW(6), "+offset+"), NOW(6)) "+
			"ON DUPLICATE KEY UPDATE owner_worker_id = VALUES(owner_worker_id), epoch = VALUES(epoch), lease_expire_at = VALUES(lease_expire_at)", taskID, owner, epoch)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestHealthStoreContractOnMySQL(t *testing.T) {
	for i, root := range rootDSNs(t) {
		root := root
		t.Run(fmt.Sprintf("server%d", i), func(t *testing.T) {
			db, m := scratchDB(t, root, fmt.Sprintf("p22_contract_%d", i))
			if err := m.Up(); err != nil {
				t.Fatal(err)
			}
			installOwnershipGuards(t, db)
			store := newMySQLTaskStoreFromDB(db, 5*time.Second)
			storetest.Run(t, storetest.Harness{New: func(*testing.T) healthstore.Store { return store }, SetLease: setLease(db)})
		})
	}
}

func TestMigrateTo9FromEachVersion(t *testing.T) {
	ctx := context.Background()
	latest := latestMigration(t)
	for i, root := range rootDSNs(t) {
		for _, from := range []uint{6, 7, 8} {
			root, from := root, from
			t.Run(fmt.Sprintf("server%d/from%d", i, from), func(t *testing.T) {
				db, m := scratchDB(t, root, fmt.Sprintf("p22_mig_%d_%d", i, from))
				if err := m.Migrate(from); err != nil {
					t.Fatal(err)
				}
				store := newMySQLTaskStoreFromDB(db, 5*time.Second)
				at := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
				legacy := tasks.Finding{TaskID: "legacy", Kind: tasks.FindingEventPersistFailed, Subject: "TASK_CREATED", Severity: tasks.FindingWarning, Detail: "v0.5.66", FirstSeen: at, LastSeen: at, Count: 3}
				if from >= 7 {
					if err := store.UpsertFinding(ctx, legacy); err != nil {
						t.Fatal(err)
					}
				}
				f := health.Fence{Mode: health.FenceStandalone, Node: "n1", Instance: "i1"}
				k := health.Key{TaskID: "t", Kind: health.SegmentOverlap, Subject: "a"}
				tr := health.Transition{Key: k, Op: health.OpOpen, At: at, FactAt: at, Instance: health.Instance{Key: k, Severity: health.Critical, Segments: []string{"a"}}}
				if _, err := store.ApplyTransitions(ctx, "t", f, []health.Transition{tr}, health.TaskHealth{TaskID: "t", Status: health.StatusUnhealthy}); !errors.Is(err, healthstore.ErrSchemaAbsent) {
					t.Fatalf("before 000009: %v, want ErrSchemaAbsent", err)
				}
				if err := m.Up(); err != nil {
					t.Fatal(err)
				}
				installOwnershipGuards(t, db)
				if v, dirty, _ := m.Version(); v != latest || dirty {
					t.Fatalf("version %d dirty %v, want %d", v, dirty, latest)
				}
				if _, err := store.ApplyTransitions(ctx, "t", f, []health.Transition{tr}, health.TaskHealth{TaskID: "t", Status: health.StatusUnhealthy}); err != nil {
					t.Fatalf("after 000009 the same store works without restart: %v", err)
				}
				if from >= 7 {
					old, err := store.ListOpenFindingsForTask(ctx, "legacy")
					if err != nil || len(old) != 1 || old[0].Generation != 0 || old[0].Detail != "v0.5.66" {
						t.Fatalf("v0.5.66 row after upgrade %+v %v", old, err)
					}
				}
				legacy.Count = 4
				if err := store.UpsertFinding(ctx, legacy); err != nil {
					t.Fatalf("v0.5.66 upsert on schema %d: %v", latest, err)
				}
				if err := m.Migrate(8); err != nil {
					t.Fatal(err)
				}
				if v, dirty, _ := m.Version(); v != 8 || dirty {
					t.Fatalf("after down: version %d dirty %v, want 8", v, dirty)
				}
				if err := store.UpsertFinding(ctx, legacy); err != nil {
					t.Fatalf("v0.5.66 upsert after down to 8: %v", err)
				}
				if got, err := store.ListFindings(ctx, "legacy", false, 10); err != nil || len(got) != 1 {
					t.Fatalf("v0.5.66 read after down %+v %v", got, err)
				}
				if _, err := store.ApplyTransitions(ctx, "t", f, nil, health.TaskHealth{TaskID: "t", Status: health.StatusUnhealthy}); !errors.Is(err, healthstore.ErrSchemaAbsent) {
					t.Fatalf("after down: %v, want ErrSchemaAbsent", err)
				}
				if err := m.Up(); err != nil {
					t.Fatal(err)
				}
				installOwnershipGuards(t, db)
				if _, err := store.ApplyTransitions(ctx, "t", f, []health.Transition{tr}, health.TaskHealth{TaskID: "t", Status: health.StatusUnhealthy}); err != nil {
					t.Fatalf("after up again (re-probe, nothing remembered): %v", err)
				}
			})
		}
	}
}

func TestRehashCursorOnMySQL(t *testing.T) {
	ctx := context.Background()
	for i, root := range rootDSNs(t) {
		db, m := scratchDB(t, root, fmt.Sprintf("p23_rehash_%d", i))
		if err := m.Up(); err != nil {
			t.Fatal(err)
		}
		installOwnershipGuards(t, db)
		store := newMySQLTaskStoreFromDB(db, 5*time.Second)
		if task, seg, err := store.LoadRehashCursor(ctx); err != nil || task != "" || seg != "" {
			t.Fatalf("empty cursor %q %q %v", task, seg, err)
		}
		for _, c := range [][2]string{{"1", "mysql-bin.000001"}, {"2", "mysql-bin.000009"}} {
			if err := store.SaveRehashCursor(ctx, c[0], c[1]); err != nil {
				t.Fatal(err)
			}
			if task, seg, err := store.LoadRehashCursor(ctx); err != nil || task != c[0] || seg != c[1] {
				t.Fatalf("cursor %q %q %v, want %v", task, seg, err, c)
			}
		}
	}
}
