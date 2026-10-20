// Package tasks provides module-level functionality for tasks.
// input: task_health.chain_owner (healthstore.ChainOwnerStore), the task lease row, and worker_heartbeats when the task store implements workerHeartbeatReader
// output: delegatedFence (the last chain owner writes chain.local after its lease is released, bound to that lease epoch and node, design §30), noteOwnerChanged (OWNER_CHANGED when a new holder is a different node), noteNodeGone (chain.local incomplete and Facts.NodeGone after node_gone_after; a heartbeat read error is not gone), abandonYieldedTail (FAILED with desired still RUN once that gone owner had yielded a tail, §31.6)
// pos: ADR 0009 phase 2 P2-6.2 (docs/design/0009-phase2-health-evaluator.md §30); standalone never reaches this file's fences
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"log"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// nodeGoneAfter is health.node_gone_after (design §30). 0 means 3 sweeps.
func (e *Evaluator) nodeGoneAfter() time.Duration {
	if e.cfg.NodeGoneAfter > 0 {
		return e.cfg.NodeGoneAfter
	}
	sweep := e.cfg.Sweep
	if sweep <= 0 {
		sweep = time.Minute
	}
	return 3 * sweep
}

func (e *Evaluator) noteDelegated(id string, on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.delegated == nil {
		e.delegated = map[string]bool{}
	}
	if on {
		e.delegated[id] = true
		return
	}
	delete(e.delegated, id)
}

func (e *Evaluator) isDelegated(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.delegated[id]
}

// chainOwner reads task_health.chain_owner. A store that cannot answer, or
// a read error while this node is not already the cached owner, is ( "",
// false ): the caller must not close findings or declare the node gone.
func (e *Evaluator) chainOwner(ctx context.Context, taskID string) (string, bool) {
	reader, ok := e.cfg.Store.(healthstore.ChainOwnerStore)
	if !ok {
		return "", false
	}
	owner, err := reader.ChainOwner(ctx, taskID)
	if err != nil {
		if e.isDelegated(taskID) {
			return e.cfg.NodeID, true
		}
		return "", false
	}
	return owner, true
}

// delegatedFence is this node's authority to evaluate chain.local of a task
// it no longer holds (design §30). The row owner, when it is this node, is
// fenceFor instead. Control-plane nodes do not delegate.
func (e *Evaluator) delegatedFence(t Task) (health.Fence, bool) {
	if !e.cfg.Cluster || e.cfg.ControlPlaneOnly || e.cfg.NodeID == "" {
		return health.Fence{}, false
	}
	if t.ID == "" || t.ID == ClusterFindingTask || t.ID == HealthLeaseID {
		return health.Fence{}, false
	}
	if t.OwnerWorkerID == e.cfg.NodeID && t.Epoch > 0 {
		return health.Fence{}, false
	}
	owner, ok := e.chainOwner(context.Background(), t.ID)
	if !ok || owner != e.cfg.NodeID {
		if ok {
			e.noteDelegated(t.ID, false)
		}
		return health.Fence{}, false
	}
	view, ok := e.leaseManager().(LeaseView)
	if !ok {
		return health.Fence{}, false
	}
	leaseOwner, epoch, present, err := view.ViewLease(context.Background(), t.ID)
	if err != nil || !present || epoch <= 0 {
		return health.Fence{}, false
	}
	if leaseOwner != "" && leaseOwner != e.cfg.NodeID {
		e.noteDelegated(t.ID, false)
		return health.Fence{}, false
	}
	e.noteDelegated(t.ID, true)
	return health.Fence{Mode: health.FenceCluster, TaskEpoch: epoch, Node: e.cfg.NodeID, Instance: e.cfg.Instance, Delegated: true}, true
}

// noteOwnerChanged sets Facts.ChainOwnerChanged when this cluster holder is
// not the node that last evaluated the directory. A read error does not.
func (e *Evaluator) noteOwnerChanged(ctx context.Context, fence health.Fence, facts *health.Facts) {
	if fence.Mode != health.FenceCluster || fence.Proxy || fence.Delegated || facts == nil {
		return
	}
	owner, ok := e.chainOwner(ctx, facts.TaskID)
	if !ok || owner == "" || owner == fence.Node {
		return
	}
	facts.ChainOwnerChanged = true
}

func (e *Evaluator) taskStore() any {
	if e == nil || e.s == nil {
		return nil
	}
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	return e.s.store
}

// healthNow is the clock a heartbeat age is compared with: the database
// clock when the health store has one, otherwise the evaluator clock.
func (e *Evaluator) healthNow(ctx context.Context) time.Time {
	if clock, ok := e.cfg.Store.(healthstore.StoreClock); ok {
		if t, err := clock.StoreNow(ctx); err == nil && !t.IsZero() {
			return t
		}
	}
	return e.cfg.Now()
}

// ownerGone reports whether workerID has no heartbeat inside node_gone_after.
// No heartbeat interface means not gone. A list error is returned and is not
// gone. A missing row is gone.
func (e *Evaluator) ownerGone(ctx context.Context, workerID string, now time.Time) (bool, error) {
	if workerID == "" {
		return false, nil
	}
	reader, ok := e.taskStore().(workerHeartbeatReader)
	if !ok {
		return false, nil
	}
	items, err := reader.ListWorkerHeartbeats(ctx, 10000)
	if err != nil {
		return false, err
	}
	var seen time.Time
	found := false
	for _, hb := range items {
		if hb.WorkerID != workerID {
			continue
		}
		found = true
		if hb.LastSeenAt.After(seen) {
			seen = hb.LastSeenAt
		}
	}
	if !found {
		return true, nil
	}
	return now.Sub(seen) >= e.nodeGoneAfter(), nil
}

// noteNodeGone marks chain.local produced but incomplete when the recorded
// chain owner is another node and that node is gone (design §30). This pass
// does not read the directory.
func (e *Evaluator) noteNodeGone(ctx context.Context, facts *health.Facts) {
	if facts == nil {
		return
	}
	owner, ok := e.chainOwner(ctx, facts.TaskID)
	if !ok || owner == "" || owner == e.cfg.NodeID {
		return
	}
	gone, err := e.ownerGone(ctx, owner, e.healthNow(ctx))
	if err != nil || !gone {
		return
	}
	facts.NodeGone = true
	if facts.Produced == nil {
		facts.Produced = map[health.Scope]bool{}
	}
	facts.Produced[health.ScopeChainLocal] = true
	delete(facts.Complete, health.ScopeChainLocal)
}

// abandonYieldedTail writes FAILED, desired still RUN, once the chain owner
// of a yielded tail has been gone longer than node_gone_after (§31.6).
// The same pass then derives TASK_FAILED from that FAILED row.
func (e *Evaluator) abandonYieldedTail(task Task, facts *health.Facts) {
	if e == nil || e.s == nil || facts == nil || facts.Task == nil || !facts.Task.TailYielded || !facts.NodeGone {
		return
	}
	if err := e.s.failYieldedTail(task.ID); err != nil {
		log.Printf("health: yielded tail task=%s: %v", task.ID, err)
		return
	}
	fresh, err := e.s.GetTask(task.ID)
	if err != nil {
		return
	}
	facts.Task = rowTaskFacts(fresh)
}
