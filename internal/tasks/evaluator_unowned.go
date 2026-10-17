// Package tasks provides module-level functionality for tasks.
// input: the cluster lease manager, the task row and its task_leases row
// output: the _health lease (one node per cluster, any role, renewed each sweep, TTL 3x sweep; a store error on that write is FINDINGS_PERSIST_FAILED on this node, §24.14) and the proxy authority with which that node evaluates task.state of tasks whose row names no owner (FAILED, STOPPED, CREATED, unclaimed STARTING) or names an owner whose lease expired (DeadOwnerLister / ExpiredLeaseTaskLister, timed from when this node first saw it, §24.13): Fence{cluster, last lease epoch, Proxy} or Fence{never}; unownedFacts (task.state from the row only, no signals, no chain, no liveness)
// pos: ADR 0009 phase 2 P2-6c (docs/design/0009-phase2-health-evaluator.md §24, #421): a cluster FAILED is never silent
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"log"
	"time"

	"binlog_server/internal/health"
)

// HealthLeaseID is the task_leases row that elects the node evaluating
// unowned tasks. Its epoch only picks the node; it is never written into a
// finding or health row and never compared with a task epoch (§8, §24.2).
const HealthLeaseID = "_health"

// healthLeaseTTL is 3 sweeps: a holder that vanished is replaced within it.
func (e *Evaluator) healthLeaseTTL() time.Duration { return 3 * e.cfg.Sweep }

// refreshHealthLease renews the _health lease this node holds, or tries to
// acquire it, and records whether this node is the holder. Standalone and a
// scheduler without a lease manager never hold it.
func (e *Evaluator) refreshHealthLease(ctx context.Context) bool {
	lm := e.leaseManager()
	if !e.cfg.Cluster || lm == nil || e.cfg.NodeID == "" {
		e.healthHeld.Store(false)
		return false
	}
	ttl := e.healthLeaseTTL()
	if epoch := e.healthEpoch.Load(); epoch > 0 {
		ok, err := lm.Renew(ctx, HealthLeaseID, e.cfg.NodeID, epoch, e.cfg.Now(), ttl)
		e.noteLeaseWrite(err)
		if err == nil && ok {
			e.healthHeld.Store(true)
			return true
		}
		if err != nil {
			// The store is unreachable: a proxy write would fail as well.
			e.healthHeld.Store(false)
			return false
		}
		e.healthEpoch.Store(0)
	}
	epoch, ok, err := lm.Acquire(ctx, HealthLeaseID, e.cfg.NodeID, ttl)
	e.noteLeaseWrite(err)
	if err != nil || !ok {
		e.healthHeld.Store(false)
		return false
	}
	if !e.healthHeld.Load() {
		log.Printf("health: this node evaluates unowned tasks (%s epoch=%d)", HealthLeaseID, epoch)
	}
	e.healthEpoch.Store(epoch)
	e.healthHeld.Store(true)
	return true
}

func (e *Evaluator) leaseManager() LeaseManager {
	if e.s == nil {
		return nil
	}
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	return e.s.leaseManager
}

// unowned: the task row names no owner, so no node evaluates it as holder.
func unowned(t Task) bool { return t.OwnerWorkerID == "" || t.Epoch <= 0 }

// noteLeaseWrite records the result of a _health lease write: a store error
// is FINDINGS_PERSIST_FAILED on this node until a write gets through
// (design §24.14, #425). A refused acquire (held elsewhere) is not an error.
func (e *Evaluator) noteLeaseWrite(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.leaseWriteErr, e.leaseWriteErrAt = nil, time.Time{}
		return
	}
	if e.leaseWriteErr == nil {
		e.leaseWriteErrAt = e.cfg.Now()
	}
	e.leaseWriteErr = err
}

// DeadOwnerLister lists the tasks whose row names an owner (owner set,
// epoch > 0), that desire RUN and are not FAILED or STOPPED, and whose
// task lease expired by the store's clock: the owner is gone and nobody
// claimed the task (design §24.13). Without it the evaluator falls back to
// ExpiredLeaseTaskLister (RUNNING / LEASE_DEGRADED / RETRY_BACKOFF).
type DeadOwnerLister interface {
	ListTaskIDsWithExpiredOwnerLease(ctx context.Context) ([]string, error)
}

// refreshExpiredLeases is the _health holder's list of tasks whose owner's
// lease expired, each with the time this node first saw it so. Other nodes
// keep none: they do not proxy.
func (e *Evaluator) refreshExpiredLeases(ctx context.Context) {
	if !e.cfg.Cluster || !e.healthHeld.Load() || e.s == nil {
		e.mu.Lock()
		e.leaseExpired = map[string]time.Time{}
		e.mu.Unlock()
		return
	}
	e.s.mu.Lock()
	store := e.s.store
	e.s.mu.Unlock()
	var ids []string
	var err error
	switch l := store.(type) {
	case DeadOwnerLister:
		ids, err = l.ListTaskIDsWithExpiredOwnerLease(ctx)
	case ExpiredLeaseTaskLister:
		var list []Task
		list, err = l.ListTasksWithExpiredLease(ctx)
		for _, t := range list {
			if t.State != StateStopping && !unowned(t) {
				ids = append(ids, t.ID)
			}
		}
	default:
		return
	}
	if err != nil {
		return // keep the last list; the store is failing
	}
	now := e.cfg.Now()
	e.mu.Lock()
	next := make(map[string]time.Time, len(ids))
	for _, id := range ids {
		if seen, ok := e.leaseExpired[id]; ok {
			next[id] = seen
		} else {
			next[id] = now
		}
	}
	e.leaseExpired = next
	e.mu.Unlock()
}

// expiredSince is when this node first saw the task's owner lease expired
// (zero when it is not, or this node does not hold _health).
func (e *Evaluator) expiredSince(id string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leaseExpired[id]
}

// proxyCandidates (§24.13): the stored tasks this process does not hold
// in memory, for the _health holder. A control-plane node has no claim
// loop, so a task created and run on a worker is never in its memory;
// without this its proxy evaluation would see nothing. Only the holder
// pays the full list, once per sweep; a failing store adds nothing.
func (e *Evaluator) proxyCandidates(listed []Task) []Task {
	if !e.cfg.Cluster || !e.healthHeld.Load() || e.s == nil {
		return nil
	}
	e.s.mu.Lock()
	store := e.s.store
	e.s.mu.Unlock()
	if store == nil {
		return nil
	}
	ctx, cancel := e.s.withReadTimeout(context.Background())
	defer cancel()
	stored, err := store.ListTasks(ctx)
	if err != nil {
		return nil
	}
	known := make(map[string]bool, len(listed))
	for _, t := range listed {
		known[t.ID] = true
	}
	var out []Task
	for _, t := range stored {
		if !known[t.ID] && (unowned(t) || !e.expiredSince(t.ID).IsZero()) {
			out = append(out, t)
		}
	}
	return out
}

// mayEvaluate: this node evaluates t, as holder or by proxy, without a
// store read (the health summary calls it).
func (e *Evaluator) mayEvaluate(t Task) bool {
	if _, ok := e.fenceFor(t); ok {
		return true
	}
	return e.cfg.Cluster && e.healthHeld.Load() && (unowned(t) || !e.expiredSince(t.ID).IsZero())
}

// proxyFence is the _health holder's authority for an unowned task: the
// last task lease epoch on behalf of its owner (a released lease has none
// left, the holder writes in its own name), or the never-leased fence. The
// store re-checks it under lock: nobody may hold the task lease (§24.3).
func (e *Evaluator) proxyFence(t Task) (health.Fence, bool) {
	if !e.cfg.Cluster || !e.healthHeld.Load() || t.ID == ClusterFindingTask {
		return health.Fence{}, false
	}
	if !unowned(t) && e.expiredSince(t.ID).IsZero() {
		return health.Fence{}, false // owned, and its owner's lease is live
	}
	view, ok := e.leaseManager().(LeaseView)
	if !ok {
		return health.Fence{}, false
	}
	owner, epoch, present, err := view.ViewLease(context.Background(), t.ID)
	if err != nil {
		return health.Fence{}, false
	}
	if !present {
		return health.Fence{Mode: health.FenceNeverLeased, Node: e.cfg.NodeID, Instance: e.cfg.Instance}, true
	}
	if owner == "" {
		owner = e.cfg.NodeID
	}
	return health.Fence{Mode: health.FenceCluster, TaskEpoch: epoch, Node: owner, Instance: e.cfg.Instance, Proxy: true}, true
}

// authorityFor is the fence this node evaluates the task with: as the task
// lease holder (fenceFor), or as the _health holder for an unowned task.
func (e *Evaluator) authorityFor(t Task) (health.Fence, bool) {
	if f, ok := e.fenceFor(t); ok {
		return f, true
	}
	return e.proxyFence(t)
}

// unownedFacts is task.state for a proxy pass: the row as stored. The row
// is not cross-checked against task_leases (that guards a stale owner's
// STOPPED; the proxy write itself requires nobody to hold the lease), and
// nothing local is read: no signals (this node may keep some from when it
// held the task), no liveness, no chain. Only task.state is produced, so
// the other scopes neither close their rows nor make the health UNKNOWN.
//
// expiredAt (§24.13): the row still names an owner whose lease expired;
// the task has been without a worker since this node first saw that, not
// since the row was last written (a long RUNNING row is old).
func unownedFacts(task Task, facts *health.Facts, expiredAt time.Time) {
	facts.Task = rowTaskFacts(task)
	if !expiredAt.IsZero() && !unowned(task) {
		facts.Task.LeaseExpired = true
		if expiredAt.After(facts.Task.StateSince) {
			facts.Task.StateSince = expiredAt
		}
	}
	facts.Signals = map[health.SignalKey]health.SignalState{}
	facts.Complete[health.ScopeTaskState] = true
	facts.Produced = map[health.Scope]bool{health.ScopeTaskState: true}
}
