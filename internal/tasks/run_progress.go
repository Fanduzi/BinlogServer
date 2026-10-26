// Package tasks provides module-level functionality for tasks.
// input: startTask taking the task at an epoch (start or claim, before a leftover dump KILL; an operator Start resets the clock), the runner's progress reports (ReportReplicationProgress: file, pos, at-tip) and caught-up reports (ReportSourceCaughtUp: a heartbeat), and the scheduler's data_dir
// output: one progress book per task this process holds at an epoch (it outlives runs, retries and a run that never opened, design §24.15) (run start, last advance, current segment file) and the health.Progress facts read from it, with InDataDir telling whether a segment for that file exists under {data_dir}/{task_id}; TASK_NO_PROGRESS is derived from them
// pos: ADR 0009 P2-6d (docs/design/0009-phase2-health-evaluator.md §24.9): the result-based backstop for a run that holds a task and gets nowhere (#421 sc1)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"strings"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/health"
)

// runProgress is the progress book of the task while this node holds it at
// one epoch (design §24.15): it outlives a single run, so retries, a run
// that never opened (a leftover dump that cannot be KILLed) and backoff do
// not restart the clock.
type runProgress struct {
	epoch       int64
	since       time.Time
	lastAdvance time.Time
	file        string
	pos         uint32
}

// beginRunProgressLocked opens the book when this node takes the task at
// epoch (start or claim). The same epoch keeps its clock, unless reset (an
// operator Start: a new ask counts from now). Caller holds s.mu.
func (s *Scheduler) beginRunProgressLocked(id string, epoch int64, reset bool) {
	if s.progress == nil {
		s.progress = make(map[string]runProgress)
	}
	if p, ok := s.progress[id]; ok && p.epoch == epoch && !reset {
		return
	}
	s.progress[id] = runProgress{epoch: epoch, since: time.Now()}
}

// heldProgressLocked is the book while this node still holds the task at
// the book's epoch. Caller holds s.mu.
func (s *Scheduler) heldProgressLocked(id string) (runProgress, bool) {
	p, ok := s.progress[id]
	if !ok {
		return runProgress{}, false
	}
	t, ok := s.tasks[id]
	if !ok || t.Epoch != p.epoch || (s.leaseManager != nil && t.OwnerWorkerID != s.clusterWorkerID) {
		return runProgress{}, false
	}
	return p, true
}

// noteRunProgressLocked records a progress report: a new file, a later
// position, or a confirmed at-tip is an advance. Caller holds s.mu.
func (s *Scheduler) noteRunProgressLocked(id, file string, pos uint32, atTip bool) {
	p, ok := s.progress[id]
	if !ok {
		return
	}
	advanced := atTip
	if file != "" && file != p.file {
		p.file, p.pos, advanced = file, pos, true
	} else if pos > p.pos {
		p.pos, advanced = pos, true
	}
	if advanced {
		p.lastAdvance = time.Now()
	}
	s.progress[id] = p
}

// ReportSourceCaughtUp records that the run is caught up with its source:
// MySQL and MariaDB send a heartbeat only once the dump thread has sent the
// whole binlog and waits at its end. An idle source is therefore progress.
func (s *Scheduler) ReportSourceCaughtUp(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.progress[taskID]; ok {
		p.lastAdvance = time.Now()
		s.progress[taskID] = p
	}
}

// runProgressFactsLocked is the book as health facts. Caller holds s.mu.
func (s *Scheduler) runProgressFactsLocked(id string) *health.Progress {
	p, ok := s.heldProgressLocked(id)
	if !ok {
		return nil
	}
	return &health.Progress{RunSince: p.since, LastAdvance: p.lastAdvance, File: p.file, InDataDir: segmentInDataDir(s.dataDir, id, p.file)}
}

// segmentInDataDir reports whether {dataDir}/{taskID} holds a segment of
// the source file (open or sealed, any epoch, with or without a server
// identity prefix). An empty data_dir (tests only) is not checked.
func segmentInDataDir(dataDir, taskID, file string) bool {
	if strings.TrimSpace(dataDir) == "" {
		return true
	}
	file = strings.TrimSpace(file)
	dir, ok := taskBinlogDir(dataDir, taskID)
	if !ok || file == "" || strings.ContainsAny(file, `/\`) {
		return false
	}
	entries, err := binlog.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if named, ok := binlog.ClassifySegment(name); ok && (named.Source == file || strings.HasSuffix(named.Source, "."+file)) {
			return true
		}
		if name == file {
			return true
		}
	}
	return false
}

// runProgressIdle is how long the held task has gone without an advance
// (ignoring where it wrote): the cheap check of the liveness tick. false:
// this node does not hold the task.
func (s *Scheduler) runProgressIdle(id string, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.heldProgressLocked(id)
	if !ok {
		return 0, false
	}
	since := p.since
	if p.lastAdvance.After(since) {
		since = p.lastAdvance
	}
	return now.Sub(since), true
}
