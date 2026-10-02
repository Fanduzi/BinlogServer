// Package api provides module-level functionality for api.
// input: HTTP requests, router params, ListClusterObservation, worker heartbeats, GetTask, and ListRuns
// output: REST cluster overview/workers from the unfiltered store ownership copy (store list errors are 5xx); a heartbeats-less in-process owner is single_process with worker_count 0, plus lease and run history views
// pos: external control-plane API layer for cluster observation and per-task lease/run reads
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

// clusterOverview is GET /api/cluster/overview.
// SingleProcess is true when this process pulls tasks and no worker heartbeat exists.
type clusterOverview struct {
	TaskCount        int          `json:"task_count"`
	WorkerCount      int          `json:"worker_count"`
	RunningTaskCount int          `json:"running_task_count"`
	LeasedTaskCount  int          `json:"leased_task_count"`
	SingleProcess    bool         `json:"single_process"`
	Workers          []workerItem `json:"workers"`
}

const workerOnlineThreshold = 15 * time.Second

// localProcessWorkerID is the in-process owner app assigns when this process
// pulls tasks itself. It is not a cluster worker unless a heartbeat row exists.
const localProcessWorkerID = "standalone"

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

// overviewWorkers drops the in-process owner when it has no heartbeat so
// overview worker_count matches GET /api/workers. A heartbeat for that id is
// copied onto the overview row. singleProcess reports that this process pulls.
func overviewWorkers(owners []workerItem, heartbeats []tasks.WorkerHeartbeat, now time.Time) ([]workerItem, bool) {
	hbByID := make(map[string]tasks.WorkerHeartbeat, len(heartbeats))
	for _, hb := range heartbeats {
		hbByID[hb.WorkerID] = hb
	}
	workers := make([]workerItem, 0, len(owners))
	droppedLocal := false
	for _, owner := range owners {
		if owner.WorkerID != localProcessWorkerID {
			workers = append(workers, owner)
			continue
		}
		hb, ok := hbByID[owner.WorkerID]
		if !ok {
			droppedLocal = true
			continue
		}
		workers = append(workers, applyHeartbeat(owner, hb, now))
	}
	singleProcess := droppedLocal && len(workers) == 0 && len(heartbeats) == 0
	return workers, singleProcess
}

func applyHeartbeat(stats workerItem, hb tasks.WorkerHeartbeat, now time.Time) workerItem {
	online := hb.Status == "ONLINE" && !hb.LastSeenAt.IsZero() && now.Sub(hb.LastSeenAt) <= workerOnlineThreshold
	status := hb.Status
	if !online {
		status = "OFFLINE"
	}
	stats.WorkerID = hb.WorkerID
	stats.Host = hb.Host
	stats.Version = hb.Version
	stats.LastSeenAt = hb.LastSeenAt
	stats.Status = status
	stats.Online = online
	stats.UpdatedAt = hb.LastSeenAt
	stats.HasUpdated = !hb.LastSeenAt.IsZero()
	return stats
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

	observed, err := s.tasks.ListClusterObservation(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	taskStats := make(map[string]workerItem, len(observed))
	for _, item := range buildWorkerItems(observed) {
		taskStats[item.WorkerID] = item
	}

	now := time.Now()
	items := make([]workerItem, 0, len(heartbeats))
	for _, hb := range heartbeats {
		items = append(items, applyHeartbeat(taskStats[hb.WorkerID], hb, now))
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

	items, err := s.tasks.ListClusterObservation(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	heartbeats, err := s.tasks.ListWorkerHeartbeats(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	workers, singleProcess := overviewWorkers(buildWorkerItems(items), heartbeats, time.Now())
	resp := clusterOverview{
		TaskCount:     len(items),
		WorkerCount:   len(workers),
		SingleProcess: singleProcess,
		Workers:       workers,
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
