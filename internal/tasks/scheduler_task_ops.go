// Package tasks provides module-level functionality for tasks.
// input: task mutation requests, metadata source policy, full create specs, and TaskStore GetTask/ListTasksPage
// output: source-isolated task CRUD/config updates, primary-key GetTask refresh, and paged list reads
// pos: scheduler task-management operations layer (non-runner lifecycle actions)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"
)

func (s *Scheduler) CreateTask(name, clusterKey string) (Task, error) {
	return s.createTask(name, clusterKey, nil, nil, nil, false)
}

// CreateTaskFromSpec 在一次校验通过后才落库；HTTP 400 路径不得留下半成品任务。
func (s *Scheduler) CreateTaskFromSpec(name string, clusterKey string, source *SourceConfig, start *StartConfig, storage *Storage) (Task, error) {
	return s.createTask(name, clusterKey, source, start, storage, true)
}

func (s *Scheduler) createTask(name, clusterKey string, source *SourceConfig, start *StartConfig, storage *Storage, requireSource bool) (Task, error) {
	validatedName, err := normalizeAndValidateTaskName(name)
	if err != nil {
		return Task{}, err
	}
	validatedClusterKey, err := normalizeAndValidateClusterKey(clusterKey)
	if err != nil {
		return Task{}, err
	}

	var validatedSource SourceConfig
	if requireSource {
		if source == nil {
			return Task{}, ErrSourceRequired
		}
		if source.Password == "" {
			return Task{}, ErrSourcePasswordRequired
		}
		validatedSource, err = s.normalizeAndValidateSourceConfig(*source)
		if err != nil {
			return Task{}, err
		}
		validatedSource.Password = source.Password
	} else if source != nil {
		validatedSource, err = s.normalizeAndValidateSourceConfig(*source)
		if err != nil {
			return Task{}, err
		}
		validatedSource.Password = source.Password
	}

	validatedStart := StartConfig{Mode: StartModeLatest}
	if start != nil {
		validatedStart, err = normalizeAndValidateStartConfig(*start)
		if err != nil {
			return Task{}, err
		}
	}

	validatedStorage := Storage{RetentionDays: defaultRetentionDays}
	if storage != nil {
		candidate := *storage
		if candidate.RetentionDays == 0 && candidate.Dir == "" {
			candidate.RetentionDays = defaultRetentionDays
		}
		validatedStorage, err = normalizeAndValidateStorage(candidate)
		if err != nil {
			return Task{}, err
		}
	}

	if err := s.syncTasksFromStore(); err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isClusterKeyUniqueLocked(validatedClusterKey, "") {
		return Task{}, ErrClusterKeyExists
	}

	s.seq++
	id := strconv.Itoa(s.seq)
	now := time.Now()
	task := Task{
		ID:         id,
		Name:       validatedName,
		ClusterKey: validatedClusterKey,
		State:      StateCreated,
		Source:     validatedSource,
		Start:      validatedStart,
		Storage:    validatedStorage,
		UpdatedAt:  now,
	}
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_CREATED", "task created", "")
	if err := s.persistTaskLocked(task); err != nil {
		delete(s.tasks, id)
		s.seq--
		return Task{}, err
	}
	return task, nil
}

// ConfigureSource 更新任务源库配置。
func (s *Scheduler) ConfigureSource(id string, source SourceConfig) error {
	normalized, err := s.normalizeAndValidateSourceConfig(source)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	if normalized.Password == "" {
		normalized.Password = task.Source.Password
	}
	task.Source = normalized
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_SOURCE_CONFIGURED", "source configured", normalized.Host)
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

// ConfigureStart 更新任务拉流起点配置。
func (s *Scheduler) ConfigureStart(id string, start StartConfig) error {
	normalized, err := normalizeAndValidateStartConfig(start)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	task.Start = normalized
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_START_CONFIGURED", "start strategy configured", string(normalized.Mode))
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

// ConfigureStorage 更新任务存储策略。
func (s *Scheduler) ConfigureStorage(id string, storage Storage) error {
	normalized, err := normalizeAndValidateStorage(storage)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	task.Storage = normalized
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STORAGE_CONFIGURED", "storage configured", "")
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

// UpdateTask 以原子方式应用 patch（先校验，后一次落库）。
func (s *Scheduler) UpdateTask(id string, patch TaskPatch) (Task, error) {
	// 先做整包校验，再一次性落库；避免“前几项成功、后几项失败”的部分持久化副作用。
	validatedClusterKey, err := normalizeAndValidateClusterKey(patch.ClusterKey)
	if err != nil {
		return Task{}, err
	}

	var validatedName *string
	if patch.Name != nil {
		name, err := normalizeAndValidateTaskName(*patch.Name)
		if err != nil {
			return Task{}, err
		}
		validatedName = &name
	}

	var validatedSource *SourceConfig
	if patch.Source != nil {
		source, err := s.normalizeAndValidateSourceConfig(*patch.Source)
		if err != nil {
			return Task{}, err
		}
		validatedSource = &source
	}

	var validatedStart *StartConfig
	if patch.Start != nil {
		start, err := normalizeAndValidateStartConfig(*patch.Start)
		if err != nil {
			return Task{}, err
		}
		validatedStart = &start
	}

	var validatedStorage *Storage
	if patch.Storage != nil {
		storage, err := normalizeAndValidateStorage(*patch.Storage)
		if err != nil {
			return Task{}, err
		}
		validatedStorage = &storage
	}

	if err := s.syncTasksFromStore(); err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	if !s.isClusterKeyUniqueLocked(validatedClusterKey, id) {
		return Task{}, ErrClusterKeyExists
	}

	// 基于 current 构造 next，保证未传字段保持原值（partial update 语义）。
	next := current
	next.ClusterKey = validatedClusterKey
	if validatedName != nil {
		next.Name = *validatedName
	}
	if validatedSource != nil {
		if validatedSource.Password == "" {
			validatedSource.Password = current.Source.Password
		}
		next.Source = *validatedSource
	}
	if validatedStart != nil {
		next.Start = *validatedStart
	}
	if validatedStorage != nil {
		next.Storage = *validatedStorage
	}
	next.UpdatedAt = time.Now()

	if err := s.persistTaskLocked(next); err != nil {
		return Task{}, err
	}

	s.tasks[id] = next
	s.appendEventLocked(id, "TASK_UPDATED", "task updated", "")
	return next, nil
}

// ConfigureClusterKey 更新任务 cluster_key（要求全局唯一）。
func (s *Scheduler) ConfigureClusterKey(id, clusterKey string) error {
	validatedClusterKey, err := normalizeAndValidateClusterKey(clusterKey)
	if err != nil {
		return err
	}
	if err := s.syncTasksFromStore(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	if !s.isClusterKeyUniqueLocked(validatedClusterKey, id) {
		return ErrClusterKeyExists
	}
	task.ClusterKey = validatedClusterKey
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_CLUSTER_KEY_CONFIGURED", "cluster key configured", validatedClusterKey)
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

// ConfigureName 更新任务名。
func (s *Scheduler) ConfigureName(id, name string) error {
	validatedName, err := normalizeAndValidateTaskName(name)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	task.Name = validatedName
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_RENAMED", "task renamed", validatedName)
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

// StartTask 启动任务；cluster 模式下会先 acquire lease。

func (s *Scheduler) GetTask(id string) (Task, error) {
	// 常见误解：
	// GetTask 返回的不一定是“纯内存快照”，当配置了 store 时会按主键拉取持久化最新值，
	// 目的是让 API 视图更接近真实元数据状态。
	s.mu.Lock()
	task, ok := s.tasks[id]
	store := s.store
	s.mu.Unlock()

	if store != nil {
		ctx, cancel := s.withReadTimeout(context.Background())
		item, err := store.GetTask(ctx, id)
		cancel()
		if err == nil {
			s.mu.Lock()
			s.tasks[id] = item
			s.mu.Unlock()
			return item, nil
		}
		if errors.Is(err, ErrTaskNotFound) {
			return Task{}, ErrTaskNotFound
		}
		if ok {
			return task, nil
		}
		return Task{}, err
	}

	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return task, nil
}

// DeleteTask 删除任务，并尝试释放 lease/停止运行。
func (s *Scheduler) DeleteTask(id string) error {
	s.mu.Lock()
	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
	delete(s.runs, id)
	delete(s.tasks, id)
	delete(s.events, id)
	delete(s.replica, id)
	if s.store != nil {
		ctx, cancel := s.withWriteTimeout(context.Background())
		if err := s.store.DeleteTask(ctx, id); err != nil {
			cancel()
			s.mu.Unlock()
			return err
		}
		cancel()
	}
	s.mu.Unlock()

	if s.leaseManager != nil && task.OwnerWorkerID != "" && task.Epoch > 0 {
		ctx, cancel := s.withLeaseTimeout(context.Background())
		released, err := s.leaseManager.Release(ctx, id, task.OwnerWorkerID, task.Epoch)
		cancel()
		if err != nil || !released {
			log.Printf("lease release on delete failed task=%s owner=%s epoch=%d released=%v err=%v", id, task.OwnerWorkerID, task.Epoch, released, err)
		}
	}
	return nil
}

// ListTasks 列出当前内存视图中的全部任务。
func (s *Scheduler) ListTasks() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out
}

// ListTasksPage 返回过滤后的一页任务。有 store 时走 SQL 分页；standalone 仍切内存快照。
func (s *Scheduler) ListTasksPage(ctx context.Context, filter TaskListFilter) ([]Task, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if store != nil {
		readCtx, cancel := s.withReadTimeout(ctx)
		defer cancel()
		return store.ListTasksPage(readCtx, filter)
	}
	page, total := PageTasks(s.ListTasks(), filter)
	return page, total, nil
}

// ReportReplicationProgress 上报最新复制进度，供延迟计算和展示。
