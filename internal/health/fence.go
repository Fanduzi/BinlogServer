// Package health provides module-level functionality for health.
// input: the writer's view of the task lease, and a transition
// output: Fence (which counter and node guard a health write, design §8) and TransitionDigest (content identity for replay, §5.4)
// pos: pure fence and replay identity shared by every health store
// note: if this file changes, update this header and module README.md.
package health

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// FenceMode says where a health write's authority comes from.
type FenceMode string

const (
	// FenceCluster: the task has a task_leases row; TaskEpoch is its epoch.
	FenceCluster FenceMode = "cluster"
	// FenceStandalone: no cluster lease (standalone, with or without a meta
	// DSN); authority is the persisted node id, a new instance of the same
	// node may take over.
	FenceStandalone FenceMode = "standalone"
	// FenceNeverLeased: cluster mode, the task never had a lease row
	// (CREATED, never started); epoch 0, written by the _health holder.
	FenceNeverLeased FenceMode = "never"
)

// Fence guards one health write. Only the task lease epoch is ever
// compared; the _health lease's own epoch never is.
type Fence struct {
	Mode      FenceMode
	TaskEpoch int64
	Node      string
	Instance  string
	// Proxy: the _health holder writes task.state/store for a task nobody
	// holds, on behalf of the last (owner, epoch); the lease must be
	// expired or released and still at that epoch.
	Proxy bool
	// Delegated: the last owner evaluates chain.* of its own directory for
	// a task nobody holds (non-shared storage, D2); same lease check as Proxy.
	Delegated bool
}

// TransitionDigest is the content identity of a transition: a replay of
// the same content is recognised by it, never by generation alone.
func TransitionDigest(taskID string, t Transition) string {
	fact := t.FactAt
	if fact.IsZero() {
		fact = t.At
	}
	h := sha256.New()
	for _, part := range []string{taskID, string(t.Kind), t.Subject, string(t.Op), t.Reason, string(t.Instance.Severity),
		t.Instance.Evidence, t.Instance.Detail, fact.UTC().Format(time.RFC3339Nano)} {
		h.Write([]byte(strings.ReplaceAll(part, "\x00", "")))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
