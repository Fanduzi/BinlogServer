// Package meta provides module-level functionality for meta.
// input: a session-kill bound and the side-kill sources
// output: proof that a mismatched or failed wait_timeout readback is degraded, a missing procedure stays off, the client sends no kill statement, idle pool time stays under wait_timeout, and a sleeping lease holder is matched by exact task id
// pos: ADR 0011 PR-2 P2-8 and P2-9 without a server
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSessionTimeoutMismatchIsDegraded(t *testing.T) {
	st := &sessionState{expectSec: 5}
	st.noteTimeout(28800)
	store := &MySQLTaskStore{session: st}
	ok, detail := store.SessionDegraded()
	if !ok || !strings.Contains(detail, "28800") {
		t.Fatalf("degraded %v %q", ok, detail)
	}
	st.noteTimeout(5)
	ok, detail = store.SessionDegraded()
	if ok || detail != "" {
		t.Fatalf("match stayed degraded %v %q", ok, detail)
	}
}

func TestSideKillStaysOffWhenProcedureMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{mode: "auto", bound: time.Second, flavor: "mysql", armed: "off"}
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM information_schema.routines").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	store.RefreshSideKill(context.Background(), "n1")
	if store.SideKill() != "off" {
		t.Fatalf("side_kill %s, want off", store.SideKill())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSideKillOffDoesNotQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{mode: "off", bound: time.Second, flavor: "mysql"}
	store.RefreshSideKill(context.Background(), "n1")
	if store.SideKill() != "off" {
		t.Fatalf("side_kill %s", store.SideKill())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPoolIdleClosesBeforeWaitTimeout(t *testing.T) {
	for _, bound := range []time.Duration{5 * time.Second, 2 * time.Second, time.Second} {
		idle := poolIdleBeforeWait(bound)
		if idle <= 0 || idle >= bound {
			t.Fatalf("bound %s idle %s", bound, idle)
		}
		if bound >= 3*time.Second && 2*idle >= bound {
			t.Fatalf("bound %s idle %s can outlive wait_timeout", bound, idle)
		}
	}
}

func TestLeaseReadbackStaysOnLeaseTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewLeaseStore(db)
	store.session = &sessionState{expectSec: 5}
	now := time.Unix(1735689600, 0).UTC()
	ttl := 15 * time.Second
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(acquireLeaseSQL)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(getLeaseSQL)).
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "owner_worker_id", "epoch", "lease_expire_at", "renewed_at",
		}).AddRow("task-1", "worker-a", int64(7), now.Add(ttl), now))
	mock.ExpectQuery(regexp.QuoteMeta(currentDBTimeSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"NOW(6)"}).AddRow(now))
	mock.ExpectQuery("SELECT @@session.wait_timeout").
		WillReturnError(errors.New("read failed"))
	mock.ExpectCommit()
	epoch, ok, err := store.Acquire(context.Background(), "task-1", "worker-a", ttl)
	if err != nil || !ok || epoch != 7 {
		t.Fatalf("acquire epoch=%d ok=%v err=%v", epoch, ok, err)
	}
	taskStore := &MySQLTaskStore{session: store.session}
	degraded, detail := taskStore.SessionDegraded()
	if !degraded || !strings.Contains(detail, "read failed") {
		t.Fatalf("degraded %v %q", degraded, detail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSleepingLeaseHolderMatchesExactTask(t *testing.T) {
	if strings.Contains(mysqlSleepingLeaseSQL, "LIKE ?") || strings.Contains(mariaSleepingLeaseSQL, "LIKE ?") {
		t.Fatal("task id must not be a LIKE pattern")
	}
	if !strings.Contains(mysqlSleepingLeaseSQL, "LOCK_DATA = ?") || !strings.Contains(mariaSleepingLeaseSQL, "lock_data = ?") {
		t.Fatal("both flavors must filter the lock by the task id")
	}
	for _, flavor := range []string{"mysql", "mariadb"} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		store := newMySQLTaskStoreFromDB(db, time.Second)
		store.session = &sessionState{flavor: flavor}
		mock.ExpectQuery("trx_id").WithArgs("t1", "t1").
			WillReturnRows(sqlmock.NewRows([]string{"trx_id"}).AddRow("99"))
		id, ok := store.sleepingLeaseHolder(context.Background(), "t1")
		if !ok || id != "99" {
			t.Fatalf("%s holder %q %v", flavor, id, ok)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestSideKillClientDoesNotSendKill(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	killWord := regexp.MustCompile(`(?i)\bKILL\s`)
	for _, name := range []string{"side_kill.go", "session_kill.go"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		if killWord.MatchString(b.String()) {
			t.Fatalf("%s contains a kill statement", name)
		}
	}
}
