// Package healthstore provides module-level functionality for healthstore.
// input: health transitions and health verdicts for one task, guarded by a health.Fence
// output: the Store contract (fenced, transactional apply with store-allocated generations and digest replay; complete open-set paging; health listing), its errors, the fence rule (CheckFence) and the standalone file store
// pos: persistence seam of ADR 0009 phase 2 (P2-2); meta implements the MySQL store, this package the standalone file store
// note: if this file changes, update this header and module README.md.
package healthstore

import (
	"context"
	"errors"
	"time"

	"binlog_server/internal/health"
)

var (
	// ErrFenced: a newer owner (different owner or epoch, or another
	// standalone node) holds the task. Stop evaluating it.
	ErrFenced = errors.New("health write fenced by a newer owner epoch")
	// ErrLeaseLagging: same owner and epoch, the lease only expired (late
	// renew). Keep the transitions and retry after renew.
	ErrLeaseLagging = errors.New("lease expired but still ours; retry after renew")
	// ErrSchemaAbsent: migration 000009 is not applied (missing table or
	// column). The caller falls back to the v0.5.66 finding upsert; the
	// store remembers nothing, so the next call probes again.
	ErrSchemaAbsent = errors.New("health tables are absent; run ./migrate up to 000009")
	// ErrModeUnsupported: the store cannot check this fence mode (the file
	// store has no lease table).
	ErrModeUnsupported = errors.New("fence mode not supported by this store")
)

// Lease is the task_leases row a cluster fence is checked against.
type Lease struct {
	Owner   string
	Epoch   int64
	Expired bool // judged by the store's clock (MySQL NOW(6)), never the caller's
}

// Stored is the fence last written to task_health.
type Stored struct {
	Mode     health.FenceMode
	Epoch    int64
	Node     string
	Instance string
}

// CheckFence is the one fence rule (design §8.3). lease is nil when the
// task has no task_leases row; stored is nil before the first write.
func CheckFence(f health.Fence, lease *Lease, stored *Stored) error {
	switch f.Mode {
	case health.FenceStandalone:
		if stored != nil && stored.Mode != "" && stored.Mode != health.FenceStandalone {
			return ErrFenced // a cluster wrote this task; a standalone node must not
		}
		if stored != nil && stored.Node != "" && stored.Node != f.Node {
			return ErrFenced // another node on the same meta; take over explicitly
		}
		return nil
	case health.FenceNeverLeased:
		if lease != nil {
			return ErrFenced // the task has a lease now: its holder writes
		}
		if stored != nil && stored.Mode == health.FenceCluster {
			return ErrFenced
		}
		return nil
	case health.FenceCluster:
	default:
		return ErrModeUnsupported
	}
	if lease == nil {
		return ErrFenced
	}
	if f.Proxy || f.Delegated {
		// On behalf of the last (owner, epoch): only while nobody holds it.
		if lease.Epoch != f.TaskEpoch || (!lease.Expired && lease.Owner != "") {
			return ErrFenced
		}
	} else {
		if lease.Owner != f.Node || lease.Epoch != f.TaskEpoch {
			return ErrFenced
		}
		if lease.Expired {
			return ErrLeaseLagging
		}
	}
	if stored != nil && stored.Mode == health.FenceCluster {
		if stored.Epoch > f.TaskEpoch || (stored.Epoch == f.TaskEpoch && stored.Node != "" && stored.Node != f.Node) {
			return ErrFenced
		}
	}
	return nil
}

// Applied is the result of ApplyTransitions: the generation allocated to
// each transition in order, 0 for a replay of content already stored.
type Applied struct {
	Generations []int64
}

// Store persists findings, transitions and health under a fence.
type Store interface {
	// ApplyTransitions checks the fence and, in one transaction, allocates
	// generation = task_health.generation + 1 per new transition, skips a
	// transition whose digest is already stored, writes the finding rows,
	// the finding_transitions rows and task_health.
	ApplyTransitions(ctx context.Context, taskID string, fence health.Fence, ts []health.Transition, h health.TaskHealth) (Applied, error)
	// ListOpenFindingsForTask returns every open finding of the task
	// (keyset paging until done, no cap).
	ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error)
	ListHealth(ctx context.Context) ([]health.TaskHealth, error)
}

// Transition is one stored finding_transitions row.
type Transition struct {
	TaskID, Kind, Subject, Op, Reason, Severity, Digest string
	// Detail is the instance detail at the transition (a CONFIG close
	// carries its audit note here, design 0009 §21.5).
	Detail      string
	Generation  int64
	KindVersion int
	FactAt      time.Time
}

// TransitionLister lists stored transitions in generation order (tests,
// the notifier outbox in P2-9).
type TransitionLister interface {
	ListTransitions(ctx context.Context, taskID string) ([]Transition, error)
}
