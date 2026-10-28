// Package tasks provides module-level functionality for tasks.
// input: task mutation requests, metadata source policy, full create specs, TaskStore GetTask/ListTasks/ListTasksPage, and optional TaskDashboardRollup
// output: source-isolated task CRUD/config updates, a spec_revision bump when source/start/storage/cluster_key change so the control loop restarts one dump, Delete through ownership.Step (a live lease becomes STOPPING with pending_delete; an idle row is removed), adopt of a leftover data directory onto the same id, primary-key GetTask refresh that fails on store errors, does not copy a live run's row onto memory, and cancels that run when the row asks to stop, a GetTask read that started before the latest publish left in memory, GetTask that still returns this process's pending dump after STOPPED when the column is absent, standalone leftover data_dir discovery when no task store is configured, unfiltered cluster observation from store.ListTasks, paged list reads, and dashboard counters that use SQL rollups when the store implements them; an update that omits source.tls keeps the stored block (ADR 0012)
// pos: scheduler task-management operations layer (non-runner lifecycle actions)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/mysqltls"
	"binlog_server/internal/ownership"
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
	// A leftover directory already owns this numeric id. The next task must not reuse it.
	if s.store == nil {
		diskMax, err := maxNumericDiskBackupID(s.dataDir)
		if err != nil {
			return Task{}, err
		}
		if diskMax > s.seq {
			s.seq = diskMax
		}
	}

	s.seq++
	id := strconv.Itoa(s.seq)
	now := time.Now()
	task := Task{
		ID:         id,
		Name:       validatedName,
		ClusterKey: validatedClusterKey,
		DesiredRun: TaskDesiredStop,
		Source:     validatedSource,
		Start:      validatedStart,
		Storage:    validatedStorage,
		UpdatedAt:  now,
	}
	if err := s.commitTransitionLocked(task, StateCreated, transitionRecord{Action: "create", Type: "TASK_CREATED", Message: "task created"}); err != nil {
		s.seq--
		return Task{}, err
	}
	return s.tasks[id], nil
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
	return s.commitTransitionLocked(task, task.State, transitionRecord{Action: "source change", Type: "TASK_SOURCE_CONFIGURED", Message: "source configured", Detail: normalized.Host})
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
	task.StartFromDisk, task.OperatorStart = false, nil
	task.UpdatedAt = time.Now()
	return s.commitTransitionLocked(task, task.State, transitionRecord{Action: "start change", Type: "TASK_START_CONFIGURED", Message: "start strategy configured", Detail: string(normalized.Mode)})
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
	return s.commitTransitionLocked(task, task.State, transitionRecord{Action: "storage change", Type: "TASK_STORAGE_CONFIGURED", Message: "storage configured", Detail: ""})
}

// AdoptDiskBackup attaches source identity to a leftover {data_dir}/{id} directory.
// The id stays the directory name. Replication is not started.
// With a task store configured, directories are not adopted from disk.
func (s *Scheduler) AdoptDiskBackup(id string, patch TaskPatch) (adopted Task, adoptErr error) {
	defer func() {
		// Runs after the lock below is released: closes the legacy
		// TASK_UNADOPTED at once (tip QA b4c52f4 P1).
		if adoptErr == nil {
			s.reconcileUnadoptedLegacy()
		}
	}()
	id = strings.TrimSpace(id)
	if id == "" {
		return Task{}, ErrTaskNotFound
	}
	validatedClusterKey, err := normalizeAndValidateClusterKey(patch.ClusterKey)
	if err != nil {
		return Task{}, err
	}
	var validatedName string
	if patch.Name != nil {
		validatedName, err = normalizeAndValidateTaskName(*patch.Name)
		if err != nil {
			return Task{}, err
		}
	} else {
		validatedName, err = normalizeAndValidateTaskName(id)
		if err != nil {
			return Task{}, err
		}
	}
	if patch.Source == nil {
		return Task{}, ErrSourceRequired
	}
	if patch.Source.Password == "" {
		return Task{}, ErrSourcePasswordRequired
	}
	validatedSource, err := s.normalizeAndValidateSourceConfig(*patch.Source)
	if err != nil {
		return Task{}, err
	}
	validatedSource.Password = patch.Source.Password

	explicitStart := patch.Start != nil && strings.TrimSpace(string(patch.Start.Mode)) != ""
	var validatedStart StartConfig
	if explicitStart {
		validatedStart, err = normalizeAndValidateStartConfig(*patch.Start)
		if err != nil {
			return Task{}, err
		}
	}

	var validatedStorage Storage
	if patch.Storage != nil {
		candidate := *patch.Storage
		if candidate.RetentionDays == 0 && candidate.Dir == "" {
			candidate.RetentionDays = defaultRetentionDays
		}
		validatedStorage, err = normalizeAndValidateStorage(candidate)
		if err != nil {
			return Task{}, err
		}
	} else {
		validatedStorage = Storage{RetentionDays: defaultRetentionDays}
	}

	if err := s.syncTasksFromStore(); err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[id]; ok {
		return Task{}, ErrTaskAlreadyHasMetadata
	}
	if s.store != nil {
		return Task{}, ErrTaskNotFound
	}
	if _, found, err := lookupDiskBackupTask(s.dataDir, id); err != nil {
		return Task{}, err
	} else if !found {
		return Task{}, ErrTaskNotFound
	}
	if !explicitStart {
		validatedStart, err = diskResumeStart(s.dataDir, id)
		if err != nil {
			return Task{}, err
		}
	}
	if !s.isClusterKeyUniqueLocked(validatedClusterKey, id) {
		return Task{}, ErrClusterKeyExists
	}
	if n, convErr := strconv.Atoi(id); convErr == nil && n > s.seq {
		s.seq = n
	}

	task := Task{
		ID:                id,
		Name:              validatedName,
		ClusterKey:        validatedClusterKey,
		Source:            validatedSource,
		Start:             validatedStart,
		Storage:           validatedStorage,
		KeepLocalSegments: true,
		StartFromDisk:     !explicitStart,
		UpdatedAt:         time.Now(),
	}
	if err := s.commitTransitionLocked(task, StateStopped, transitionRecord{Action: "adopt", Type: "TASK_ADOPTED", Message: "on-disk backup adopted", Detail: validatedClusterKey}); err != nil {
		return Task{}, err
	}
	return s.tasks[id], nil
}

// UpdateTask 以原子方式应用 patch（先校验，后一次落库）。
// 改源库、起点、保留或 cluster_key 时 spec_revision 加一。控制回路关掉当前 dump，
// Close 返回后再开一条。只改名字不增加修订号。
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

	current, ok := s.tasks[id]
	if !ok {
		if err := readOnlyDiskBackup(s.store, s.dataDir, id); err != nil {
			s.mu.Unlock()
			return Task{}, err
		}
		s.mu.Unlock()
		return Task{}, ErrTaskNotFound
	}
	if !s.isClusterKeyUniqueLocked(validatedClusterKey, id) {
		s.mu.Unlock()
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
		// A console edit sends source without tls. Empty means "leave it".
		// mode disabled is an explicit clear.
		if validatedSource.TLS == (mysqltls.Config{}) {
			validatedSource.TLS = current.Source.TLS
		}
		next.Source = *validatedSource
	}
	if validatedStart != nil {
		next.Start = *validatedStart
		next.StartFromDisk, next.OperatorStart = false, nil
	}
	if validatedStorage != nil {
		next.Storage = *validatedStorage
	}
	specBumped := false
	if dumpConfigChanged(current, validatedClusterKey, validatedSource, validatedStart, validatedStorage) {
		next.SpecRevision++
		specBumped = true
	}
	next.UpdatedAt = time.Now()

	if err := s.commitTransitionLocked(next, next.State, transitionRecord{Action: "update", Type: "TASK_UPDATED", Message: "task updated"}); err != nil {
		s.mu.Unlock()
		return Task{}, err
	}
	s.mu.Unlock()
	if specBumped && s.runner != nil {
		_, _ = s.convergeTask(id)
	}
	return next, nil
}

// dumpConfigChanged 判断这次 patch 会不会改拉流正在用的源库、起点、保留或 cluster_key。
// 空密码表示保留原密码。没传的字段不算改动。
func dumpConfigChanged(current Task, clusterKey string, source *SourceConfig, start *StartConfig, storage *Storage) bool {
	if clusterKey != current.ClusterKey {
		return true
	}
	if source != nil {
		next := *source
		if next.Password == "" {
			next.Password = current.Source.Password
		}
		if next != current.Source {
			return true
		}
	}
	if start != nil && *start != current.Start {
		return true
	}
	if storage != nil && *storage != current.Storage {
		return true
	}
	return false
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
	return s.commitTransitionLocked(task, task.State, transitionRecord{Action: "cluster_key change", Type: "TASK_CLUSTER_KEY_CONFIGURED", Message: "cluster key configured", Detail: validatedClusterKey})
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
	return s.commitTransitionLocked(task, task.State, transitionRecord{Action: "rename", Type: "TASK_RENAMED", Message: "task renamed", Detail: validatedName})
}

func (s *Scheduler) GetTask(id string) (Task, error) {
	// 有 store 时按主键读最新值。store 说没有就是没有；其它错误原样失败，
	// 不把内存里的旧主人/epoch 抄本当成读成功，也不改扫 data_dir。
	// 行已经是 STOPPING 或 STOPPED、且本进程还在跑时，取消这次执行，不用这行覆盖内存里的 owner/epoch。
	// 没有 store 时先读内存名单，再认 {data_dir}/{id} 里仍有分段的目录。
	s.mu.Lock()
	task, ok := s.tasks[id]
	store := s.store
	dataDir := s.dataDir
	seen := s.persisted[id].published
	s.mu.Unlock()

	if store != nil {
		ctx, cancel := s.withReadTimeout(context.Background())
		item, err := store.GetTask(ctx, id)
		cancel()
		if err != nil {
			return Task{}, err
		}
		s.mu.Lock()
		if _, pending := s.unpublished[id]; pending {
			// The store has not taken this task's latest transition yet; the
			// memory row is the truth until the retry lands.
			if mem, exists := s.tasks[id]; exists {
				mem = s.overlayPendingDumpLocked(mem)
				s.mu.Unlock()
				return mem, nil
			}
		}
		// 读发生在这次 publish 之前时，行还是 STOPPED / epoch 0。不能把它写回刚 Acquire 的抄本。
		// 本进程还有 dump 时，也不把这行抄进内存：owner/epoch 留在这次执行上。
		liveRun := false
		if done, ok := s.runs[id]; ok && !isClosed(done) {
			liveRun = true
		}
		if !s.staleStoreReadLocked(id, seen) {
			if liveRun {
				s.noteRemoteStopLocked(item)
				if mem, ok := s.tasks[id]; ok && item.PendingDumpCleanup != nil {
					mem.PendingDumpCleanup = item.PendingDumpCleanup
					s.tasks[id] = mem
				}
			} else if !s.noteRemoteStopLocked(item) {
				item = s.keepPendingDump(id, item)
				s.tasks[id] = item
			}
		}
		item = s.overlayPendingDumpLocked(item)
		if mem, exists := s.tasks[id]; exists {
			mem.PendingDumpCleanup = item.PendingDumpCleanup
			s.tasks[id] = mem
		}
		s.flushPendingEventsLocked()
		s.mu.Unlock()
		return item, nil
	}

	if ok {
		s.mu.Lock()
		task = s.overlayPendingDumpLocked(task)
		if mem, exists := s.tasks[id]; exists {
			mem.PendingDumpCleanup = task.PendingDumpCleanup
			s.tasks[id] = mem
		}
		s.mu.Unlock()
		return task, nil
	}
	disk, found, err := lookupDiskBackupTask(dataDir, id)
	if err != nil {
		return Task{}, err
	}
	if !found {
		return Task{}, ErrTaskNotFound
	}
	s.mu.Lock()
	if current, live := s.tasks[id]; live {
		s.mu.Unlock()
		return current, nil
	}
	s.mu.Unlock()
	return disk, nil
}

// DeleteTask 删除任务，并尝试释放 lease/停止运行。
func (s *Scheduler) DeleteTask(id string) error {
	s.mu.Lock()
	task, ok := s.tasks[id]
	if !ok {
		err := readOnlyDiskBackup(s.store, s.dataDir, id)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrTaskNotFound
	}
	now := time.Now()
	st := s.ownershipState(task, now)
	if task.OwnerWorkerID != "" && task.Epoch > 0 {
		st.Lease.Present = true
		st.Lease.Owner = task.OwnerWorkerID
		st.Lease.Epoch = task.Epoch
		st.Lease.ExpireAt = now.Add(time.Minute)
	}
	res := ownership.Step(st, ownership.Event{Kind: ownership.EventOperatorDelete, Node: s.holderNode()}, now)
	if res.Decision == ownership.DecisionRejected {
		s.mu.Unlock()
		status := res.HTTP
		if status == 0 {
			status = 409
		}
		return &OperatorError{Status: status, Detail: res.Detail}
	}
	if res.Decision == ownership.DecisionApplied && !res.State.Deleted {
		task.DesiredRun = string(res.State.Desired)
		task.PendingDelete = true
		task.SpecRevision = res.State.SpecRevision
		task.UpdatedAt = now
		if err := s.commitTransitionLocked(task, StateStopping, transitionRecord{Action: "delete", Type: "TASK_STOPPING", Message: "delete recorded; the lease is still live"}); err != nil {
			s.mu.Unlock()
			return err
		}
		if cancel, ok := s.cancels[id]; ok {
			cancel()
			delete(s.cancels, id)
		}
		s.mu.Unlock()
		return nil
	}
	if err := s.commitDeleteLocked(id); err != nil {
		s.mu.Unlock()
		return err
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

// ListTasks 列出当前内存视图中的全部任务。没有 task store 时，把仍有分段的
// {data_dir}/<id> 目录并进同一份名单。磁盘读失败时仍返回内存任务。
func (s *Scheduler) ListTasks() []Task {
	items, err := s.listMemoryAndDiskTasks()
	if err != nil {
		log.Printf("list on-disk backups: %v", err)
	}
	return items
}

func (s *Scheduler) listMemoryAndDiskTasks() ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Task, 0, len(s.tasks))
	known := make(map[string]struct{}, len(s.tasks))
	for id, task := range s.tasks {
		out = append(out, task)
		known[id] = struct{}{}
	}
	if s.store != nil {
		return out, nil
	}
	disk, err := listDiskBackupTasks(s.dataDir, known)
	// S15: every list path (ListTasks, API pages, observation) goes through
	// here, so a data_dir that cannot be listed is a finding wherever it shows.
	if err != nil {
		s.SignalFailure(Finding{TaskID: ClusterFindingTask, Kind: FindingDiskListFailed, Subject: s.dataDir, Severity: FindingWarning, Detail: "on-disk backups under data_dir are not listed: " + err.Error()})
		return out, err
	}
	s.SignalSuccess(ClusterFindingTask, FindingDiskListFailed, s.dataDir)
	return append(out, disk...), nil
}

// ListClusterObservation 返回全库所有权抄本。有 store 时读 store.ListTasks，
// 不走任务观测过滤，也不用启动时的内存名单。没有 store 时仍用同一份内存名单。
func (s *Scheduler) ListClusterObservation(ctx context.Context) ([]Task, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if store != nil {
		readCtx, cancel := s.withReadTimeout(ctx)
		defer cancel()
		return store.ListTasks(readCtx)
	}
	return s.listMemoryAndDiskTasks()
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
	items, err := s.listMemoryAndDiskTasks()
	if err != nil {
		return nil, 0, err
	}
	page, total := PageTasks(items, filter)
	return page, total, nil
}

// DashboardCounters returns filtered state counts, per-source rollups, and RUNNING refs.
// Limit and offset are ignored. A TaskDashboardRollup store answers with SQL; other stores use one filtered read.
func (s *Scheduler) DashboardCounters(ctx context.Context, filter TaskListFilter) (DashboardCounters, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	filter.Limit = 0
	filter.Offset = 0
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if rollup, ok := store.(TaskDashboardRollup); ok {
		readCtx, cancel := s.withReadTimeout(ctx)
		defer cancel()
		states, err := rollup.CountTaskStates(readCtx, filter)
		if err != nil {
			return DashboardCounters{}, err
		}
		sources, err := rollup.CountTasksBySource(readCtx, filter)
		if err != nil {
			return DashboardCounters{}, err
		}
		if sources == nil {
			sources = []TaskSourceCount{}
		}
		var running []RunningTaskRef
		if states.Running > 0 {
			running, err = rollup.ListRunningTaskRefs(readCtx, filter)
			if err != nil {
				return DashboardCounters{}, err
			}
		}
		return DashboardCounters{States: states, Sources: sources, Running: running}, nil
	}
	items, err := s.tasksMatchingFilter(ctx, filter)
	if err != nil {
		return DashboardCounters{}, err
	}
	sources := SummarizeTasksBySource(items)
	if sources == nil {
		sources = []TaskSourceCount{}
	}
	return DashboardCounters{
		States:  SummarizeTaskStates(items),
		Sources: sources,
		Running: RunningRefs(items),
	}, nil
}

func (s *Scheduler) tasksMatchingFilter(ctx context.Context, filter TaskListFilter) ([]Task, error) {
	filter.Limit = 0
	filter.Offset = 0
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if store != nil {
		readCtx, cancel := s.withReadTimeout(ctx)
		defer cancel()
		items, _, err := store.ListTasksPage(readCtx, filter)
		return items, err
	}
	items, err := s.listMemoryAndDiskTasks()
	if err != nil {
		return nil, err
	}
	return FilterTasks(items, filter), nil
}

// ReportReplicationProgress 上报最新复制进度，供延迟计算和展示。
