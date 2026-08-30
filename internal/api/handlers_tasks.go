// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces, shared source endpoint identity
// output: REST API JSON responses including single/batch task creation, numeric-id-ordered paginated task/dashboard data, loopback-equivalent source lookup, independent STARTING/RUNNING counters, at-tip delay 0/NORMAL, and structured 400 bodies
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
		case tasks.StateStarting:
			resp.Starting++
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

	resp := sourceLookupResponse{
		Host:    host,
		Port:    port,
		TaskIDs: []string{},
	}
	for _, task := range s.tasks.ListTasks() {
		sameHost := task.Source.Host == host ||
			(tasks.IsLoopbackHost(task.Source.Host) && tasks.IsLoopbackHost(host))
		if !sameHost || task.Source.Port != port {
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
	items := filterTasksByQuery(s.tasks.ListTasks(), query)
	sortTasksByID(items)
	pageStart, pageEnd := taskPageBounds(len(items), query.Offset, query.Limit)
	now := time.Now()
	resp := dashboardResponse{
		GeneratedAt:      now,
		ThresholdSeconds: defaultDelayThresholdSeconds,
		Total:            len(items),
		Limit:            query.Limit,
		Offset:           query.Offset,
		Summary: summaryResponse{
			Total: len(items),
		},
		Tasks:   make([]dashboardTaskItem, 0, pageEnd-pageStart),
		Sources: []sourceOverview{},
	}

	sourceMap := make(map[string]*sourceOverview)
	for i, task := range items {
		switch task.State {
		case tasks.StateRunning:
			resp.Summary.Running++
		case tasks.StateStarting:
			resp.Summary.Starting++
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

		if i >= pageStart && i < pageEnd {
			resp.Tasks = append(resp.Tasks, dashboardTaskItem{
				Task:        sanitizeTask(task),
				Replication: rep,
			})
		}

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
		if task.State == tasks.StateStarting {
			item.Starting++
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
	// Storage 存储策略：retention_days 必须在 1..3650。
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
		items := filterTasksByQuery(s.tasks.ListTasks(), query)
		sortTasksByID(items)
		page := paginateTasks(items, query.Offset, query.Limit)
		writeJSON(w, http.StatusOK, taskListResponse{
			Items:  sanitizeTaskList(page),
			Total:  len(items),
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
	// /api/tasks/{id}/files/retry-upload
	if len(parts) == 3 && parts[1] == "files" && parts[2] == "retry-upload" {
		s.handleTaskRetryUpload(w, r, taskID)
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
	out := make([]tasks.Task, 0, len(items))
	for _, task := range items {
		if query.Host != "" && task.Source.Host != query.Host {
			continue
		}
		if query.Port != nil && task.Source.Port != *query.Port {
			continue
		}
		if query.State != nil && task.State != *query.State {
			continue
		}
		out = append(out, task)
	}
	return out
}

// sortTasksByID keeps API pages aligned with the metadata store's
// ORDER BY CAST(id AS UNSIGNED), id convention: numeric ids ascending,
// then non-numeric ids by string.
func sortTasksByID(items []tasks.Task) {
	sort.SliceStable(items, func(i, j int) bool {
		return lessTaskID(items[i].ID, items[j].ID)
	})
}

func lessTaskID(a, b string) bool {
	ai, aOK := parseNumericTaskID(a)
	bi, bOK := parseNumericTaskID(b)
	switch {
	case aOK && bOK:
		if ai != bi {
			return ai < bi
		}
		return a < b
	case aOK:
		return true
	case bOK:
		return false
	default:
		return a < b
	}
}

func parseNumericTaskID(id string) (uint64, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func taskPageBounds(total, offset, limit int) (int, int) {
	if offset >= total {
		return total, total
	}
	end := total
	if limit <= total-offset {
		end = offset + limit
	}
	return offset, end
}

func paginateTasks(items []tasks.Task, offset, limit int) []tasks.Task {
	start, end := taskPageBounds(len(items), offset, limit)
	return items[start:end]
}

// filterTasksBySource 按 source 查询参数过滤任务集合。
func (s *Server) filterTasksBySource(items []tasks.Task, r *http.Request) ([]tasks.Task, bool, error) {
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	portRaw := strings.TrimSpace(r.URL.Query().Get("port"))
	if host == "" && portRaw == "" {
		return items, false, nil
	}
	var port uint16
	if portRaw != "" {
		p, err := parsePort(portRaw)
		if err != nil {
			return nil, false, errors.New("invalid port")
		}
		port = p
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
			if progress.AtTip {
				// Dump is at the source tip (LATEST start or idle). Header age is not lag.
				resp.DelaySeconds = 0
			} else {
				delay := int64(now.Sub(progress.LastEventAt).Seconds())
				if delay < 0 {
					delay = 0
				}
				resp.DelaySeconds = delay
			}
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
		errors.Is(err, tasks.ErrInvalidRetentionDays)
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
