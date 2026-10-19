// Package meta provides module-level functionality for meta.
// input: sqlmock with a recording query matcher, one standalone OPEN and then CLOSE of META_UNREACHABLE whose fact times are the outage start and the first answer
// output: proof that the health store stamps task_findings, finding_transitions and task_health with UTC_TIMESTAMP(6) (never the session-time-zone NOW(6)), writes first_seen from the OPEN's FactAt and resolved_at from the CLOSE's FactAt, and that StoreNow reads UTC_TIMESTAMP(6) (§28)
// pos: ADR 0009 phase 2, docs/design/0009-phase2-health-evaluator.md §25.8 R3 (#427 QA r1)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql/driver"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"

	"github.com/DATA-DOG/go-sqlmock"
)

// capture matches any argument and keeps it.
type capture struct {
	mu  *sync.Mutex
	got *[]driver.Value
}

func (c capture) Match(v driver.Value) bool {
	c.mu.Lock()
	*c.got = append(*c.got, v)
	c.mu.Unlock()
	return true
}

func captures(n int, mu *sync.Mutex, got *[]driver.Value) []driver.Value {
	out := make([]driver.Value, n)
	for i := range out {
		out[i] = capture{mu: mu, got: got}
	}
	return out
}

func TestHealthStoreStampsUTCAndFactTimes(t *testing.T) {
	var mu sync.Mutex
	var statements []string
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		mu.Lock()
		statements = append(statements, actual)
		mu.Unlock()
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	fence := health.Fence{Mode: health.FenceStandalone, Node: "standalone", Instance: "i1"}
	outage := time.Date(2026, 10, 19, 1, 7, 25, 0, time.FixedZone("UTC+8", 8*3600)) // a local-zone fact time
	answered := outage.Add(30 * time.Second)
	key := health.Key{TaskID: "_cluster", Kind: health.MetaUnreachable, Subject: "standalone"}
	inst := health.Instance{Key: key, Severity: health.Critical, Detail: "meta away"}

	var openArgs, closeArgs []driver.Value
	mock.ExpectQuery(regexp.QuoteMeta("SELECT VERSION()")).WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("8.0.36"))
	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT fence_mode").WillReturnRows(sqlmock.NewRows([]string{"m", "e", "n", "i", "g", "c"}).AddRow("", 0, "", "", 0, ""))
	mock.ExpectQuery("FROM finding_transitions").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectExec("INSERT INTO finding_transitions").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO task_findings").WithArgs(captures(10, &mu, &openArgs)...).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE task_health").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if _, err := store.ApplyTransitions(context.Background(), "_cluster", fence, []health.Transition{{Key: key, Op: health.OpOpen, Reason: health.ReasonEval,
		At: answered.Add(time.Second), FactAt: outage, KindVersion: 1, Instance: inst}}, health.TaskHealth{TaskID: "_cluster", Status: health.StatusUnhealthy}); err != nil {
		t.Fatalf("OPEN: %v", err)
	}

	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT fence_mode").WillReturnRows(sqlmock.NewRows([]string{"m", "e", "n", "i", "g", "c"}).AddRow("standalone", 0, "standalone", "i1", 1, ""))
	mock.ExpectQuery("FROM finding_transitions").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectExec("INSERT INTO finding_transitions").WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectExec("UPDATE task_findings SET resolved_at").WithArgs(captures(5, &mu, &closeArgs)...).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE task_health").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if _, err := store.ApplyTransitions(context.Background(), "_cluster", fence, []health.Transition{{Key: key, Op: health.OpClose, Reason: health.ReasonEval,
		At: answered.Add(5 * time.Second), FactAt: answered, KindVersion: 1, Instance: inst}}, health.TaskHealth{TaskID: "_cluster", Status: health.StatusHealthy}); err != nil {
		t.Fatalf("CLOSE: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	for _, q := range statements {
		if strings.Contains(q, "NOW(") && !strings.Contains(q, "lease_expire_at") {
			t.Errorf("statement stamps the session time zone, want UTC_TIMESTAMP(6): %s", q)
		}
	}
	asTime := func(v driver.Value) time.Time { tv, _ := v.(time.Time); return tv }
	if len(openArgs) != 10 || !asTime(openArgs[7]).Equal(outage) {
		t.Errorf("task_findings OPEN args %v, want first_seen (8th) = the OPEN fact time %s", openArgs, outage)
	}
	if len(closeArgs) != 5 || !asTime(closeArgs[0]).Equal(answered) {
		t.Errorf("task_findings CLOSE args %v, want resolved_at (1st) = the CLOSE fact time %s", closeArgs, answered)
	}
}

func TestHealthStoreNowUsesUTC(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	want := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT UTC_TIMESTAMP\(6\)`).WillReturnRows(sqlmock.NewRows([]string{"utc"}).AddRow(want))
	got, err := store.StoreNow(context.Background())
	if err != nil || !got.Equal(want) {
		t.Fatalf("StoreNow %v %v, want %s", got, err, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
