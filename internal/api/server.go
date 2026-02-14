package api

import (
	"net/http"

	"binlog_server/internal/tasks"
)

type taskService interface {
	CreateTask(name string) (tasks.Task, error)
	ListTasks() []tasks.Task
	StartTask(id string) error
	StopTask(id string) error
}

type Server struct {
	tasks taskService
	mux   *http.ServeMux
}

func NewServer(taskSvc taskService) http.Handler {
	s := &Server{
		tasks: taskSvc,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/tasks", s.handleTasks)
	s.mux.HandleFunc("/api/tasks/", s.handleTaskAction)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
