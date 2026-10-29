// Package tasks provides module-level functionality for tasks.
// input: the in-memory task row, ownership.Step, and the cluster storage mode
// output: task phase, desired, failed_reason, and lease fields copied from one Step result; SOURCE_SWITCHOVER and a used SOURCE_UNREACHABLE budget are terminal FAILED and are not claimed again; a loaded row_generation of 0 still fences the yield; a memory lease has no task_leases row, so that step is kept and the task upsert writes it; a holder event synthesizes a lease only for the decision and does not invent an epoch on a row that had none; a late event that updates nothing is a no-op; an open task_node_refusal row blocks Acquire after restart; Start and DumpReady delete those rows
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

	"binlog_server/internal/mysqltls"
	"binlog_server/internal/ownership"
)

const localHolderNode = "local"

// failureKind maps a runner message onto the one Step event that may write it.
// SOURCE_AUTH and LOG_BIN_OFF retry, then become terminal. SOURCE_SWITCHOVER
// is terminal on the first occurrence: the address now reaches another server
// and every node would see the same fact. SOURCE_UNREACHABLE is terminal only
// when the scheduler has already used the budget of 10; that call is the
// give-up, and it must not yield. STORAGE_INCONSISTENT is terminal too: the
// contradiction lives in the shared catalog or object store, so a peer that
// takes over reads the same facts and yields again. A missing segment on
// this worker is a tail yield. Everything else is EpochLost.
func failureKind(message string) (ownership.Kind, ownership.Cause) {
	switch {
	case strings.Contains(message, CodeSourceAccessDenied):
		return ownership.EventTerminalError, ownership.CauseSourceAuth
	case strings.Contains(message, CodeSourceLogBinOff):
		return ownership.EventTerminalError, ownership.CauseLogBinOff
	case strings.Contains(message, CodeSourceSwitchover):
		return ownership.EventTerminalError, ownership.CauseSourceSwitchover
	case strings.Contains(message, CodeSourceUnreachable):
		return ownership.EventTerminalError, ownership.CauseSourceUnreachable
	case strings.Contains(message, CodeStorageInconsistent):
		return ownership.EventTerminalError, ownership.CauseStorageInconsistent
	case strings.Contains(message, CodeSegmentNotOnWorker):
		return ownership.EventTailUnreadable, ownership.CauseSegmentNotOnWorker
	case mysqltls.IsVerifyFailure(errors.New(message)):
		return ownership.EventTerminalError, ownership.CauseSourceAuth
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
// gate is configured and the lease row lives in task_leases. A zero-row
// update is a no-op. A memory lease has no such row: CommitYield would
// report the lease lost and the caller would drop FAILED. That step stays
// in memory and the following task upsert writes it. Without a gate the
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
	if !s.leaseStoredInMeta() {
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
	// A step that did not bump leaves a negative expectation. 0 is a real
	// loaded generation, so this only clamps the unread underflow. The SQL
	// still requires row_generation = 0; it does not skip the predicate.
	if row.ExpectGeneration < 0 {
		row.ExpectGeneration = 0
	}
	if task, ok := s.tasks[id]; ok && task.RowGenerationKnown {
		row.ExpectGeneration = task.RowGeneration
	} else if _, ok := s.tasks[id]; ok {
		// The column was not on this copy. Read it before the yield so a
		// fabricated 0 does not stand in for a generation the row already has.
		s.mu.Unlock()
		ctx, cancel := s.withReadTimeout(context.Background())
		loaded, loadedOK := s.readRowGeneration(ctx, id)
		cancel()
		s.mu.Lock()
		if !loadedOK {
			log.Printf("yield skipped task=%s: row_generation was not loaded", id)
			return false
		}
		row.ExpectGeneration = loaded
		s.noteRowGenerationLocked(id, loaded)
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

// leaseStoredInMeta reports that this process's Acquire writes task_leases.
// MemoryLease, and a process with no lease manager, do not.
func (s *Scheduler) leaseStoredInMeta() bool {
	if s == nil || s.leaseManager == nil {
		return false
	}
	_, mem := s.leaseManager.(*MemoryLease)
	return !mem
}

func (s *Scheduler) readRowGeneration(ctx context.Context, id string) (int64, bool) {
	if s == nil || s.store == nil {
		return 0, false
	}
	reader, ok := s.store.(rowGenerationReader)
	if !ok {
		return 0, false
	}
	gen, err := reader.TaskRowGeneration(ctx, id)
	if err != nil {
		return 0, false
	}
	return gen, true
}

func (s *Scheduler) noteStepIntentsLocked(id string, res ownership.Result) {
	clear := false
	for _, in := range res.Intents {
		if in.Kind == ownership.IntentTombstone {
			s.recordTombstoneLocked(id)
			leaveDiskTombstone(s.dataDir, id)
		}
		if in.Kind == ownership.IntentDeleteRefusals {
			clear = true
		}
	}
	if clear {
		s.deleteStoredRefusalsLocked(id)
	}
}

// refuseStoredClaimLocked reads task_node_refusal. A restart has an empty
// memory map; the row is what still blocks Acquire. Caller holds s.mu.
func (s *Scheduler) refuseStoredClaimLocked(id string) error {
	store := s.store
	node := s.clusterWorkerID
	q, ok := store.(interface {
		RefusalOpen(context.Context, string, string) (bool, error)
	})
	if !ok {
		return nil
	}
	s.mu.Unlock()
	ctx, cancel := s.withLeaseTimeout(context.Background())
	open, err := q.RefusalOpen(ctx, id, node)
	cancel()
	s.mu.Lock()
	if err != nil {
		return err
	}
	if open {
		return ErrLeaseNotAcquired
	}
	return nil
}

// deleteStoredRefusalsLocked removes the task's refusal rows. Caller holds s.mu.
func (s *Scheduler) deleteStoredRefusalsLocked(id string) {
	store := s.store
	del, ok := store.(interface {
		DeleteTaskRefusals(context.Context, string) error
	})
	if !ok {
		return
	}
	s.mu.Unlock()
	ctx, cancel := s.withWriteTimeout(context.Background())
	err := del.DeleteTaskRefusals(ctx, id)
	cancel()
	s.mu.Lock()
	if err != nil {
		log.Printf("delete task refusals task=%s err=%v", id, err)
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
// an older protocol (MIXED_VERSION), and resolves that finding once
// every live registration speaks the current protocol. Caller holds s.mu. The lock is dropped
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
	s.SignalSuccess(ClusterFindingTask, FindingMixedVersion, "")
	return nil
}

// claimEligibilityLocked rejects a start the state machine does not allow.
// Caller holds s.mu. The claim paths are: a clean dispatch STARTING row
// (owner/epoch/run empty), an expired RUNNING/LEASE_DEGRADED takeover after
// Acquire (a failed Acquire changes nothing), or an idle local resume —
// boot attach / leaseless standalone — including a STOPPING row the operator
// restarted before Close returned; the same owner reopens that epoch.
func (s *Scheduler) claimEligibilityLocked(task Task, hasLiveRun bool) error {
	canClaimDispatched := task.State == StateStarting &&
		task.OwnerWorkerID == "" &&
		task.Epoch == 0 &&
		task.RunID == "" &&
		s.runner != nil &&
		s.leaseManager != nil
	canTakeoverExpired := s.runner != nil && s.leaseManager != nil && !hasLiveRun &&
		(task.State == StateRunning || task.State == StateLeaseDegraded)
	canResumeIdle := s.runner != nil && !hasLiveRun &&
		(task.State == StateRunning || task.State == StateLeaseDegraded || task.State == StateStarting || task.State == StateRetryBackoff ||
			(task.State == StateStopping && effectiveDesired(task) == TaskDesiredRun)) &&
		(s.leaseManager == nil || task.OwnerWorkerID == s.clusterWorkerID)
	if task.State != StateCreated && task.State != StateStopped && task.State != StateRetryBackoff && task.State != StateFailed && !canClaimDispatched && !canTakeoverExpired && !canResumeIdle {
		return fmt.Errorf("cannot start from state %s", task.State)
	}
	return nil
}

// claimLeaseGateLocked applies the refusal gates a lease claim must pass:
// a mixed-version cluster, an open refusal row for this node, then a stored
// refusal for this task. The memory row is reloaded last because a gate may
// have rewritten it. Caller holds s.mu.
func (s *Scheduler) claimLeaseGateLocked(id string) (Task, error) {
	if err := s.refuseMixedClaimLocked(); err != nil {
		return Task{}, err
	}
	if s.refusalBlocksLocked(id, s.clusterWorkerID, time.Now()) {
		return Task{}, ErrLeaseNotAcquired
	}
	if err := s.refuseStoredClaimLocked(id); err != nil {
		return Task{}, err
	}
	task, ok := s.tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return task, nil
}

// WithStorageMode sets cluster.storage. Anything other than shared is per_node.
func WithStorageMode(mode string) Option {
	return func(s *Scheduler) {
		s.storageMode = strings.TrimSpace(mode)
	}
}

// WithRefusalCap sets the shared and EpochLost refusal cap.
func WithRefusalCap(d time.Duration) Option {
	return func(s *Scheduler) {
		s.refusalCap = d
	}
}

// ClusterGate is the meta side of a claim: register cluster.storage, and
// count registrations whose protocol is older than this binary.
type ClusterGate interface {
	RegisterClusterStorage(ctx context.Context, mode string) (registered string, err error)
	CountOlderProtocols(ctx context.Context, self string, version int64) (int, error)
}

// WithClusterGate turns on the claim refusal for a storage mismatch or a
// mixed-version registration. Claims skip it when the gate is nil.
func WithClusterGate(g ClusterGate) Option {
	return func(s *Scheduler) {
		s.clusterGate = g
	}
}

// rowGenerationReader is the meta store's row_generation column. A fake
// store does not implement it. After a task write, the scheduler copies
// the committed value so the next yield fences on it instead of sending 0.
type rowGenerationReader interface {
	TaskRowGeneration(ctx context.Context, id string) (int64, error)
}

// noteRowGenerationLocked records a generation this process just read.
// A later read of an older value does not move it backwards: pending-dump
// saves and task upserts both bump the column, and the next yield has to
// name the newer one. A loaded 0 is kept. Caller holds s.mu.
func (s *Scheduler) noteRowGenerationLocked(id string, gen int64) {
	task, ok := s.tasks[id]
	if !ok {
		return
	}
	if task.RowGenerationKnown && gen < task.RowGeneration {
		return
	}
	task.RowGeneration = gen
	task.RowGenerationKnown = true
	s.tasks[id] = task
}

// WithClusterMode marks this process as mode cluster. Coverage and restore
// plan then refuse instead of answering from a partial per-node directory.
// Standalone workers also hold a lease manager, so the lease is not the signal.
func WithClusterMode() Option {
	return func(s *Scheduler) {
		s.clusterMode = true
	}
}

// WithClusterLeaseManager 启用 cluster lease 管理。
func WithClusterLeaseManager(manager LeaseManager) Option {
	return func(s *Scheduler) {
		s.leaseManager = manager
	}
}

// WithClusterWorkerID 设置当前实例在 cluster 中的 worker_id。
func WithClusterWorkerID(workerID string) Option {
	return func(s *Scheduler) {
		s.clusterWorkerID = workerID
	}
}

// WithClusterLease 配置 lease 的 TTL、续租周期和降级宽限时间。
func WithClusterLease(ttl, renewInterval, grace time.Duration) Option {
	return func(s *Scheduler) {
		if ttl > 0 {
			s.leaseTTL = ttl
		}
		if renewInterval > 0 {
			s.leaseRenewInterval = renewInterval
		}
		if grace > 0 {
			s.leaseGrace = grace
		}
	}
}
