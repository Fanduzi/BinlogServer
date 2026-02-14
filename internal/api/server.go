package api

import (
	"context"
	"net/http"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
	"binlog_server/internal/ui"

	"github.com/gin-gonic/gin"
)

type taskService interface {
	CreateTask(name string) (tasks.Task, error)
	ConfigureName(id string, name string) error
	ConfigureSource(id string, source tasks.SourceConfig) error
	ConfigureStart(id string, start tasks.StartConfig) error
	ConfigureStorage(id string, storage tasks.Storage) error
	GetTask(id string) (tasks.Task, error)
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
	s.gin.POST("/api/tasks", gin.WrapF(s.handleTasks))
	s.gin.GET("/api/tasks", gin.WrapF(s.handleTasks))
	s.gin.Any("/api/tasks/*path", gin.WrapF(s.handleTaskAction))

	uiHandler := http.StripPrefix("/ui/", ui.Handler())
	s.gin.Any("/ui/*path", gin.WrapH(uiHandler))
	s.gin.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/ui/")
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.gin.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
