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
	s.gin.GET("/api/summary", gin.WrapF(s.handleSummary))
	s.gin.GET("/api/dashboard", gin.WrapF(s.handleDashboard))
	s.gin.GET("/api/sources/lookup", gin.WrapF(s.handleSourceLookup))
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
