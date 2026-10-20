// Package meta provides module-level functionality for meta.
// input: cluster.side_kill, information_schema, and meta_canary when that table exists
// output: side_kill on only after the canary passes; otherwise off. The client never sends a server kill statement
// pos: ADR 0011 PR-2 drill and auto (P2-9). Creating the procedure is a later migration
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"log"
	"strings"
	"time"

	"binlog_server/internal/ownership"
)

// RefreshSideKill runs the canary when cluster.side_kill is auto. A missing
// procedure, a missing canary table, or a failed drill leaves the switch off
// and does not fail the sweep.
func (s *MySQLTaskStore) RefreshSideKill(ctx context.Context, nodeID string) {
	if s == nil || s.session == nil || s.db == nil {
		return
	}
	if s.session.mode != "auto" {
		s.session.setKill(false)
		return
	}
	passed := false
	func() {
		cctx, cancel := context.WithTimeout(ctx, 3*s.session.bound+time.Second)
		defer cancel()
		ok, err := s.drillSideKill(cctx, nodeID)
		if err != nil || !ok {
			s.logDrillOnce()
			return
		}
		passed = true
	}()
	s.session.setKill(passed)
}

func (s *MySQLTaskStore) logDrillOnce() {
	s.session.mu.Lock()
	defer s.session.mu.Unlock()
	if s.session.logged {
		return
	}
	s.session.logged = true
	log.Printf("cluster.side_kill=auto stayed off: procedure, canary, or privilege check did not pass")
}

// SideKillStalled asks the server procedure to drop a stalled task_leases
// holder. It runs only after the drill passed, from a connection that is
// not waiting on the row, and only when two probes idle_kill apart see the
// same sleeping transaction.
func (s *MySQLTaskStore) SideKillStalled(ctx context.Context, taskID string) {
	if s == nil || s.session == nil || s.session.kill() != "on" || taskID == "" {
		return
	}
	go s.probeAndCall(context.WithoutCancel(ctx), taskID)
}

func (s *MySQLTaskStore) probeAndCall(ctx context.Context, taskID string) {
	id, ok := s.sleepingLeaseHolder(ctx, taskID)
	if !ok || id == "" {
		return
	}
	wait := s.session.bound
	if wait <= 0 {
		wait = time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	again, ok := s.sleepingLeaseHolder(ctx, taskID)
	if !ok || again != id {
		return
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.ExecContext(ctx, "CALL binlog_server_kill_trx(?)", id)
}

func (s *MySQLTaskStore) drillSideKill(ctx context.Context, nodeID string) (bool, error) {
	if strings.TrimSpace(nodeID) == "" {
		return false, nil
	}
	n, err := s.countQuery(ctx, "SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema = DATABASE() AND routine_name = 'binlog_server_kill_trx'")
	if err != nil || n == 0 {
		return false, err
	}
	n, err = s.countQuery(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'meta_canary'")
	if err != nil || n == 0 {
		return false, err
	}
	process := s.hasProcessPrivilege(ctx)
	dataLocks := false
	if s.session.flavor != "mariadb" {
		dataLocks = s.canReadDataLocks(ctx)
	}
	if !process {
		return false, nil
	}
	if s.session.flavor != "mariadb" && !dataLocks {
		return false, nil
	}
	return s.runCanary(ctx, nodeID, process, dataLocks)
}

func (s *MySQLTaskStore) countQuery(ctx context.Context, q string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

func (s *MySQLTaskStore) hasProcessPrivilege(ctx context.Context) bool {
	rows, err := s.db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER()")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return false
		}
		up := strings.ToUpper(grant)
		if strings.Contains(up, "ALL PRIVILEGES ON *.*") || strings.Contains(up, "PROCESS") || strings.Contains(up, "SUPER") {
			return true
		}
	}
	return false
}

func (s *MySQLTaskStore) canReadDataLocks(ctx context.Context) bool {
	_, err := s.db.ExecContext(ctx, "SELECT ENGINE_TRANSACTION_ID FROM performance_schema.data_locks LIMIT 0")
	return err == nil
}

func (s *MySQLTaskStore) runCanary(ctx context.Context, nodeID string, process, dataLocks bool) (bool, error) {
	a, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer a.Close()
	tx, err := a.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta_canary (node_id) VALUES (?) ON DUPLICATE KEY UPDATE node_id = node_id", nodeID); err != nil {
		return false, nil
	}
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT node_id FROM meta_canary WHERE node_id = ? FOR UPDATE", nodeID).Scan(&id); err != nil {
		return false, nil
	}
	var trxID string
	if err := tx.QueryRowContext(ctx, "SELECT trx_id FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = CONNECTION_ID()").Scan(&trxID); err != nil || trxID == "" {
		return false, nil
	}
	waiter := s.session.flavor != "mariadb"
	var stopWait context.CancelFunc
	if s.session.flavor == "mariadb" {
		waiter, stopWait = s.armMariaDBWaiter(ctx, nodeID)
		if !waiter {
			return false, nil
		}
	}
	if stopWait != nil {
		defer stopWait()
	}
	plan := ownership.PlanSideKill(ownership.SideKillInput{
		Mode:               "auto",
		Direct:             true,
		Procedure:          true,
		CanaryPassed:       true,
		Flavor:             s.session.flavor,
		MariaDBWaiter:      waiter,
		ProcessPrivilege:   process,
		DataLocksPrivilege: dataLocks,
	})
	if !plan.CallProcedure || plan.ClientSendsKill {
		return false, nil
	}
	b, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer b.Close()
	if _, err := b.ExecContext(ctx, "CALL binlog_server_kill_trx(?)", trxID); err != nil {
		return false, nil
	}
	if err := a.PingContext(ctx); err == nil {
		return false, nil
	}
	lockCtx, cancel := context.WithTimeout(ctx, s.session.bound+time.Second)
	defer cancel()
	btx, err := b.BeginTx(lockCtx, nil)
	if err != nil {
		return false, nil
	}
	defer func() { _ = btx.Rollback() }()
	if err := btx.QueryRowContext(lockCtx, "SELECT node_id FROM meta_canary WHERE node_id = ? FOR UPDATE", nodeID).Scan(&id); err != nil {
		return false, nil
	}
	return true, nil
}

func (s *MySQLTaskStore) armMariaDBWaiter(ctx context.Context, nodeID string) (bool, context.CancelFunc) {
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := s.db.Conn(wctx)
		if err != nil {
			return
		}
		defer conn.Close()
		tx, err := conn.BeginTx(wctx, nil)
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		var id string
		_ = tx.QueryRowContext(wctx, "SELECT node_id FROM meta_canary WHERE node_id = ? FOR UPDATE", nodeID).Scan(&id)
	}()
	deadline := time.Now().Add(s.session.bound)
	for time.Now().Before(deadline) {
		var n int
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.innodb_trx WHERE trx_state = 'LOCK WAIT'").Scan(&n)
		if err == nil && n > 0 {
			return true, func() {
				cancel()
				<-done
			}
		}
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return false, nil
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
	return false, nil
}

func (s *MySQLTaskStore) sleepingLeaseHolder(ctx context.Context, taskID string) (string, bool) {
	if s.session.flavor == "mariadb" {
		var id string
		err := s.db.QueryRowContext(ctx, `
SELECT t.trx_id
FROM information_schema.innodb_lock_waits w
JOIN information_schema.innodb_locks l ON l.lock_id = w.blocking_lock_id
JOIN information_schema.innodb_trx t ON t.trx_id = w.blocking_trx_id
WHERE l.lock_table LIKE '%task_leases%'
  AND t.trx_state = 'RUNNING'
  AND (t.trx_query IS NULL OR t.trx_query = '')
LIMIT 1`).Scan(&id)
		if err != nil || id == "" {
			return "", false
		}
		return id, true
	}
	var id string
	err := s.db.QueryRowContext(ctx, `
SELECT trx.trx_id
FROM information_schema.innodb_trx trx
JOIN performance_schema.data_locks dl ON dl.ENGINE_TRANSACTION_ID = trx.trx_id
WHERE trx.trx_state = 'RUNNING'
  AND (trx.trx_query IS NULL OR trx.trx_query = '')
  AND dl.OBJECT_SCHEMA = DATABASE()
  AND dl.OBJECT_NAME = 'task_leases'
  AND dl.LOCK_STATUS = 'GRANTED'
  AND dl.LOCK_DATA LIKE ?
LIMIT 1`, "%"+taskID+"%").Scan(&id)
	if err != nil || id == "" {
		return "", false
	}
	return id, true
}
