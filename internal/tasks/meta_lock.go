// Package tasks provides module-level functionality for tasks.
// input: a metadata lock wait (1205 or the session-kill bound) and a later acquisition of that task_leases row
// output: META_LOCK_STALLED on _cluster after two observations, closed by the same observer's two acquisitions inside lock_wait, or by the _health holder after the observer heartbeat is older than node_gone_after
// pos: ADR 0011 PR-2 F12 and N6. The transition is ownership.Step. Reconcile does not own this kind
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sync"
	"time"

	"binlog_server/internal/ownership"
)

type metaLockBook struct {
	mu   sync.Mutex
	rows map[string]*metaLockRow
}

type metaLockRow struct {
	state ownership.State
	hits  int
}

// LockSeen is the last heartbeat of a worker, when the caller has one.
type LockSeen func(workerID string) (time.Time, bool)

func (b *metaLockBook) row(taskID string, p ownership.Params) *metaLockRow {
	if b.rows == nil {
		b.rows = map[string]*metaLockRow{}
	}
	r := b.rows[taskID]
	if r == nil {
		r = &metaLockRow{state: ownership.State{TaskID: taskID}}
		b.rows[taskID] = r
	}
	r.state.Params = p
	r.state.TaskID = taskID
	return r
}

// NoteMetaLockTimeout counts lock-wait observations. The finding and the
// 503 open on the second one, by ownership.Step(EventMetaLockTimeout).
// The first observation is not a page. opened is true only on that second step.
func (s *Scheduler) NoteMetaLockTimeout(taskID, node string, now time.Time, p ownership.Params) (opened bool) {
	if s == nil || taskID == "" || taskID == ClusterFindingTask {
		return false
	}
	s.metaLocks.mu.Lock()
	defer s.metaLocks.mu.Unlock()
	r := s.metaLocks.row(taskID, p)
	if r.state.MetaLockStalled {
		return false
	}
	r.hits++
	if r.hits < 2 {
		return false
	}
	r.hits = 0
	res := ownership.Step(r.state, ownership.Event{Kind: ownership.EventMetaLockTimeout, Node: node}, now)
	r.state = res.State
	if res.Decision != ownership.DecisionApplied {
		return false
	}
	s.Report(Finding{
		TaskID:   ClusterFindingTask,
		Kind:     FindingMetaLockStalled,
		Subject:  taskID,
		Severity: FindingCritical,
		Detail:   "META_LOCK_STALLED",
	})
	return true
}

// NoteMetaLockHeld records one acquisition of the stalled row. Another node
// does not count. The _health holder counts only after the observer heartbeat
// is older than nodeGone. Two acquisitions inside lock_wait close the finding.
func (s *Scheduler) NoteMetaLockHeld(taskID, node string, now time.Time, p ownership.Params, healthHolder bool, nodeGone time.Duration, seen LockSeen) {
	if s == nil || taskID == "" || taskID == ClusterFindingTask {
		return
	}
	s.metaLocks.mu.Lock()
	defer s.metaLocks.mu.Unlock()
	r := s.metaLocks.row(taskID, p)
	if !r.state.MetaLockStalled {
		r.hits = 0
		return
	}
	if node != r.state.LockObserver {
		if !healthHolder || !observerGone(r.state.LockObserver, now, nodeGone, seen) {
			return
		}
		r.state.LockObserver = node
		if r.state.Lock.Held && r.state.Lock.HeldBy != node {
			r.state.Lock = ownership.Lock{}
		}
	}
	was := r.state.MetaLockStalled
	res := ownership.Step(r.state, ownership.Event{Kind: ownership.EventLock, Node: node}, now)
	r.state = res.State
	if was && !r.state.MetaLockStalled {
		s.Resolve(ClusterFindingTask, FindingMetaLockStalled, taskID)
	}
}

// MetaLockAwaitingSecond is true when one closing acquisition is already
// recorded and the same node still has to take the row again inside lock_wait.
func (s *Scheduler) MetaLockAwaitingSecond(taskID, node string) bool {
	if s == nil {
		return false
	}
	s.metaLocks.mu.Lock()
	defer s.metaLocks.mu.Unlock()
	r := s.metaLocks.rows[taskID]
	if r == nil || !r.state.MetaLockStalled {
		return false
	}
	return r.state.LockObserver == node && r.state.LockRecoveries == 1
}

func observerGone(observer string, now time.Time, nodeGone time.Duration, seen LockSeen) bool {
	if observer == "" || seen == nil || nodeGone <= 0 {
		return false
	}
	at, ok := seen(observer)
	if !ok || at.IsZero() {
		return false
	}
	return now.Sub(at) > nodeGone
}

func (e *Evaluator) ownershipParams() ownership.Params {
	return ownership.Params{
		LeaseTTL:        e.cfg.LeaseTTL,
		ClaimInterval:   e.cfg.ClaimInterval,
		Sweep:           e.cfg.Sweep,
		LockWait:        e.cfg.LockWait,
		IdleKill:        e.cfg.IdleKill,
		StartToReady:    e.cfg.StartToReady,
		NodeGoneAfter:   e.cfg.NodeGoneAfter,
		NoProgressAfter: e.cfg.NoProgressAfter,
		StaleAfter:      e.cfg.StaleAfter,
	}
}

func (e *Evaluator) noteMetaLockTimeout(id string) {
	if e == nil || e.s == nil {
		return
	}
	if e.s.NoteMetaLockTimeout(id, e.cfg.NodeID, e.cfg.Now(), e.ownershipParams()) {
		if k, ok := e.cfg.Store.(interface {
			SideKillStalled(context.Context, string)
		}); ok {
			k.SideKillStalled(context.Background(), id)
		}
	}
}

func (e *Evaluator) noteMetaLockHeld(ctx context.Context, id string) {
	if e == nil || e.s == nil {
		return
	}
	p := e.ownershipParams()
	e.s.NoteMetaLockHeld(id, e.cfg.NodeID, e.cfg.Now(), p, e.healthHeld.Load(), ownership.EffectiveNodeGone(p), e.workerSeen(ctx))
	if !e.s.MetaLockAwaitingSecond(id, e.cfg.NodeID) {
		return
	}
	toucher, ok := e.cfg.Store.(interface {
		TouchTaskLeaseLock(context.Context, string) error
	})
	if !ok {
		return
	}
	if err := toucher.TouchTaskLeaseLock(ctx, id); err != nil {
		return
	}
	e.s.NoteMetaLockHeld(id, e.cfg.NodeID, e.cfg.Now(), p, e.healthHeld.Load(), ownership.EffectiveNodeGone(p), e.workerSeen(ctx))
}

func (e *Evaluator) workerSeen(ctx context.Context) LockSeen {
	q, ok := e.cfg.Store.(interface {
		WorkerLastSeen(context.Context, string) (time.Time, bool, error)
	})
	if !ok {
		return nil
	}
	return func(workerID string) (time.Time, bool) {
		at, found, err := q.WorkerLastSeen(ctx, workerID)
		if err != nil || !found {
			return time.Time{}, false
		}
		return at, true
	}
}

func (e *Evaluator) refreshSideKill(ctx context.Context) {
	if e == nil {
		return
	}
	r, ok := e.cfg.Store.(interface {
		RefreshSideKill(context.Context, string)
	})
	if !ok {
		return
	}
	r.RefreshSideKill(ctx, e.cfg.NodeID)
}
