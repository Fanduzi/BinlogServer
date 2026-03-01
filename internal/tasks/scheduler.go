// Package tasks provides module-level functionality for tasks.
// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, scheduling decisions, and execution coordination
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"runtime/debug"
	"sort"
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

// Runner 定义任务执行器接口。
type Runner interface {
	// Run 启动任务执行主循环；返回时表示本次运行结束。
	Run(ctx context.Context, task Task) error
}

// LeaseManager 定义 cluster lease 的获取/续租/释放接口。
type LeaseManager interface {
	// Acquire 尝试获取任务 lease，成功时返回当前 epoch。
	Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error)
	// Renew 续租当前 lease，返回是否续租成功。
	Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error)
	// Release 主动释放 lease（best-effort）。
	Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error)
}

type runnerWithNotify interface {
	// RunWithNotify 可选能力：runner 在真正 ready 时主动通知 Scheduler。
	// 这让 STARTING -> RUNNING 的切换更准确。
	RunWithNotify(ctx context.Context, task Task, onReady func()) error
}

// TaskStore 定义任务元数据持久化接口。
type TaskStore interface {
	// UpsertTask 持久化任务配置与状态。
	UpsertTask(ctx context.Context, task Task) error
	// ListTasks 列出全部任务。
	ListTasks(ctx context.Context) ([]Task, error)
	// DeleteTask 删除指定任务。
	DeleteTask(ctx context.Context, taskID string) error
}

type taskRunReader interface {
	ListTaskRuns(ctx context.Context, taskID string, limit int) ([]TaskRun, error)
}

type workerHeartbeatReader interface {
	ListWorkerHeartbeats(ctx context.Context, limit int) ([]WorkerHeartbeat, error)
}

// CheckpointReader 定义 checkpoint 读取接口。
type CheckpointReader interface {
	// LoadCheckpoint 读取任务 checkpoint。
	LoadCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error)
}

// EventStore 定义任务事件存储接口。
type EventStore interface {
	// AppendEvent 追加任务事件。
	AppendEvent(ctx context.Context, event TaskEvent) error
	// ListEvents 按倒序读取任务事件。
	ListEvents(ctx context.Context, taskID string, limit int) ([]TaskEvent, error)
}

// FileStore 定义 binlog 文件元数据存储接口。
type FileStore interface {
	// UpsertBinlogFile 写入/更新文件元数据。
	UpsertBinlogFile(ctx context.Context, meta BinlogFile) error
	// ListBinlogFiles 按倒序读取文件元数据。
	ListBinlogFiles(ctx context.Context, taskID string, limit int) ([]BinlogFile, error)
}

type failedUploadFileReader interface {
	ListFailedUploadBinlogFiles(ctx context.Context, taskID string, limit int) ([]BinlogFile, error)
}

// FileUploader 定义对象存储上传接口。
type FileUploader interface {
	// UploadFile 上传本地 sealed 文件到对象存储。
	UploadFile(ctx context.Context, taskID, localPath, objectKey string) error
}

type uploadFailureCounter interface {
	CountUploadFailures(ctx context.Context) (int64, error)
}

type uploadFailureReasonReader interface {
	ListUploadFailureReasons(ctx context.Context, taskID string, limit int) ([]UploadFailureReason, error)
}

// Option 用于配置 Scheduler 的可选注入项。
type Option func(*Scheduler)

// WithRunner 注入任务执行器。
func WithRunner(runner Runner) Option {
	return func(s *Scheduler) {
		s.runner = runner
	}
}

// WithStore 注入任务持久化存储。
func WithStore(store TaskStore) Option {
	return func(s *Scheduler) {
		s.store = store
	}
}

// WithRetryBackoff 配置 runner 错误后的退避参数。
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

// WithCheckpointReader 注入 checkpoint 读取器。
func WithCheckpointReader(reader CheckpointReader) Option {
	return func(s *Scheduler) {
		s.checkpointReader = reader
	}
}

// WithEventStore 注入事件存储。
func WithEventStore(store EventStore) Option {
	return func(s *Scheduler) {
		s.eventStore = store
	}
}

// WithFileStore 注入文件元数据存储。
func WithFileStore(store FileStore) Option {
	return func(s *Scheduler) {
		s.fileStore = store
	}
}

// WithFileUploader 注入对象存储上传器。
func WithFileUploader(uploader FileUploader) Option {
	return func(s *Scheduler) {
		s.fileUploader = uploader
	}
}

// WithClusterLeaseManager 启用 cluster lease 管理。
func WithClusterLeaseManager(manager LeaseManager) Option {
	return func(s *Scheduler) {
		s.leaseManager = manager
	}
}

// WithClusterWorkerID 设置当前实例在 cluster 中的 worker_id。
func WithClusterWorkerID(workerID string) Option {
	return func(s *Scheduler) {
		s.clusterWorkerID = workerID
	}
}

// WithClusterLease 配置 lease 的 TTL、续租周期和降级宽限时间。
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

// Scheduler 负责任务配置管理、状态机推进和运行编排。
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

	// cluster 相关字段：当 leaseManager != nil 时，Scheduler 进入分布式单执行语义。
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
	retrySuccess     int64
	retryFailed      int64
	retrySkipped     int64
	retryLastTS      int64
	eventSeq         int64
}

// NewScheduler 创建调度器并应用可选项。
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

// SetRunner 动态替换 runner 实现（测试和运行时装配会用到）。
func (s *Scheduler) SetRunner(runner Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = runner
}

// CreateTask 创建任务并写入默认配置（start=LATEST, retention=7）。
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

// ConfigureSource 更新任务源库配置。
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
func (s *Scheduler) StartTask(id string) error {
	s.mu.Lock()

	// Step 1: 校验任务存在、状态可启动、source 最小配置可用。
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
	// 仅允许 claim “干净的 dispatch STARTING 任务”，避免误接管非预期中间态。
	if task.State != StateCreated && task.State != StateStopped && task.State != StateRetryBackoff && !canClaimDispatched {
		s.mu.Unlock()
		return fmt.Errorf("cannot start from state %s", task.State)
	}
	// Scheduler 在 start 前强制校验最小 source config。
	if task.Source.Host == "" || task.Source.Port == 0 || task.Source.User == "" {
		s.mu.Unlock()
		return ErrInvalidSourceConfig
	}
	// Step 2: control-plane dispatch-only 分支（本地无 runner）。
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

	// Step 3: worker 执行分支，先 acquire lease，再进入 STARTING。
	if s.leaseManager != nil {
		epoch, acquired, err := s.leaseManager.Acquire(context.Background(), id, s.clusterWorkerID, s.leaseTTL)
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

	// Step 4: 启动 run/renew goroutine，进入真实执行期。
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
	// 常见误解：
	// 这里不是“抢占 RUNNING 任务”，而是只处理 dispatch 出去且仍为 STARTING 的任务。
	// 真正是否能执行仍要依赖 StartTask 内部 lease Acquire 结果。
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

	// 基于持久化快照做 best-effort 认领；
	// 并发竞争由 StartTask 内的状态校验 + lease Acquire 结果保证安全。
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

// prepareStartingTaskClaim 把 store 里的 STARTING 任务注入内存，并过滤本机不可接管场景。
func (s *Scheduler) prepareStartingTaskClaim(item Task) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if done, ok := s.runs[item.ID]; ok && !isClosed(done) {
		// 本机已有活跃 run goroutine，拒绝重复接管。
		return false
	}

	if current, ok := s.tasks[item.ID]; ok {
		if current.State == StateRunning || current.State == StateRetryBackoff || current.State == StateLeaseDegraded || current.State == StateStopping {
			// 本地状态已进入执行/停止路径，不用 store 快照覆盖。
			return false
		}
	}

	s.tasks[item.ID] = item
	return true
}

// MarkRetryableError 将任务标记为 RETRY_BACKOFF 并记录错误信息。
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

// StopTask 请求停止任务（两阶段：STOPPING -> STOPPED）。
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
	// 常见误解：
	// “调用 StopTask 后应立刻看到 STOPPED”并不成立。这里先写 STOPPING，
	// 只有 run goroutine 真正退出后才会转为 STOPPED，确保状态语义等于“执行已结束”。
	// 两阶段停止：先对外可见 STOPPING，再等待 run goroutine defer 收敛到 STOPPED。
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

// GetTask 读取任务详情；必要时会从 store 刷新。
func (s *Scheduler) GetTask(id string) (Task, error) {
	// 常见误解：
	// GetTask 返回的不一定是“纯内存快照”，当配置了 store 时会尝试拉取持久化最新值，
	// 目的是让 API 视图更接近真实元数据状态。
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
		if err := s.store.DeleteTask(context.Background(), id); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()

	if s.leaseManager != nil && task.OwnerWorkerID != "" && task.Epoch > 0 {
		if released, err := s.leaseManager.Release(context.Background(), id, task.OwnerWorkerID, task.Epoch); err != nil || !released {
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

// ReportReplicationProgress 上报最新复制进度，供延迟计算和展示。
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
		// 这样 API 层的 STOPPED 表示“执行路径已结束”，而不是“仅发出停止请求”。
		if currentTask, ok := s.tasks[id]; ok && currentTask.State == StateStopping {
			_ = s.markStoppedLocked(id)
			if s.leaseManager != nil && currentTask.OwnerWorkerID != "" && currentTask.Epoch > 0 {
				releaseOwner = currentTask.OwnerWorkerID
				releaseEpoch = currentTask.Epoch
			}
		}
		s.mu.Unlock()
		if s.leaseManager != nil && releaseOwner != "" && releaseEpoch > 0 {
			if released, err := s.leaseManager.Release(context.Background(), id, releaseOwner, releaseEpoch); err != nil || !released {
				log.Printf("lease release on run exit failed task=%s owner=%s epoch=%d released=%v err=%v", id, releaseOwner, releaseEpoch, released, err)
			}
		}
		// 常见误解：
		// done 不是“任务开始执行”的信号，而是“本轮执行完全结束”的信号。
		// StopTask/状态收敛逻辑依赖这个 close 时机判断是否可标记 STOPPED。
		close(done)
	}()

	// Step 1: 调用 runRunner 执行一次会话；错误则进入退避重试。
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

		// Step 2: 指数退避等待，避免瞬时故障导致热重试风暴。
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
		// Step 3: 重试前先回到 STARTING，等待下一轮 runner ready 回调。
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

// runRunner 负责把 Scheduler 状态机与 Runner 生命周期对齐。
func (s *Scheduler) runRunner(ctx context.Context, id string, task Task) error {
	// Step 1: 定义 ready 回调，把任务状态收敛到 RUNNING。
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
	// Step 2: 兼容旧 runner（无 notify），采用乐观 ready 语义。
	// 向后兼容旧 runner：没有 notify 能力时，在 Run 前乐观置为 RUNNING。
	// 该路径可能出现“短暂 RUNNING 后立即失败”；失败会在 runTask 的错误分支回收状态。
	onReady()
	return s.runner.Run(ctx, task)
}

// renewLeaseLoop 在 cluster 模式下周期续租，并处理降级/失租停机。
func (s *Scheduler) renewLeaseLoop(ctx context.Context, id, workerID string, epoch int64) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("lease renew loop panic task=%s worker=%s epoch=%d panic=%v stack=%s", id, workerID, epoch, r, debug.Stack())
			s.mu.Lock()
			s.failSafeStopLocked(id, "TASK_LEASE_RENEW_PANIC", fmt.Sprintf("lease renew loop panic: %v", r))
			s.mu.Unlock()
		}
	}()

	ticker := time.NewTicker(s.leaseRenewInterval)
	defer ticker.Stop()

	// Step 1: 正常续租时保持/恢复 RUNNING。
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
				// 从降级状态恢复后，清空降级计时并收敛回 RUNNING。
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
			// lease 被抢占/丢失：立刻 fail-safe stop，避免双写同一任务。
			s.mu.Lock()
			s.failSafeStopLocked(id, "TASK_LEASE_LOST", "lease lost")
			s.mu.Unlock()
			return
		}

		// Step 2: 续租报错进入 LEASE_DEGRADED，并开始 grace 计时。
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
		// Step 3: 超过 grace 仍不可续租，触发 fail-safe 停止。
		if now.Sub(degradedSince) >= s.leaseGrace {
			// 超过 grace 仍无法续租，必须停止，优先保证文件语义与单执行安全。
			s.mu.Lock()
			s.failSafeStopLocked(id, "TASK_LEASE_GRACE_EXCEEDED", "lease renew grace exceeded")
			s.mu.Unlock()
			return
		}
	}
}

// failSafeStopLocked 在持锁上下文内触发强制停止（用于 lease 异常场景）。
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
		// 这里只发取消信号；最终 STOPPED 由 runTask defer 统一收敛。
		cancel()
		delete(s.cancels, id)
	}
}

// markStoppedLocked 将任务收敛到最终 STOPPED 并清理运行时 ownership 字段。
func (s *Scheduler) markStoppedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	if task.State == StateStopped {
		return nil
	}
	task.State = StateStopped
	// STOPPED 是“无执行归属”的稳定终态，清空运行时 ownership 字段。
	task.OwnerWorkerID = ""
	task.Epoch = 0
	task.RunID = ""
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPED", "task stopped", "")
	return s.persistTaskLocked(task)
}

// isClosed 判断 channel 是否已关闭（nil 视为已关闭）。
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

// Restore 从持久化层恢复任务到内存视图。
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
func (s *Scheduler) persistTaskLocked(task Task) error {
	if s.store == nil {
		return nil
	}
	// 避免持锁执行潜在慢 I/O（DB），降低调度锁的阻塞影响。
	s.mu.Unlock()
	err := s.store.UpsertTask(context.Background(), task)
	s.mu.Lock()
	return err
}

// retryDelay 计算指数退避时长（有上限）。
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

// GetCheckpoint 读取任务 checkpoint（优先 reader，不可用时返回未命中）。
func (s *Scheduler) GetCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	s.mu.Lock()
	_, ok := s.tasks[taskID]
	store := s.store
	s.mu.Unlock()

	// Step 1: 内存未命中时，从 store 同步一次任务视图。
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
	return s.fileStore.ListBinlogFiles(context.Background(), taskID, limit)
}

// RetryFailedUploads 手动重试失败上传（仅 sealed 且状态为 UPLOAD_FAILED）。
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

	// Step 1: 参数归一化 + 任务存在性 + 并发互斥校验。
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

	// Step 2: 拉取候选并逐个重试，单文件失败不影响其他文件。
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

	// Step 3: 汇总到全局 retry metrics。
	s.recordUploadRetryMetrics(stats)

	return stats, nil
}

// listRetryUploadCandidates 优先使用“失败文件专用查询”，否则退化到全量查询。
func (s *Scheduler) listRetryUploadCandidates(taskID string, limit int, fileStore FileStore) ([]BinlogFile, error) {
	if reader, ok := fileStore.(failedUploadFileReader); ok {
		return reader.ListFailedUploadBinlogFiles(context.Background(), taskID, limit)
	}
	return fileStore.ListBinlogFiles(context.Background(), taskID, limit)
}

// markRetryUploadFailure 记录单文件补传失败状态和错误原因。
func (s *Scheduler) markRetryUploadFailure(fileStore FileStore, file BinlogFile, reason string) error {
	file.UploadState = "UPLOAD_FAILED"
	file.UploadError = reason
	return fileStore.UpsertBinlogFile(context.Background(), file)
}

// isSealedFileForRetry 判定文件是否满足补传前提（已 seal 且非 open 文件）。
func isSealedFileForRetry(file BinlogFile) bool {
	name := strings.ToLower(strings.TrimSpace(file.FileName))
	path := strings.ToLower(strings.TrimSpace(file.FilePath))
	if strings.Contains(name, ".open.e") || strings.Contains(path, ".open.e") {
		return false
	}
	return !file.SealedAt.IsZero()
}

// CountUploadFailures 统计全局上传失败记录数（metrics 使用）。
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

// GetUploadRetryMetrics 返回 retry-upload 的累计观测指标。
func (s *Scheduler) GetUploadRetryMetrics() UploadRetryMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return UploadRetryMetrics{
		Success: s.retrySuccess,
		Failed:  s.retryFailed,
		Skipped: s.retrySkipped,
		LastTs:  s.retryLastTS,
	}
}

// recordUploadRetryMetrics 累加 retry-upload 的观测计数。
func (s *Scheduler) recordUploadRetryMetrics(stats UploadRetryStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retrySuccess += int64(stats.Succeeded)
	s.retryFailed += int64(stats.Failed)
	s.retrySkipped += int64(stats.Skipped)
	s.retryLastTS = time.Now().Unix()
}

// ListUploadFailureReasons 按原因聚合上传失败，便于排障。
func (s *Scheduler) ListUploadFailureReasons(taskID string, limit int) ([]UploadFailureReason, error) {
	// 常见误解：
	// 这里返回的是“归一化后的原因聚合”，不是原始错误明细。
	// 设计目的：压缩噪声，便于直接看 Top N 问题类别。
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if err := s.syncTasksFromStore(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if _, ok := s.tasks[taskID]; !ok {
		s.mu.Unlock()
		return nil, ErrTaskNotFound
	}
	fileStore := s.fileStore
	s.mu.Unlock()

	if fileStore == nil {
		return []UploadFailureReason{}, nil
	}
	if reader, ok := fileStore.(uploadFailureReasonReader); ok {
		return reader.ListUploadFailureReasons(context.Background(), taskID, limit)
	}

	const allFilesLimit = int(^uint(0) >> 1)
	files, err := fileStore.ListBinlogFiles(context.Background(), taskID, allFilesLimit)
	if err != nil {
		return nil, err
	}
	agg := make(map[string]UploadFailureReason)
	for _, file := range files {
		if !strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
			continue
		}
		reason := NormalizeUploadFailureReason(file.UploadError)
		item := agg[reason]
		item.Reason = reason
		item.Count++
		latest := file.UploadedAt
		if latest.IsZero() {
			latest = file.SealedAt
		}
		if latest.IsZero() {
			latest = file.CreatedAt
		}
		if latest.After(item.LatestTime) {
			item.LatestTime = latest
		}
		agg[reason] = item
	}

	out := make([]UploadFailureReason, 0, len(agg))
	for _, item := range agg {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if !out[i].LatestTime.Equal(out[j].LatestTime) {
			return out[i].LatestTime.After(out[j].LatestTime)
		}
		return out[i].Reason < out[j].Reason
	})
	if limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// NormalizeUploadFailureReason 归一化失败原因（trim/压缩空白/空值转 unknown）。
func NormalizeUploadFailureReason(reason string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(reason)), " ")
	if normalized == "" {
		return "unknown"
	}
	return normalized
}

// ListRuns 返回任务运行历史（按 started_at 倒序）。
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
		return reader.ListWorkerHeartbeats(context.Background(), limit)
	}
	return []WorkerHeartbeat{}, nil
}

// appendEventLocked 追加事件到内存并 best-effort 写入持久化层。
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

// isClusterKeyUniqueLocked 校验 cluster_key 在任务集合中是否唯一。
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

// syncTasksFromStore 把 store 任务视图同步到内存。
func (s *Scheduler) syncTasksFromStore() error {
	// 常见误解：
	// sync 不是“全量覆盖重建内存”，而是 upsert 合并；
	// 删除语义仍由 DeleteTask 控制，避免把临时运行态误删。
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
	// 这里采用 upsert 到内存视图，不主动清空 map：
	// 1) 兼容本进程刚创建但尚未来得及从 store 读回的任务；
	// 2) 删除路径由 DeleteTask 显式清理，避免周期 sync 误抹临时态。
	for _, task := range list {
		s.tasks[task.ID] = task
		if n, convErr := strconv.Atoi(task.ID); convErr == nil && n > s.seq {
			s.seq = n
		}
	}
	return nil
}

// normalizeAndValidateClusterKey 归一化并校验 cluster_key 合法性。
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

// normalizeAndValidateTaskName 归一化并校验任务名。
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

// normalizeAndValidateSourceConfig 归一化并校验源库配置。
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

// normalizeAndValidateStartConfig 归一化并校验起点配置。
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

// normalizeAndValidateStorage 归一化并校验存储策略。
func normalizeAndValidateStorage(storage Storage) (Storage, error) {
	normalized := storage
	if normalized.RetentionDays < minRetentionDays || normalized.RetentionDays > maxRetentionDays {
		return Storage{}, ErrInvalidRetentionDays
	}
	return normalized, nil
}

// hasWhitespace 判断字符串是否包含空白字符。
func hasWhitespace(value string) bool {
	return strings.ContainsAny(value, " \t\r\n")
}
