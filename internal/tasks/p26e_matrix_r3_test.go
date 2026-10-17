// Package tasks provides module-level functionality for tasks.
// input: the failure matrix env of p26d_failure_matrix_test.go
// output: meta read-only wrappers for that env (roNodeStore: task row writes; roLease: lease acquire/renew/release, on top of a freezable renew for a node that died; roHealthStore: health writes), all failing with the MariaDB read-only error while env.ro is set, and the QA r3 row helpers
// pos: ADR 0009 design §24.13-24.16 (#424 QA r3, #425): dead owner with only a control-plane node left, meta read-only that must not end in a silent STOPPED, an owner retrying with no run open
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

var errMetaReadOnly = errors.New("Error 1290 (HY000): The MariaDB server is running with the --read-only option so it cannot execute this statement")

type roNodeStore struct {
	*nodeStore
	ro *atomic.Bool
}

func (n *roNodeStore) UpsertTask(ctx context.Context, task Task) error {
	if n.ro.Load() {
		return errMetaReadOnly
	}
	return n.nodeStore.UpsertTask(ctx, task)
}

func (n *roNodeStore) UpsertTaskFenced(ctx context.Context, task Task, owner string, epoch int64) error {
	if n.ro.Load() {
		return errMetaReadOnly
	}
	return n.nodeStore.UpsertTaskFenced(ctx, task, owner, epoch)
}

type roLease struct {
	*freezableLease
	ro *atomic.Bool
}

func (l *roLease) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	if l.ro.Load() {
		return 0, false, errMetaReadOnly
	}
	return l.freezableLease.Acquire(ctx, taskID, workerID, ttl)
}

func (l *roLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	if l.ro.Load() {
		return false, errMetaReadOnly
	}
	return l.freezableLease.Renew(ctx, taskID, workerID, epoch, now, ttl)
}

func (l *roLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	if l.ro.Load() {
		return false, errMetaReadOnly
	}
	return l.freezableLease.Release(ctx, taskID, workerID, epoch)
}

type roHealthStore struct {
	*leaseHealthStore
	ro *atomic.Bool
}

func (s *roHealthStore) ApplyTransitions(ctx context.Context, taskID string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	if s.ro.Load() {
		return healthstore.Applied{}, errMetaReadOnly
	}
	return s.leaseHealthStore.ApplyTransitions(ctx, taskID, f, ts, h)
}

// expireLease moves the task lease past its expiry, as a dead owner leaves it.
func (env *matrixEnv) expireLease(id string) {
	env.leases.mu.Lock()
	row := env.leases.rows[id]
	row.expire = time.Now().Add(-time.Second)
	env.leases.rows[id] = row
	env.leases.mu.Unlock()
}

// assertHealth checks GET /api/health on every node listed.
func assertHealth(t *testing.T, nodes []*matrixNode, want string, what string) {
	t.Helper()
	for _, n := range nodes {
		if sum := n.s.HealthSummary(context.Background()); sum.Status != want {
			t.Errorf("%s: %s /api/health %+v, want %s", what, n.name, sum, want)
		}
	}
}

// sweepAll sweeps every node, ignoring store errors (meta read-only).
func (env *matrixEnv) sweepAll() {
	for _, n := range env.nodes {
		_ = n.e.SweepNow(context.Background())
	}
}
