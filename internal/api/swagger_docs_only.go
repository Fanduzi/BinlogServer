// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses and status codes for task/cluster operations
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module AGENTS.md.
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
// @Param body body updateTaskRequest true "Task update payload（cluster_key 必填，仅允许 [a-zA-Z0-9._-]，禁止 / \\ ..；name/source/start/storage 按字段规则校验）"
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

// swaggerTaskRetryUploadDoc godoc
// @Summary Retry failed upload binlog files
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Param limit query int false "Retry upload limit" default(100) maximum(1000)
// @Success 200 {object} tasks.UploadRetryStats
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Failure 409 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/files/retry-upload [post]
func (s *Server) swaggerTaskRetryUploadDoc() {}

// swaggerTaskUploadFailureReasonsDoc godoc
// @Summary Aggregate upload failure reasons
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Param limit query int false "Failure reason list limit" default(20) maximum(200)
// @Success 200 {array} tasks.UploadFailureReason
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/upload-failures/reasons [get]
func (s *Server) swaggerTaskUploadFailureReasonsDoc() {}

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

// swaggerWorkersDoc godoc
// @Summary List cluster workers
// @Tags Cluster
// @Produce json
// @Param limit query int false "Workers list limit (max 200)"
// @Success 200 {array} workerItem
// @Failure 405 {string} string
// @Router /api/workers [get]
func (s *Server) swaggerWorkersDoc() {}

// swaggerClusterOverviewDoc godoc
// @Summary Get cluster overview
// @Tags Cluster
// @Produce json
// @Success 200 {object} clusterOverview
// @Failure 405 {string} string
// @Router /api/cluster/overview [get]
func (s *Server) swaggerClusterOverviewDoc() {}

// swaggerTaskLeaseDoc godoc
// @Summary Get task lease info
// @Tags Cluster
// @Produce json
// @Param id path string true "Task ID"
// @Success 200 {object} taskLeaseView
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/lease [get]
func (s *Server) swaggerTaskLeaseDoc() {}

// swaggerTaskRunsDoc godoc
// @Summary List task run sessions
// @Tags Cluster
// @Produce json
// @Param id path string true "Task ID"
// @Param limit query int false "Run history limit (default 10, max 200)"
// @Success 200 {array} taskRunView
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/runs [get]
func (s *Server) swaggerTaskRunsDoc() {}
