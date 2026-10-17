// Package tasks provides module-level functionality for tasks.
// input: the cluster lease manager, the task row and its task_leases row
// output: the _health lease (one node per cluster, renewed each sweep, TTL 3x sweep) and the proxy authority with which that node evaluates task.state of tasks whose row names no owner (FAILED, STOPPED, CREATED, unclaimed STARTING): Fence{cluster, last lease epoch, Proxy} or Fence{never}; unownedFacts (task.state from the row only, no signals, no chain, no liveness)
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

// proxyFence is the _health holder's authority for an unowned task: the
// last task lease epoch on behalf of its owner (a released lease has none
// left, the holder writes in its own name), or the never-leased fence. The
// store re-checks it under lock: nobody may hold the task lease (§24.3).
func (e *Evaluator) proxyFence(t Task) (health.Fence, bool) {
	if !e.cfg.Cluster || !e.healthHeld.Load() || !unowned(t) || t.ID == ClusterFindingTask {
		return health.Fence{}, false
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
func unownedFacts(task Task, facts *health.Facts) {
	facts.Task = rowTaskFacts(task)
	facts.Signals = map[health.SignalKey]health.SignalState{}
	facts.Complete[health.ScopeTaskState] = true
	facts.Produced = map[health.Scope]bool{health.ScopeTaskState: true}
}
