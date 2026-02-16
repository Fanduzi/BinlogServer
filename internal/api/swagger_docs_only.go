package api

import (
	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

var (
	_ tasks.Task
	_ binlog.Checkpoint
)

// swaggerTaskGetDoc godoc
// @Summary Get task by ID
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Success 200 {object} tasks.Task
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id} [get]
func (s *Server) swaggerTaskGetDoc() {}

// swaggerTaskUpdateDoc godoc
// @Summary Update task configuration
// @Tags Tasks
// @Accept json
// @Produce json
// @Param id path string true "Task ID"
// @Param body body updateTaskRequest true "Task update payload"
// @Success 200 {object} tasks.Task
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id} [put]
func (s *Server) swaggerTaskUpdateDoc() {}

// swaggerTaskDeleteDoc godoc
// @Summary Delete task
// @Tags Tasks
// @Param id path string true "Task ID"
// @Success 204 {string} string
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Router /api/tasks/{id} [delete]
func (s *Server) swaggerTaskDeleteDoc() {}

// swaggerTaskStartDoc godoc
// @Summary Start task
// @Tags Tasks
// @Param id path string true "Task ID"
// @Success 204 {string} string
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Router /api/tasks/{id}/start [post]
func (s *Server) swaggerTaskStartDoc() {}

// swaggerTaskStopDoc godoc
// @Summary Stop task
// @Tags Tasks
// @Param id path string true "Task ID"
// @Success 204 {string} string
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Router /api/tasks/{id}/stop [post]
func (s *Server) swaggerTaskStopDoc() {}

// swaggerTaskCheckpointDoc godoc
// @Summary Get task checkpoint
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Success 200 {object} binlog.Checkpoint
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/checkpoint [get]
func (s *Server) swaggerTaskCheckpointDoc() {}

// swaggerTaskEventsDoc godoc
// @Summary List task events
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Param limit query int false "Event list limit"
// @Success 200 {array} tasks.TaskEvent
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/events [get]
func (s *Server) swaggerTaskEventsDoc() {}

// swaggerTaskFilesDoc godoc
// @Summary List binlog file metadata
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Param limit query int false "File list limit"
// @Success 200 {array} tasks.BinlogFile
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/files [get]
func (s *Server) swaggerTaskFilesDoc() {}

// swaggerTaskReplicationDoc godoc
// @Summary Get task replication delay and latest position
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Success 200 {object} taskReplicationResponse
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/replication [get]
func (s *Server) swaggerTaskReplicationDoc() {}
