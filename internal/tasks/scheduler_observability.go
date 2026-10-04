// Package tasks provides module-level functionality for tasks.
// input: replication/checkpoint/event/file/history read requests and TaskStore.GetTask for missing-task refresh
// output: observability-facing task progress including at-tip lag, events, meta or on-disk files in ascending source-index replay order, leftover-directory file lists, the resume file/pos (and gtid_set when the stored checkpoint matches) the next Start continues from, a catalog file_path takeover position instead of a position-4 rewind, runs, worker heartbeat views, and the runner's retention-blocked file counts for metrics
// pos: scheduler read/query layer for API and metrics consumption; missing-task checkpoint refresh uses GetTask
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

func (s *Scheduler) ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32, atTip bool) {
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
	progress.AtTip = atTip
	progress.UpdatedAt = time.Now()
	s.replica[taskID] = progress
}

// GetReplicationProgress 获取任务复制进度快照。
func (s *Scheduler) GetReplicationProgress(taskID string) (ReplicationProgress, bool, error) {
	s.mu.Lock()
	_, ok := s.tasks[taskID]
	store := s.store
	dataDir := s.dataDir
	progress, hasProgress := s.replica[taskID]
	s.mu.Unlock()

	if !ok {
		if store != nil {
			return ReplicationProgress{}, false, ErrTaskNotFound
		}
		_, found, err := lookupDiskBackupTask(dataDir, taskID)
		if err != nil {
			return ReplicationProgress{}, false, err
		}
		if !found {
			return ReplicationProgress{}, false, ErrTaskNotFound
		}
		return ReplicationProgress{}, false, nil
	}
	return progress, hasProgress, nil
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
	dataDir := s.dataDir
	s.mu.Unlock()

	// Step 1: 内存未命中时，按主键补齐该任务，不加载整表。
	if !ok && store != nil {
		readCtx, cancel := s.withReadTimeout(ctx)
		item, err := store.GetTask(readCtx, taskID)
		cancel()
		if err != nil {
			return binlog.Checkpoint{}, false, err
		}
		s.mu.Lock()
		s.tasks[item.ID] = item
		s.mu.Unlock()
	}
	if !ok && store == nil {
		_, found, err := lookupDiskBackupTask(dataDir, taskID)
		if err != nil {
			return binlog.Checkpoint{}, false, err
		}
		if !found {
			return binlog.Checkpoint{}, false, ErrTaskNotFound
		}
	}
	// Step 2: 读 checkpoint（未配置 reader 时返回未命中）。
	if s.checkpointReader == nil {
		return binlog.Checkpoint{}, false, nil
	}
	return s.checkpointReader.LoadCheckpoint(ctx, taskID)
}

// ResumePosition is the file, pos, and gtid_set the next Start continues from.
// It reads the same open-segment cursor the runner uses, then the stored checkpoint.
// GetCheckpoint stays the stored row and does not invent one from disk.
func (s *Scheduler) ResumePosition(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return binlog.Checkpoint{}, false, err
	}
	stored, ok, err := s.GetCheckpoint(ctx, taskID)
	if err != nil {
		return binlog.Checkpoint{}, false, err
	}
	s.mu.Lock()
	dataDir := s.dataDir
	s.mu.Unlock()
	resume, found := NextResumePosition(dataDir, task, stored, ok)
	if task.Epoch > 1 && !task.KeepLocalSegments && s.fileStore != nil {
		readCtx, cancel := s.withReadTimeout(ctx)
		files, err := s.fileStore.ListBinlogFiles(readCtx, taskID, segmentInventoryLimit)
		cancel()
		if err != nil {
			return binlog.Checkpoint{}, false, err
		}
		decision := ResolveTakeover(dataDir, task, stored, ok, files)
		if decision.Missing != "" && ok && stored.File != "" && stored.Pos > 0 {
			return stored, true, nil
		}
		if decision.Apply {
			return decision.Checkpoint, true, nil
		}
	}
	return resume, found, nil
}

// ListEvents 列出任务事件，limit<=0 时按默认值处理。
func (s *Scheduler) ListEvents(taskID string, limit int) ([]TaskEvent, error) {
	s.mu.Lock()
	_, ok := s.tasks[taskID]
	if !ok {
		store := s.store
		dataDir := s.dataDir
		s.mu.Unlock()
		if store != nil {
			return nil, ErrTaskNotFound
		}
		_, found, err := lookupDiskBackupTask(dataDir, taskID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrTaskNotFound
		}
		return []TaskEvent{}, nil
	}
	defer s.mu.Unlock()

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

// ListFiles 列出任务文件。元数据目录非空时返回目录结果。
// MySQL 目录已按源序号升序排好，同序号先封存再 open epoch；limit 保留序号最大的窗口。
// 未配置文件库，或该任务一条目录都没有时，扫描 {data_dir}/{task_id}。
// 没有 task store、内存里也没有这个 id 时，目录里仍有分段则同样扫描。
func (s *Scheduler) ListFiles(taskID string, limit int) ([]BinlogFile, error) {
	s.mu.Lock()
	_, ok := s.tasks[taskID]
	store := s.fileStore
	taskStore := s.store
	dataDir := s.dataDir
	s.mu.Unlock()
	if !ok {
		if taskStore != nil {
			return nil, ErrTaskNotFound
		}
		files, err := listTaskBinlogFilesOnDisk(dataDir, taskID, limit)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, ErrTaskNotFound
		}
		return files, nil
	}
	if store != nil {
		ctx, cancel := s.withReadTimeout(context.Background())
		files, err := store.ListBinlogFiles(ctx, taskID, limit)
		cancel()
		if err != nil {
			return nil, err
		}
		if len(files) > 0 || strings.TrimSpace(dataDir) == "" {
			return files, nil
		}
		disk, err := listTaskBinlogFilesOnDisk(dataDir, taskID, limit)
		if err != nil {
			return nil, err
		}
		if len(disk) == 0 {
			return files, nil
		}
		return disk, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return []BinlogFile{}, nil
	}
	return listTaskBinlogFilesOnDisk(dataDir, taskID, limit)
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

// RetentionBlockedFiles returns how many expired sealed files the runner is
// keeping because they are not uploaded. The count is per task id and drops
// when a later retention pass purges those files. A runner that does not
// report the count contributes nothing.
func (s *Scheduler) RetentionBlockedFiles() map[string]int {
	s.mu.Lock()
	runner := s.runner
	s.mu.Unlock()
	counter, ok := runner.(interface {
		RetentionBlockedFiles() map[string]int
	})
	if !ok || counter == nil {
		return map[string]int{}
	}
	counts := counter.RetentionBlockedFiles()
	if counts == nil {
		return map[string]int{}
	}
	return counts
}
