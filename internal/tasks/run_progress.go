// Package tasks provides module-level functionality for tasks.
// input: the run registration of startTask, the runner's progress reports (ReportReplicationProgress: file, pos, at-tip) and caught-up reports (ReportSourceCaughtUp: a heartbeat), and the scheduler's data_dir
// output: one progress book per run this process has open (run start, last advance, current segment file) and the health.Progress facts read from it, with InDataDir telling whether a segment for that file exists under {data_dir}/{task_id}; TASK_NO_PROGRESS is derived from them
// pos: ADR 0009 P2-6d (docs/design/0009-phase2-health-evaluator.md §24.9): the result-based backstop for a run that holds a task and gets nowhere (#421 sc1)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"strings"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/health"
)

// runProgress is one open run's progress book.
type runProgress struct {
	since       time.Time
	lastAdvance time.Time
	file        string
	pos         uint32
}

// beginRunProgressLocked starts the book of a run that was just registered.
// Caller holds s.mu.
func (s *Scheduler) beginRunProgressLocked(id string) {
	if s.progress == nil {
		s.progress = make(map[string]runProgress)
	}
	s.progress[id] = runProgress{since: time.Now()}
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
	p, ok := s.progress[id]
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
	entries, err := os.ReadDir(dir)
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

// runProgressIdle is how long the run has gone without an advance (ignoring
// where it wrote): the cheap check of the liveness tick. false: no open run.
func (s *Scheduler) runProgressIdle(id string, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if done, ok := s.runs[id]; !ok || isClosed(done) {
		return 0, false
	}
	p, ok := s.progress[id]
	if !ok {
		return 0, false
	}
	since := p.since
	if p.lastAdvance.After(since) {
		since = p.lastAdvance
	}
	return now.Sub(since), true
}
