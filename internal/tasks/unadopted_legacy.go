// Package tasks provides module-level functionality for tasks.
// input: a standalone data_dir (no task store) and this process's in-memory tasks and tombstones
// output: reconcileUnadoptedLegacy: CRITICAL TASK_UNADOPTED for each leftover task directory through SignalFailure (the legacy sink while the evaluator does not own the kind: off, shadow, before it is built), closed through SignalSuccess when the task is adopted, deleted or the directory leaves data_dir
// pos: tip QA b4c52f4 P1 (ADR 0010 §Interaction with TASK_UNADOPTED): a leftover directory is never silent in any evaluator mode
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"binlog_server/internal/health"
)

var findingTaskUnadopted = FindingKind(health.TaskUnadopted)

// reconcileUnadoptedLegacy reports leftover directories while the evaluator
// does not derive TASK_UNADOPTED itself. Called after Restore, after adopt
// and delete, and on every finding-sink retry tick (a moved directory).
func (s *Scheduler) reconcileUnadoptedLegacy() {
	if s.healthEval.Load().manages(findingTaskUnadopted) {
		return // the evaluator derives it (and closes legacy rows of the kind)
	}
	s.mu.Lock()
	if s.store != nil || s.dataDir == "" {
		s.mu.Unlock()
		return
	}
	dataDir := s.dataDir
	known := make(map[string]struct{}, len(s.tasks))
	for id := range s.tasks {
		known[id] = struct{}{}
	}
	s.mu.Unlock()
	disk, err := listDiskBackupTasks(dataDir, known)
	if err != nil {
		return // a listing failure is reported by its own path; keep what is open
	}
	now := map[string]bool{}
	s.mu.Lock()
	for _, t := range disk {
		if _, ok := s.tasks[t.ID]; ok || s.recordTombstonedLocked(t.ID) {
			continue
		}
		now[t.ID] = true
	}
	prev := s.unadoptedOpen
	s.unadoptedOpen = now
	s.mu.Unlock()
	for id := range now {
		if !prev[id] {
			s.SignalFailure(Finding{TaskID: id, Kind: findingTaskUnadopted, Severity: FindingCritical, Detail: health.TaskUnadoptedDetail(id)})
		}
	}
	for id := range prev {
		if !now[id] {
			s.SignalSuccess(id, findingTaskUnadopted, "")
		}
	}
}
