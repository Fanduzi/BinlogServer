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
}

type clusterOverview struct {
	TaskCount        int          `json:"task_count"`
	WorkerCount      int          `json:"worker_count"`
	RunningTaskCount int          `json:"running_task_count"`
	LeasedTaskCount  int          `json:"leased_task_count"`
	Workers          []workerItem `json:"workers"`
}

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

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, buildWorkerItems(s.tasks.ListTasks()))
}

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

func (s *Server) handleTaskRuns(w http.ResponseWriter, r *http.Request, taskID string) {
	task, err := s.tasks.GetTask(taskID)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	runs := []taskRunView{}
	if task.RunID != "" {
		runs = append(runs, taskRunView{
			RunID:     task.RunID,
			TaskID:    task.ID,
			WorkerID:  task.OwnerWorkerID,
			Epoch:     task.Epoch,
			StartedAt: task.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, runs)
}

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
