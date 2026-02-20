package tasks

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/binlog"
)

var ErrTaskNotFound = errors.New("task not found")
var ErrInvalidSourceConfig = errors.New("invalid source config")
var ErrRunnerNotConfigured = errors.New("runner is not configured")
var ErrLeaseNotAcquired = errors.New("lease not acquired")
var ErrClusterWorkerIDRequired = errors.New("cluster worker id is required")
var ErrClusterKeyRequired = errors.New("cluster_key is required")
var ErrClusterKeyExists = errors.New("cluster_key already exists")
var ErrInvalidClusterKey = errors.New("invalid cluster_key")
var ErrInvalidTaskName = errors.New("invalid name")
var ErrInvalidRetryUploadLimit = errors.New("invalid retry upload limit")
var ErrUploadRetryNotAvailable = errors.New("upload retry is not available")
var ErrUploadRetryInProgress = errors.New("upload retry already in progress")
var ErrFilePosRequired = errors.New("file/pos is required")
var ErrGTIDSetRequired = errors.New("gtid_set is required")
var ErrInvalidStartMode = errors.New("invalid start mode")
var ErrInvalidRetentionDays = errors.New("invalid retention_days")

var clusterKeyAllowedPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

const defaultRetentionDays = 7
const maxTaskNameLength = 255
const maxSourceHostLength = 255
const maxSourceUserLength = 128
const maxFlavorLength = 32
const maxStartFileLength = 255
const minRetentionDays = 1
const maxRetentionDays = 3650

type Runner interface {
	Run(ctx context.Context, task Task) error
}

type LeaseManager interface {
	Acquire(ctx context.Context, taskID, workerID string, now time.Time, ttl time.Duration) (int64, bool, error)
	Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error)
	Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error)
}

type runnerWithNotify interface {
	// RunWithNotify 可选能力：runner 在真正 ready 时主动通知 Scheduler。
	// 这让 STARTING -> RUNNING 的切换更准确。
	RunWithNotify(ctx context.Context, task Task, onReady func()) error
}

type TaskStore interface {
	UpsertTask(ctx context.Context, task Task) error
	ListTasks(ctx context.Context) ([]Task, error)
	DeleteTask(ctx context.Context, taskID string) error
}

type taskRunReader interface {
	ListTaskRuns(ctx context.Context, taskID string, limit int) ([]TaskRun, error)
}

type workerHeartbeatReader interface {
	ListWorkerHeartbeats(ctx context.Context, limit int) ([]WorkerHeartbeat, error)
}

type CheckpointReader interface {
	LoadCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error)
}

type EventStore interface {
	AppendEvent(ctx context.Context, event TaskEvent) error
	ListEvents(ctx context.Context, taskID string, limit int) ([]TaskEvent, error)
}

type FileStore interface {
	UpsertBinlogFile(ctx context.Context, meta BinlogFile) error
	ListBinlogFiles(ctx context.Context, taskID string, limit int) ([]BinlogFile, error)
}

type failedUploadFileReader interface {
	ListFailedUploadBinlogFiles(ctx context.Context, taskID string, limit int) ([]BinlogFile, error)
}

type FileUploader interface {
	UploadFile(ctx context.Context, taskID, localPath, objectKey string) error
}

type uploadFailureCounter interface {
	CountUploadFailures(ctx context.Context) (int64, error)
}

type Option func(*Scheduler)

func WithRunner(runner Runner) Option {
	return func(s *Scheduler) {
		s.runner = runner
	}
}

func WithStore(store TaskStore) Option {
	return func(s *Scheduler) {
		s.store = store
	}
}

func WithRetryBackoff(base, max time.Duration) Option {
	return func(s *Scheduler) {
		if base > 0 {
			s.retryBaseDelay = base
		}
		if max > 0 {
			s.retryMaxDelay = max
		}
	}
}

func WithCheckpointReader(reader CheckpointReader) Option {
	return func(s *Scheduler) {
		s.checkpointReader = reader
	}
}

func WithEventStore(store EventStore) Option {
	return func(s *Scheduler) {
		s.eventStore = store
	}
}

func WithFileStore(store FileStore) Option {
	return func(s *Scheduler) {
		s.fileStore = store
	}
}

func WithFileUploader(uploader FileUploader) Option {
	return func(s *Scheduler) {
		s.fileUploader = uploader
	}
}

func WithClusterLeaseManager(manager LeaseManager) Option {
	return func(s *Scheduler) {
		s.leaseManager = manager
	}
}

func WithClusterWorkerID(workerID string) Option {
	return func(s *Scheduler) {
		s.clusterWorkerID = workerID
	}
}

func WithClusterLease(ttl, renewInterval, grace time.Duration) Option {
	return func(s *Scheduler) {
		if ttl > 0 {
			s.leaseTTL = ttl
		}
		if renewInterval > 0 {
			s.leaseRenewInterval = renewInterval
		}
		if grace > 0 {
			s.leaseGrace = grace
		}
	}
}

type Scheduler struct {
	mu      sync.Mutex
	seq     int
	tasks   map[string]Task
	events  map[string][]TaskEvent
	replica map[string]ReplicationProgress
	runner  Runner
	store   TaskStore
	cancels map[string]context.CancelFunc
	runs    map[string]chan struct{}

	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration

	leaseManager       LeaseManager
	clusterWorkerID    string
	leaseTTL           time.Duration
	leaseRenewInterval time.Duration
	leaseGrace         time.Duration

	checkpointReader CheckpointReader
	eventStore       EventStore
	fileStore        FileStore
	fileUploader     FileUploader
	retryUploads     map[string]struct{}
	eventSeq         int64
}

func NewScheduler(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:   make(map[string]Task),
		events:  make(map[string][]TaskEvent),
		replica: make(map[string]ReplicationProgress),
		cancels: make(map[string]context.CancelFunc),
		runs:    make(map[string]chan struct{}),

		retryBaseDelay:     time.Second,
		retryMaxDelay:      30 * time.Second,
		leaseTTL:           15 * time.Second,
		leaseRenewInterval: 5 * time.Second,
		leaseGrace:         30 * time.Second,
		retryUploads:       make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Scheduler) SetRunner(runner Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = runner
}

func (s *Scheduler) CreateTask(name, clusterKey string) (Task, error) {
	validatedName, err := normalizeAndValidateTaskName(name)
	if err != nil {
		return Task{}, err
	}
	validatedClusterKey, err := normalizeAndValidateClusterKey(clusterKey)
	if err != nil {
		return Task{}, err
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
		Start: StartConfig{
			Mode: StartModeLatest,
		},
		Storage: Storage{
			RetentionDays: defaultRetentionDays,
		},
		UpdatedAt: now,
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

func (s *Scheduler) ConfigureSource(id string, source SourceConfig) error {
	normalized, err := normalizeAndValidateSourceConfig(source)
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

func (s *Scheduler) UpdateTask(id string, patch TaskPatch) (Task, error) {
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
		source, err := normalizeAndValidateSourceConfig(*patch.Source)
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

func (s *Scheduler) StartTask(id string) error {
	s.mu.Lock()

	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	canClaimDispatched := task.State == StateStarting &&
		task.OwnerWorkerID == "" &&
		task.Epoch == 0 &&
		task.RunID == "" &&
		s.runner != nil &&
		s.leaseManager != nil
	if task.State != StateCreated && task.State != StateStopped && task.State != StateRetryBackoff && !canClaimDispatched {
		s.mu.Unlock()
		return fmt.Errorf("cannot start from state %s", task.State)
	}
	// Scheduler 在 start 前强制校验最小 source config。
	if task.Source.Host == "" || task.Source.Port == 0 || task.Source.User == "" {
		s.mu.Unlock()
		return ErrInvalidSourceConfig
	}
	// cluster control-plane 允许 dispatch-only start：仅写入 STARTING，由 worker 接管执行。
	if s.runner == nil {
		if s.leaseManager == nil {
			// 非 cluster dispatch 场景仍要求本地 runner，防止误标记状态。
			s.mu.Unlock()
			return ErrRunnerNotConfigured
		}
		task.State = StateStarting
		task.LastError = ""
		task.OwnerWorkerID = ""
		task.Epoch = 0
		task.RunID = ""
		task.UpdatedAt = time.Now()
		s.tasks[id] = task
		s.appendEventLocked(id, "TASK_START_DISPATCHED", "task start dispatched to worker", "")
		if err := s.persistTaskLocked(task); err != nil {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()
		return nil
	}
	if s.leaseManager != nil && s.clusterWorkerID == "" {
		s.mu.Unlock()
		return ErrClusterWorkerIDRequired
	}

	if s.leaseManager != nil {
		epoch, acquired, err := s.leaseManager.Acquire(context.Background(), id, s.clusterWorkerID, time.Now(), s.leaseTTL)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if !acquired {
			s.mu.Unlock()
			return ErrLeaseNotAcquired
		}
		task.OwnerWorkerID = s.clusterWorkerID
		task.Epoch = epoch
		task.RunID = fmt.Sprintf("%s-%d", id, time.Now().UnixNano())
	}

	task.State = StateStarting
	task.UpdatedAt = time.Now()
	// 注意：这里仅表示“已发起启动流程”，不是“runner 已 ready”。
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STARTED", "task started", "")
	if err := s.persistTaskLocked(task); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	if oldCancel, ok := s.cancels[id]; ok {
		// 防御性处理：task 快速重启时替换旧的 cancel function。
		oldCancel()
	}
	s.cancels[id] = cancel
	s.runs[id] = done
	taskForRun := s.tasks[id]
	s.mu.Unlock()

	if s.leaseManager != nil && taskForRun.Epoch > 0 {
		go s.renewLeaseLoop(ctx, id, taskForRun.OwnerWorkerID, taskForRun.Epoch)
	}

	go s.runTask(ctx, id, taskForRun, done)
	return nil
}

// ClaimStartingTasks 让在线 worker 在常驻状态下接管 control-plane dispatch 的 STARTING 任务。
func (s *Scheduler) ClaimStartingTasks() (int, error) {
	s.mu.Lock()
	store := s.store
	runner := s.runner
	leaseManager := s.leaseManager
	s.mu.Unlock()

	if store == nil || runner == nil || leaseManager == nil {
		return 0, nil
	}

	list, err := store.ListTasks(context.Background())
	if err != nil {
		return 0, err
	}

	claimed := 0
	for _, item := range list {
		if item.State != StateStarting {
			continue
		}
		if !s.prepareStartingTaskClaim(item) {
			continue
		}
		if err := s.StartTask(item.ID); err != nil {
			// 竞争窗口下可能被其他 worker 先拿到 lease，或状态已变；这里按 best-effort 跳过。
			continue
		}
		claimed++
	}
	return claimed, nil
}

func (s *Scheduler) prepareStartingTaskClaim(item Task) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if done, ok := s.runs[item.ID]; ok && !isClosed(done) {
		return false
	}

	if current, ok := s.tasks[item.ID]; ok {
		if current.State == StateRunning || current.State == StateRetryBackoff || current.State == StateLeaseDegraded || current.State == StateStopping {
			return false
		}
	}

	s.tasks[item.ID] = item
	return true
}

func (s *Scheduler) MarkRetryableError(id, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	if task.State != StateRunning && task.State != StateStarting {
		return fmt.Errorf("cannot mark retryable error from state %s", task.State)
	}

	task.State = StateRetryBackoff
	task.LastError = msg
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_RETRY_BACKOFF", "task entered retry backoff", msg)
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) StopTask(id string) error {
	s.mu.Lock()

	task, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return ErrTaskNotFound
	}
	if task.State != StateRunning && task.State != StateRetryBackoff && task.State != StateStarting && task.State != StateLeaseDegraded {
		s.mu.Unlock()
		return fmt.Errorf("cannot stop from state %s", task.State)
	}

	done, hasRun := s.runs[id]
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}

	task.State = StateStopping
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPING", "task stopping", "")
	if err := s.persistTaskLocked(task); err != nil {
		s.mu.Unlock()
		return err
	}

	// 没有运行中的 goroutine（或已经退出）时，直接收敛到 STOPPED。
	if !hasRun || isClosed(done) {
		if err := s.markStoppedLocked(id); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) GetTask(id string) (Task, error) {
	s.mu.Lock()
	task, ok := s.tasks[id]
	store := s.store
	s.mu.Unlock()

	if store != nil {
		list, err := store.ListTasks(context.Background())
		if err == nil {
			for _, item := range list {
				if item.ID != id {
					continue
				}
				s.mu.Lock()
				s.tasks[id] = item
				s.mu.Unlock()
				return item, nil
			}
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
		if err := s.store.DeleteTask(context.Background(), id); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()

	if s.leaseManager != nil && task.OwnerWorkerID != "" && task.Epoch > 0 {
		_, _ = s.leaseManager.Release(context.Background(), id, task.OwnerWorkerID, task.Epoch)
	}
	return nil
}

func (s *Scheduler) ListTasks() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out
}

func (s *Scheduler) ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
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

func (s *Scheduler) GetReplicationProgress(taskID string) (ReplicationProgress, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
		return ReplicationProgress{}, false, ErrTaskNotFound
	}
	progress, ok := s.replica[taskID]
	return progress, ok, nil
}

func (s *Scheduler) runTask(ctx context.Context, id string, task Task, done chan struct{}) {
	defer func() {
		var (
			releaseOwner string
			releaseEpoch int64
		)
		s.mu.Lock()
		if currentDone, ok := s.runs[id]; ok && currentDone == done {
			delete(s.runs, id)
		}
		// 收到 stop 请求后，直到执行 goroutine 真退出才收敛到 STOPPED。
		if currentTask, ok := s.tasks[id]; ok && currentTask.State == StateStopping {
			_ = s.markStoppedLocked(id)
			if s.leaseManager != nil && currentTask.OwnerWorkerID != "" && currentTask.Epoch > 0 {
				releaseOwner = currentTask.OwnerWorkerID
				releaseEpoch = currentTask.Epoch
			}
		}
		s.mu.Unlock()
		if s.leaseManager != nil && releaseOwner != "" && releaseEpoch > 0 {
			_, _ = s.leaseManager.Release(context.Background(), id, releaseOwner, releaseEpoch)
		}
		close(done)
	}()

	attempt := 0
	for {
		// runRunner 是一次“会话级”执行：内部会一直拉 binlog，直到 stop 或报错才返回。
		err := s.runRunner(ctx, id, task)
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}

		s.mu.Lock()
		current, ok := s.tasks[id]
		if !ok {
			s.mu.Unlock()
			return
		}
		if current.State == StateStopped || current.State == StateStopping {
			s.mu.Unlock()
			return
		}

		current.State = StateRetryBackoff
		current.LastError = err.Error()
		current.UpdatedAt = time.Now()
		s.tasks[id] = current
		s.appendEventLocked(id, "TASK_RUNNER_ERROR", "runner error", err.Error())
		_ = s.persistTaskLocked(current)
		s.mu.Unlock()

		attempt++
		// 使用 exponential backoff，保护 source DB 并避免热重试。
		delay := s.retryDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		s.mu.Lock()
		current, ok = s.tasks[id]
		if !ok {
			s.mu.Unlock()
			return
		}
		if current.State == StateStopped || current.State == StateStopping {
			s.mu.Unlock()
			return
		}
		current.State = StateStarting
		current.UpdatedAt = time.Now()
		s.tasks[id] = current
		// 重试前先回到 STARTING，等 runner onReady 后再切 RUNNING。
		s.appendEventLocked(id, "TASK_RETRYING", "retrying runner", "")
		_ = s.persistTaskLocked(current)
		task = current
		s.mu.Unlock()
	}
}

func (s *Scheduler) runRunner(ctx context.Context, id string, task Task) error {
	onReady := func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		current, ok := s.tasks[id]
		if !ok {
			return
		}
		if current.State == StateStopped || current.State == StateStopping {
			return
		}
		if current.State == StateRunning {
			return
		}

		current.State = StateRunning
		current.LastError = ""
		current.UpdatedAt = time.Now()
		s.tasks[id] = current
		s.appendEventLocked(id, "TASK_RUNNING", "runner is running", "")
		_ = s.persistTaskLocked(current)
	}

	if n, ok := s.runner.(runnerWithNotify); ok {
		// 类型断言：如果 runner 支持 RunWithNotify，就走精确 ready 语义。
		return n.RunWithNotify(ctx, task, onReady)
	}
	// 向后兼容旧 runner：没有 notify 能力时，在 Run 前乐观置为 RUNNING。
	onReady()
	return s.runner.Run(ctx, task)
}

func (s *Scheduler) renewLeaseLoop(ctx context.Context, id, workerID string, epoch int64) {
	ticker := time.NewTicker(s.leaseRenewInterval)
	defer ticker.Stop()

	var degradedSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		ok, err := s.leaseManager.Renew(context.Background(), id, workerID, epoch, time.Now(), s.leaseTTL)
		if err == nil && ok {
			if !degradedSince.IsZero() {
				degradedSince = time.Time{}
				s.mu.Lock()
				task, exists := s.tasks[id]
				if exists && task.State == StateLeaseDegraded {
					task.State = StateRunning
					task.LastError = ""
					task.UpdatedAt = time.Now()
					s.tasks[id] = task
					s.appendEventLocked(id, "TASK_LEASE_RENEWED", "lease renewed", "")
					_ = s.persistTaskLocked(task)
				}
				s.mu.Unlock()
			}
			continue
		}

		if err == nil && !ok {
			s.mu.Lock()
			s.failSafeStopLocked(id, "TASK_LEASE_LOST", "lease lost")
			s.mu.Unlock()
			return
		}

		now := time.Now()
		s.mu.Lock()
		task, exists := s.tasks[id]
		if exists && task.State != StateStopping && task.State != StateStopped {
			if task.State != StateLeaseDegraded {
				task.State = StateLeaseDegraded
				s.appendEventLocked(id, "TASK_LEASE_DEGRADED", "lease renew failed", err.Error())
			}
			task.LastError = err.Error()
			task.UpdatedAt = now
			s.tasks[id] = task
			_ = s.persistTaskLocked(task)
		}
		s.mu.Unlock()

		if degradedSince.IsZero() {
			degradedSince = now
		}
		if now.Sub(degradedSince) >= s.leaseGrace {
			s.mu.Lock()
			s.failSafeStopLocked(id, "TASK_LEASE_GRACE_EXCEEDED", "lease renew grace exceeded")
			s.mu.Unlock()
			return
		}
	}
}

func (s *Scheduler) failSafeStopLocked(id, eventType, message string) {
	task, ok := s.tasks[id]
	if !ok {
		return
	}
	if task.State == StateStopping || task.State == StateStopped {
		return
	}
	task.State = StateStopping
	task.LastError = message
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, eventType, message, "")
	_ = s.persistTaskLocked(task)
	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}
}

func (s *Scheduler) markStoppedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	if task.State == StateStopped {
		return nil
	}
	task.State = StateStopped
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPED", "task stopped", "")
	return s.persistTaskLocked(task)
}

func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

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

func (s *Scheduler) persistTaskLocked(task Task) error {
	if s.store == nil {
		return nil
	}
	return s.store.UpsertTask(context.Background(), task)
}

func (s *Scheduler) retryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return s.retryBaseDelay
	}

	delay := float64(s.retryBaseDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(s.retryMaxDelay) {
		return s.retryMaxDelay
	}
	return time.Duration(delay)
}

func (s *Scheduler) GetCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	s.mu.Lock()
	_, ok := s.tasks[taskID]
	store := s.store
	s.mu.Unlock()

	if !ok && store != nil {
		list, err := store.ListTasks(context.Background())
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
	if s.checkpointReader == nil {
		return binlog.Checkpoint{}, false, nil
	}
	return s.checkpointReader.LoadCheckpoint(ctx, taskID)
}

func (s *Scheduler) ListEvents(taskID string, limit int) ([]TaskEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
		return nil, ErrTaskNotFound
	}
	if s.eventStore != nil {
		// 优先读持久化事件，避免重启后只看到内存中的事件片段。
		return s.eventStore.ListEvents(context.Background(), taskID, limit)
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

func (s *Scheduler) ListFiles(taskID string, limit int) ([]BinlogFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[taskID]; !ok {
		return nil, ErrTaskNotFound
	}
	if s.fileStore == nil {
		return []BinlogFile{}, nil
	}
	return s.fileStore.ListBinlogFiles(context.Background(), taskID, limit)
}

func (s *Scheduler) RetryFailedUploads(taskID string, limit int) (UploadRetryStats, error) {
	const (
		defaultLimit = 100
		maxLimit     = 1000
	)
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		return UploadRetryStats{}, ErrInvalidRetryUploadLimit
	}
	if err := s.syncTasksFromStore(); err != nil {
		return UploadRetryStats{}, err
	}

	s.mu.Lock()
	if _, ok := s.tasks[taskID]; !ok {
		s.mu.Unlock()
		return UploadRetryStats{}, ErrTaskNotFound
	}
	if _, running := s.retryUploads[taskID]; running {
		s.mu.Unlock()
		return UploadRetryStats{}, ErrUploadRetryInProgress
	}
	fileStore := s.fileStore
	uploader := s.fileUploader
	s.retryUploads[taskID] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.retryUploads, taskID)
		s.mu.Unlock()
	}()

	if fileStore == nil || uploader == nil {
		return UploadRetryStats{}, ErrUploadRetryNotAvailable
	}

	files, err := s.listRetryUploadCandidates(taskID, limit, fileStore)
	if err != nil {
		return UploadRetryStats{}, err
	}

	var stats UploadRetryStats
	for _, file := range files {
		stats.Scanned++
		if !strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
			stats.Skipped++
			continue
		}
		if !isSealedFileForRetry(file) {
			stats.Skipped++
			continue
		}
		if strings.TrimSpace(file.FilePath) == "" {
			stats.Failed++
			_ = s.markRetryUploadFailure(fileStore, file, "retry upload skipped: empty file_path")
			continue
		}
		if strings.TrimSpace(file.ObjectKey) == "" {
			stats.Failed++
			_ = s.markRetryUploadFailure(fileStore, file, "retry upload skipped: empty object_key")
			continue
		}

		if err := uploader.UploadFile(context.Background(), taskID, file.FilePath, file.ObjectKey); err != nil {
			stats.Failed++
			_ = s.markRetryUploadFailure(fileStore, file, err.Error())
			continue
		}

		file.UploadState = "UPLOADED"
		file.UploadError = ""
		file.UploadedAt = time.Now()
		if err := fileStore.UpsertBinlogFile(context.Background(), file); err != nil {
			stats.Failed++
			continue
		}
		stats.Succeeded++
	}

	return stats, nil
}

func (s *Scheduler) listRetryUploadCandidates(taskID string, limit int, fileStore FileStore) ([]BinlogFile, error) {
	if reader, ok := fileStore.(failedUploadFileReader); ok {
		return reader.ListFailedUploadBinlogFiles(context.Background(), taskID, limit)
	}
	return fileStore.ListBinlogFiles(context.Background(), taskID, limit)
}

func (s *Scheduler) markRetryUploadFailure(fileStore FileStore, file BinlogFile, reason string) error {
	file.UploadState = "UPLOAD_FAILED"
	file.UploadError = reason
	return fileStore.UpsertBinlogFile(context.Background(), file)
}

func isSealedFileForRetry(file BinlogFile) bool {
	name := strings.ToLower(strings.TrimSpace(file.FileName))
	path := strings.ToLower(strings.TrimSpace(file.FilePath))
	if strings.Contains(name, ".open.e") || strings.Contains(path, ".open.e") {
		return false
	}
	return !file.SealedAt.IsZero()
}

func (s *Scheduler) CountUploadFailures() (int64, error) {
	s.mu.Lock()
	fileStore := s.fileStore
	taskIDs := make([]string, 0, len(s.tasks))
	for taskID := range s.tasks {
		taskIDs = append(taskIDs, taskID)
	}
	s.mu.Unlock()

	if fileStore == nil {
		return 0, nil
	}
	if counter, ok := fileStore.(uploadFailureCounter); ok {
		return counter.CountUploadFailures(context.Background())
	}

	var total int64
	const allFilesLimit = int(^uint(0) >> 1)
	for _, taskID := range taskIDs {
		files, err := fileStore.ListBinlogFiles(context.Background(), taskID, allFilesLimit)
		if err != nil {
			continue
		}
		for _, file := range files {
			if strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
				total++
			}
		}
	}
	return total, nil
}

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
		return reader.ListTaskRuns(context.Background(), taskID, limit)
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
		return reader.ListWorkerHeartbeats(context.Background(), limit)
	}
	return []WorkerHeartbeat{}, nil
}

func (s *Scheduler) appendEventLocked(taskID, eventType, message, detail string) {
	// 函数名里的 Locked 表示：调用方必须已经持有 s.mu。
	// 这里会修改 eventSeq 和 events map，需要同一把锁保护。
	s.eventSeq++
	event := TaskEvent{
		TaskID:   taskID,
		Type:     eventType,
		Message:  message,
		Detail:   detail,
		Time:     time.Now(),
		Sequence: s.eventSeq,
	}
	s.events[taskID] = append(s.events[taskID], event)
	if s.eventStore != nil {
		// 事件落库失败不阻断主流程，保证调度与拉流优先可用。
		_ = s.eventStore.AppendEvent(context.Background(), event)
	}
}

func (s *Scheduler) isClusterKeyUniqueLocked(clusterKey, excludeTaskID string) bool {
	for _, task := range s.tasks {
		if task.ID == excludeTaskID {
			continue
		}
		if task.ClusterKey == clusterKey {
			return false
		}
	}
	return true
}

func (s *Scheduler) syncTasksFromStore() error {
	s.mu.Lock()
	store := s.store
	s.mu.Unlock()
	if store == nil {
		return nil
	}

	list, err := store.ListTasks(context.Background())
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range list {
		s.tasks[task.ID] = task
		if n, convErr := strconv.Atoi(task.ID); convErr == nil && n > s.seq {
			s.seq = n
		}
	}
	return nil
}

func normalizeAndValidateClusterKey(clusterKey string) (string, error) {
	key := strings.TrimSpace(clusterKey)
	if key == "" {
		return "", ErrClusterKeyRequired
	}
	if strings.Contains(key, "/") || strings.Contains(key, `\`) || strings.Contains(key, "..") {
		return "", ErrInvalidClusterKey
	}
	if !clusterKeyAllowedPattern.MatchString(key) {
		return "", ErrInvalidClusterKey
	}
	return key, nil
}

func normalizeAndValidateTaskName(name string) (string, error) {
	normalized := strings.TrimSpace(name)
	if normalized == "" {
		return "", ErrInvalidTaskName
	}
	if len(normalized) > maxTaskNameLength {
		return "", ErrInvalidTaskName
	}
	return normalized, nil
}

func normalizeAndValidateSourceConfig(source SourceConfig) (SourceConfig, error) {
	normalized := source
	normalized.Host = strings.TrimSpace(source.Host)
	normalized.User = strings.TrimSpace(source.User)
	normalized.Flavor = strings.TrimSpace(source.Flavor)
	if normalized.Flavor == "" {
		normalized.Flavor = "mysql"
	}

	if normalized.Host == "" || normalized.Port == 0 || normalized.User == "" {
		return SourceConfig{}, ErrInvalidSourceConfig
	}
	if len(normalized.Host) > maxSourceHostLength || hasWhitespace(normalized.Host) {
		return SourceConfig{}, ErrInvalidSourceConfig
	}
	if len(normalized.User) > maxSourceUserLength || hasWhitespace(normalized.User) {
		return SourceConfig{}, ErrInvalidSourceConfig
	}
	if len(normalized.Flavor) > maxFlavorLength || !clusterKeyAllowedPattern.MatchString(normalized.Flavor) {
		return SourceConfig{}, ErrInvalidSourceConfig
	}
	return normalized, nil
}

func normalizeAndValidateStartConfig(start StartConfig) (StartConfig, error) {
	normalized := start
	if normalized.Mode == "" {
		normalized.Mode = StartModeLatest
	}
	switch normalized.Mode {
	case StartModeLatest:
		normalized.File = ""
		normalized.Pos = 0
		normalized.GTIDSet = ""
		return normalized, nil
	case StartModeFilePos:
		normalized.File = strings.TrimSpace(normalized.File)
		if normalized.File == "" || normalized.Pos == 0 || len(normalized.File) > maxStartFileLength {
			return StartConfig{}, ErrFilePosRequired
		}
		normalized.GTIDSet = ""
		return normalized, nil
	case StartModeGTID:
		normalized.GTIDSet = strings.TrimSpace(normalized.GTIDSet)
		if normalized.GTIDSet == "" {
			return StartConfig{}, ErrGTIDSetRequired
		}
		normalized.File = ""
		normalized.Pos = 0
		return normalized, nil
	default:
		return StartConfig{}, ErrInvalidStartMode
	}
}

func normalizeAndValidateStorage(storage Storage) (Storage, error) {
	normalized := storage
	if normalized.RetentionDays < minRetentionDays || normalized.RetentionDays > maxRetentionDays {
		return Storage{}, ErrInvalidRetentionDays
	}
	return normalized, nil
}

func hasWhitespace(value string) bool {
	return strings.ContainsAny(value, " \t\r\n")
}
