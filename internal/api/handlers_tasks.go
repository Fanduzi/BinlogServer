package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/tasks"
)

type summaryResponse struct {
	Total        int `json:"total"`
	Running      int `json:"running"`
	RetryBackoff int `json:"retry_backoff"`
	Stopped      int `json:"stopped"`
	Failed       int `json:"failed"`
	Normal       int `json:"normal"`
	Delayed      int `json:"delayed"`
	Abnormal     int `json:"abnormal"`
}

const defaultDelayThresholdSeconds = int64(30)

type taskReplicationResponse struct {
	TaskID           string      `json:"task_id"`
	State            tasks.State `json:"state"`
	Status           string      `json:"status"`
	Reason           string      `json:"reason,omitempty"`
	LastError        string      `json:"last_error,omitempty"`
	ThresholdSeconds int64       `json:"threshold_seconds"`
	HasProgress      bool        `json:"has_progress"`
	DelaySeconds     int64       `json:"delay_seconds,omitempty"`
	LastEventAt      *time.Time  `json:"last_event_at,omitempty"`
	LastEventFile    string      `json:"last_event_file,omitempty"`
	LastEventPos     uint32      `json:"last_event_pos,omitempty"`
	UpdatedAt        *time.Time  `json:"updated_at,omitempty"`
}

type dashboardTaskItem struct {
	Task        tasks.Task              `json:"task"`
	Replication taskReplicationResponse `json:"replication"`
}

type sourceOverview struct {
	Host      string `json:"host"`
	Port      uint16 `json:"port"`
	TaskCount int    `json:"task_count"`
	Running   int    `json:"running"`
	Normal    int    `json:"normal"`
	Delayed   int    `json:"delayed"`
	Abnormal  int    `json:"abnormal"`
}

type dashboardResponse struct {
	GeneratedAt      time.Time           `json:"generated_at"`
	ThresholdSeconds int64               `json:"threshold_seconds"`
	Summary          summaryResponse     `json:"summary"`
	Tasks            []dashboardTaskItem `json:"tasks"`
	Sources          []sourceOverview    `json:"sources"`
}

type sourceLookupResponse struct {
	Host    string   `json:"host"`
	Port    uint16   `json:"port"`
	Exists  bool     `json:"exists"`
	Count   int      `json:"count"`
	TaskIDs []string `json:"task_ids"`
}

// handleSummary godoc
// @Summary Get task summary counters
// @Tags Dashboard
// @Produce json
// @Param host query string false "Filter by source host"
// @Param port query int false "Filter by source port"
// @Success 200 {object} summaryResponse
// @Failure 400 {string} string
// @Failure 405 {string} string
// @Router /api/summary [get]
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	items, _, err := s.filterTasksBySource(s.tasks.ListTasks(), r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := summaryResponse{Total: len(items)}
	now := time.Now()
	for _, task := range items {
		switch task.State {
		case tasks.StateRunning:
			resp.Running++
		case tasks.StateRetryBackoff:
			resp.RetryBackoff++
		case tasks.StateStopped:
			resp.Stopped++
		case tasks.StateFailed:
			resp.Failed++
		}

		progress, ok, _ := s.tasks.GetReplicationProgress(task.ID)
		rep := buildReplicationResponse(task, progress, ok, now, defaultDelayThresholdSeconds)
		switch rep.Status {
		case "NORMAL":
			resp.Normal++
		case "DELAYED":
			resp.Delayed++
		case "ABNORMAL":
			resp.Abnormal++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSourceLookup godoc
// @Summary Lookup tasks by source host and port
// @Tags Dashboard
// @Produce json
// @Param host query string true "MySQL source host"
// @Param port query int true "MySQL source port"
// @Success 200 {object} sourceLookupResponse
// @Failure 400 {string} string
// @Failure 405 {string} string
// @Router /api/sources/lookup [get]
func (s *Server) handleSourceLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		http.Error(w, "host is required", http.StatusBadRequest)
		return
	}
	portRaw := strings.TrimSpace(r.URL.Query().Get("port"))
	if portRaw == "" {
		http.Error(w, "port is required", http.StatusBadRequest)
		return
	}
	portNum, err := strconv.Atoi(portRaw)
	if err != nil || portNum <= 0 || portNum > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	port := uint16(portNum)

	resp := sourceLookupResponse{
		Host:    host,
		Port:    port,
		TaskIDs: []string{},
	}
	for _, task := range s.tasks.ListTasks() {
		if task.Source.Host != host || task.Source.Port != port {
			continue
		}
		resp.TaskIDs = append(resp.TaskIDs, task.ID)
	}
	sort.Strings(resp.TaskIDs)
	resp.Count = len(resp.TaskIDs)
	resp.Exists = resp.Count > 0

	writeJSON(w, http.StatusOK, resp)
}

// handleDashboard godoc
// @Summary Get task dashboard with replication delay overview
// @Tags Dashboard
// @Produce json
// @Param host query string false "Filter by source host"
// @Param port query int false "Filter by source port"
// @Success 200 {object} dashboardResponse
// @Failure 400 {string} string
// @Failure 405 {string} string
// @Router /api/dashboard [get]
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	items, _, err := s.filterTasksBySource(s.tasks.ListTasks(), r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	resp := dashboardResponse{
		GeneratedAt:      now,
		ThresholdSeconds: defaultDelayThresholdSeconds,
		Summary: summaryResponse{
			Total: len(items),
		},
		Tasks:   make([]dashboardTaskItem, 0, len(items)),
		Sources: []sourceOverview{},
	}

	sourceMap := make(map[string]*sourceOverview)
	for _, task := range items {
		switch task.State {
		case tasks.StateRunning:
			resp.Summary.Running++
		case tasks.StateRetryBackoff:
			resp.Summary.RetryBackoff++
		case tasks.StateStopped:
			resp.Summary.Stopped++
		case tasks.StateFailed:
			resp.Summary.Failed++
		}

		progress, ok, _ := s.tasks.GetReplicationProgress(task.ID)
		rep := buildReplicationResponse(task, progress, ok, now, defaultDelayThresholdSeconds)
		switch rep.Status {
		case "NORMAL":
			resp.Summary.Normal++
		case "DELAYED":
			resp.Summary.Delayed++
		case "ABNORMAL":
			resp.Summary.Abnormal++
		}

		resp.Tasks = append(resp.Tasks, dashboardTaskItem{
			Task:        sanitizeTask(task),
			Replication: rep,
		})

		key := task.Source.Host + ":" + strconv.Itoa(int(task.Source.Port))
		item, exists := sourceMap[key]
		if !exists {
			item = &sourceOverview{
				Host: task.Source.Host,
				Port: task.Source.Port,
			}
			sourceMap[key] = item
		}
		item.TaskCount++
		if task.State == tasks.StateRunning {
			item.Running++
		}
		switch rep.Status {
		case "NORMAL":
			item.Normal++
		case "DELAYED":
			item.Delayed++
		case "ABNORMAL":
			item.Abnormal++
		}
	}

	for _, v := range sourceMap {
		resp.Sources = append(resp.Sources, *v)
	}
	sort.Slice(resp.Sources, func(i, j int) bool {
		if resp.Sources[i].Host == resp.Sources[j].Host {
			return resp.Sources[i].Port < resp.Sources[j].Port
		}
		return resp.Sources[i].Host < resp.Sources[j].Host
	})

	writeJSON(w, http.StatusOK, resp)
}

type createTaskRequest struct {
	// Name 任务名（trim 后 1-255 字符）。
	Name string `json:"name"`
	// ClusterKey 集群标识（必填；仅允许 [a-zA-Z0-9._-]；禁止 / \ ..）。
	ClusterKey string `json:"cluster_key"`
	// Source 源库配置：host/user 必填且不得含空白；port 1-65535；flavor 为空默认 mysql。
	Source *tasks.SourceConfig `json:"source,omitempty"`
	// Start 起点策略：LATEST|FILE_POS|GTID。
	Start *tasks.StartConfig `json:"start,omitempty"`
	// Storage 存储策略：retention_days 必须在 1..3650。
	Storage *tasks.Storage `json:"storage,omitempty"`
}

type updateTaskRequest struct {
	// Name 任务名（trim 后 1-255 字符）。
	Name *string `json:"name,omitempty"`
	// ClusterKey 集群标识（必填；仅允许 [a-zA-Z0-9._-]；禁止 / \ ..）。
	ClusterKey string `json:"cluster_key"`
	// Source 源库配置：host/user 必填且不得含空白；port 1-65535；flavor 为空默认 mysql。
	Source *tasks.SourceConfig `json:"source,omitempty"`
	// Start 起点策略：LATEST|FILE_POS|GTID。
	Start *tasks.StartConfig `json:"start,omitempty"`
	// Storage 存储策略：retention_days 必须在 1..3650。
	Storage *tasks.Storage `json:"storage,omitempty"`
}

// handleTasks godoc
// @Summary Create or list tasks
// @Tags Tasks
// @Accept json
// @Produce json
// @Param host query string false "Filter by source host (GET only)"
// @Param port query int false "Filter by source port (GET only)"
// @Param body body createTaskRequest true "Task create payload (name:1-255, cluster_key:[a-zA-Z0-9._-], source/start/storage 按字段规则校验)"
// @Success 200 {array} tasks.Task
// @Success 201 {object} tasks.Task
// @Failure 400 {string} string
// @Failure 405 {string} string
// @Router /api/tasks [get]
// @Router /api/tasks [post]
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req createTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		task, err := s.tasks.CreateTask(req.Name, req.ClusterKey)
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
		writeJSON(w, http.StatusCreated, sanitizeTask(task))
	case http.MethodGet:
		items, _, err := s.filterTasksBySource(s.tasks.ListTasks(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, sanitizeTaskList(items))
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
		if r.Method == http.MethodGet && action == "events" {
			events, err := s.tasks.ListEvents(taskID, parseLimit(r, 200))
			if err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, events)
			return
		}
		if r.Method == http.MethodGet && action == "files" {
			files, err := s.tasks.ListFiles(taskID, parseLimit(r, 200))
			if err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, files)
			return
		}
		if r.Method == http.MethodGet && action == "replication" {
			task, err := s.tasks.GetTask(taskID)
			if err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			progress, ok, err := s.tasks.GetReplicationProgress(taskID)
			if err != nil {
				if errors.Is(err, tasks.ErrTaskNotFound) {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, buildReplicationResponse(task, progress, ok, time.Now(), defaultDelayThresholdSeconds))
			return
		}
		if r.Method == http.MethodGet && action == "lease" {
			s.handleTaskLease(w, r, taskID)
			return
		}
		if r.Method == http.MethodGet && action == "runs" {
			s.handleTaskRuns(w, r, taskID)
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

func parseLimit(r *http.Request, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func (s *Server) filterTasksBySource(items []tasks.Task, r *http.Request) ([]tasks.Task, bool, error) {
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	portRaw := strings.TrimSpace(r.URL.Query().Get("port"))
	if host == "" && portRaw == "" {
		return items, false, nil
	}
	var port uint16
	if portRaw != "" {
		p, err := strconv.Atoi(portRaw)
		if err != nil || p <= 0 || p > 65535 {
			return nil, false, errors.New("invalid port")
		}
		port = uint16(p)
	}

	out := make([]tasks.Task, 0, len(items))
	for _, task := range items {
		if host != "" && task.Source.Host != host {
			continue
		}
		if portRaw != "" && task.Source.Port != port {
			continue
		}
		out = append(out, task)
	}
	return out, true, nil
}

func buildReplicationResponse(task tasks.Task, progress tasks.ReplicationProgress, exists bool, now time.Time, thresholdSeconds int64) taskReplicationResponse {
	resp := taskReplicationResponse{
		TaskID:           task.ID,
		State:            task.State,
		ThresholdSeconds: thresholdSeconds,
		HasProgress:      exists,
		Status:           "IDLE",
	}
	if exists {
		if !progress.LastEventAt.IsZero() {
			lastEventAt := progress.LastEventAt
			resp.LastEventAt = &lastEventAt
			delay := int64(now.Sub(progress.LastEventAt).Seconds())
			if delay < 0 {
				delay = 0
			}
			resp.DelaySeconds = delay
		}
		if !progress.UpdatedAt.IsZero() {
			updatedAt := progress.UpdatedAt
			resp.UpdatedAt = &updatedAt
		}
		resp.LastEventFile = progress.LastEventFile
		resp.LastEventPos = progress.LastEventPos
	}

	switch task.State {
	case tasks.StateFailed, tasks.StateRetryBackoff:
		resp.Status = "ABNORMAL"
		if task.LastError != "" {
			resp.LastError = task.LastError
			resp.Reason = "RUNNER_ERROR"
		} else {
			resp.Reason = "TASK_STATE_ERROR"
		}
	case tasks.StateRunning:
		if !exists || progress.LastEventAt.IsZero() {
			resp.Status = "ABNORMAL"
			resp.Reason = "NO_PROGRESS"
			return resp
		}
		if resp.DelaySeconds > thresholdSeconds {
			resp.Status = "DELAYED"
			resp.Reason = "DELAY_EXCEEDS_THRESHOLD"
		} else {
			resp.Status = "NORMAL"
		}
	default:
		resp.Status = "IDLE"
	}
	return resp
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
		writeJSON(w, http.StatusOK, sanitizeTask(task))
	case http.MethodPut:
		var req updateTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.ClusterKey) == "" {
			http.Error(w, "cluster_key is required", http.StatusBadRequest)
			return
		}
		if err := s.tasks.ConfigureClusterKey(taskID, req.ClusterKey); err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		writeJSON(w, http.StatusOK, sanitizeTask(task))
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

func sanitizeTask(task tasks.Task) tasks.Task {
	task.Source.Password = ""
	return task
}

func sanitizeTaskList(items []tasks.Task) []tasks.Task {
	out := make([]tasks.Task, len(items))
	for i := range items {
		out[i] = sanitizeTask(items[i])
	}
	return out
}
