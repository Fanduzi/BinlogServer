// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses including SQL-paged task lists, task batch creation, /healthz, and /api/health
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"net/http"

	"binlog_server/internal/binlog"
	_ "binlog_server/internal/swaggerdocs"
	"binlog_server/internal/tasks"
	"binlog_server/internal/ui"

	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

type taskService interface {
	// 任务 CRUD/控制。
	CreateTask(name string, clusterKey string) (tasks.Task, error)
	CreateTaskFromSpec(name string, clusterKey string, source *tasks.SourceConfig, start *tasks.StartConfig, storage *tasks.Storage) (tasks.Task, error)
	UpdateTask(id string, patch tasks.TaskPatch) (tasks.Task, error)
	ConfigureClusterKey(id string, clusterKey string) error
	ConfigureName(id string, name string) error
	ConfigureSource(id string, source tasks.SourceConfig) error
	ConfigureStart(id string, start tasks.StartConfig) error
	ConfigureStorage(id string, storage tasks.Storage) error
	GetTask(id string) (tasks.Task, error)
	GetReplicationProgress(id string) (tasks.ReplicationProgress, bool, error)
	GetCheckpoint(ctx context.Context, id string) (binlog.Checkpoint, bool, error)
	ListEvents(id string, limit int) ([]tasks.TaskEvent, error)
	ListFiles(id string, limit int) ([]tasks.BinlogFile, error)
	RetryFailedUploads(id string, limit int) (tasks.UploadRetryStats, error)
	GetUploadRetryMetrics() tasks.UploadRetryMetrics
	ListUploadFailureReasons(id string, limit int) ([]tasks.UploadFailureReason, error)
	ListRuns(id string, limit int) ([]tasks.TaskRun, error)
	ListWorkerHeartbeats(limit int) ([]tasks.WorkerHeartbeat, error)
	ListTasks() []tasks.Task
	ListTasksPage(ctx context.Context, filter tasks.TaskListFilter) ([]tasks.Task, int, error)
	DeleteTask(id string) error
	StartTask(id string) error
	StopTask(id string) error
}

// Server 是 Gin HTTP 入口，负责路由和 handler 组织。
type Server struct {
	tasks          taskService
	gin            *gin.Engine
	auth           AuthConfig
	metricsHandler http.Handler
	tracing        TracingConfig
	rateLimiter    *IPRateLimiter
}

// NewServer 构建 API/UI HTTP handler。
func NewServer(taskSvc taskService, opts ...ServerOption) http.Handler {
	options := defaultServerOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	options.auth = normalizeAuthConfig(options.auth)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	s := &Server{
		tasks:          taskSvc,
		gin:            engine,
		auth:           options.auth,
		metricsHandler: newMetricsHandler(taskSvc),
		tracing:        options.tracing,
		rateLimiter:    NewIPRateLimiter(options.rateLimit),
	}
	s.routes()
	return s
}

// routes 注册所有 HTTP 路由（system/tasks/cluster/swagger/ui）。
func (s *Server) routes() {
	// 添加 tracing 中间件。
	if s.tracing.Enabled {
		s.gin.Use(tracingMiddleware(s.tracing))
	}

	// 添加速率限制中间件（应用于所有路由）。
	if s.rateLimiter != nil && s.rateLimiter.config.Enabled {
		s.gin.Use(RateLimitMiddleware(s.rateLimiter))
	}

	s.gin.GET("/healthz", gin.WrapF(s.handleHealth))
	metricsHandlers := []gin.HandlerFunc{}
	if s.auth.Enabled && s.auth.ProtectMetrics {
		metricsHandlers = append(metricsHandlers, s.authMiddleware())
	}
	metricsHandlers = append(metricsHandlers, gin.WrapF(s.handleMetrics))
	s.gin.GET("/metrics", metricsHandlers...)

	apiHandlers := []gin.HandlerFunc{}
	if s.auth.Enabled && s.auth.ProtectAPI {
		apiHandlers = append(apiHandlers, s.authMiddleware())
	}
	apiGroup := s.gin.Group("/api", apiHandlers...)
	apiGroup.GET("/health", gin.WrapF(s.handleAPIHealth))
	apiGroup.GET("/summary", gin.WrapF(s.handleSummary))
	apiGroup.GET("/dashboard", gin.WrapF(s.handleDashboard))
	apiGroup.GET("/sources/lookup", gin.WrapF(s.handleSourceLookup))
	apiGroup.GET("/workers", gin.WrapF(s.handleWorkers))
	apiGroup.GET("/cluster/overview", gin.WrapF(s.handleClusterOverview))
	apiGroup.POST("/tasks", gin.WrapF(s.handleTasks))
	apiGroup.GET("/tasks", gin.WrapF(s.handleTasks))
	apiGroup.Any("/tasks/*path", gin.WrapF(s.handleTaskAction))
	s.gin.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))

	uiHandler := http.StripPrefix("/ui/", ui.Handler())
	s.gin.Any("/ui/*path", gin.WrapH(uiHandler))
	s.gin.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/ui/")
	})
}

// ServeHTTP 实现 http.Handler 接口。
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

// handleAPIHealth godoc
// @Summary Service health check (JSON)
// @Tags System
// @Produce json
// @Success 200 {object} map[string]string
// @Router /api/health [get]
func (s *Server) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMetrics 返回 Prometheus 文本格式指标。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.metricsHandler.ServeHTTP(w, r)
}
