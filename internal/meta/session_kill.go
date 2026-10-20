// Package meta provides module-level functionality for meta.
// input: a cluster meta DSN, the lease TTL, and cluster.side_kill
// output: a pool whose every connection and reconnect sets the session-kill timeouts, with innodb_lock_wait_timeout strictly below the client deadline, ConnMaxIdleTime short enough that the pool closes idle connections before wait_timeout, a readback of @@session.wait_timeout and MariaDB @@session.idle_transaction_timeout on the lease transaction's own connection (a failed read is degraded), and lock_detail=reduced when the account cannot read lock tables (not degraded)
// pos: ADR 0011 PR-2 session kill (P2-7, P2-8); standalone pools are not armed
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
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
	st.mu.Lock()
	defer st.mu.Unlock()
	if got != st.expectSec {
		st.degraded = true
		st.detail = fmt.Sprintf("@@session.%s=%d, want %d", name, got, st.expectSec)
		return
	}
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

// lockWaitSeconds is innodb_lock_wait_timeout. It rounds the client deadline
// down to a whole second, stays strictly under that deadline, and is at least 1.
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

// healthDeadline is the context budget around a health write. It stays
// strictly above innodb_lock_wait_timeout so a row lock ends as server 1205.
func (st *sessionState) healthDeadline() time.Duration {
	if st == nil || st.bound <= 0 {
		return 0
	}
	lockSec := st.lockWaitSec
	if lockSec <= 0 {
		lockSec = lockWaitSeconds(st.bound)
	}
	lock := time.Duration(lockSec) * time.Second
	if lock >= st.bound {
		return lock + time.Second
	}
	return st.bound
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
	connector, err := mysqlDriver.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetConnMaxLifetime(30 * time.Second)
	db.SetMaxOpenConns(32)
	if bound <= 2*time.Second {
		db.SetMaxIdleConns(0)
	} else {
		db.SetMaxIdleConns(8)
	}
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

// readSessionTimeout reads @@session.wait_timeout, and on MariaDB
// @@session.idle_transaction_timeout, on the lease transaction's own
// connection. A mismatch or a failed read is degraded; the lease write
// still commits.
func (s *LeaseStore) readSessionTimeout(ctx context.Context, tx *sql.Tx) {
	if s == nil || s.session == nil || s.session.expectSec <= 0 || tx == nil {
		return
	}
	var got int
	if err := tx.QueryRowContext(ctx, "SELECT @@session.wait_timeout").Scan(&got); err != nil {
		s.session.noteReadbackFailed(err)
		return
	}
	if got != s.session.expectSec {
		s.session.noteTimeout(got)
		return
	}
	if s.session.flavor == "mariadb" {
		var idle int
		if err := tx.QueryRowContext(ctx, "SELECT @@session.idle_transaction_timeout").Scan(&idle); err != nil {
			s.session.noteReadbackFailed(fmt.Errorf("idle_transaction_timeout: %w", err))
			return
		}
		if idle != s.session.expectSec {
			s.session.noteNamed("idle_transaction_timeout", idle)
			return
		}
	}
	s.session.noteTimeout(got)
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
		log.Printf("meta account cannot read performance_schema.data_lock_waits or information_schema.innodb_trx; lock holder name is omitted; stall detection still uses server error 1205")
	}
}

func (s *MySQLTaskStore) lockTablesReadable(ctx context.Context) bool {
	if s.queryOK(ctx, "SELECT 1 FROM performance_schema.data_lock_waits LIMIT 0") {
		return true
	}
	// innodb_trx without PROCESS shows only this session, so it cannot name another holder.
	return s.queryOK(ctx, "SELECT 1 FROM information_schema.innodb_trx LIMIT 0") && s.hasProcessPrivilege(ctx)
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
