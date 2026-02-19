package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"
	_ "binlog_server/internal/swaggerdocs"
	"binlog_server/internal/tasks"
	"binlog_server/internal/ui"

	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

type taskService interface {
	CreateTask(name string) (tasks.Task, error)
	ConfigureName(id string, name string) error
	ConfigureSource(id string, source tasks.SourceConfig) error
	ConfigureStart(id string, start tasks.StartConfig) error
	ConfigureStorage(id string, storage tasks.Storage) error
	GetTask(id string) (tasks.Task, error)
	GetReplicationProgress(id string) (tasks.ReplicationProgress, bool, error)
	GetCheckpoint(ctx context.Context, id string) (binlog.Checkpoint, bool, error)
	ListEvents(id string, limit int) ([]tasks.TaskEvent, error)
	ListFiles(id string, limit int) ([]tasks.BinlogFile, error)
	ListRuns(id string, limit int) ([]tasks.TaskRun, error)
	ListWorkerHeartbeats(limit int) ([]tasks.WorkerHeartbeat, error)
	ListTasks() []tasks.Task
	DeleteTask(id string) error
	StartTask(id string) error
	StopTask(id string) error
}

type Server struct {
	tasks taskService
	gin   *gin.Engine
}

func NewServer(taskSvc taskService) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	s := &Server{
		tasks: taskSvc,
		gin:   engine,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.gin.GET("/healthz", gin.WrapF(s.handleHealth))
	s.gin.GET("/metrics", gin.WrapF(s.handleMetrics))
	s.gin.GET("/api/summary", gin.WrapF(s.handleSummary))
	s.gin.GET("/api/dashboard", gin.WrapF(s.handleDashboard))
	s.gin.GET("/api/sources/lookup", gin.WrapF(s.handleSourceLookup))
	s.gin.GET("/api/workers", gin.WrapF(s.handleWorkers))
	s.gin.GET("/api/cluster/overview", gin.WrapF(s.handleClusterOverview))
	s.gin.POST("/api/tasks", gin.WrapF(s.handleTasks))
	s.gin.GET("/api/tasks", gin.WrapF(s.handleTasks))
	s.gin.Any("/api/tasks/*path", gin.WrapF(s.handleTaskAction))
	s.gin.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))

	uiHandler := http.StripPrefix("/ui/", ui.Handler())
	s.gin.Any("/ui/*path", gin.WrapH(uiHandler))
	s.gin.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/ui/")
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.gin.ServeHTTP(w, r)
}

// handleHealth godoc
// @Summary Service health check
// @Tags System
// @Success 200 {string} string "ok"
// @Router /healthz [get]
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.renderPrometheusMetrics()))
}

func (s *Server) renderPrometheusMetrics() string {
	var b strings.Builder
	now := time.Now()
	items := s.tasks.ListTasks()
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})

	b.WriteString("# HELP binlog_server_task_state_count Number of tasks by state.\n")
	b.WriteString("# TYPE binlog_server_task_state_count gauge\n")
	stateCount := make(map[string]int)
	for _, task := range items {
		stateCount[string(task.State)]++
	}
	states := make([]string, 0, len(stateCount))
	for state := range stateCount {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		writePromSample(&b, "binlog_server_task_state_count", map[string]string{
			"state": state,
		}, float64(stateCount[state]))
	}

	b.WriteString("# HELP binlog_server_replication_lag_seconds Replication lag seconds by task.\n")
	b.WriteString("# TYPE binlog_server_replication_lag_seconds gauge\n")
	for _, task := range items {
		progress, ok, err := s.tasks.GetReplicationProgress(task.ID)
		if err != nil || !ok || progress.LastEventAt.IsZero() {
			continue
		}
		lag := now.Sub(progress.LastEventAt).Seconds()
		if lag < 0 {
			lag = 0
		}
		writePromSample(&b, "binlog_server_replication_lag_seconds", map[string]string{
			"task_id": task.ID,
		}, lag)
	}

	b.WriteString("# HELP binlog_server_checkpoint_age_seconds Checkpoint age seconds by task.\n")
	b.WriteString("# TYPE binlog_server_checkpoint_age_seconds gauge\n")
	for _, task := range items {
		checkpoint, ok, err := s.tasks.GetCheckpoint(context.Background(), task.ID)
		if err != nil || !ok || checkpoint.UpdatedAt.IsZero() {
			continue
		}
		age := now.Sub(checkpoint.UpdatedAt).Seconds()
		if age < 0 {
			age = 0
		}
		writePromSample(&b, "binlog_server_checkpoint_age_seconds", map[string]string{
			"task_id": task.ID,
		}, age)
	}

	b.WriteString("# HELP binlog_server_worker_online Worker online status (1=online,0=offline).\n")
	b.WriteString("# TYPE binlog_server_worker_online gauge\n")
	heartbeats, err := s.tasks.ListWorkerHeartbeats(200)
	if err == nil {
		sort.Slice(heartbeats, func(i, j int) bool {
			return heartbeats[i].WorkerID < heartbeats[j].WorkerID
		})
		for _, hb := range heartbeats {
			online := strings.EqualFold(hb.Status, "ONLINE") && !hb.LastSeenAt.IsZero() && now.Sub(hb.LastSeenAt) <= workerOnlineThreshold
			value := 0.0
			if online {
				value = 1.0
			}
			writePromSample(&b, "binlog_server_worker_online", map[string]string{
				"worker_id": hb.WorkerID,
			}, value)
		}
	}

	b.WriteString("# HELP binlog_server_upload_failures_total Total number of upload failed file records.\n")
	b.WriteString("# TYPE binlog_server_upload_failures_total gauge\n")
	var uploadFailuresTotal int64
	if counter, ok := s.tasks.(interface{ CountUploadFailures() (int64, error) }); ok {
		total, err := counter.CountUploadFailures()
		if err == nil {
			uploadFailuresTotal = total
		}
	} else {
		const allFilesLimit = int(^uint(0) >> 1)
		for _, task := range items {
			files, err := s.tasks.ListFiles(task.ID, allFilesLimit)
			if err != nil {
				continue
			}
			for _, file := range files {
				if strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
					uploadFailuresTotal++
				}
			}
		}
	}
	writePromSample(&b, "binlog_server_upload_failures_total", nil, float64(uploadFailuresTotal))

	return b.String()
}

func writePromSample(b *strings.Builder, name string, labels map[string]string, value float64) {
	b.WriteString(name)
	if len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, `%s="%s"`, key, escapePromLabelValue(labels[key]))
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	b.WriteByte('\n')
}

func escapePromLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}
