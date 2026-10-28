// Package meta provides module-level functionality for meta.
// input: sqlmock for pool checkout, side_kill probes, naming-view denials, and startup session readback; BINLOG_TEST_MYSQL_ROOT_DSNS for a SET the server rejects
// output: proof that pool starvation is a lock stall only with lock evidence, a changed second probe does not call the procedure, side_kill stays off while off and is unavailable when auto cannot name a holder, the privilege probe reads a row so MariaDB applies the PROCESS check, startup readback degrades before any lease write, and a rejected session SET refuses startup
// pos: ADR 0011 PR-2 deadline margin, pool checkout, and the privilege probe
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/healthstore"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestStallFromPoolWait(t *testing.T) {
	cases := []struct {
		saturated, confirmed, exceeded, want bool
	}{
		{true, true, false, true},
		{true, false, true, true},
		{true, true, true, true},
		{true, false, false, false},
		{false, true, true, false},
		{false, false, false, false},
	}
	for _, tc := range cases {
		got, reason := stallFromPoolWait(tc.saturated, tc.confirmed, tc.exceeded)
		if got != tc.want {
			t.Fatalf("saturated=%v confirmed=%v exceeded=%v got %v, want %v", tc.saturated, tc.confirmed, tc.exceeded, got, tc.want)
		}
		if tc.want && reason != poolStarvationReason {
			t.Fatalf("reason %q", reason)
		}
		if !tc.want && reason != "" {
			t.Fatalf("unconfirmed reason %q", reason)
		}
	}
}

func TestPoolStarvedNeedsLockEvidence(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	hold, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: time.Second, lockWaitSec: 1}
	if store.poolStarved() {
		t.Fatal("a full pool with no in-flight lock statement is a stall")
	}
	mark := store.flight.begin()
	if store.poolStarved() {
		t.Fatal("a lock statement inside its budget, and not confirmed, is a stall")
	}
	mark.confirmed.Store(true)
	if !store.poolStarved() {
		t.Fatal("a confirmed lock wait on a full pool was not a stall")
	}
	mark.confirmed.Store(false)
	mark.at = time.Now().Add(-3 * time.Second)
	if !store.poolStarved() {
		t.Fatal("a statement past its bound on a full pool was not a stall")
	}
	store.flight.end(mark)
	db.SetMaxOpenConns(2)
	mark = store.flight.begin()
	mark.confirmed.Store(true)
	defer store.flight.end(mark)
	if store.poolStarved() {
		t.Fatal("a pool with a free slot was a stall")
	}
}

func TestAcquireConnStarvationIsLockWait(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	hold, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: time.Second, lockWaitSec: 1}
	mark := store.flight.begin()
	mark.confirmed.Store(true)
	defer store.flight.end(mark)
	_, err = store.acquireConn(context.Background())
	if !errors.Is(err, healthstore.ErrLockWait) || !strings.Contains(err.Error(), poolStarvationReason) {
		t.Fatalf("starvation: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("starvation also matches a client deadline: %v", err)
	}
}

func TestAcquireConnSlowPoolStaysUnconfirmed(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	hold, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: time.Second, lockWaitSec: 1}
	_, err = store.acquireConn(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, healthstore.ErrLockWait) {
		t.Fatalf("slow pool: %v", err)
	}
}

func TestSideKillSecondProbeMustMatch(t *testing.T) {
	var got []string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		got = append(got, actual)
		if expected == "EXTRA" || strings.Contains(actual, expected) {
			return nil
		}
		return fmt.Errorf("%s not in %s", expected, actual)
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: time.Millisecond, flavor: "mysql", armed: "on"}
	store.sideKillRun = func(fn func()) { fn() }
	mock.ExpectQuery("trx_id").WillReturnRows(sqlmock.NewRows([]string{"trx_id"}).AddRow("9"))
	mock.ExpectQuery("trx_id").WillReturnRows(sqlmock.NewRows([]string{"trx_id"}).AddRow("8"))
	mock.ExpectExec("EXTRA").WillReturnResult(sqlmock.NewResult(0, 0))
	store.SideKillStalled(context.Background(), "t1")
	for _, q := range got {
		if strings.Contains(q, "binlog_server_kill_trx") || strings.Contains(strings.ToUpper(q), "CALL") {
			t.Fatalf("second probe changed and the procedure was still called: %s", q)
		}
	}
}

func TestSideKillStalledStaysOff(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: time.Millisecond, flavor: "mysql", armed: "off"}
	store.sideKillRun = func(fn func()) {
		t.Fatal("side_kill ran while off")
	}
	store.SideKillStalled(context.Background(), "t1")
	if store.SideKill() != "off" {
		t.Fatalf("side_kill %s", store.SideKill())
	}
}

func TestDrillStaysOffWithoutProcess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{mode: "auto", bound: time.Millisecond, flavor: "mysql", armed: "off"}
	denied := &mysqlDriver.MySQLError{Number: 1227, Message: "Access denied to user processor on qa_process_db"}
	mock.ExpectQuery("routines").WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery("tables").WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery("ENGINE_TRANSACTION_ID").WillReturnError(denied)
	mock.ExpectQuery("innodb_lock_waits").WillReturnError(&mysqlDriver.MySQLError{Number: 1142, Message: "SELECT denied on qa_process_db"})
	mock.ExpectExec("ENGINE_TRANSACTION_ID").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectBegin()
	mock.ExpectExec("meta_canary").WillReturnResult(sqlmock.NewResult(0, 1))
	store.RefreshSideKill(context.Background(), "n1")
	if store.SideKill() != "unavailable" {
		t.Fatalf("side_kill %s, want unavailable", store.SideKill())
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("the canary ran without a naming view")
	}
}

func TestPrivilegeProbeDoesNotMatchGrantText(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	for _, name := range []string{"side_kill.go", "session_kill.go", "health_store.go"} {
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
		if strings.Contains(b.String(), "SHOW GRANTS") {
			t.Fatalf("%s still matches grant text", name)
		}
	}
}

func TestPrivilegeProbeReadsARow(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	checks := []struct{ file, fn string }{
		{"session_kill.go", "func (s *MySQLTaskStore) lockTablesReadable"},
		{"side_kill.go", "func (s *MySQLTaskStore) canSeeOtherLocks"},
		{"side_kill.go", "func (s *MySQLTaskStore) canReadDataLocks"},
	}
	for _, c := range checks {
		raw, err := os.ReadFile(filepath.Join(dir, c.file))
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
		text := b.String()
		i := strings.Index(text, c.fn)
		if i < 0 {
			t.Fatalf("%s missing %s", c.file, c.fn)
		}
		rest := text[i:]
		if j := strings.Index(rest[len(c.fn):], "\nfunc "); j >= 0 {
			rest = rest[:len(c.fn)+j]
		}
		if strings.Contains(rest, "LIMIT 0") {
			t.Fatalf("%s %s uses LIMIT 0, which does not fill information_schema", c.file, c.fn)
		}
		if !strings.Contains(rest, "LIMIT 1") {
			t.Fatalf("%s %s does not read a row", c.file, c.fn)
		}
	}
}

func TestNamingQueryDenialStaysReduced(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	processor := &mysqlDriver.MySQLError{Number: 1227, Message: "Access denied for user 'processor'@'%'"}
	processDB := &mysqlDriver.MySQLError{Number: 1142, Message: "SELECT command denied to user for table in qa_process_db"}
	mock.ExpectQuery("data_lock_waits").WillReturnError(processor)
	mock.ExpectQuery("innodb_lock_waits").WillReturnError(processDB)
	if store.lockTablesReadable(context.Background()) {
		t.Fatal("1227/1142 counted as a naming view, for a user named processor and a database named qa_process_db")
	}
	mock.ExpectQuery("data_lock_waits").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if !store.lockTablesReadable(context.Background()) {
		t.Fatal("a naming query that answers is not full")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("data_locks").WillReturnError(processor)
	mock.ExpectQuery("innodb_lock_waits").WillReturnError(processDB)
	if store.canSeeOtherLocks(context.Background()) {
		t.Fatal("a denial whose text contains processor counted as a privilege")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStartupReadbackMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{expectSec: 5, lockWaitSec: 4, flavor: "mariadb"}
	expectSession := func(wait, netRead, netWrite, lock, idle int, failIdle bool) {
		mock.ExpectQuery("SELECT @@session.wait_timeout").WillReturnRows(sqlmock.NewRows([]string{"t"}).AddRow(wait))
		mock.ExpectQuery("SELECT @@session.net_read_timeout").WillReturnRows(sqlmock.NewRows([]string{"t"}).AddRow(netRead))
		mock.ExpectQuery("SELECT @@session.net_write_timeout").WillReturnRows(sqlmock.NewRows([]string{"t"}).AddRow(netWrite))
		mock.ExpectQuery("SELECT @@session.innodb_lock_wait_timeout").WillReturnRows(sqlmock.NewRows([]string{"t"}).AddRow(lock))
		q := mock.ExpectQuery("SELECT @@session.idle_transaction_timeout")
		if failIdle {
			q.WillReturnError(errors.New("stripped"))
			return
		}
		q.WillReturnRows(sqlmock.NewRows([]string{"t"}).AddRow(idle))
	}
	expectSession(5, 5, 5, 4, 28800, false)
	store.readArmedOnFreshConn(context.Background())
	ok, detail := store.SessionDegraded()
	if !ok || !strings.Contains(detail, "idle_transaction_timeout") || !strings.Contains(detail, "28800") {
		t.Fatalf("startup mismatch %v %q", ok, detail)
	}
	expectSession(5, 5, 5, 4, 5, false)
	store.readArmedOnFreshConn(context.Background())
	ok, detail = store.SessionDegraded()
	if ok || detail != "" {
		t.Fatalf("a matching readback stayed degraded %v %q", ok, detail)
	}
	expectSession(5, 5, 5, 4, 0, true)
	store.readArmedOnFreshConn(context.Background())
	ok, detail = store.SessionDegraded()
	if !ok || !strings.Contains(detail, "read failed") {
		t.Fatalf("failed startup readback %v %q", ok, detail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClusterPoolUsesTheIdleSettings(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "session_kill.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, needle := range []string{
		"db.SetMaxIdleConns(poolMaxIdle(bound))",
		"db.SetConnMaxIdleTime(poolIdleBeforeWait(bound))",
		"store.readArmedOnFreshConn(rctx)",
		`return nil, fmt.Errorf("session timeout: %w", err)`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("OpenClusterMeta is missing %s", needle)
		}
	}
}

func TestRejectedSetRefusesStartup(t *testing.T) {
	roots := rootDSNs(t)
	prev := testArmExtra
	t.Cleanup(func() { testArmExtra = prev })
	testArmExtra = func(cfg *mysqlDriver.Config) {
		if cfg.Params == nil {
			cfg.Params = map[string]string{}
		}
		cfg.Params["binlog_server_not_a_variable"] = "1"
	}
	for i, root := range roots {
		dsn := strings.TrimRight(strings.TrimSpace(root), "/") + "/?parseTime=true"
		store, err := OpenClusterMeta(dsn, 5*time.Second, "", 15*time.Second, "off")
		if store != nil {
			_ = store.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "session timeout") {
			t.Fatalf("server %d: rejected SET err %v", i, err)
		}
	}
}
