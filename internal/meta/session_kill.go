// Package meta provides module-level functionality for meta.
// input: a cluster meta DSN, the lease TTL, and cluster.side_kill
// output: a pool whose every connection and reconnect sets the session-kill timeouts, ConnMaxIdleTime shorter than that bound, and a lease-transaction readback of @@session.wait_timeout
// pos: ADR 0011 PR-2 session kill (P2-7, P2-8); standalone pools are not armed
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/ownership"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

// sessionState is the armed session-kill bound for one cluster pool.
type sessionState struct {
	bound     time.Duration
	expectSec int
	flavor    string
	mode      string
	mu        sync.Mutex
	degraded  bool
	detail    string
	armed     string
	logged    bool
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
	if st == nil || st.expectSec <= 0 {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if got != st.expectSec {
		st.degraded = true
		st.detail = fmt.Sprintf("@@session.wait_timeout=%d, want %d", got, st.expectSec)
		return
	}
	st.degraded = false
	st.detail = ""
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
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	timeouts := []string{"wait_timeout", "net_read_timeout", "net_write_timeout", "innodb_lock_wait_timeout"}
	if flavor == "mariadb" {
		timeouts = append(timeouts, "idle_transaction_timeout")
	}
	text := strconv.Itoa(sec)
	for _, name := range timeouts {
		cfg.Params[name] = text
	}
	connector, err := mysqlDriver.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetConnMaxLifetime(30 * time.Second)
	db.SetMaxIdleConns(8)
	db.SetMaxOpenConns(32)
	idle := bound - time.Second
	if idle <= 0 {
		idle = bound / 2
	}
	db.SetConnMaxIdleTime(idle)
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
	store.session = &sessionState{bound: bound, expectSec: sec, flavor: flavor, mode: mode, armed: "off"}
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

func (s *LeaseStore) checkSessionTimeout(ctx context.Context) {
	if s == nil || s.session == nil || s.session.expectSec <= 0 || s.db == nil {
		return
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	var got int
	if err := tx.QueryRowContext(ctx, "SELECT @@session.wait_timeout").Scan(&got); err != nil {
		return
	}
	s.session.noteTimeout(got)
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
