package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"binlog_server/internal/tasks"
)

type createTaskRequest struct {
	Name    string              `json:"name"`
	Source  *tasks.SourceConfig `json:"source,omitempty"`
	Start   *tasks.StartConfig  `json:"start,omitempty"`
	Storage *tasks.Storage      `json:"storage,omitempty"`
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req createTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		task, err := s.tasks.CreateTask(req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Source != nil {
			if err := s.tasks.ConfigureSource(task.ID, *req.Source); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if req.Start != nil {
			if err := s.tasks.ConfigureStart(task.ID, *req.Start); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		task, err = s.tasks.GetTask(task.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, task)
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.tasks.ListTasks())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTaskAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	taskID := parts[0]
	action := parts[1]

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var err error
	switch action {
	case "start":
		err = s.tasks.StartTask(taskID)
	case "stop":
		err = s.tasks.StopTask(taskID)
	default:
		http.NotFound(w, r)
		return
	}

	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
