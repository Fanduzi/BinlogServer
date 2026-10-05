// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces, ListClusterObservation, shared source endpoint identity
// output: REST API JSON responses including single/batch task creation, dashboard/summary counters from SQL GROUP BY (or one filtered read) with LIMIT/OFFSET task pages and replication progress on the visible page plus RUNNING-id delay counts, lookup from the unfiltered store ownership copy then SameSourceHost filter, independent STARTING/RUNNING counters, at-tip delay_seconds encoded as JSON 0 with NORMAL (omitted only when there is no event-time sample), structured 400 bodies, 400 on updates of read-only on-disk backups, plain-text 400 when a live dump's source, start, storage, or cluster_key would change, 200 when POST adopt attaches source identity to that same id, GET /api/tasks/{id}/checkpoint as the next Start file/pos with gtid_set when the stored checkpoint matches, GET /api/tasks/{id}/replay one on-disk path per source index with the source.flavor client hint, the same route with stop_datetime returning the UTC point-in-time paths and command, GET /api/tasks/{id}/replay/archive one ustar of that selection, and GET /api/tasks/{id}/files/{name} raw bytes of one inventory segment from local disk or a sealed uploaded object
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/tasks"

	"github.com/gin-gonic/gin/binding"
	validator "github.com/go-playground/validator/v10"
)

type summaryResponse struct {
	Total        int `json:"total"`
	Running      int `json:"running"`
	Starting     int `json:"starting"`
	RetryBackoff int `json:"retry_backoff"`
	Stopped      int `json:"stopped"`
	Failed       int `json:"failed"`
	Normal       int `json:"normal"`
	Delayed      int `json:"delayed"`
	Abnormal     int `json:"abnormal"`
}

const defaultDelayThresholdSeconds = int64(30)

const (
	defaultTaskListLimit = 100
	maxTaskListLimit     = 500
	maxBatchCreateItems  = 100
)

type taskReplicationResponse struct {
	TaskID           string      `json:"task_id"`
	State            tasks.State `json:"state"`
	Status           string      `json:"status"`
	Reason           string      `json:"reason,omitempty"`
	LastError        string      `json:"last_error,omitempty"`
	ThresholdSeconds int64       `json:"threshold_seconds"`
	HasProgress      bool        `json:"has_progress"`
	// DelaySeconds is a sampled lag. Nil means there is no event-time sample, so JSON omits the field.
	// A non-nil 0 is caught up. omitempty on a plain int64 would drop that zero and the Console would show "--".
	DelaySeconds  *int64     `json:"delay_seconds,omitempty"`
	LastEventAt   *time.Time `json:"last_event_at,omitempty"`
	LastEventFile string     `json:"last_event_file,omitempty"`
	LastEventPos  uint32     `json:"last_event_pos,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
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
	Starting  int    `json:"starting"`
	Normal    int    `json:"normal"`
	Delayed   int    `json:"delayed"`
	Abnormal  int    `json:"abnormal"`
}

type dashboardResponse struct {
	GeneratedAt      time.Time           `json:"generated_at"`
	ThresholdSeconds int64               `json:"threshold_seconds"`
	Total            int                 `json:"total"`
	Limit            int                 `json:"limit"`
	Offset           int                 `json:"offset"`
	Summary          summaryResponse     `json:"summary"`
	Tasks            []dashboardTaskItem `json:"tasks"`
	Sources          []sourceOverview    `json:"sources"`
}

type taskListResponse struct {
	Items  []tasks.Task `json:"items"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

type taskListQuery struct {
	Host   string
	Port   *uint16
	State  *tasks.State
	Limit  int
	Offset int
}

type sourceLookupResponse struct {
	Host    string   `json:"host"`
	Port    uint16   `json:"port"`
	Exists  bool     `json:"exists"`
	Count   int      `json:"count"`
	TaskIDs []string `json:"task_ids"`
}

type sourceLookupQuery struct {
	Host string `form:"host" binding:"required"`
	Port string `form:"port" binding:"required"`
}

type listLimitQuery struct {
	Limit *int `form:"limit" binding:"omitempty,min=1"`
}

type retryUploadLimitQuery struct {
	Limit *int `form:"limit" binding:"omitempty,min=1,max=1000"`
}

type uploadFailureReasonsLimitQuery struct {
	Limit *int `form:"limit" binding:"omitempty,min=1,max=200"`
}

// handleSummary godoc
// @Summary Get task summary counters
// @Tags Dashboard
// @Produce json
// @Param host query string false "Filter by source host; localhost and explicit loopback literals share one identity, other hosts match exact spelling"
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

	query, err := parseTaskListQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	summary, _, err := s.dashboardObservation(r.Context(), query, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// handleSourceLookup godoc
// @Summary Lookup tasks by source host and port
// @Tags Dashboard
// @Produce json
// @Param host query string true "MySQL source host (required); localhost and explicit loopback literals (127/8, ::1, including bracketed IPv6) share one identity, other hosts match exact spelling"
// @Param port query int true "MySQL source port (required, 1-65535)"
// @Success 200 {object} sourceLookupResponse
// @Failure 400 {string} string
// @Failure 405 {string} string
// @Router /api/sources/lookup [get]
func (s *Server) handleSourceLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var query sourceLookupQuery
	if err := binding.Query.Bind(r, &query); err != nil {
		http.Error(w, mapSourceLookupBindError(err), http.StatusBadRequest)
		return
	}
	host := strings.TrimSpace(query.Host)
	if host == "" {
		http.Error(w, "host is required", http.StatusBadRequest)
		return
	}
	port, err := parsePort(query.Port)
	if err != nil {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}

	items, err := s.tasks.ListClusterObservation(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := sourceLookupResponse{
		Host:    host,
		Port:    port,
		TaskIDs: []string{},
	}
	matched := tasks.FilterTasks(items, tasks.TaskListFilter{Host: host, Port: &port})
	for _, task := range matched {
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
// @Param host query string false "Filter by source host; localhost and explicit loopback literals share one identity, other hosts match exact spelling"
// @Param port query int false "Filter by source port"
// @Param state query string false "Filter by task state"
// @Param limit query int false "Page size (default 100, range 1-500; values above 500 return 400)"
// @Param offset query int false "Zero-based page offset (default 0)"
// @Success 200 {object} dashboardResponse
// @Failure 400 {string} string
// @Failure 405 {string} string
// @Router /api/dashboard [get]
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query, err := parseTaskListQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	summary, sources, err := s.dashboardObservation(r.Context(), query, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page, _, err := s.tasks.ListTasksPage(r.Context(), query.toFilter())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := dashboardResponse{
		GeneratedAt:      now,
		ThresholdSeconds: defaultDelayThresholdSeconds,
		Total:            summary.Total,
		Limit:            query.Limit,
		Offset:           query.Offset,
		Summary:          summary,
		Tasks:            make([]dashboardTaskItem, 0, len(page)),
		Sources:          sources,
	}
	for _, task := range page {
		progress, ok, _ := s.tasks.GetReplicationProgress(task.ID)
		resp.Tasks = append(resp.Tasks, dashboardTaskItem{
			Task:        sanitizeTask(task),
			Replication: buildReplicationResponse(task, progress, ok, now, defaultDelayThresholdSeconds),
		})
	}

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
	// Storage 存储策略：retention_days 必须在 1..3650。local_retention_days 与 bucket_retention_days 省略或 0 时等于 retention_days。桶保留短于本地保留则 400。
	Storage *tasks.Storage `json:"storage,omitempty"`
}

type batchCreateRequest struct {
	Items []createTaskRequest `json:"items"`
}

type batchCreateEnvelope struct {
	Items json.RawMessage `json:"items"`
}

type batchCreateResult struct {
	Index      int           `json:"index"`
	ClusterKey string        `json:"cluster_key"`
	Task       *tasks.Task   `json:"task,omitempty"`
	Error      *apiErrorBody `json:"error,omitempty"`
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
	// Storage 存储策略：retention_days 必须在 1..3650。local_retention_days 与 bucket_retention_days 省略或 0 时等于 retention_days。桶保留短于本地保留则 400。
	Storage *tasks.Storage `json:"storage,omitempty"`
}

// handleTasks godoc
// @Summary Create or list tasks
// @Tags Tasks
// @Accept json
// @Produce json
// @Param body body createTaskRequest true "Task create payload (name:1-255, cluster_key:[a-zA-Z0-9._-], source/start/storage 按字段规则校验)"
// @Success 201 {object} tasks.Task
// @Failure 400 {object} apiErrorBody
// @Failure 405 {string} string
// @Router /api/tasks [post]
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req createTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "invalid json")
			return
		}

		task, err := s.tasks.CreateTaskFromSpec(req.Name, req.ClusterKey, req.Source, req.Start, req.Storage)
		if err != nil {
			writeTaskError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, sanitizeTask(task))
	case http.MethodGet:
		query, err := parseTaskListQuery(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		page, total, err := s.tasks.ListTasksPage(r.Context(), query.toFilter())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, taskListResponse{
			Items:  sanitizeTaskList(page),
			Total:  total,
			Limit:  query.Limit,
			Offset: query.Offset,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleTaskBatch creates valid items independently while preserving request order.
// @Summary Create tasks in batch
// @Tags Tasks
// @Accept json
// @Produce json
// @Param body body batchCreateRequest true "Batch task create payload (1-100 items)"
// @Success 200 {array} batchCreateResult
// @Failure 400 {object} apiErrorBody
// @Failure 405 {string} string
// @Router /api/tasks/batch [post]
func (s *Server) handleTaskBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var envelope batchCreateEnvelope
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&envelope); err != nil {
		writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "invalid json")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "invalid json")
		return
	}
	if len(bytes.TrimSpace(envelope.Items)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Items), []byte("null")) {
		writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "items is required")
		return
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(envelope.Items, &rawItems); err != nil {
		writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "items must be an array")
		return
	}
	if len(rawItems) == 0 {
		writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "items must not be empty")
		return
	}
	if len(rawItems) > maxBatchCreateItems {
		writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "items must contain at most 100 items")
		return
	}

	results := make([]batchCreateResult, len(rawItems))
	for i, rawItem := range rawItems {
		result := batchCreateResult{Index: i}
		var req createTaskRequest
		if err := json.Unmarshal(rawItem, &req); err != nil {
			result.Error = &apiErrorBody{Error: "invalid json", Code: tasks.CodeInvalidRequest}
			results[i] = result
			continue
		}
		result.ClusterKey = req.ClusterKey

		task, err := s.tasks.CreateTaskFromSpec(req.Name, req.ClusterKey, req.Source, req.Start, req.Storage)
		if err != nil {
			errorBody := taskErrorBody(err)
			result.Error = &errorBody
			results[i] = result
			continue
		}
		sanitized := sanitizeTask(task)
		result.ClusterKey = sanitized.ClusterKey
		result.Task = &sanitized
		results[i] = result
	}

	writeJSON(w, http.StatusOK, results)
}

// handleTaskAction 处理 start/stop 等任务动作请求。
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
	if len(parts) == 1 && taskID == "batch" {
		s.handleTaskBatch(w, r)
		return
	}
	// /api/tasks/{id}
	if len(parts) == 1 {
		s.handleTaskEntity(w, r, taskID)
		return
	}
	// /api/tasks/{id}/files/retry-upload and /api/tasks/{id}/files/{name}
	if len(parts) >= 3 && parts[1] == "files" {
		if len(parts) == 3 && parts[2] == "retry-upload" {
			s.handleTaskRetryUpload(w, r, taskID)
			return
		}
		s.handleTaskFileDownload(w, r, taskID, strings.Join(parts[2:], "/"))
		return
	}
	// /api/tasks/{id}/replay/archive
	if len(parts) == 3 && parts[1] == "replay" && parts[2] == "archive" {
		s.handleTaskReplayArchive(w, r, taskID)
		return
	}
	// /api/tasks/{id}/upload-failures/reasons
	if len(parts) == 3 && parts[1] == "upload-failures" && parts[2] == "reasons" {
		s.handleTaskUploadFailureReasons(w, r, taskID)
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	action := parts[1]

	if r.Method != http.MethodPost {
		if r.Method == http.MethodGet && action == "checkpoint" {
			checkpoint, ok, err := s.tasks.ResumePosition(r.Context(), taskID)
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
		if r.Method == http.MethodGet && action == "replay" {
			s.handleTaskReplay(w, r, taskID)
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
	case "adopt":
		s.handleAdoptDiskBackup(w, r, taskID)
		return
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

// handleTaskFileDownload streams one inventory segment.
// A local {data_dir}/{id}/{name} file wins, including an open segment.
// A missing local sealed file with upload_state UPLOADED and a non-empty
// object_key is read from the configured object store. Replay paths are unchanged.
func (s *Server) handleTaskFileDownload(w http.ResponseWriter, r *http.Request, taskID, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, size, err := s.tasks.OpenTaskSegment(taskID, name)
	if err != nil {
		switch {
		case errors.Is(err, tasks.ErrInvalidSegmentName):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, tasks.ErrTaskNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, tasks.ErrSegmentNotOnProcess):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	defer body.Close()

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if disposition == "" {
		http.Error(w, tasks.ErrInvalidSegmentName.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// handleTaskReplay returns one on-disk path per source index from the files
// inventory window, plus the binlog client for source.flavor.
// stop_datetime switches the same route to the UTC point-in-time window.
func (s *Server) handleTaskReplay(w http.ResponseWriter, r *http.Request, taskID string) {
	start, stop, enabled, qerr := parsePITRQuery(r)
	if qerr != nil {
		http.Error(w, qerr.Error(), http.StatusBadRequest)
		return
	}
	if enabled {
		set, err := s.tasks.PITRReplay(taskID, start, stop)
		if err != nil {
			writeSegmentOpenError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, set)
		return
	}
	files, err := s.tasks.ListFiles(taskID, parseLimit(r, 200))
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
	selected := tasks.SelectReplayFiles(files)
	paths := make([]string, 0, len(selected))
	for _, file := range selected {
		paths = append(paths, file.FilePath)
	}
	client, hint := tasks.ReplayClient(task.Source.Flavor)
	writeJSON(w, http.StatusOK, tasks.ReplaySet{
		Flavor:     task.Source.Flavor,
		Client:     client,
		ClientHint: hint,
		Paths:      paths,
		Locations:  tasks.ReplayLocations(selected),
	})
}

// handleTaskReplayArchive streams one ustar of the replay selection.
// limit is the same inventory window as GET /replay. stop_datetime uses the
// point-in-time selection instead. Member names are the basenames of those
// paths. Bytes follow GET /files/{name}. An empty selection is 200 and an
// empty tar. A segment that cannot be opened fails the request before any
// archive byte is written.
func (s *Server) handleTaskReplayArchive(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start, stop, enabled, qerr := parsePITRQuery(r)
	if qerr != nil {
		http.Error(w, qerr.Error(), http.StatusBadRequest)
		return
	}
	var body io.ReadCloser
	var size int64
	var err error
	if enabled {
		body, size, err = s.tasks.OpenPITRArchive(taskID, start, stop)
	} else {
		body, size, err = s.tasks.OpenReplayArchive(taskID, parseLimit(r, 200))
	}
	if err != nil {
		writeSegmentOpenError(w, err)
		return
	}
	defer body.Close()

	filename := "task-" + taskID + "-replay.tar"
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		http.Error(w, tasks.ErrInvalidSegmentName.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// handleTaskRetryUpload 触发指定任务的失败文件重传并返回统计。
func (s *Server) handleTaskRetryUpload(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit, err := parseRetryUploadLimit(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stats, err := s.tasks.RetryFailedUploads(taskID, limit)
	if err != nil {
		switch {
		case errors.Is(err, tasks.ErrTaskNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, tasks.ErrInvalidRetryUploadLimit):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, tasks.ErrUploadRetryInProgress):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, tasks.ErrUploadRetryNotAvailable):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

// handleTaskUploadFailureReasons 返回指定任务的上传失败原因聚合。
func (s *Server) handleTaskUploadFailureReasons(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit, err := parseUploadFailureReasonsLimit(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	items, err := s.tasks.ListUploadFailureReasons(taskID, limit)
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// parsePITRQuery reads the point-in-time window. Neither parameter leaves
// the limit replay path unchanged. stop_datetime is required once either
// parameter is present. An empty start_datetime is the stop-only window.
func parsePITRQuery(r *http.Request) (start *time.Time, stop time.Time, enabled bool, err error) {
	q := r.URL.Query()
	_, stopSet := q["stop_datetime"]
	_, startSet := q["start_datetime"]
	if !stopSet && !startSet {
		return nil, time.Time{}, false, nil
	}
	if !stopSet || strings.TrimSpace(q.Get("stop_datetime")) == "" {
		if !stopSet {
			return nil, time.Time{}, false, errors.New("stop_datetime is required")
		}
		return nil, time.Time{}, false, errors.New("invalid stop_datetime")
	}
	stop, err = tasks.ParsePITRDatetime(q.Get("stop_datetime"))
	if err != nil {
		return nil, time.Time{}, false, errors.New("invalid stop_datetime")
	}
	if startSet && strings.TrimSpace(q.Get("start_datetime")) != "" {
		parsed, serr := tasks.ParsePITRDatetime(q.Get("start_datetime"))
		if serr != nil {
			return nil, time.Time{}, false, errors.New("invalid start_datetime")
		}
		if parsed.After(stop) {
			return nil, time.Time{}, false, errors.New("start_datetime is after stop_datetime")
		}
		start = &parsed
	}
	return start, stop, true, nil
}

func writeSegmentOpenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tasks.ErrInvalidSegmentName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, tasks.ErrTaskNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, tasks.ErrSegmentNotOnProcess):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// parseLimit 解析通用分页参数 limit，失败时回退默认值。
func parseLimit(r *http.Request, fallback int) int {
	// 常见误解：
	// 这里对非法 limit 不报 400，而是回退到 fallback，目的是提升列表接口容错性。
	var query listLimitQuery
	if err := binding.Query.Bind(r, &query); err != nil || query.Limit == nil {
		// 列表类接口对非法 limit 采用 fallback，避免影响主流程可用性。
		return fallback
	}
	return *query.Limit
}

// parseRetryUploadLimit 解析重传接口的 limit 参数并做边界校验。
func parseRetryUploadLimit(r *http.Request) (int, error) {
	var query retryUploadLimitQuery
	if err := binding.Query.Bind(r, &query); err != nil {
		return 0, errors.New("invalid limit")
	}
	if query.Limit == nil {
		return 100, nil
	}
	return *query.Limit, nil
}

// parseUploadFailureReasonsLimit 解析失败原因接口的 limit 参数并做边界校验。
func parseUploadFailureReasonsLimit(r *http.Request) (int, error) {
	var query uploadFailureReasonsLimitQuery
	if err := binding.Query.Bind(r, &query); err != nil {
		return 0, errors.New("invalid limit")
	}
	if query.Limit == nil {
		return 20, nil
	}
	return *query.Limit, nil
}

// parseTaskListQuery 解析任务列表与 dashboard 共用的过滤、分页参数。
func parseTaskListQuery(r *http.Request) (taskListQuery, error) {
	values := r.URL.Query()
	query := taskListQuery{
		Host:   strings.TrimSpace(values.Get("host")),
		Limit:  defaultTaskListLimit,
		Offset: 0,
	}

	if _, ok := values["port"]; ok {
		port, err := parsePort(values.Get("port"))
		if err != nil {
			return taskListQuery{}, errors.New("invalid port")
		}
		query.Port = &port
	}
	if _, ok := values["state"]; ok {
		state, err := parseTaskState(values.Get("state"))
		if err != nil {
			return taskListQuery{}, err
		}
		query.State = &state
	}
	if _, ok := values["limit"]; ok {
		limit, err := strconv.Atoi(strings.TrimSpace(values.Get("limit")))
		if err != nil || limit <= 0 {
			return taskListQuery{}, errors.New("invalid limit")
		}
		if limit > maxTaskListLimit {
			return taskListQuery{}, errors.New("invalid limit")
		}
		query.Limit = limit
	}
	if _, ok := values["offset"]; ok {
		offset, err := strconv.Atoi(strings.TrimSpace(values.Get("offset")))
		if err != nil || offset < 0 {
			return taskListQuery{}, errors.New("invalid offset")
		}
		query.Offset = offset
	}
	return query, nil
}

type dashboardCounterService interface {
	DashboardCounters(context.Context, tasks.TaskListFilter) (tasks.DashboardCounters, error)
}

func (s *Server) dashboardObservation(ctx context.Context, query taskListQuery, now time.Time) (summaryResponse, []sourceOverview, error) {
	counter, ok := s.tasks.(dashboardCounterService)
	if !ok {
		return summaryResponse{}, nil, errors.New("dashboard counters are unavailable")
	}
	counters, err := counter.DashboardCounters(ctx, query.toFilter())
	if err != nil {
		return summaryResponse{}, nil, err
	}
	summary := summaryFromCounts(counters.States)
	sources := sourcesFromCounts(counters.Sources)
	applyRunningReplication(&summary, sources, counters.Running, now, func(id string) (tasks.ReplicationProgress, bool) {
		progress, ok, _ := s.tasks.GetReplicationProgress(id)
		return progress, ok
	})
	return summary, sources, nil
}

func summaryFromCounts(counts tasks.TaskStateCounts) summaryResponse {
	return summaryResponse{
		Total:        counts.Total,
		Running:      counts.Running,
		Starting:     counts.Starting,
		RetryBackoff: counts.RetryBackoff,
		Stopped:      counts.Stopped,
		Failed:       counts.Failed,
		Abnormal:     counts.Failed + counts.RetryBackoff,
	}
}

func sourcesFromCounts(items []tasks.TaskSourceCount) []sourceOverview {
	out := make([]sourceOverview, 0, len(items))
	for _, item := range items {
		out = append(out, sourceOverview{
			Host:      item.Host,
			Port:      item.Port,
			TaskCount: item.TaskCount,
			Running:   item.Running,
			Starting:  item.Starting,
			Abnormal:  item.Failed + item.RetryBackoff,
		})
	}
	return out
}

func applyRunningReplication(summary *summaryResponse, sources []sourceOverview, refs []tasks.RunningTaskRef, now time.Time, progress func(string) (tasks.ReplicationProgress, bool)) {
	bySource := make(map[string]*sourceOverview, len(sources))
	for i := range sources {
		bySource[sourceOverviewKey(sources[i].Host, sources[i].Port)] = &sources[i]
	}
	for _, ref := range refs {
		prog, ok := progress(ref.ID)
		rep := buildReplicationResponse(tasks.Task{ID: ref.ID, State: tasks.StateRunning}, prog, ok, now, defaultDelayThresholdSeconds)
		src := bySource[sourceOverviewKey(ref.Host, ref.Port)]
		switch rep.Status {
		case "NORMAL":
			summary.Normal++
			if src != nil {
				src.Normal++
			}
		case "DELAYED":
			summary.Delayed++
			if src != nil {
				src.Delayed++
			}
		case "ABNORMAL":
			summary.Abnormal++
			if src != nil {
				src.Abnormal++
			}
		}
	}
}

func sourceOverviewKey(host string, port uint16) string {
	return host + ":" + strconv.Itoa(int(port))
}

func (q taskListQuery) toFilter() tasks.TaskListFilter {
	return tasks.TaskListFilter{
		Host:   q.Host,
		Port:   q.Port,
		State:  q.State,
		Limit:  q.Limit,
		Offset: q.Offset,
	}
}

func parseTaskState(raw string) (tasks.State, error) {
	state := tasks.State(strings.TrimSpace(raw))
	switch state {
	case tasks.StateCreated,
		tasks.StateStarting,
		tasks.StateRunning,
		tasks.StateLeaseDegraded,
		tasks.StateRebuildingFile,
		tasks.StateRetryBackoff,
		tasks.StateFailed,
		tasks.StateStopping,
		tasks.StateStopped:
		return state, nil
	default:
		return "", errors.New("invalid state")
	}
}

func filterTasksByQuery(items []tasks.Task, query taskListQuery) []tasks.Task {
	return tasks.FilterTasks(items, query.toFilter())
}

// sortTasksByID keeps API pages aligned with the metadata store's
// ORDER BY CAST(id AS UNSIGNED), id convention: numeric ids ascending,
// then non-numeric ids by string.
func sortTasksByID(items []tasks.Task) {
	tasks.SortTasksByID(items)
}

func paginateTasks(items []tasks.Task, offset, limit int) []tasks.Task {
	return tasks.PaginateTasks(items, offset, limit)
}

// parsePort 解析并校验 TCP 端口号（1-65535）。
func parsePort(raw string) (uint16, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 || n > 65535 {
		return 0, errors.New("invalid port")
	}
	return uint16(n), nil
}

func mapSourceLookupBindError(err error) string {
	var validationErrs validator.ValidationErrors
	if errors.As(err, &validationErrs) {
		for _, fieldErr := range validationErrs {
			switch fieldErr.Field() {
			case "Host":
				return "host is required"
			case "Port":
				return "port is required"
			}
		}
	}
	return "invalid port"
}

// buildReplicationResponse 组装复制状态响应并计算延迟与异常原因。
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
			delay := int64(0)
			if !progress.AtTip {
				// Header age is lag only while the dump is still behind the source tip.
				delay = int64(now.Sub(progress.LastEventAt).Seconds())
				if delay < 0 {
					delay = 0
				}
			}
			resp.DelaySeconds = &delay
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
		if resp.DelaySeconds != nil && *resp.DelaySeconds > thresholdSeconds {
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

// handleTaskEntity 处理单任务详情、更新与删除请求。
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
			writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "invalid json")
			return
		}
		// PUT 走 Scheduler.UpdateTask 原子更新：
		// 统一校验后一次性落库，避免“部分字段成功、后续字段失败”导致的数据不一致。
		updated, err := s.tasks.UpdateTask(taskID, tasks.TaskPatch{
			Name:       req.Name,
			ClusterKey: req.ClusterKey,
			Source:     req.Source,
			Start:      req.Start,
			Storage:    req.Storage,
		})
		if err != nil {
			if errors.Is(err, tasks.ErrTaskNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if isTaskUpdateBadRequest(err) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, sanitizeTask(updated))
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

// isTaskUpdateBadRequest 判断任务更新错误是否应返回 4xx。
func isTaskUpdateBadRequest(err error) bool {
	return errors.Is(err, tasks.ErrClusterKeyRequired) ||
		errors.Is(err, tasks.ErrInvalidClusterKey) ||
		errors.Is(err, tasks.ErrClusterKeyExists) ||
		errors.Is(err, tasks.ErrInvalidTaskName) ||
		errors.Is(err, tasks.ErrInvalidSourceConfig) ||
		errors.Is(err, tasks.ErrSourceRequired) ||
		errors.Is(err, tasks.ErrSourcePasswordRequired) ||
		errors.Is(err, tasks.ErrFilePosRequired) ||
		errors.Is(err, tasks.ErrGTIDSetRequired) ||
		errors.Is(err, tasks.ErrInvalidStartMode) ||
		errors.Is(err, tasks.ErrInvalidRetentionDays) ||
		errors.Is(err, tasks.ErrDiskBackupReadOnly) ||
		errors.Is(err, tasks.ErrTaskAlreadyHasMetadata) ||
		errors.Is(err, tasks.ErrDiskResumePosition) ||
		errors.Is(err, tasks.ErrTaskDumpConfigLocked)
}

// handleAdoptDiskBackup attaches source identity to a leftover data directory on the same id.
func (s *Server) handleAdoptDiskBackup(w http.ResponseWriter, r *http.Request, taskID string) {
	var req updateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, tasks.CodeInvalidRequest, "invalid json")
		return
	}
	updated, err := s.tasks.AdoptDiskBackup(taskID, tasks.TaskPatch{
		Name:       req.Name,
		ClusterKey: req.ClusterKey,
		Source:     req.Source,
		Start:      req.Start,
		Storage:    req.Storage,
	})
	if err != nil {
		if errors.Is(err, tasks.ErrTaskNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if isTaskUpdateBadRequest(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeTask(updated))
}

type apiErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeAPIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiErrorBody{Error: msg, Code: code})
}

func writeTaskError(w http.ResponseWriter, err error) {
	body := taskErrorBody(err)
	status := http.StatusInternalServerError
	if body.Code == "TASK_NOT_FOUND" {
		status = http.StatusNotFound
	} else if body.Code == tasks.CodeInvalidRequest {
		status = http.StatusBadRequest
	}
	writeAPIError(w, status, body.Code, body.Error)
}

func taskErrorBody(err error) apiErrorBody {
	if errors.Is(err, tasks.ErrTaskNotFound) {
		return apiErrorBody{Error: err.Error(), Code: "TASK_NOT_FOUND"}
	}
	if isTaskUpdateBadRequest(err) {
		return apiErrorBody{Error: err.Error(), Code: tasks.CodeInvalidRequest}
	}
	return apiErrorBody{Error: "internal server error", Code: "INTERNAL_ERROR"}
}

// writeJSON 统一输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sanitizeTask 对任务输出做脱敏处理（如密码字段）。
func sanitizeTask(task tasks.Task) tasks.Task {
	task.Source.Password = ""
	return task
}

// sanitizeTaskList 对任务列表执行批量脱敏。
func sanitizeTaskList(items []tasks.Task) []tasks.Task {
	out := make([]tasks.Task, len(items))
	for i := range items {
		out[i] = sanitizeTask(items[i])
	}
	return out
}
