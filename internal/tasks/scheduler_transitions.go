// Package tasks provides module-level functionality for tasks.
// input: locked task snapshots plus runner and lease lifecycle signals
// output: private state/event/persistence transitions plus best-effort persistence failure logs
// pos: centralized lifecycle transition recipes shared by scheduler orchestration loops
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"log"
	"time"
)

func (s *Scheduler) markStartDispatchedLocked(task Task) error {
	task.State = StateStarting
	task.LastError = ""
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = task
	s.appendEventLocked(task.ID, "TASK_START_DISPATCHED", "task start dispatched to worker", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markStartingLocked(task Task) error {
	task.State = StateStarting
	task.LastError = ""
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = task
	s.appendEventLocked(task.ID, "TASK_STARTED", "task started", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markRetryBackoffLocked(task Task, message string) error {
	task.State = StateRetryBackoff
	task.LastError = message
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = task
	s.appendEventLocked(task.ID, "TASK_RETRY_BACKOFF", "task entered retry backoff", message)
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markRetryingLocked(task *Task) error {
	task.State = StateStarting
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = *task
	s.appendEventLocked(task.ID, "TASK_RETRYING", "retrying runner", "")
	return s.persistTaskLocked(*task)
}

func (s *Scheduler) markRunnerReadyLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok || task.State == StateStopped || task.State == StateStopping || task.State == StateRunning {
		return nil
	}
	task.State = StateRunning
	task.LastError = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_RUNNING", "runner is running", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markStoppingLocked(task Task) error {
	task.State = StateStopping
	task.UpdatedAt = time.Now()
	s.tasks[task.ID] = task
	s.appendEventLocked(task.ID, "TASK_STOPPING", "task stopping", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markLeaseDegradedLocked(id, message string, now time.Time) error {
	task, ok := s.tasks[id]
	if !ok || task.State == StateStopping || task.State == StateStopped {
		return nil
	}
	if task.State != StateLeaseDegraded {
		task.State = StateLeaseDegraded
		s.appendEventLocked(id, "TASK_LEASE_DEGRADED", "lease renew failed", message)
	}
	task.LastError = message
	task.UpdatedAt = now
	s.tasks[id] = task
	return s.persistTaskLocked(task)
}

func (s *Scheduler) markLeaseRenewedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok || task.State != StateLeaseDegraded {
		return nil
	}
	task.State = StateRunning
	task.LastError = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_LEASE_RENEWED", "lease renewed", "")
	return s.persistTaskLocked(task)
}

func (s *Scheduler) failSafeStopLocked(id, eventType, message string) error {
	task, ok := s.tasks[id]
	if !ok || task.State == StateStopping || task.State == StateStopped {
		return nil
	}
	task.State = StateStopping
	task.LastError = message
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, eventType, message, "")
	err := s.persistTaskLocked(task)
	if cancel, ok := s.cancels[id]; ok {
		// 这里只发取消信号；最终 STOPPED 由 runTask defer 统一收敛。
		cancel()
		delete(s.cancels, id)
	}
	return err
}

func logTransitionPersistError(id string, state State, err error) {
	if err != nil {
		log.Printf("persist task transition failed task=%s state=%s err=%v", id, state, err)
	}
}

// markFailedLocked 将任务收敛到 FAILED，用于不可恢复的源库/配置错误。
// StartTask 允许从 FAILED 再次启动，方便值班改完密码/配置后重试。
func (s *Scheduler) markFailedLocked(id, message string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	if task.State == StateFailed {
		task.LastError = message
		s.tasks[id] = task
		return s.persistTaskLocked(task)
	}
	task.State = StateFailed
	task.LastError = message
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_FAILED", "task failed", message)
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	return s.persistTaskLocked(task)
}

// markStoppedLocked 将任务收敛到最终 STOPPED 并清理运行时 ownership 字段。
func (s *Scheduler) markStoppedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok || task.State == StateStopped {
		return nil
	}
	task.State = StateStopped
	// STOPPED 是“无执行归属”的稳定终态，清空运行时 ownership 字段。
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPED", "task stopped", "")
	return s.persistTaskLocked(task)
}
