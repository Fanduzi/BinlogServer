// Package meta provides module-level functionality for meta.
// input: health transitions and verdicts under a health.Fence
// output: the MySQL healthstore.Store on migration 000009 (task_findings health columns, task_health, finding_transitions); WaiverStore on the same finding rows (§22.3); every stamp in UTC (UTC_TIMESTAMP(6), Go times converted to UTC), first_seen of an OPEN = its FactAt and resolved_at of a CLOSE = its FactAt (§25.8 R3); StoreNow reads UTC_TIMESTAMP(6) for HEALTH_STALE (§28); ChainOwner reads task_health.chain_owner and only a cluster holder write changes it (§30); missing tables/columns (1146/1054) map to healthstore.ErrSchemaAbsent on every call, never cached; a single apply, with MySQL 1205 and a session deadline that fired during a confirmed task_leases or task_health lock wait returned as healthstore.ErrLockWait (a plain slow statement is not a lock wait) and 1213 retried once; the one-row rehash_cursor (LoadRehashCursor/SaveRehashCursor)
// pos: metadata persistence for ADR 0009 phase 2 (P2-2)
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/tasks"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

var _ healthstore.Store = (*MySQLTaskStore)(nil)
var _ healthstore.ChainOwnerStore = (*MySQLTaskStore)(nil)
var _ healthstore.StoreClock = (*MySQLTaskStore)(nil)
var _ healthstore.WaiverStore = (*MySQLTaskStore)(nil)
var _ healthstore.TransitionLister = (*MySQLTaskStore)(nil)
var _ tasks.RehashCursorStore = (*MySQLTaskStore)(nil)

// openFindingsPage is the keyset page size of ListOpenFindingsForTask.
const openFindingsPage = 1000

func healthErr(err error) error {
	if err == nil {
		return nil
	}
	if isMySQLTableNotFoundError(err) || isMySQLUnknownColumnError(err) {
		return fmt.Errorf("%w: %v", healthstore.ErrSchemaAbsent, err)
	}
	return err
}

func isLockWaitError(err error) bool {
	var me *mysqlDriver.MySQLError
	return errors.As(err, &me) && me.Number == 1205
}

func isDeadlock(err error) bool {
	var me *mysqlDriver.MySQLError
	return errors.As(err, &me) && me.Number == 1213
}

// lockWaitError is a 1205, or a session deadline that fired while this
// connection was waiting on task_leases or task_health.
type lockWaitError struct{ error }

func (e lockWaitError) Unwrap() error { return e.error }

func classifyHealthErr(err error) error {
	if err == nil {
		return nil
	}
	var lw lockWaitError
	if errors.As(err, &lw) || isLockWaitError(err) {
		return fmt.Errorf("%w: %v", healthstore.ErrLockWait, err)
	}
	return healthErr(err)
}

const insertHealthRowSQL = "INSERT IGNORE INTO task_health (task_id, status, evaluated_at) VALUES (?, '', UTC_TIMESTAMP(6))"

// insertHealthRow creates the health row before the applying transaction so
// concurrent writers queue on its record lock. The statement is inside the
// session-kill bound, and a 1205 here is a lock stall.
func (s *MySQLTaskStore) insertHealthRow(ctx context.Context, taskID string) error {
	if s.sessionBound() == 0 {
		_, err := s.db.ExecContext(ctx, insertHealthRowSQL, taskID)
		return classifyLock(err, false)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var thread int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&thread); err != nil {
		return err
	}
	return s.withLock(thread, func() error {
		_, err := conn.ExecContext(ctx, insertHealthRowSQL, taskID)
		return err
	})
}

func classifyLock(err error, waiting bool) error {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if isLockWaitError(err) || (errors.Is(err, context.DeadlineExceeded) && waiting) {
		return lockWaitError{err}
	}
	return err
}

func (s *MySQLTaskStore) withLock(thread int64, fn func() error) (err error) {
	var stop func() bool
	if thread > 0 {
		stop = s.armLockWatch(thread)
		defer func() { stop() }()
	}
	err = fn()
	saw := false
	if stop != nil {
		saw = stop()
	}
	if errors.Is(err, context.DeadlineExceeded) && !saw && thread > 0 {
		saw = s.sawLockWait(thread)
	}
	return classifyLock(err, saw)
}

// armLockWatch polls for a lock wait while a lock statement runs. The
// returned function stops the poll and reports whether a wait was visible.
func (s *MySQLTaskStore) armLockWatch(thread int64) func() bool {
	var seen atomic.Bool
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if s.sawLockWait(thread) {
					seen.Store(true)
					return
				}
			}
		}
	}()
	return func() bool {
		select {
		case <-done:
		default:
			close(done)
		}
		return seen.Load()
	}
}

func (s *MySQLTaskStore) sawLockWait(thread int64) bool {
	if s == nil || thread <= 0 {
		return false
	}
	if s.lockWaitSeen != nil {
		return s.lockWaitSeen(thread)
	}
	return s.connectionWaiting(thread)
}

// connectionWaiting is true when this server thread is in a lock wait.
// performance_schema.data_lock_waits is preferred; information_schema.innodb_trx
// covers servers without that view, including MariaDB.
func (s *MySQLTaskStore) connectionWaiting(thread int64) bool {
	if s == nil || s.db == nil || thread <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM performance_schema.data_lock_waits w
JOIN performance_schema.threads th ON th.THREAD_ID = w.REQUESTING_THREAD_ID
WHERE th.PROCESSLIST_ID = ?`, thread).Scan(&n)
	if err == nil && n > 0 {
		return true
	}
	err = s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.innodb_trx
WHERE trx_mysql_thread_id = ? AND trx_state = 'LOCK WAIT'`, thread).Scan(&n)
	return err == nil && n > 0
}

func (s *MySQLTaskStore) useAlias(ctx context.Context) (bool, error) {
	switch s.healthUpsertAlias.Load() {
	case 1:
		return true, nil
	case 2:
		return false, nil
	}
	var v string
	if err := s.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return false, err
	}
	alias := upsertWithAlias(v)
	if alias {
		s.healthUpsertAlias.Store(1)
	} else {
		s.healthUpsertAlias.Store(2)
	}
	return alias, nil
}

// ApplyTransitions implements healthstore.Store. One transaction: the lease
// row (cluster / never-leased) and the task_health row are locked, the
// fence is checked against both, each new transition gets
// task_health.generation+1, a transition whose content digest is stored
// already is a replay (generation 0).
func (s *MySQLTaskStore) ApplyTransitions(ctx context.Context, taskID string, fence health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	switch fence.Mode {
	case health.FenceCluster, health.FenceStandalone, health.FenceNeverLeased:
	default:
		return healthstore.Applied{}, healthstore.ErrModeUnsupported
	}
	alias, err := s.useAlias(ctx)
	if err != nil {
		return healthstore.Applied{}, err
	}
	parent := ctx
	bound := s.sessionBound()
	if bound > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(parent), bound)
		defer cancel()
	}
	var out healthstore.Applied
	for attempt := 0; ; attempt++ {
		if err = s.insertHealthRow(ctx, taskID); err != nil {
			if isDeadlock(err) && attempt == 0 {
				continue
			}
			break
		}
		out, err = s.applyOnce(ctx, taskID, fence, ts, h, alias)
		if isDeadlock(err) && attempt == 0 {
			continue
		}
		break
	}
	return out, classifyHealthErr(err)
}

func (s *MySQLTaskStore) applyOnce(ctx context.Context, taskID string, fence health.Fence, ts []health.Transition, h health.TaskHealth, alias bool) (out healthstore.Applied, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var thread int64
	if s.sessionBound() > 0 {
		if err = tx.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&thread); err != nil {
			return out, err
		}
	}
	var lease *healthstore.Lease
	if fence.Mode != health.FenceStandalone {
		var l healthstore.Lease
		err = s.withLock(thread, func() error {
			return tx.QueryRowContext(ctx, "SELECT owner_worker_id, epoch, lease_expire_at <= NOW(6) FROM task_leases WHERE task_id = ? FOR UPDATE", taskID).Scan(&l.Owner, &l.Epoch, &l.Expired)
		})
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = nil
		case err != nil:
			return out, err
		default:
			lease = &l
		}
	}
	var stored healthstore.Stored
	var mode string
	var gen int64
	err = s.withLock(thread, func() error {
		return tx.QueryRowContext(ctx, "SELECT fence_mode, fence_epoch, fence_node, fence_instance, generation, chain_owner FROM task_health WHERE task_id = ? FOR UPDATE", taskID).
			Scan(&mode, &stored.Epoch, &stored.Node, &stored.Instance, &gen, &stored.ChainOwner)
	})
	if err != nil {
		return out, err
	}
	stored.Mode = health.FenceMode(mode)
	var sp *healthstore.Stored
	if mode != "" {
		sp = &stored
	}
	if err = healthstore.CheckFence(fence, lease, sp); err != nil {
		return out, err
	}
	out.Generations = make([]int64, len(ts))
	for i, t := range ts {
		digest := health.TransitionDigest(taskID, t)
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM finding_transitions WHERE task_id = ? AND transition_digest = ?", taskID, digest).Scan(&n); err != nil {
			return out, err
		}
		if n > 0 {
			continue
		}
		gen++
		out.Generations[i] = gen
		fact := t.FactAt
		if fact.IsZero() {
			fact = t.At
		}
		var factArg any = fact.UTC()
		if fact.IsZero() {
			factArg = nil
		}
		kv := t.KindVersion
		if kv == 0 {
			kv = 1
		}
		reason := t.Reason
		if reason == "" {
			reason = "EVAL"
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO finding_transitions (task_id, kind, subject, op, reason, generation, kind_version, fact_at, severity, detail, at, transition_digest) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(?, UTC_TIMESTAMP(6)), ?, ?, UTC_TIMESTAMP(6), ?)",
			taskID, string(t.Kind), t.Subject, string(t.Op), reason, gen, kv, factArg, string(t.Instance.Severity), t.Instance.Detail, digest); err != nil {
			return out, err
		}
		if t.Op == health.OpClose {
			// resolved_at is the CLOSE's fact time (the first answer for a
			// signal), in UTC like every stamp here (design §25.8 R3).
			if _, err = tx.ExecContext(ctx, "UPDATE task_findings SET resolved_at = COALESCE(?, UTC_TIMESTAMP(6)), generation = ? WHERE task_id = ? AND kind = ? AND subject = ? AND resolved_at IS NULL",
				factArg, gen, taskID, string(t.Kind), t.Subject); err != nil {
				return out, err
			}
			continue
		}
		segs, _ := json.Marshal(t.Instance.Segments)
		if _, err = tx.ExecContext(ctx, findingUpsertSQL(alias), taskID, string(t.Kind), t.Subject, string(t.Instance.Severity), t.Instance.Detail,
			t.Instance.Evidence, string(segs), factArg, gen, fence.TaskEpoch); err != nil {
			return out, err
		}
	}
	reasons, _ := json.Marshal(h.Reasons)
	status := string(h.Status)
	if status == "" {
		status = string(health.StatusUnknown)
	}
	// Only a cluster holder records the chain owner. Proxy and delegated
	// writes leave the column alone (design §30): a proxy used to clear it.
	chainSQL := "chain_owner = chain_owner"
	args := []any{status, string(reasons), gen, fence.Node, fence.TaskEpoch, string(fence.Mode), fence.Instance, fence.TaskEpoch, fence.Node}
	if fence.Mode == health.FenceCluster && !fence.Proxy && !fence.Delegated {
		chainSQL = "chain_owner = ?"
		args = append(args, fence.Node)
	}
	args = append(args, taskID)
	if _, err = tx.ExecContext(ctx, "UPDATE task_health SET status = ?, reasons = ?, evaluated_at = UTC_TIMESTAMP(6), generation = ?, owner = ?, owner_epoch = ?, "+
		"fence_mode = ?, fence_instance = ?, fence_epoch = ?, fence_node = ?, "+chainSQL+" WHERE task_id = ?", args...); err != nil {
		return out, err
	}
	err = tx.Commit()
	return out, err
}

func findingUpsertSQL(alias bool) string {
	ins := "INSERT INTO task_findings (task_id, kind, subject, severity, detail, evidence, segments, first_seen, last_seen, count, generation, owner_epoch, evaluated_at) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(?, UTC_TIMESTAMP(6)), UTC_TIMESTAMP(6), 1, ?, ?, UTC_TIMESTAMP(6))"
	ref := func(c string) string { return "VALUES(" + c + ")" }
	if alias {
		ins += " AS new"
		ref = func(c string) string { return "new." + c }
	}
	// Assignments run left to right: first_seen and count read resolved_at
	// before it is cleared (a resolved row reopens with a fresh history).
	return ins + " ON DUPLICATE KEY UPDATE " +
		"first_seen = IF(task_findings.resolved_at IS NULL, task_findings.first_seen, " + ref("first_seen") + "), " +
		"count = IF(task_findings.resolved_at IS NULL, task_findings.count + 1, 1), " +
		"severity = " + ref("severity") + ", detail = " + ref("detail") + ", evidence = " + ref("evidence") + ", segments = " + ref("segments") + ", " +
		"last_seen = " + ref("last_seen") + ", generation = " + ref("generation") + ", owner_epoch = " + ref("owner_epoch") + ", " +
		"evaluated_at = " + ref("evaluated_at") + ", resolved_at = NULL"
}

// ListOpenFindingsForTask implements healthstore.Store (keyset pages, no cap).
func (s *MySQLTaskStore) ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error) {
	var out []health.Finding
	lastKind, lastSubject := "", ""
	for {
		rows, err := s.db.QueryContext(ctx, "SELECT kind, subject, severity, detail, evidence, segments, first_seen, last_seen, generation FROM task_findings "+
			"WHERE task_id = ? AND resolved_at IS NULL AND (kind > ? OR (kind = ? AND subject > ?)) ORDER BY kind, subject LIMIT ?",
			taskID, lastKind, lastKind, lastSubject, openFindingsPage)
		if err != nil {
			return nil, healthErr(err)
		}
		n := 0
		for rows.Next() {
			var f health.Finding
			var kind, sev string
			var detail, evidence, segs sql.NullString
			if err := rows.Scan(&kind, &f.Subject, &sev, &detail, &evidence, &segs, &f.FirstSeen, &f.LastSeen, &f.Generation); err != nil {
				rows.Close()
				return nil, healthErr(err)
			}
			f.TaskID, f.Kind, f.Severity, f.Detail, f.Evidence = taskID, health.Kind(kind), health.Severity(sev), detail.String, evidence.String
			if segs.Valid && segs.String != "" && segs.String != "null" {
				_ = json.Unmarshal([]byte(segs.String), &f.Segments)
			}
			out = append(out, f)
			lastKind, lastSubject = kind, f.Subject
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, healthErr(err)
		}
		if n < openFindingsPage {
			return out, nil
		}
	}
}

// ChainOwner is the last cluster holder recorded for the task directory
// (design §30). A missing row is an empty owner, not an error.
func (s *MySQLTaskStore) ChainOwner(ctx context.Context, taskID string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, "SELECT chain_owner FROM task_health WHERE task_id = ?", taskID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return owner, healthErr(err)
}

// StoreNow is the database clock HEALTH_STALE ages against (design §28).
// The same UTC_TIMESTAMP(6) stamps evaluated_at, so a node clock ±5 minutes
// off the database does not open or close the finding.
func (s *MySQLTaskStore) StoreNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := s.db.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now)
	return now, healthErr(err)
}

// ListHealth implements healthstore.Store. Placeholder rows of a write that
// was fenced (status ”) are not verdicts and are skipped.
func (s *MySQLTaskStore) ListHealth(ctx context.Context) ([]health.TaskHealth, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT task_id, status, reasons, evaluated_at FROM task_health WHERE status <> '' ORDER BY task_id")
	if err != nil {
		return nil, healthErr(err)
	}
	defer rows.Close()
	var out []health.TaskHealth
	for rows.Next() {
		var h health.TaskHealth
		var status string
		var reasons sql.NullString
		if err := rows.Scan(&h.TaskID, &status, &reasons, &h.EvaluatedAt); err != nil {
			return nil, healthErr(err)
		}
		h.Status = health.Status(status)
		if reasons.Valid && reasons.String != "" && reasons.String != "null" {
			_ = json.Unmarshal([]byte(reasons.String), &h.Reasons)
		}
		out = append(out, h)
	}
	return out, healthErr(rows.Err())
}

// ListTransitions implements healthstore.TransitionLister.
func (s *MySQLTaskStore) ListTransitions(ctx context.Context, taskID string) ([]healthstore.Transition, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT kind, subject, op, reason, severity, COALESCE(detail, ''), transition_digest, generation, kind_version, fact_at FROM finding_transitions WHERE task_id = ? ORDER BY generation, id", taskID)
	if err != nil {
		return nil, healthErr(err)
	}
	defer rows.Close()
	var out []healthstore.Transition
	for rows.Next() {
		t := healthstore.Transition{TaskID: taskID}
		if err := rows.Scan(&t.Kind, &t.Subject, &t.Op, &t.Reason, &t.Severity, &t.Detail, &t.Digest, &t.Generation, &t.KindVersion, &t.FactAt); err != nil {
			return nil, healthErr(err)
		}
		out = append(out, t)
	}
	return out, healthErr(rows.Err())
}

// upsertWithAlias: MySQL 8.0.20+ uses "INSERT … AS new" (VALUES() is
// deprecated there); 5.7 and MariaDB keep VALUES().
func upsertWithAlias(version string) bool {
	if strings.Contains(strings.ToLower(version), "mariadb") {
		return false
	}
	core := version
	if i := strings.IndexAny(core, "-+ "); i >= 0 {
		core = core[:i]
	}
	parts := strings.SplitN(core, ".", 3)
	num := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n, _ := strconv.Atoi(parts[i])
		return n
	}
	major, minor, patch := num(0), num(1), num(2)
	switch {
	case major > 8:
		return true
	case major == 8:
		return minor > 0 || patch >= 20
	}
	return false
}

// LoadRehashCursor implements tasks.RehashCursorStore: the one rehash_cursor
// row, ("", "") when there is none yet.
func (s *MySQLTaskStore) LoadRehashCursor(ctx context.Context) (string, string, error) {
	var task, seg string
	err := s.db.QueryRowContext(ctx, "SELECT task_id, segment FROM rehash_cursor WHERE id = 1").Scan(&task, &seg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return task, seg, healthErr(err)
}

// SaveRehashCursor implements tasks.RehashCursorStore.
func (s *MySQLTaskStore) SaveRehashCursor(ctx context.Context, taskID, segment string) error {
	alias, err := s.useAlias(ctx)
	if err != nil {
		return err
	}
	q := "INSERT INTO rehash_cursor (id, task_id, segment, updated_at) VALUES (1, ?, ?, NOW(6)) " +
		"ON DUPLICATE KEY UPDATE task_id = VALUES(task_id), segment = VALUES(segment), updated_at = VALUES(updated_at)"
	if alias {
		q = "INSERT INTO rehash_cursor (id, task_id, segment, updated_at) VALUES (1, ?, ?, NOW(6)) AS new " +
			"ON DUPLICATE KEY UPDATE task_id = new.task_id, segment = new.segment, updated_at = new.updated_at"
	}
	_, err = s.db.ExecContext(ctx, q, taskID, segment)
	return healthErr(err)
}

// ListWaivers implements healthstore.WaiverStore (open and resolved rows).
func (s *MySQLTaskStore) ListWaivers(ctx context.Context, taskID string) ([]health.Waiver, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT kind, subject, waiver_evidence, waiver_reason, waiver_actor, waived_at FROM task_findings "+
		"WHERE task_id = ? AND waiver_evidence IS NOT NULL AND waiver_evidence <> '' ORDER BY kind, subject", taskID)
	if err != nil {
		return nil, healthErr(err)
	}
	defer rows.Close()
	var out []health.Waiver
	for rows.Next() {
		var w health.Waiver
		var kind, evidence, reason, actor sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&kind, &w.Subject, &evidence, &reason, &actor, &at); err != nil {
			return nil, healthErr(err)
		}
		w.TaskID, w.Kind = taskID, health.Kind(kind.String)
		w.Evidence, w.Reason, w.Actor = evidence.String, reason.String, actor.String
		if at.Valid {
			w.At = at.Time
		}
		out = append(out, w)
	}
	return out, healthErr(rows.Err())
}

// PutWaiver implements healthstore.WaiverStore.
func (s *MySQLTaskStore) PutWaiver(ctx context.Context, taskID string, fence health.Fence, w health.Waiver) error {
	return s.waiverWrite(ctx, taskID, fence, w.Key, "WAIVE", w)
}

// DeleteWaiver implements healthstore.WaiverStore.
func (s *MySQLTaskStore) DeleteWaiver(ctx context.Context, taskID string, fence health.Fence, key health.Key) error {
	return s.waiverWrite(ctx, taskID, fence, key, "UNWAIVE", health.Waiver{Key: key})
}

func (s *MySQLTaskStore) waiverWrite(ctx context.Context, taskID string, fence health.Fence, key health.Key, op string, w health.Waiver) error {
	switch fence.Mode {
	case health.FenceCluster, health.FenceStandalone, health.FenceNeverLeased:
	default:
		return healthstore.ErrModeUnsupported
	}
	if _, err := s.db.ExecContext(ctx, "INSERT IGNORE INTO task_health (task_id, status, evaluated_at) VALUES (?, '', UTC_TIMESTAMP(6))", taskID); err != nil {
		return healthErr(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return healthErr(err)
	}
	defer func() { _ = tx.Rollback() }()
	var lease *healthstore.Lease
	if fence.Mode != health.FenceStandalone {
		var l healthstore.Lease
		err = tx.QueryRowContext(ctx, "SELECT owner_worker_id, epoch, lease_expire_at <= NOW(6) FROM task_leases WHERE task_id = ? FOR UPDATE", taskID).Scan(&l.Owner, &l.Epoch, &l.Expired)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = nil
		case err != nil:
			return healthErr(err)
		default:
			lease = &l
		}
	}
	var stored healthstore.Stored
	var mode string
	var gen int64
	if err = tx.QueryRowContext(ctx, "SELECT fence_mode, fence_epoch, fence_node, fence_instance, generation, chain_owner FROM task_health WHERE task_id = ? FOR UPDATE", taskID).
		Scan(&mode, &stored.Epoch, &stored.Node, &stored.Instance, &gen, &stored.ChainOwner); err != nil {
		return healthErr(err)
	}
	stored.Mode = health.FenceMode(mode)
	var sp *healthstore.Stored
	if mode != "" {
		sp = &stored
	}
	if err = healthstore.CheckFence(fence, lease, sp); err != nil {
		return err
	}
	gen++
	now := time.Now().UTC()
	detail := w.Actor + ": " + w.Reason
	digest := health.TransitionDigest(taskID, health.Transition{Key: key, Op: health.Op(op), Reason: w.Reason, At: now, FactAt: w.At, KindVersion: 1,
		Instance: health.Instance{Key: key, Evidence: w.Evidence, Detail: detail}})
	if _, err = tx.ExecContext(ctx, "INSERT INTO finding_transitions (task_id, kind, subject, op, reason, generation, kind_version, fact_at, severity, detail, at, transition_digest) "+
		"VALUES (?, ?, ?, ?, ?, ?, 1, COALESCE(?, UTC_TIMESTAMP(6)), '', ?, UTC_TIMESTAMP(6), ?)",
		taskID, string(key.Kind), key.Subject, op, w.Reason, gen, nullTime(w.At), detail, digest); err != nil {
		return healthErr(err)
	}
	if op == "WAIVE" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO task_findings (task_id, kind, subject, severity, detail, evidence, first_seen, last_seen, count, generation, waiver_evidence, waiver_reason, waiver_actor, waived_at, waiver_kind_version) "+
			"VALUES (?, ?, ?, 'CRITICAL', '', ?, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6), 1, ?, ?, ?, ?, COALESCE(?, UTC_TIMESTAMP(6)), 1) "+
			"ON DUPLICATE KEY UPDATE waiver_evidence = VALUES(waiver_evidence), waiver_reason = VALUES(waiver_reason), waiver_actor = VALUES(waiver_actor), "+
			"waived_at = VALUES(waived_at), waiver_kind_version = 1, generation = VALUES(generation)",
			taskID, string(key.Kind), key.Subject, w.Evidence, gen, w.Evidence, w.Reason, w.Actor, nullTime(w.At)); err != nil {
			return healthErr(err)
		}
	} else {
		if _, err = tx.ExecContext(ctx, "UPDATE task_findings SET waiver_evidence = NULL, waiver_reason = NULL, waiver_actor = NULL, waived_at = NULL, waiver_kind_version = NULL, generation = ? "+
			"WHERE task_id = ? AND kind = ? AND subject = ?", gen, taskID, string(key.Kind), key.Subject); err != nil {
			return healthErr(err)
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE task_health SET generation = ?, fence_mode = ?, fence_instance = ?, fence_epoch = ?, fence_node = ? WHERE task_id = ?",
		gen, string(fence.Mode), fence.Instance, fence.TaskEpoch, fence.Node, taskID); err != nil {
		return healthErr(err)
	}
	return healthErr(tx.Commit())
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
