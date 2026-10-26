// Package tasks provides module-level functionality for tasks.
// input: the in-memory task row, ownership.Step, and the cluster storage mode
// output: task phase, desired, failed_reason, and lease fields copied from one Step result; a holder event synthesizes a lease only for the decision and does not invent an epoch on a row that had none; a late event that updates nothing is a no-op
// pos: the only place the scheduler asks ADR 0011 what the next phase is
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"binlog_server/internal/ownership"
)

const localHolderNode = "local"

// failureKind maps a runner message onto the one Step event that may write it.
// SOURCE_AUTH and LOG_BIN_OFF are the terminal allowlist. A missing segment
// on this worker is a tail yield. Everything else is EpochLost.
func failureKind(message string) (ownership.Kind, ownership.Cause) {
	switch {
	case strings.Contains(message, CodeSourceAccessDenied):
		return ownership.EventTerminalError, ownership.CauseSourceAuth
	case strings.Contains(message, CodeSourceLogBinOff):
		return ownership.EventTerminalError, ownership.CauseLogBinOff
	case strings.Contains(message, CodeSegmentNotOnWorker):
		return ownership.EventTailUnreadable, ownership.CauseSegmentNotOnWorker
	case strings.Contains(message, "no such file") || strings.Contains(message, "ENOENT"):
		return ownership.EventEpochLost, ownership.CauseENOENT
	case strings.Contains(message, "no space") || strings.Contains(message, "ENOSPC"):
		return ownership.EventEpochLost, ownership.CauseENOSPC
	case strings.Contains(message, "read-only file system") || strings.Contains(message, "EROFS"):
		return ownership.EventEpochLost, ownership.CauseEROFS
	default:
		return ownership.EventEpochLost, ownership.CauseUnclassified
	}
}

func isTerminalAllowlist(err error) bool {
	if err == nil {
		return false
	}
	var pe *PermanentError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Code == CodeSourceAccessDenied || pe.Code == CodeSourceLogBinOff
}

func (s *Scheduler) holderNode() string {
	if id := strings.TrimSpace(s.clusterWorkerID); id != "" {
		return id
	}
	return localHolderNode
}

func (s *Scheduler) ownershipParams() ownership.Params {
	p := ownership.DefaultParams()
	if s.leaseTTL > 0 {
		p.LeaseTTL = s.leaseTTL
	}
	switch strings.TrimSpace(s.storageMode) {
	case string(ownership.StorageShared):
		p.Storage = ownership.StorageShared
	default:
		p.Storage = ownership.StoragePerNode
	}
	if s.refusalCap < 0 {
		p.RefusalRetryCap = s.refusalCap
	} else if s.refusalCap > 0 {
		p.RefusalRetryCap = s.refusalCap
	}
	return p
}

func (s *Scheduler) ownershipState(task Task, now time.Time) ownership.State {
	st := ownership.New()
	st.TaskID = task.ID
	st.Phase = ownership.Phase(task.State)
	switch task.DesiredRun {
	case TaskDesiredRun:
		st.Desired = ownership.DesiredRun
	case TaskDesiredStop:
		st.Desired = ownership.DesiredStop
	default:
		// Rows created before desired_run was filled still run. An active
		// phase is RUN. CREATED and STOPPED without a value stay STOP.
		switch task.State {
		case StateStarting, StateRunning, StateRetryBackoff, StateLeaseDegraded:
			st.Desired = ownership.DesiredRun
		default:
			st.Desired = ownership.DesiredStop
		}
	}
	st.Params = s.ownershipParams()
	st.SpecRevision = task.SpecRevision
	st.RowGeneration = task.RowGeneration
	st.RunID = task.RunID
	st.LastError = task.LastError
	st.RetryAttempt = int(task.RetryAttempt)
	st.FailedReason = task.FailedReason
	st.FailedAt = task.FailedAt
	st.FailedSpec = task.FailedSpecRevision
	st.TailSince = task.TailUnreadableSince
	st.PendingDelete = task.PendingDelete
	st.Lease.Epoch = task.Epoch
	st.Lease.Owner = task.OwnerWorkerID
	st.Lease.Present = task.OwnerWorkerID != "" || task.Epoch > 0
	if task.OwnerWorkerID != "" && task.Epoch > 0 {
		st.Lease.ExpireAt = now.Add(st.Params.LeaseTTL)
		st.Lease.Present = true
	}
	st.Chain.Owner = task.ChainOwner
	st.Chain.VerifiedAt = task.ChainVerifiedAt
	if task.ChainOwner != "" {
		st.Nodes = []ownership.Node{{
			ID:          task.ChainOwner,
			CanRead:     true,
			HeartbeatAt: now,
		}}
	}
	return st
}

func copyOwnership(task Task, st ownership.State, synthesized bool, prevEpoch int64) Task {
	task.State = State(st.Phase)
	task.DesiredRun = string(st.Desired)
	task.SpecRevision = st.SpecRevision
	task.RowGeneration = st.RowGeneration
	task.RunID = st.RunID
	task.LastError = st.LastError
	task.RetryAttempt = int64(st.RetryAttempt)
	task.FailedReason = st.FailedReason
	task.FailedAt = st.FailedAt
	task.FailedSpecRevision = st.FailedSpec
	task.TailUnreadableSince = st.TailSince
	task.PendingDelete = st.PendingDelete
	task.ChainOwner = st.Chain.Owner
	task.ChainVerifiedAt = st.Chain.VerifiedAt
	task.UpdatedAt = time.Now()
	if synthesized {
		task.Epoch = prevEpoch
		task.OwnerWorkerID = ""
		return task
	}
	task.OwnerWorkerID = st.Lease.Owner
	task.Epoch = st.Lease.Epoch
	return task
}

// applyHolderEventLocked runs Step for an event this process is producing.
// An empty owner is given a lease only inside Step, so a standalone row can
// leave RUNNING. The stored epoch is not replaced by that temporary lease.
// Caller holds s.mu. A discarded zero-row result leaves the task unchanged.
func (s *Scheduler) applyHolderEventLocked(id string, kind ownership.Kind, cause ownership.Cause, now time.Time) (ownership.Result, bool) {
	task, ok := s.tasks[id]
	if !ok {
		return ownership.Result{Decision: ownership.DecisionDiscarded, ZeroRow: true}, false
	}
	st := s.ownershipState(task, now)
	node := s.holderNode()
	synthesized := st.Lease.Owner == "" || !ownership.LeaseLive(st, now)
	if synthesized {
		if st.Lease.Epoch <= 0 {
			st.Lease.Epoch = 1
		}
		st.Lease.Present = true
		st.Lease.Owner = node
		st.Lease.ExpireAt = now.Add(st.Params.LeaseTTL)
	}
	ev := ownership.Held(st, kind, node)
	ev.Cause = cause
	res := ownership.Step(st, ev, now)
	if res.ZeroRow || res.Decision == ownership.DecisionDiscarded || res.Decision == ownership.DecisionRejected {
		return res, false
	}
	if res.Decision != ownership.DecisionApplied && res.Decision != ownership.DecisionIdempotent {
		return res, false
	}
	if !s.commitYieldLocked(id, kind, res) {
		return ownership.Result{Decision: ownership.DecisionDiscarded, ZeroRow: true, Detail: "noop"}, false
	}
	task, ok = s.tasks[id]
	if !ok {
		return ownership.Result{Decision: ownership.DecisionDiscarded, ZeroRow: true}, false
	}
	next := copyOwnership(task, res.State, synthesized, task.Epoch)
	if kind == ownership.EventDumpReady && !synthesized {
		next.OwnerWorkerID = task.OwnerWorkerID
		next.Epoch = task.Epoch
	}
	s.tasks[id] = next
	s.rememberRefusalLocked(id, res.State)
	s.noteStepIntentsLocked(id, res)
	return res, true
}

func (s *Scheduler) rememberRefusalLocked(id string, st ownership.State) {
	if s.refusals == nil {
		s.refusals = map[string]map[string]ownership.Refusal{}
	}
	if len(st.Refusal) == 0 {
		delete(s.refusals, id)
		return
	}
	s.refusals[id] = st.Refusal
}

func (s *Scheduler) refusalBlocksLocked(id, node string, now time.Time) bool {
	row, ok := s.refusals[id][node]
	if !ok {
		return false
	}
	if row.OpenEnded || row.Until.IsZero() {
		return row.OpenEnded
	}
	return now.Before(row.Until)
}

// YieldCommit is one yield, terminal failure, or tail refusal written in a
// single meta transaction (ADR 0011 F12).
type YieldCommit struct {
	TaskID           string
	State            string
	Desired          string
	LastError        string
	Owner            string
	Epoch            int64
	RunID            string
	FailedReason     string
	FailedAt         time.Time
	TailSince        time.Time
	PendingDelete    bool
	RowGeneration    int64
	FailedSpec       int64
	UpdatedAt        time.Time
	RefusalNode      string
	RefusalReason    string
	RefusalUntil     time.Time
	RefusalOpen      bool
	RefusalAttempts  int
	Release          bool
	ReleaseEpoch     int64
	ExpectGeneration int64
	ExpectEpoch      int64
}

type yieldWriter interface {
	CommitYield(ctx context.Context, row YieldCommit) (int64, error)
}

// commitYieldLocked writes the Step result in one transaction when a cluster
// gate is configured. A zero-row update is a no-op. Without a gate the
// in-memory row is the record. Caller holds s.mu.
func (s *Scheduler) commitYieldLocked(id string, kind ownership.Kind, res ownership.Result) bool {
	if s.clusterGate == nil || s.store == nil {
		return true
	}
	switch kind {
	case ownership.EventEpochLost, ownership.EventTailUnreadable, ownership.EventTerminalError:
	default:
		return true
	}
	writer, ok := s.store.(yieldWriter)
	if !ok {
		return true
	}
	st := res.State
	row := YieldCommit{
		TaskID: id, State: string(st.Phase), Desired: string(st.Desired),
		LastError: st.LastError, Owner: st.Lease.Owner, Epoch: st.Lease.Epoch, RunID: st.RunID,
		FailedReason: st.FailedReason, FailedAt: st.FailedAt, TailSince: st.TailSince,
		PendingDelete: st.PendingDelete, FailedSpec: st.FailedSpec, UpdatedAt: time.Now(),
		ExpectGeneration: st.RowGeneration - 1, ExpectEpoch: st.Lease.Epoch,
	}
	if row.ExpectGeneration < 0 {
		row.ExpectGeneration = 0
	}
	for node, refusal := range st.Refusal {
		row.RefusalNode = node
		row.RefusalReason = refusal.Reason
		row.RefusalUntil = refusal.Until
		row.RefusalOpen = refusal.OpenEnded
		row.RefusalAttempts = refusal.Attempts
		break
	}
	for _, in := range res.Intents {
		if in.Kind == ownership.IntentRelease {
			row.Release = true
			row.ReleaseEpoch = st.Lease.Epoch
		}
	}
	s.mu.Unlock()
	ctx, cancel := s.withWriteTimeout(context.Background())
	n, err := writer.CommitYield(ctx, row)
	cancel()
	s.mu.Lock()
	if err != nil {
		log.Printf("yield commit failed task=%s err=%v", id, err)
		return true
	}
	return n > 0
}

func (s *Scheduler) noteStepIntentsLocked(id string, res ownership.Result) {
	for _, in := range res.Intents {
		if in.Kind == ownership.IntentTombstone {
			s.recordTombstoneLocked(id)
			leaveDiskTombstone(s.dataDir, id)
		}
	}
}

// terminalWindowOpen is N7. The first allowlist error starts a clock.
// Further allowlist errors stay retryable until terminalRetry has elapsed.
// A non-positive terminalRetry means the window is already closed.
func (s *Scheduler) terminalWindowOpen(id string, now time.Time) bool {
	if s.terminalRetry <= 0 {
		return false
	}
	if s.terminalSince == nil {
		s.terminalSince = map[string]time.Time{}
	}
	since, ok := s.terminalSince[id]
	if !ok {
		s.terminalSince[id] = now
		return true
	}
	return now.Sub(since) < s.terminalRetry
}

// confirmChainVerified applies ChainVerified, and ChainOwnerBack when this
// read is newer than failed_at on a CHAIN_OWNER_GONE row. A rejected step
// leaves the flag clear so task_health.chain_owner is not written.
func (s *Scheduler) confirmChainVerified(id, node string) bool {
	if node == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return false
	}
	now := time.Now()
	st := s.ownershipState(task, now)
	if st.Lease.Owner == "" || !ownership.LeaseLive(st, now) {
		st.Lease.Owner = node
		st.Lease.Present = true
		if st.Lease.Epoch <= 0 {
			st.Lease.Epoch = 1
		}
		st.Lease.ExpireAt = now.Add(st.Params.LeaseTTL)
	}
	st.Nodes = []ownership.Node{{ID: node, CanRead: true, HeartbeatAt: now}}
	res := ownership.Step(st, ownership.Event{Kind: ownership.EventChainVerified, Node: node, ReadOK: true, Epoch: st.Lease.Epoch}, now)
	if res.Decision != ownership.DecisionApplied {
		return false
	}
	next := copyOwnership(task, res.State, false, task.Epoch)
	next.OwnerWorkerID = task.OwnerWorkerID
	next.Epoch = task.Epoch
	if next.FailedReason == ownership.ReasonChainOwnerGone && next.State == StateFailed {
		st2 := s.ownershipState(next, now)
		st2.Nodes = []ownership.Node{{ID: node, CanRead: true, HeartbeatAt: now}}
		back := ownership.Step(st2, ownership.Event{Kind: ownership.EventChainOwnerBack, Node: node, ReadOK: true}, now)
		if back.Decision == ownership.DecisionApplied {
			restored := copyOwnership(next, back.State, false, next.Epoch)
			restored.OwnerWorkerID = next.OwnerWorkerID
			restored.Epoch = next.Epoch
			next = restored
		}
	}
	s.tasks[id] = next
	return true
}

// noteChainOwnerGone asks Step to enter FAILED / CHAIN_OWNER_GONE when the
// verified owner's directory is already gone. A guard rejection changes nothing.
func (s *Scheduler) noteChainOwnerGone(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return
	}
	now := time.Now()
	st := s.ownershipState(task, now)
	if task.ChainOwner != "" {
		st.Nodes = []ownership.Node{{ID: task.ChainOwner, CanRead: false, HeartbeatAt: time.Time{}}}
	}
	res := ownership.Step(st, ownership.Event{Kind: ownership.EventChainOwnerGone, Node: s.holderNode()}, now)
	if res.Decision != ownership.DecisionApplied {
		return
	}
	next := copyOwnership(task, res.State, false, task.Epoch)
	next.OwnerWorkerID = task.OwnerWorkerID
	next.Epoch = task.Epoch
	s.tasks[id] = next
}

func (s *Scheduler) clearTerminalWindow(id string) {
	delete(s.terminalSince, id)
}

// clusterProtocolVersion matches meta.ClusterProtocolVersion. This package
// cannot import meta (meta already imports tasks).
const clusterProtocolVersion int64 = 11

// refuseMixedClaimLocked registers cluster.storage and refuses the claim when
// another mode is already registered, or when a live registration is still on
// an older protocol (MIXED_VERSION). Caller holds s.mu. The lock is dropped
// for the meta calls and held again on return.
func (s *Scheduler) refuseMixedClaimLocked() error {
	gate := s.clusterGate
	if gate == nil {
		return nil
	}
	worker := s.clusterWorkerID
	mode := strings.TrimSpace(s.storageMode)
	if mode == "" {
		mode = string(ownership.StoragePerNode)
	}
	s.mu.Unlock()
	ctx, cancel := s.withLeaseTimeout(context.Background())
	registered, regErr := gate.RegisterClusterStorage(ctx, mode)
	var older int
	var countErr error
	if regErr == nil && (registered == "" || registered == mode) {
		older, countErr = gate.CountOlderProtocols(ctx, worker, clusterProtocolVersion)
	}
	cancel()
	s.mu.Lock()
	if regErr != nil {
		return regErr
	}
	if registered != "" && registered != mode {
		return fmt.Errorf("cluster.storage %s does not match registered %s", mode, registered)
	}
	if countErr != nil {
		return countErr
	}
	if older > 0 {
		s.SignalFailure(Finding{
			TaskID:   ClusterFindingTask,
			Kind:     FindingMixedVersion,
			Severity: FindingCritical,
			Detail:   "a registration is still on a protocol older than this binary; claims are refused",
		})
		return fmt.Errorf("mixed version: %d registration(s) below protocol %d", older, clusterProtocolVersion)
	}
	return nil
}
