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

type updateTaskRequest struct {
	Name    *string             `json:"name,omitempty"`
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
		if req.Storage != nil {
			if err := s.tasks.ConfigureStorage(task.ID, *req.Storage); err != nil {
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
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")
	if parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	taskID := parts[0]
	if len(parts) == 1 {
		s.handleTaskEntity(w, r, taskID)
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	action := parts[1]

	if r.Method != http.MethodPost {
		if r.Method == http.MethodGet && action == "checkpoint" {
			checkpoint, ok, err := s.tasks.GetCheckpoint(r.Context(), taskID)
			if err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !ok {
				http.Error(w, "checkpoint not found", http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, checkpoint)
			return
		}
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

func (s *Server) handleTaskEntity(w http.ResponseWriter, r *http.Request, taskID string) {
	switch r.Method {
	case http.MethodGet:
		task, err := s.tasks.GetTask(taskID)
		if err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, task)
	case http.MethodPut:
		var req updateTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Name != nil {
			if err := s.tasks.ConfigureName(taskID, *req.Name); err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if req.Source != nil {
			if err := s.tasks.ConfigureSource(taskID, *req.Source); err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if req.Start != nil {
			if err := s.tasks.ConfigureStart(taskID, *req.Start); err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if req.Storage != nil {
			if err := s.tasks.ConfigureStorage(taskID, *req.Storage); err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		task, err := s.tasks.GetTask(taskID)
		if err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, task)
	case http.MethodDelete:
		if err := s.tasks.DeleteTask(taskID); err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
