// Package meta provides module-level functionality for meta.
// input: an injected clock, sqlmock, and one real per-statement deadline
// output: proof that a lock-statement deadline after innodb_lock_wait_timeout is ErrLockWait classified by elapsed time, including when the statement blocks until its own deadline; a cancel before that timeout is not, an outer cancel is not, the context deadline is the lock timeout plus 2s, and the normal suite kills the per-statement deadline, the watch confirm bit, mark removal, in-flight publication, and an exceeded checkout
// pos: ADR 0011 PR-2 elapsed lock-wait classification and the mutants the QA cells do not pin
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"

	"github.com/DATA-DOG/go-sqlmock"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestElapsedDeadlineIsLockWait(t *testing.T) {
	store := &MySQLTaskStore{session: &sessionState{bound: 5 * time.Second, lockWaitSec: 4}}
	start := time.Unix(1_700_000_000, 0)
	var n int
	store.nowFn = func() time.Time {
		n++
		if n == 1 {
			return start
		}
		return start.Add(4 * time.Second)
	}
	err := store.lockStmt(context.Background(), func(context.Context) error {
		return context.DeadlineExceeded
	})
	if !errors.Is(err, healthstore.ErrLockWait) || !errors.Is(err, errClassifiedByElapsed) {
		t.Fatalf("%v, want ErrLockWait classified by elapsed time", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("elapsed classification still matches a client deadline: %v", err)
	}
	var me *mysqlDriver.MySQLError
	if errors.As(err, &me) {
		t.Fatalf("elapsed classification invented mysql %d", me.Number)
	}
}

func TestApplyTransitionsElapsedDeadlineIsLockWait(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newMySQLTaskStoreFromDB(db, time.Second)
	store.session = &sessionState{bound: 5 * time.Second, lockWaitSec: 4}
	store.lockWaitSeen = func(int64) bool { return false }
	start := time.Unix(1_700_000_000, 0)
	var n int
	store.nowFn = func() time.Time {
		n++
		if n%2 == 1 || n <= 2 {
			return start
		}
		return start.Add(4 * time.Second)
	}
	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("8.0.36"))
	mock.ExpectQuery("SELECT CONNECTION_ID\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(9)))
	mock.ExpectExec("INSERT IGNORE INTO task_health").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT CONNECTION_ID\\(\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(9)))
	mock.ExpectQuery("FROM task_leases").WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()
	_, err = store.ApplyTransitions(context.Background(), "t1", health.Fence{Mode: health.FenceCluster, Node: "n", Instance: "i", TaskEpoch: 1}, nil, health.TaskHealth{TaskID: "t1"})
	if !errors.Is(err, healthstore.ErrLockWait) || !errors.Is(err, errClassifiedByElapsed) {
		t.Fatalf("%v, want ErrLockWait classified by elapsed time", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("elapsed classification still matches a client deadline: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelBeforeLockTimeoutIsNotLockWait(t *testing.T) {
	store := &MySQLTaskStore{session: &sessionState{bound: 5 * time.Second, lockWaitSec: 4}}
	start := time.Unix(1_700_000_000, 0)
	var n int
	store.nowFn = func() time.Time {
		n++
		if n == 1 {
			return start
		}
		return start.Add(4*time.Second - time.Nanosecond)
	}
	err := store.lockStmt(context.Background(), func(context.Context) error {
		return context.DeadlineExceeded
	})
	if errors.Is(err, healthstore.ErrLockWait) || errors.Is(err, errClassifiedByElapsed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel before the lock timeout: %v", err)
	}
}

func TestOuterCancelIsNotLockWait(t *testing.T) {
	store := &MySQLTaskStore{session: &sessionState{bound: 5 * time.Second, lockWaitSec: 4}}
	start := time.Unix(1_700_000_000, 0)
	var n int
	store.nowFn = func() time.Time {
		n++
		if n == 1 {
			return start
		}
		return start.Add(time.Hour)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	err := store.lockStmt(parent, func(sctx context.Context) error {
		return sctx.Err()
	})
	if errors.Is(err, healthstore.ErrLockWait) || errors.Is(err, errClassifiedByElapsed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("outer cancel: %v", err)
	}

	n = 0
	past, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	err = store.lockStmt(past, func(context.Context) error {
		return context.DeadlineExceeded
	})
	if errors.Is(err, healthstore.ErrLockWait) || errors.Is(err, errClassifiedByElapsed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("outer deadline: %v", err)
	}
}

// TestOwnDeadlineExpiryIsElapsedLockWait kills the mutant that asks
// noteElapsedLock about the statement context. Once that context has
// expired, its Err is set, and the elapsed rule never applies.
// context.WithTimeout uses the real clock, so this waits out the injected
// 1s lock timeout plus the 2s margin. The parent outlives that budget.
func TestOwnDeadlineExpiryIsElapsedLockWait(t *testing.T) {
	store := &MySQLTaskStore{session: &sessionState{bound: 2 * time.Second, lockWaitSec: 1}}
	parent, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	err := store.lockStmt(parent, func(sctx context.Context) error {
		<-sctx.Done()
		return sctx.Err()
	})
	elapsed := time.Since(start)
	if elapsed < time.Second {
		t.Fatalf("returned in %s, the statement deadline did not fire", elapsed)
	}
	if !errors.Is(err, healthstore.ErrLockWait) || !errors.Is(err, errClassifiedByElapsed) {
		t.Fatalf("after %s: %v, want ErrLockWait classified by elapsed time", elapsed, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("elapsed classification still matches a client deadline: %v", err)
	}
}

// TestStatementDeadlineIsLockTimeoutPlusTwoSeconds kills a 1s margin.
// The deadline is read off the statement context, not off the helper that
// would move with the constant.
func TestStatementDeadlineIsLockTimeoutPlusTwoSeconds(t *testing.T) {
	const lock = 4 * time.Second
	store := &MySQLTaskStore{session: &sessionState{bound: 5 * time.Second, lockWaitSec: 4}}
	start := time.Now()
	err := store.lockStmt(context.Background(), func(sctx context.Context) error {
		dl, ok := sctx.Deadline()
		if !ok {
			return errors.New("statement context has no deadline")
		}
		got := dl.Sub(start)
		low := lock + time.Second + 500*time.Millisecond
		high := lock + 2*time.Second + 100*time.Millisecond
		if got <= low || got > high {
			return errors.New("statement deadline " + got.String() + ", want " + (lock + 2*time.Second).String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLockStmtUsesItsOwnDeadline kills the mutant that passes the outer
// context into the statement, so there is no per-statement deadline.
func TestLockStmtUsesItsOwnDeadline(t *testing.T) {
	store := &MySQLTaskStore{session: &sessionState{bound: 5 * time.Second, lockWaitSec: 4}}
	outer, cancel := context.WithTimeout(context.Background(), store.healthDeadline())
	defer cancel()
	err := store.lockStmt(outer, func(sctx context.Context) error {
		dl, ok := sctx.Deadline()
		if !ok {
			return errors.New("statement context has no deadline")
		}
		got := time.Until(dl)
		want := store.statementDeadline()
		if got < want-time.Second || got > want+time.Second {
			return errors.New("statement deadline " + got.String() + ", want " + want.String())
		}
		odl, ok := outer.Deadline()
		if !ok || !odl.After(dl) {
			return errors.New("statement deadline is not inside the outer backstop")
		}
		cancel()
		select {
		case <-sctx.Done():
		case <-time.After(time.Second):
			return errors.New("outer cancel did not reach the statement")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLockWatchConfirmsTheMark kills the mutant that never sets confirmed.
func TestLockWatchConfirmsTheMark(t *testing.T) {
	store := &MySQLTaskStore{lockWaitSeen: func(int64) bool { return true }}
	mark := &stmtMark{at: time.Now()}
	stop := store.armLockWatch(3, mark)
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for !mark.confirmed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("lock watch never set confirmed")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWithLockPublishesTheInFlightMark kills the mutant that never calls
// flight.begin, so a saturated checkout cannot see the lock statement.
func TestWithLockPublishesTheInFlightMark(t *testing.T) {
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
	store.lockWaitSeen = func(int64) bool { return false }
	entered := make(chan struct{})
	release := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- store.withLock(8, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("withLock did not enter the statement")
	}
	store.flight.mu.Lock()
	n := len(store.flight.open)
	if n == 1 {
		skew := time.Since(store.flight.open[0].at)
		if skew < -time.Second || skew > time.Second {
			n = -1
		}
		store.flight.open[0].confirmed.Store(true)
	}
	store.flight.mu.Unlock()
	if n != 1 {
		t.Fatalf("in-flight marks %d, want one stamped at the start", n)
	}
	if !store.poolStarved() {
		t.Fatal("a lock statement in progress did not starve a full pool")
	}
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer stop()
	_, err = store.acquireConn(ctx)
	if !errors.Is(err, healthstore.ErrLockWait) || !strings.Contains(err.Error(), poolStarvationReason) {
		t.Fatalf("checkout during the lock statement: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("starvation still matches a client deadline: %v", err)
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

// TestFinishedMarkDoesNotStarveTheNextCheckout kills the mutant that never
// removes an in-flight mark, so a finished statement later looks like a stall.
func TestFinishedMarkDoesNotStarveTheNextCheckout(t *testing.T) {
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
	store.lockWaitSeen = func(int64) bool { return false }
	if err := store.withLock(4, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	store.flight.mu.Lock()
	for _, m := range store.flight.open {
		m.at = time.Now().Add(-store.statementDeadline() - time.Second)
		m.confirmed.Store(true)
	}
	left := len(store.flight.open)
	store.flight.mu.Unlock()
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer stop()
	_, err = store.acquireConn(ctx)
	if left != 0 || store.poolStarved() || errors.Is(err, healthstore.ErrLockWait) {
		t.Fatalf("finished statement left %d marks, starved %v, checkout %v", left, store.poolStarved(), err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("checkout: %v, want an unconfirmed deadline", err)
	}
}

// TestExceededMarkStarvesTheCheckout kills a stamp that never reaches the
// statement budget, and a checkout that ignores an exceeded unconfirmed mark.
// Pool pressure depends on that once the watch cannot borrow a connection.
func TestExceededMarkStarvesTheCheckout(t *testing.T) {
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
	defer store.flight.end(mark)
	skew := time.Since(mark.at)
	if skew < -time.Second || skew > time.Second {
		t.Fatalf("mark stamped %s from now", skew)
	}
	mark.at = mark.at.Add(-store.statementDeadline())
	if mark.confirmed.Load() {
		t.Fatal("fresh mark was confirmed")
	}
	if !store.poolStarved() {
		t.Fatal("a mark aged by its own statement budget was not evidence")
	}
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer stop()
	_, err = store.acquireConn(ctx)
	if !errors.Is(err, healthstore.ErrLockWait) || !strings.Contains(err.Error(), poolStarvationReason) {
		t.Fatalf("exceeded checkout: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exceeded checkout still matches a client deadline: %v", err)
	}
}
