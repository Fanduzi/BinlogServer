// Package api provides module-level functionality for api.
// input: HTTP requests, router params, scheduler/task service interfaces
// output: REST API responses/status codes and generated Swagger declarations for task/cluster operations, including SameSourceHost host-filter docs, 5xx on cluster observation store errors, GET /api/tasks/{id}/checkpoint where epoch greater than 1 follows a readable file_path and does not rewind an unreadable tail to position 4, GET /api/tasks/{id}/replay, the UTC stop_datetime window on that route, GET /api/tasks/{id}/replay/archive, and GET /api/tasks/{id}/files/{name} from local disk or a sealed uploaded object
// pos: external control-plane API layer bridging clients and domain services
// note: if this file changes, update this header and module README.md.
package api

import (
	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

var (
	_ tasks.Task
	_ binlog.Checkpoint
)

// swaggerTaskListDoc godoc
// @Summary List tasks
// @Tags Tasks
// @Produce json
// @Param host query string false "Filter by source host; localhost and explicit loopback literals share one identity, other hosts match exact spelling"
// @Param port query int false "Filter by source port"
// @Param state query string false "Filter by task state (CREATED, STARTING, RUNNING, LEASE_DEGRADED, REBUILDING_FILE, RETRY_BACKOFF, FAILED, STOPPING, STOPPED)"
// @Param limit query int false "Page size (default 100, range 1-500; values above 500 return 400 invalid limit)"
// @Param offset query int false "Zero-based page offset (default 0)"
// @Success 200 {object} taskListResponse
// @Failure 400 {string} string
// @Failure 405 {string} string
// @Router /api/tasks [get]
func (s *Server) swaggerTaskListDoc() {}

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

// swaggerTaskAdoptDoc godoc
// @Summary Adopt a leftover on-disk task directory
// @Description Attaches cluster_key and source to a standalone disk-discovered task id. Omitted start resumes at FILE_POS at the size of the highest local segment. Does not start replication.
// @Tags Tasks
// @Accept json
// @Produce json
// @Param id path string true "Task ID"
// @Param body body updateTaskRequest true "cluster_key and source (host/port/user/password/flavor) are required. name, start, and storage are optional."
// @Success 200 {object} tasks.Task
// @Failure 400 {string} string
// @Failure 404 {string} string
// @Router /api/tasks/{id}/adopt [post]
func (s *Server) swaggerTaskAdoptDoc() {}

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
// @Description Returns the file, pos, and gtid_set the next Start continues from. A local open segment with a complete event wins over the stored checkpoint. gtid_set is included when that stored checkpoint has the same file and pos. With no local event, the stored checkpoint is returned. Epoch greater than 1 rewinds pos to 4 only when the catalog has no row for that tail. A readable binlog_files.file_path continues from the last complete event in that directory. A checkpoint inside a sealed UPLOADED object is not rewound. An unreadable open segment returns the stored checkpoint; Start then fails with SEGMENT_NOT_ON_WORKER and does not open a new directory. 404 when neither exists.
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

// swaggerTaskFileDownloadDoc godoc
// @Summary Download one inventory binlog segment
// @Description Streams one basename already listed by GET /api/tasks/{id}/files. Content-Type is application/octet-stream and Content-Disposition filename is that basename. A file at {data_dir}/{id}/{name} is served at its size at open, including *.open.e* while the task is RUNNING or STOPPED. When that file is missing, a sealed row with upload_state UPLOADED and a non-empty object_key is read from the configured object store, and the body length is the object size at open. LOCAL_ONLY, UPLOAD_FAILED, an empty object_key, an open segment with no local file, or object upload not configured is 404 segment not found on this process. Catalog file_path is not a byte source. A slash, backslash, or .. is 400. A missing task is 404.
// @Tags Tasks
// @Produce application/octet-stream
// @Param id path string true "Task ID"
// @Param name path string true "On-disk basename"
// @Success 200 {file} file "raw binlog bytes"
// @Failure 400 {string} string "invalid segment name"
// @Failure 404 {string} string "task not found or segment not on this process"
// @Router /api/tasks/{id}/files/{name} [get]
func (s *Server) swaggerTaskFileDownloadDoc() {}

// swaggerTaskReplayDoc godoc
// @Summary List the mysqlbinlog replay set for a task
// @Description One on-disk file_path per source index, ascending. When a sealed name and .open.e* share an index, the path is the highest-epoch open segment. Without stop_datetime, limit is the same inventory window as GET /files and the body omits command. stop_datetime (UTC) selects the point-in-time window instead of limit: paths cover events at or after optional start_datetime and before stop_datetime, and command is the TZ=UTC client invocation. An empty cover is 200 with empty paths and command. Invalid datetimes are 400.
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Param limit query int false "Inventory window limit (same as files). Ignored when stop_datetime is set."
// @Param stop_datetime query string false "UTC stop time, YYYY-MM-DD HH:MM:SS or RFC3339. Required for the point-in-time window."
// @Param start_datetime query string false "Optional UTC start time. Requires stop_datetime."
// @Success 200 {object} tasks.PITRSet
// @Failure 400 {string} string "invalid stop_datetime, invalid start_datetime, stop_datetime is required, or start_datetime is after stop_datetime"
// @Failure 404 {string} string
// @Failure 500 {string} string
// @Router /api/tasks/{id}/replay [get]
func (s *Server) swaggerTaskReplayDoc() {}

// swaggerTaskReplayArchiveDoc godoc
// @Summary Download the mysqlbinlog replay set as one tar
// @Description USTAR archive whose members are the basenames from GET /api/tasks/{id}/replay for the same limit, one per source index. stop_datetime uses that route's point-in-time window instead of limit. A sealed name and .open.e* still keep the highest-epoch open segment. Each member uses the GET /api/tasks/{id}/files/{name} open rules: a local file wins, including *.open.e* while RUNNING or STOPPED; a missing local file is read only for a sealed UPLOADED row with a non-empty object_key. An empty selection is 200 and an empty tar. If any selected segment cannot be opened, the response is an error and the body is not a tar. A missing task is 404 task not found.
// @Tags Tasks
// @Produce application/x-tar
// @Param id path string true "Task ID"
// @Param limit query int false "Inventory window limit (same as replay). Ignored when stop_datetime is set."
// @Param stop_datetime query string false "UTC stop time. Same window as GET /api/tasks/{id}/replay."
// @Param start_datetime query string false "Optional UTC start time. Requires stop_datetime."
// @Success 200 {file} file "ustar archive task-{id}-replay.tar"
// @Failure 400 {string} string "invalid segment name or invalid datetime"
// @Failure 404 {string} string "task not found or segment not found on this process"
// @Failure 500 {string} string
// @Router /api/tasks/{id}/replay/archive [get]
func (s *Server) swaggerTaskReplayArchiveDoc() {}

// swaggerTaskRetryUploadDoc godoc
// @Summary Retry failed upload binlog files
// @Tags Tasks
// @Produce json
// @Param id path string true "Task ID"
// @Param limit query int false "Retry upload limit" minimum(1) default(100) maximum(1000)
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
// @Param limit query int false "Failure reason list limit" minimum(1) default(20) maximum(200)
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
// @Failure 500 {string} string
// @Router /api/workers [get]
func (s *Server) swaggerWorkersDoc() {}

// swaggerClusterOverviewDoc godoc
// @Summary Get cluster overview
// @Tags Cluster
// @Produce json
// @Success 200 {object} clusterOverview
// @Failure 405 {string} string
// @Failure 500 {string} string
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
