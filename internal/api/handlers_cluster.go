// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses and status codes for task/cluster operations
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"binlog_server/internal/tasks"
)

type workerItem struct {
	WorkerID   string    `json:"worker_id"`
	Host       string    `json:"host,omitempty"`
	Version    string    `json:"version,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Status     string    `json:"status"`
	Online     bool      `json:"online"`
	TaskCount  int       `json:"task_count"`
	Running    int       `json:"running"`
	Leased     int       `json:"leased"`
	UpdatedAt  time.Time `json:"updated_at"`
	HasUpdated bool      `json:"has_updated"`
}

type taskLeaseView struct {
	TaskID        string      `json:"task_id"`
	OwnerWorkerID string      `json:"owner_worker_id,omitempty"`
	Epoch         int64       `json:"epoch"`
	State         tasks.State `json:"state"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

type taskRunView struct {
	RunID     string    `json:"run_id"`
	TaskID    string    `json:"task_id"`
	WorkerID  string    `json:"worker_id,omitempty"`
	Epoch     int64     `json:"epoch"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	EndReason string    `json:"end_reason,omitempty"`
}

type clusterOverview struct {
	TaskCount        int          `json:"task_count"`
	WorkerCount      int          `json:"worker_count"`
	RunningTaskCount int          `json:"running_task_count"`
	LeasedTaskCount  int          `json:"leased_task_count"`
	Workers          []workerItem `json:"workers"`
}

const workerOnlineThreshold = 15 * time.Second

// buildWorkerItems 从任务 ownership 视图构建 worker 维度聚合结果。
func buildWorkerItems(items []tasks.Task) []workerItem {
	byID := make(map[string]*workerItem)
	for _, task := range items {
		if task.OwnerWorkerID == "" {
			continue
		}
		entry, ok := byID[task.OwnerWorkerID]
		if !ok {
			entry = &workerItem{WorkerID: task.OwnerWorkerID}
			byID[task.OwnerWorkerID] = entry
		}
		entry.TaskCount++
		if task.State == tasks.StateRunning {
			entry.Running++
		}
		if task.Epoch > 0 {
			entry.Leased++
		}
		if !task.UpdatedAt.IsZero() {
			if entry.UpdatedAt.IsZero() || task.UpdatedAt.After(entry.UpdatedAt) {
				entry.UpdatedAt = task.UpdatedAt
				entry.HasUpdated = true
			}
		}
	}

	workers := make([]workerItem, 0, len(byID))
	for _, item := range byID {
		workers = append(workers, *item)
	}
	sort.Slice(workers, func(i, j int) bool {
		return workers[i].WorkerID < workers[j].WorkerID
	})
	return workers
}

// handleWorkers 返回 worker 在线状态与任务统计列表。
func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	heartbeats, err := s.tasks.ListWorkerHeartbeats(parseLimit(r, 200))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	taskStats := make(map[string]workerItem)
	for _, task := range s.tasks.ListTasks() {
		if task.OwnerWorkerID == "" {
			continue
		}
		entry := taskStats[task.OwnerWorkerID]
		entry.TaskCount++
		if task.State == tasks.StateRunning {
			entry.Running++
		}
		if task.Epoch > 0 {
			entry.Leased++
		}
		taskStats[task.OwnerWorkerID] = entry
	}

	now := time.Now()
	items := make([]workerItem, 0, len(heartbeats))
	for _, hb := range heartbeats {
		stats := taskStats[hb.WorkerID]
		online := hb.Status == "ONLINE" && !hb.LastSeenAt.IsZero() && now.Sub(hb.LastSeenAt) <= workerOnlineThreshold
		status := hb.Status
		if !online {
			status = "OFFLINE"
		}
		items = append(items, workerItem{
			WorkerID:   hb.WorkerID,
			Host:       hb.Host,
			Version:    hb.Version,
			LastSeenAt: hb.LastSeenAt,
			Status:     status,
			Online:     online,
			TaskCount:  stats.TaskCount,
			Running:    stats.Running,
			Leased:     stats.Leased,
			UpdatedAt:  hb.LastSeenAt,
			HasUpdated: !hb.LastSeenAt.IsZero(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].WorkerID < items[j].WorkerID
	})
	writeJSON(w, http.StatusOK, items)
}

// handleTaskLease 返回指定任务的 lease 快照信息。
func (s *Server) handleTaskLease(w http.ResponseWriter, r *http.Request, taskID string) {
	task, err := s.tasks.GetTask(taskID)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, taskLeaseView{
		TaskID:        task.ID,
		OwnerWorkerID: task.OwnerWorkerID,
		Epoch:         task.Epoch,
		State:         task.State,
		UpdatedAt:     task.UpdatedAt,
	})
}

// handleTaskRuns 返回指定任务的运行历史记录。
func (s *Server) handleTaskRuns(w http.ResponseWriter, r *http.Request, taskID string) {
	limit := parseLimit(r, 10)
	if limit > 200 {
		limit = 200
	}

	runs, err := s.tasks.ListRuns(taskID, limit)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]taskRunView, 0, len(runs))
	for _, run := range runs {
		out = append(out, taskRunView{
			RunID:     run.RunID,
			TaskID:    run.TaskID,
			WorkerID:  run.WorkerID,
			Epoch:     run.Epoch,
			StartedAt: run.StartedAt,
			EndedAt:   run.EndedAt,
			EndReason: run.EndReason,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleClusterOverview 返回集群概览数据（worker、lease、任务分布）。
func (s *Server) handleClusterOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	items := s.tasks.ListTasks()
	workers := buildWorkerItems(items)
	resp := clusterOverview{
		TaskCount:   len(items),
		WorkerCount: len(workers),
		Workers:     workers,
	}
	for _, task := range items {
		if task.State == tasks.StateRunning {
			resp.RunningTaskCount++
		}
		if task.OwnerWorkerID != "" && task.Epoch > 0 {
			resp.LeasedTaskCount++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
