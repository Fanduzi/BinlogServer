// Package tasks provides module-level functionality for tasks.
// input: replication/checkpoint/event/file/history read requests and store snapshots
// output: observability-facing task progress, events, files, runs, and worker heartbeat views
// pos: scheduler read/query layer for API and metrics consumption
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strconv"
	"time"

	"binlog_server/internal/binlog"
)

func (s *Scheduler) ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return
	}
	if task.State == StateStopping || task.State == StateStopped {
		// fail-safe stop 已开始后，拒绝继续接受“健康运行态”的进度上报。
		return
	}
	progress := s.replica[taskID]
	progress.TaskID = taskID
	if !sourceEventAt.IsZero() {
		progress.LastEventAt = sourceEventAt
	}
	if file != "" {
		progress.LastEventFile = file
	}
	if pos > 0 {
		progress.LastEventPos = pos
	}
	progress.UpdatedAt = time.Now()
	s.replica[taskID] = progress
}

// GetReplicationProgress 获取任务复制进度快照。
func (s *Scheduler) GetReplicationProgress(taskID string) (ReplicationProgress, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
		return ReplicationProgress{}, false, ErrTaskNotFound
	}
	progress, ok := s.replica[taskID]
	return progress, ok, nil
}

// runTask 托管单任务执行 goroutine，包含错误重试与状态收敛逻辑。

func (s *Scheduler) Restore(ctx context.Context) error {
	if s.store == nil {
		return nil
	}

	list, err := s.store.ListTasks(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.tasks = make(map[string]Task, len(list))
	s.events = make(map[string][]TaskEvent, len(list))
	maxSeq := 0
	for _, task := range list {
		s.tasks[task.ID] = task

		// 根据持久化 ID 重建内存中的自增基线。
		if n, err := strconv.Atoi(task.ID); err == nil && n > maxSeq {
			maxSeq = n
		}
	}
	s.seq = maxSeq
	return nil
}

// persistTaskLocked 在持锁上下文下把任务状态写入 store。

func (s *Scheduler) GetCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	s.mu.Lock()
	_, ok := s.tasks[taskID]
	store := s.store
	s.mu.Unlock()

	// Step 1: 内存未命中时，从 store 同步一次任务视图。
	if !ok && store != nil {
		readCtx, cancel := s.withReadTimeout(ctx)
		list, err := store.ListTasks(readCtx)
		cancel()
		if err != nil {
			return binlog.Checkpoint{}, false, err
		}
		found := false
		s.mu.Lock()
		for _, item := range list {
			s.tasks[item.ID] = item
			if item.ID == taskID {
				found = true
			}
		}
		s.mu.Unlock()
		if !found {
			return binlog.Checkpoint{}, false, ErrTaskNotFound
		}
	}
	if !ok && store == nil {
		return binlog.Checkpoint{}, false, ErrTaskNotFound
	}
	// Step 2: 读 checkpoint（未配置 reader 时返回未命中）。
	if s.checkpointReader == nil {
		return binlog.Checkpoint{}, false, nil
	}
	return s.checkpointReader.LoadCheckpoint(ctx, taskID)
}

// ListEvents 列出任务事件，limit<=0 时按默认值处理。
func (s *Scheduler) ListEvents(taskID string, limit int) ([]TaskEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
		return nil, ErrTaskNotFound
	}
	if s.eventStore != nil {
		// 优先读持久化事件，避免重启后只看到内存中的事件片段。
		ctx, cancel := s.withReadTimeout(context.Background())
		events, err := s.eventStore.ListEvents(ctx, taskID, limit)
		cancel()
		return events, err
	}
	events := s.events[taskID]
	if limit <= 0 || limit >= len(events) {
		out := make([]TaskEvent, len(events))
		copy(out, events)
		return out, nil
	}
	out := make([]TaskEvent, limit)
	copy(out, events[len(events)-limit:])
	return out, nil
}

// ListFiles 列出任务文件元数据。
func (s *Scheduler) ListFiles(taskID string, limit int) ([]BinlogFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
		return nil, ErrTaskNotFound
	}
	if s.fileStore == nil {
		return []BinlogFile{}, nil
	}
	ctx, cancel := s.withReadTimeout(context.Background())
	files, err := s.fileStore.ListBinlogFiles(ctx, taskID, limit)
	cancel()
	return files, err
}

// RetryFailedUploads 手动重试失败上传（仅 sealed 且状态为 UPLOAD_FAILED）。

func (s *Scheduler) ListRuns(taskID string, limit int) ([]TaskRun, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 200 {
		limit = 200
	}

	s.mu.Lock()
	task, ok := s.tasks[taskID]
	store := s.store
	s.mu.Unlock()

	if !ok {
		return nil, ErrTaskNotFound
	}

	if reader, ok := store.(taskRunReader); ok {
		ctx, cancel := s.withReadTimeout(context.Background())
		runs, err := reader.ListTaskRuns(ctx, taskID, limit)
		cancel()
		return runs, err
	}

	if task.RunID == "" {
		return []TaskRun{}, nil
	}
	return []TaskRun{
		{
			RunID:     task.RunID,
			TaskID:    task.ID,
			WorkerID:  task.OwnerWorkerID,
			Epoch:     task.Epoch,
			StartedAt: task.UpdatedAt,
		},
	}, nil
}

// ListWorkerHeartbeats 返回 worker 心跳列表（用于 cluster 观测）。
func (s *Scheduler) ListWorkerHeartbeats(limit int) ([]WorkerHeartbeat, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 200 {
		limit = 200
	}

	s.mu.Lock()
	store := s.store
	s.mu.Unlock()

	if reader, ok := store.(workerHeartbeatReader); ok {
		ctx, cancel := s.withReadTimeout(context.Background())
		items, err := reader.ListWorkerHeartbeats(ctx, limit)
		cancel()
		return items, err
	}
	return []WorkerHeartbeat{}, nil
}

// appendEventLocked 追加事件到内存并 best-effort 写入持久化层。
