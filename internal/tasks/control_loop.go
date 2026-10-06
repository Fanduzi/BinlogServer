// Package tasks provides module-level functionality for tasks.
// input: backup_tasks desired_run, spec_revision, applied_spec_revision, failed_spec_revision, lease hold, and whether this process has a live dump
// output: one idempotent converge decision per task (none, stop, finish stop, restart, start on the held epoch, or acquire then start), a retry that adopts a stored Stop with a newer spec instead of covering it, and the scheduler methods that apply it Finishing an idle stop fences a held dump instead of writing STOPPED before that dump is confirmed gone.
// pos: the only starter and stopper of a dump; operator Start, Stop, and spec edits write the row and this loop catches up
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"log"
	"time"
)

// convergeAction is what one control-loop pass does for one task.
type convergeAction int

const (
	convergeNone convergeAction = iota
	// convergeStopLocal cancels the dump this process has open.
	// STOPPED is written only after that run's Close returns.
	convergeStopLocal
	// convergeFinishStop writes STOPPED when this process has no dump and
	// no other worker holds the lease.
	convergeFinishStop
	// convergeRestart cancels the dump opened for an older spec.
	// The next pass starts one dump after Close returns.
	convergeRestart
	// convergeStartHeld starts one run at the epoch this worker already holds.
	convergeStartHeld
	// convergeAcquireStart takes the lease, then starts one run.
	// Acquire is the only epoch change.
	convergeAcquireStart
)

// convergeInput is the desired/observed pair the loop compares.
// Booleans are this process's view of the lease and the local dump.
type convergeInput struct {
	Desired               string
	State                 State
	SpecRevision          int64
	FailedSpecRevision    int64
	HasLocalRun           bool
	LocalAppliedSpec      int64
	LeaseHeld             bool
	LeaseHeldByOther      bool
	LeaseMissingOrExpired bool
}

// effectiveDesired is the operator intent the loop trusts.
// spec_revision 0 and applied_spec_revision 0 means a step-4 process has not
// recorded an ask. desired_run is still the 000003 default or a value
// v0.5.44/v0.5.45 left behind, so the state is the intent until reconcile
// rewrites the column. A later Start or Stop moves spec_revision off 0.
func effectiveDesired(task Task) string {
	if task.SpecRevision == 0 && task.AppliedSpecRevision == 0 {
		return legacyDesiredFromState(task.State)
	}
	switch task.DesiredRun {
	case TaskDesiredRun, TaskDesiredStop:
		return task.DesiredRun
	default:
		return legacyDesiredFromState(task.State)
	}
}

// legacyDesiredFromState is the 000003 backfill mapping.
// v0.5.44 and v0.5.45 do not keep desired_run in step with state, so a startup
// reconcile applies this to rows no step-4 writer has touched.
func legacyDesiredFromState(state State) string {
	switch state {
	case StateRunning, StateStarting, StateRetryBackoff, StateLeaseDegraded, StateRebuildingFile:
		return TaskDesiredRun
	default:
		return TaskDesiredStop
	}
}

// decideConverge is idempotent: the same input always returns the same action.
// After the action is applied, the next observation is convergeNone until
// desired_run or spec_revision changes.
func decideConverge(in convergeInput) convergeAction {
	if in.Desired == TaskDesiredStop {
		if in.HasLocalRun {
			return convergeStopLocal
		}
		if in.State == StateStopped || in.State == StateCreated || in.State == StateFailed {
			return convergeNone
		}
		if in.LeaseHeldByOther && !in.LeaseMissingOrExpired {
			return convergeNone
		}
		return convergeFinishStop
	}

	if in.State == StateFailed && in.SpecRevision == in.FailedSpecRevision {
		return convergeNone
	}
	if in.LeaseHeldByOther && !in.LeaseMissingOrExpired {
		return convergeNone
	}
	if in.HasLocalRun && in.LocalAppliedSpec == in.SpecRevision && (in.LeaseHeld || !in.LeaseMissingOrExpired) {
		return convergeNone
	}
	if in.HasLocalRun && in.LocalAppliedSpec < in.SpecRevision {
		return convergeRestart
	}
	if in.HasLocalRun {
		return convergeNone
	}
	if in.LeaseHeld {
		return convergeStartHeld
	}
	return convergeAcquireStart
}

var errLocalRunOpen = errors.New("local run still open")

// convergeTask applies decideConverge once. started is true when this pass
// launched a run. A lost race with a run that is already open is not an error.
func (s *Scheduler) convergeTask(id string) (bool, error) {
	task, err := s.authoritativeTask(id)
	if err != nil {
		return false, err
	}
	in := s.convergeInput(task)
	switch decideConverge(in) {
	case convergeNone:
		return false, nil
	case convergeStopLocal:
		s.cancelLocal(id)
		return false, nil
	case convergeFinishStop:
		return false, s.finishIdleStop(task)
	case convergeRestart:
		done := s.cancelLocal(id)
		if done != nil {
			select {
			case <-done:
			case <-time.After(s.closeWait()):
				return false, errLocalRunOpen
			}
		}
		return s.convergeTask(id)
	case convergeStartHeld:
		err = s.startTask(id, taskStart{resetBudget: false, acquireLease: false})
	case convergeAcquireStart:
		err = s.startTask(id, taskStart{resetBudget: false, acquireLease: true})
	default:
		return false, nil
	}
	if errors.Is(err, errLocalRunOpen) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Scheduler) closeWait() time.Duration {
	if s.internalWriteTimeout > 0 {
		return s.internalWriteTimeout
	}
	return 5 * time.Second
}

func (s *Scheduler) convergeInput(task Task) convergeInput {
	s.mu.Lock()
	hasLocal := false
	localSpec := int64(0)
	if done, ok := s.runs[task.ID]; ok && !isClosed(done) {
		hasLocal = true
		localSpec = s.runSpec[task.ID]
		if mem, ok := s.tasks[task.ID]; ok {
			task.OwnerWorkerID = mem.OwnerWorkerID
			task.Epoch = mem.Epoch
		}
	} else if s.store == nil {
		if mem, ok := s.tasks[task.ID]; ok {
			task = mem
		}
	}
	s.mu.Unlock()

	held, other, free := s.leaseView(task)
	return convergeInput{
		Desired:               effectiveDesired(task),
		State:                 task.State,
		SpecRevision:          task.SpecRevision,
		FailedSpecRevision:    task.FailedSpecRevision,
		HasLocalRun:           hasLocal,
		LocalAppliedSpec:      localSpec,
		LeaseHeld:             held,
		LeaseHeldByOther:      other,
		LeaseMissingOrExpired: free,
	}
}

func (s *Scheduler) authoritativeTask(id string) (Task, error) {
	if s.store != nil {
		return s.readStoredTask(s.store, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return task, nil
}

func (s *Scheduler) leaseView(task Task) (held, other, free bool) {
	if s.leaseManager == nil {
		return true, false, false
	}
	if task.OwnerWorkerID == "" || task.Epoch <= 0 {
		return false, false, true
	}
	ctx, cancel := s.withLeaseTimeout(context.Background())
	defer cancel()
	if task.OwnerWorkerID != s.clusterWorkerID {
		ok, err := s.leaseManager.Verify(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
		if err != nil || ok {
			return false, true, false
		}
		return false, false, true
	}
	ok, err := s.leaseManager.Verify(ctx, task.ID, s.clusterWorkerID, task.Epoch)
	if err != nil || !ok {
		return false, false, true
	}
	return true, false, false
}

func (s *Scheduler) cancelLocal(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	done := s.runs[id]
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	if isClosed(done) {
		return nil
	}
	return done
}

func (s *Scheduler) finishIdleStop(task Task) error {
	task.DesiredRun = TaskDesiredStop
	return s.settleForeignStop(task)
}

// adoptNewerStopLocked copies a stored Stop onto this run.
// A retry snapshot still has the spec it opened. Writing that snapshot
// would cover the operator Stop. Caller holds s.mu. The store read releases it.
func (s *Scheduler) adoptNewerStopLocked(id string, openedSpec int64) bool {
	if s.store == nil {
		return false
	}
	fresh, err := s.intentLocked(id)
	if err != nil {
		return false
	}
	// A newer Start or password change stays a run. Only a stored Stop ends this retry.
	stop := fresh.DesiredRun == TaskDesiredStop || fresh.State == StateStopping || fresh.State == StateStopped
	if !stop || fresh.SpecRevision < openedSpec {
		return false
	}
	mem, ok := s.tasks[id]
	if !ok {
		return true
	}
	if fresh.DesiredRun == TaskDesiredStop {
		mem.DesiredRun = TaskDesiredStop
	}
	if fresh.SpecRevision > mem.SpecRevision {
		mem.SpecRevision = fresh.SpecRevision
	}
	if mem.State != StateStopped && mem.State != StateFailed {
		mem.State = StateStopping
	}
	s.tasks[id] = mem
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	return true
}

func (s *Scheduler) runSupersededLocked(id string, openedSpec int64) bool {
	current, ok := s.tasks[id]
	if !ok {
		return true
	}
	if current.DesiredRun == TaskDesiredStop {
		return true
	}
	if current.SpecRevision != openedSpec {
		return true
	}
	return false
}

func (s *Scheduler) wantReopenLocked(fresh Task, ferr error, openedSpec int64) (Task, bool) {
	if ferr != nil || fresh.DesiredRun != TaskDesiredRun || fresh.SpecRevision <= openedSpec || fresh.State == StateFailed {
		return Task{}, false
	}
	return fresh, true
}

func (s *Scheduler) keepOwnerForReopenLocked(id string, run Task, fresh Task) {
	mem, ok := s.tasks[id]
	if !ok {
		mem = fresh
	}
	if mem.OwnerWorkerID == "" {
		mem.OwnerWorkerID = run.OwnerWorkerID
		mem.Epoch = run.Epoch
	}
	mem.DesiredRun = fresh.DesiredRun
	mem.SpecRevision = fresh.SpecRevision
	mem.Source = fresh.Source
	mem.Start = fresh.Start
	mem.Storage = fresh.Storage
	mem.ClusterKey = fresh.ClusterKey
	mem.RetryAttempt = fresh.RetryAttempt
	mem.ConsecutiveSourceFailures = fresh.ConsecutiveSourceFailures
	mem.FailedSpecRevision = fresh.FailedSpecRevision
	s.tasks[id] = mem
}

func operatorStartState(state State) bool {
	switch state {
	case StateCreated, StateStopped, StateFailed, StateRetryBackoff, StateStopping:
		return true
	default:
		return false
	}
}

// writeStartIntent records operator Start. It does not Acquire and it does not
// start a dump. A control-plane process (no runner) also mirrors STARTING with
// an empty owner so an older claim query still sees the row.
func (s *Scheduler) writeStartIntent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if current, ok := s.tasks[id]; ok && current.State == StateStopping {
		if err := s.reloadIdleTaskFromStoreLocked(id); err != nil {
			return err
		}
	}
	if err := s.mergeLiveIntentLocked(id); err != nil {
		return err
	}

	task, ok := s.tasks[id]
	if !ok {
		err := readOnlyDiskBackup(s.store, s.dataDir, id)
		if err != nil {
			return err
		}
		return ErrTaskNotFound
	}
	if !operatorStartState(task.State) {
		return errors.New("cannot start from state " + string(task.State))
	}
	if task.Source.Host == "" || task.Source.Port == 0 || task.Source.User == "" {
		return ErrInvalidSourceConfig
	}
	if err := s.validateMetadataSourceEndpoint(task.Source); err != nil {
		return err
	}

	task.DesiredRun = TaskDesiredRun
	task.SpecRevision++
	task.RetryAttempt = 0
	task.ConsecutiveSourceFailures = 0
	task.LastError = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}

	if s.runner == nil {
		if s.leaseManager == nil {
			return ErrRunnerNotConfigured
		}
		return s.markStartDispatchedLocked(task)
	}
	task.State = StateStarting
	s.tasks[id] = task
	return s.persistTaskLocked(task)
}

// intentLocked reads the operator ask. The caller holds s.mu.
// A memory row with a higher spec_revision wins over a store read that
// started before that ask was committed, so a Stop exit does not cover a Start.
func (s *Scheduler) intentLocked(id string) (Task, error) {
	mem, hasMem := s.tasks[id]
	if s.store == nil {
		if !hasMem {
			return Task{}, ErrTaskNotFound
		}
		return mem, nil
	}
	store := s.store
	s.mu.Unlock()
	item, err := s.readStoredTask(store, id)
	s.mu.Lock()
	if current, ok := s.tasks[id]; ok {
		mem = current
		hasMem = true
	}
	if err != nil {
		if hasMem {
			return mem, nil
		}
		return Task{}, err
	}
	if hasMem && mem.SpecRevision > item.SpecRevision {
		item.DesiredRun = mem.DesiredRun
		item.SpecRevision = mem.SpecRevision
		item.Source = mem.Source
		item.Start = mem.Start
		item.Storage = mem.Storage
		item.ClusterKey = mem.ClusterKey
		item.Name = mem.Name
		item.RetryAttempt = mem.RetryAttempt
		item.ConsecutiveSourceFailures = mem.ConsecutiveSourceFailures
		item.FailedSpecRevision = mem.FailedSpecRevision
		item.State = mem.State
	}
	return item, nil
}

func (s *Scheduler) mergeLiveIntentLocked(id string) error {
	if s.store == nil {
		return nil
	}
	done, ok := s.runs[id]
	if !ok || isClosed(done) {
		return nil
	}
	seen := s.persisted[id].published
	store := s.store
	s.mu.Unlock()
	item, err := s.readStoredTask(store, id)
	s.mu.Lock()
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return nil
		}
		return err
	}
	if s.staleStoreReadLocked(id, seen) {
		return nil
	}
	mem, ok := s.tasks[id]
	if !ok {
		return nil
	}
	mem.Source = item.Source
	mem.Start = item.Start
	mem.Storage = item.Storage
	mem.ClusterKey = item.ClusterKey
	mem.Name = item.Name
	mem.SpecRevision = item.SpecRevision
	mem.DesiredRun = item.DesiredRun
	mem.FailedSpecRevision = item.FailedSpecRevision
	mem.AppliedSpecRevision = item.AppliedSpecRevision
	mem.RetryAttempt = item.RetryAttempt
	mem.ConsecutiveSourceFailures = item.ConsecutiveSourceFailures
	s.tasks[id] = mem
	return nil
}

// legacyDesiredReconciler rewrites desired_run for rows this binary has not applied.
type legacyDesiredReconciler interface {
	ReconcileLegacyDesiredRun(ctx context.Context) (int64, error)
}

// ReconcileLegacyDesiredRun asks the store to fix desired_run before the loop
// trusts it. Rows with spec_revision=0 and applied_spec_revision=0 were not
// written by a step-4 process. Repeating the call is a no-op once they match.
func (s *Scheduler) ReconcileLegacyDesiredRun(ctx context.Context) (int64, error) {
	if s == nil || s.store == nil {
		return 0, nil
	}
	rec, ok := s.store.(legacyDesiredReconciler)
	if !ok {
		return 0, nil
	}
	n, err := rec.ReconcileLegacyDesiredRun(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("reconciled desired_run for %d task(s) with spec_revision=0 and applied_spec_revision=0", n)
	}
	return n, nil
}
