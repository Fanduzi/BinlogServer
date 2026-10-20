// Package meta provides module-level functionality for meta.
// input: a cluster meta DSN, the lease TTL, and cluster.side_kill
// output: a pool whose every connection and reconnect sets the session-kill timeouts; each lock statement's client deadline is innodb_lock_wait_timeout plus at least 1s, with a longer outer backstop around the whole health write; ConnMaxIdleTime short enough that the pool closes idle connections before wait_timeout; a startup readback of the armed session variables on a fresh pool connection (and again on the lease transaction), a mismatch or a failed read is degraded; lock_detail=reduced when the naming queries return 1227 or 1142 (not degraded)
// pos: ADR 0011 PR-2 session kill (P2-7, P2-8); standalone pools are not armed
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/ownership"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

// sessionState is the armed session-kill bound for one cluster pool.
type sessionState struct {
	bound       time.Duration
	expectSec   int
	lockWaitSec int
	flavor      string
	mode        string
	mu          sync.Mutex
	degraded    bool
	detail      string
	// lockDetail is "full" when the account can name a holder, "reduced" when it cannot.
	lockDetail string
	armed      string
	logged     bool
}

func (st *sessionState) setKill(on bool) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if on {
		st.armed = "on"
		return
	}
	st.armed = "off"
}

func (st *sessionState) kill() string {
	if st == nil {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.armed == "" {
		return "off"
	}
	return st.armed
}

func (st *sessionState) noteTimeout(got int) {
	st.noteNamed("wait_timeout", got)
}

func (st *sessionState) noteNamed(name string, got int) {
	if st == nil || st.expectSec <= 0 {
		return
	}
	st.noteExact(name, got, st.expectSec)
	if got == st.expectSec {
		st.noteAllMatch()
	}
}

func (st *sessionState) noteExact(name string, got, want int) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.degraded = true
	st.detail = fmt.Sprintf("@@session.%s=%d, want %d", name, got, want)
}

func (st *sessionState) noteAllMatch() {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.degraded = false
	st.detail = ""
}

func (st *sessionState) noteReadbackFailed(err error) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.degraded = true
	msg := "read failed"
	if err != nil {
		msg = err.Error()
	}
	st.detail = "session timeout read failed: " + msg
}

// statementMargin is the gap between innodb_lock_wait_timeout and the
// client deadline of that one statement. One second is the minimum that
// still lets the server return 1205 first.
const statementMargin = time.Second

// healthLockStatements is how many lock-taking statements one health write
// runs: the health insert, the lease FOR UPDATE, the health FOR UPDATE, and
// the health update. The outer bound is only a backstop around all of them.
const healthLockStatements = 4

// lockWaitSeconds is innodb_lock_wait_timeout. It rounds the idle bound down
// to a whole second and is at least 1. The statement deadline sits one
// margin above this value, so the server timer fires first.
func lockWaitSeconds(deadline time.Duration) int {
	sec := int(deadline / time.Second)
	if time.Duration(sec)*time.Second >= deadline {
		sec--
	}
	if sec < 1 {
		sec = 1
	}
	return sec
}

// statementDeadline is the client budget for one lock statement. It starts
// when that statement starts, so earlier work in the transaction cannot
// shrink it under innodb_lock_wait_timeout.
func (st *sessionState) statementDeadline() time.Duration {
	if st == nil || st.bound <= 0 {
		return 0
	}
	lockSec := st.lockWaitSec
	if lockSec <= 0 {
		lockSec = lockWaitSeconds(st.bound)
	}
	return time.Duration(lockSec)*time.Second + statementMargin
}

// healthDeadline is the backstop around a whole health write. It is longer
// than one statement so a lock wait that starts late still reaches 1205.
func (st *sessionState) healthDeadline() time.Duration {
	one := st.statementDeadline()
	if one <= 0 {
		return 0
	}
	return time.Duration(healthLockStatements)*one + statementMargin
}

func armSessionTimeouts(cfg *mysqlDriver.Config, flavor string, idleSec, lockSec int) {
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	text := strconv.Itoa(idleSec)
	for _, name := range []string{"wait_timeout", "net_read_timeout", "net_write_timeout"} {
		cfg.Params[name] = text
	}
	cfg.Params["innodb_lock_wait_timeout"] = strconv.Itoa(lockSec)
	if flavor == "mariadb" {
		cfg.Params["idle_transaction_timeout"] = text
	}
}

// poolIdleBeforeWait is how long a pooled connection may sit idle.
// database/sql wakes its cleaner on about this interval, so a connection can
// outlive one interval. Stay under wait_timeout: the pool closes the
// connection before the server kills it and the driver logs the abort.
// poolMaxIdle is 0 when the idle bound is short enough that a parked
// connection would be killed by wait_timeout before the cleaner runs.
// Otherwise the pool keeps 8 idle connections.
func poolMaxIdle(bound time.Duration) int {
	if bound <= 2*time.Second {
		return 0
	}
	return 8
}

func poolIdleBeforeWait(bound time.Duration) time.Duration {
	idle := bound / 3
	if idle > 2*time.Second {
		idle = 2 * time.Second
	}
	if idle < 50*time.Millisecond {
		idle = 50 * time.Millisecond
	}
	return idle
}

func (st *sessionState) degradedReason() (bool, string) {
	if st == nil {
		return false, ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.degraded, st.detail
}

func (s *MySQLTaskStore) sessionBound() time.Duration {
	if s == nil || s.session == nil {
		return 0
	}
	return s.session.bound
}

func (s *MySQLTaskStore) healthDeadline() time.Duration {
	if s == nil || s.session == nil {
		return 0
	}
	return s.session.healthDeadline()
}

func (s *MySQLTaskStore) statementDeadline() time.Duration {
	if s == nil || s.session == nil {
		return 0
	}
	return s.session.statementDeadline()
}

// LockDetail is "reduced" when this account cannot read the lock tables.
// Detection of a stall does not use those tables. An empty string means the
// account can name the holder, or this pool is not armed.
func (s *MySQLTaskStore) LockDetail() string {
	if s == nil || s.session == nil {
		return ""
	}
	s.session.mu.Lock()
	defer s.session.mu.Unlock()
	if s.session.lockDetail == "reduced" {
		return "reduced"
	}
	return ""
}

func (st *sessionState) detailFull() bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lockDetail == "full"
}

// SessionDegraded reports a lease-transaction readback that does not match
// the armed wait_timeout. A swallowed SET does not fail startup.
func (s *MySQLTaskStore) SessionDegraded() (bool, string) {
	if s == nil {
		return false, ""
	}
	return s.session.degradedReason()
}

// SideKill is "on" only after a passing drill. The default and a failed
// drill are "off". An unarmed store returns an empty string.
func (s *MySQLTaskStore) SideKill() string {
	if s == nil || s.session == nil {
		return ""
	}
	return s.session.kill()
}

// testArmExtra adds session variables in tests. A value the server rejects
// must refuse startup.
var testArmExtra func(*mysqlDriver.Config)

// OpenClusterMeta opens the cluster metadata pool with session-kill timeouts
// on every connection. A required SET that the server rejects refuses startup.
// Standalone does not call this.
func OpenClusterMeta(dsn string, schemaTimeout time.Duration, encryptionKey string, leaseTTL time.Duration, sideKill string) (*MySQLTaskStore, error) {
	flavor, err := probeFlavor(dsn)
	if err != nil {
		return nil, err
	}
	bound := ownership.SessionKillBound(leaseTTL)
	sec := int(bound / time.Second)
	if sec < 1 {
		sec = 1
	}
	lockSec := lockWaitSeconds(bound)
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	armSessionTimeouts(cfg, flavor, sec, lockSec)
	if testArmExtra != nil {
		testArmExtra(cfg)
	}
	connector, err := mysqlDriver.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetConnMaxLifetime(30 * time.Second)
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(poolMaxIdle(bound))
	db.SetConnMaxIdleTime(poolIdleBeforeWait(bound))
	pingCtx, cancel := context.WithTimeout(context.Background(), schemaTimeoutOr(schemaTimeout))
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session timeout: %w", err)
	}
	store := newMySQLTaskStoreFromDB(db, schemaTimeout)
	mode := strings.ToLower(strings.TrimSpace(sideKill))
	if mode != "auto" {
		mode = "off"
	}
	store.session = &sessionState{bound: bound, expectSec: sec, lockWaitSec: lockSec, flavor: flavor, mode: mode, armed: "off"}
	if err := store.setSourcePasswordKey(encryptionKey); err != nil {
		_ = db.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.schemaTimeout)
	defer cancel()
	if err := store.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 2*time.Second)
	store.probeLockDetail(pctx)
	pcancel()
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	store.readArmedOnFreshConn(rctx)
	rcancel()
	return store, nil
}

func schemaTimeoutOr(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Second
	}
	return d
}

func probeFlavor(dsn string) (string, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return "", fmt.Errorf("session timeout flavor: %w", err)
	}
	if strings.Contains(strings.ToLower(version), "mariadb") {
		return "mariadb", nil
	}
	return "mysql", nil
}

// rowQuery is the lease transaction or a fresh pool connection.
type rowQuery interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readSessionTimeout reads the armed session variables on the lease
// transaction's own connection. A mismatch or a failed read is degraded;
// the lease write still commits.
func (s *LeaseStore) readSessionTimeout(ctx context.Context, tx *sql.Tx) {
	if s == nil || tx == nil {
		return
	}
	s.session.readArmed(ctx, tx)
}

// readArmedOnFreshConn reads the armed variables on a connection that just
// came out of the pool, before the first lease write. A proxy that strips
// one of them is degraded from startup.
func (s *MySQLTaskStore) readArmedOnFreshConn(ctx context.Context) {
	if s == nil || s.session == nil || s.db == nil {
		return
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		s.session.noteReadbackFailed(err)
		return
	}
	defer conn.Close()
	s.session.readArmed(ctx, conn)
}

func (st *sessionState) readArmed(ctx context.Context, q rowQuery) {
	if st == nil || st.expectSec <= 0 || q == nil {
		return
	}
	type pair struct {
		name string
		want int
	}
	vars := []pair{
		{"wait_timeout", st.expectSec},
		{"net_read_timeout", st.expectSec},
		{"net_write_timeout", st.expectSec},
	}
	if st.lockWaitSec > 0 {
		vars = append(vars, pair{"innodb_lock_wait_timeout", st.lockWaitSec})
	}
	if st.flavor == "mariadb" {
		vars = append(vars, pair{"idle_transaction_timeout", st.expectSec})
	}
	for _, v := range vars {
		var got int
		if err := q.QueryRowContext(ctx, "SELECT @@session."+v.name).Scan(&got); err != nil {
			st.noteReadbackFailed(fmt.Errorf("%s: %w", v.name, err))
			return
		}
		if got != v.want {
			st.noteExact(v.name, got, v.want)
			return
		}
	}
	st.noteAllMatch()
}

func (s *MySQLTaskStore) probeLockDetail(ctx context.Context) {
	if s == nil || s.session == nil {
		return
	}
	full := s.lockTablesReadable(ctx)
	s.session.mu.Lock()
	if full {
		s.session.lockDetail = "full"
	} else {
		s.session.lockDetail = "reduced"
	}
	s.session.mu.Unlock()
	if !full {
		log.Printf("meta account cannot name a lock holder; MySQL 8 PROCESS alone does not name the holder; lock holder name is omitted; stall detection still uses server error 1205")
	}
}

func (s *MySQLTaskStore) lockTablesReadable(ctx context.Context) bool {
	// These are the queries that name a holder. 1227 and 1142 mean the
	// account cannot use that view. information_schema.innodb_trx succeeding
	// is not enough: without PROCESS it returns only this session and does
	// not error on every server. SHOW GRANTS text is not a privilege.
	for _, q := range []string{
		`SELECT t.PROCESSLIST_ID
FROM performance_schema.data_lock_waits w
JOIN performance_schema.threads req ON req.THREAD_ID = w.REQUESTING_THREAD_ID
JOIN performance_schema.threads t ON t.THREAD_ID = w.BLOCKING_THREAD_ID
WHERE req.PROCESSLIST_ID = 0
LIMIT 0`,
		`SELECT w.blocking_trx_id
FROM information_schema.innodb_lock_waits w
JOIN information_schema.innodb_trx r ON r.trx_id = w.requesting_trx_id
WHERE r.trx_mysql_thread_id = 0
LIMIT 0`,
	} {
		if err := s.queryErr(ctx, q); err == nil {
			return true
		}
	}
	return false
}

func (s *MySQLTaskStore) queryErr(ctx context.Context, q string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("no metadata pool")
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	return rows.Close()
}

func accessDenied(err error) bool {
	var me *mysqlDriver.MySQLError
	return errors.As(err, &me) && (me.Number == 1227 || me.Number == 1142)
}

func (s *MySQLTaskStore) queryOK(ctx context.Context, q string) bool {
	if s == nil || s.db == nil {
		return false
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return false
	}
	_ = rows.Close()
	return true
}

// HoldLeaseForUpdate locks one task_leases row through this pool and blocks.
// The e2e holder uses it so the connection is armed by OpenClusterMeta.
// ready runs after the lock is held. The call returns only when the query fails.
func (s *MySQLTaskStore) HoldLeaseForUpdate(taskID string, ready func()) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("no metadata pool")
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", taskID).Scan(&owner); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	for {
		time.Sleep(time.Hour)
	}
}

// WorkerLastSeen is worker_heartbeats.last_seen_at for one worker.
func (s *MySQLTaskStore) WorkerLastSeen(ctx context.Context, workerID string) (time.Time, bool, error) {
	if s == nil || s.db == nil || workerID == "" {
		return time.Time{}, false, nil
	}
	var at time.Time
	err := s.db.QueryRowContext(ctx, "SELECT last_seen_at FROM worker_heartbeats WHERE worker_id = ?", workerID).Scan(&at)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}

// TouchTaskLeaseLock takes task_leases FOR UPDATE and commits. It is the
// second acquisition that closes META_LOCK_STALLED, and it does not write
// health.
func (s *MySQLTaskStore) TouchTaskLeaseLock(ctx context.Context, taskID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var owner string
	err = tx.QueryRowContext(ctx, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", taskID).Scan(&owner)
	if err == sql.ErrNoRows {
		err = nil
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
