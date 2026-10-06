// Package tasks provides module-level functionality for tasks.
// input: task commands/events, loopback-aware metadata source policy, runner callbacks, store/lease/uploader dependencies
// output: source validation decisions, SameSourceHost/IsLoopbackHost identity, task state transitions, scheduling decisions, TaskStore PK/page/claim contracts, expired-lease listing contract including idle STOPPING, ErrExpiredLeaseLookupNotAvailable, ErrFailedUploadLookupNotAvailable, ErrTaskDumpConfigLocked when a live dump's source, start, storage, or cluster_key would change, local data dir for disk segment listing, read-only on-disk backup identity, adopt errors for leftover directories, execution coordination, store sync that cancels a live run when the row is STOPPING or STOPPED and does not replace an in-memory owner/epoch with a store row read before that publish, task persistence that keeps the newest snapshot when an older write finishes later and puts that snapshot back if a stale read landed during the write, and event-store reads and inserts that do not hold the scheduler lock so one task's slow metadata I/O does not block Stop, Start, lease renewal, or progress for another task
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/binlog"
)

var ErrTaskNotFound = errors.New("task not found")

// ErrDiskBackupReadOnly is a leftover data directory with segments and no task row.
// It can be listed and its files read. It has no source credentials to start.
var ErrDiskBackupReadOnly = errors.New("on-disk backup has no task metadata")

// ErrTaskAlreadyHasMetadata means adopt was asked to attach source identity to a task that already has a row.
var ErrTaskAlreadyHasMetadata = errors.New("task already has metadata")

// ErrDiskResumePosition means the highest on-disk segment has no file size that can be a FILE_POS.
var ErrDiskResumePosition = errors.New("on-disk segment has no resume position")
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
var ErrExpiredLeaseLookupNotAvailable = errors.New("expired lease lookup is not available")
var ErrFailedUploadLookupNotAvailable = errors.New("failed upload lookup is not available")
var ErrFilePosRequired = errors.New("file/pos is required")
var ErrGTIDSetRequired = errors.New("gtid_set is required")
var ErrInvalidStartMode = errors.New("invalid start mode")
var ErrInvalidRetentionDays = errors.New("invalid retention_days")
var ErrSourcePasswordRequired = errors.New("source.password is required")
var ErrSourceRequired = errors.New("source.host/port/user/password is required")

// ErrTaskDumpConfigLocked means a live dump still uses the source, start,
// storage, and cluster_key captured when that session started.
// Stop the task before changing them. The next start reads the updated row.
var ErrTaskDumpConfigLocked = errors.New("stop the task before changing source, start, storage, or cluster_key")

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
	// Verify 判断 worker/epoch 是否仍持有未过期租约。封文件前与占/续/放走同一扇门。
	Verify(ctx context.Context, taskID, workerID string, epoch int64) (bool, error)
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
	// GetTask 按主键读取单个任务。
	GetTask(ctx context.Context, taskID string) (Task, error)
	// ListTasks 列出全部任务。
	ListTasks(ctx context.Context) ([]Task, error)
	// ListTasksPage 按过滤条件分页读取任务，total 为过滤后的 COUNT。
	ListTasksPage(ctx context.Context, filter TaskListFilter) ([]Task, int, error)
	// ListStartingUnownedTasks 仅读取 STARTING 且 owner 为空的任务。
	ListStartingUnownedTasks(ctx context.Context) ([]Task, error)
	// DeleteTask 删除指定任务。
	DeleteTask(ctx context.Context, taskID string) error
}

// ExpiredLeaseTaskLister lists active tasks whose cluster lease has expired.
type ExpiredLeaseTaskLister interface {
	// ListTasksWithExpiredLease lists RUNNING/LEASE_DEGRADED/RETRY_BACKOFF tasks, and STOPPING tasks, whose lease_expire_at <= NOW(6).
	// STOPPING rows are finished as STOPPED. They are not taken over.
	ListTasksWithExpiredLease(ctx context.Context) ([]Task, error)
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
	// ListEvents 读取任务事件，旧的在前。limit 保留最新的若干条。
	ListEvents(ctx context.Context, taskID string, limit int) ([]TaskEvent, error)
}

// FileStore 定义 binlog 文件元数据存储接口。
type FileStore interface {
	// UpsertBinlogFile 写入/更新文件元数据。
	UpsertBinlogFile(ctx context.Context, meta BinlogFile) error
	// ListBinlogFiles 读取文件元数据。MySQL 目录按源序号升序，同序号封存在 open epoch 之前；limit 保留序号最大的窗口。
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

// InternalCallTimeouts 定义 Scheduler 内部依赖调用超时边界。
type InternalCallTimeouts struct {
	Read   time.Duration
	Write  time.Duration
	Lease  time.Duration
	Upload time.Duration
}

// Option 用于配置 Scheduler 的可选注入项。
type Option func(*Scheduler)

// WithRunner 注入任务执行器。
func WithRunner(runner Runner) Option {
	return func(s *Scheduler) {
		s.runner = runner
	}
}

// WithMetadataSourceEndpoint prevents tasks from replicating the metadata MySQL endpoint.
func WithMetadataSourceEndpoint(host string, port uint16) Option {
	return func(s *Scheduler) {
		s.metadataSourceHost = normalizeEndpointHost(host)
		s.metadataSourcePort = port
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

// WithDataDir 设置本地 binlog 根目录。文件目录为空时，ListFiles 扫描其中的分段。
func WithDataDir(dir string) Option {
	return func(s *Scheduler) {
		s.dataDir = strings.TrimSpace(dir)
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

// WithInternalCallTimeouts 配置内部依赖调用超时（读/写/lease/上传）。
func WithInternalCallTimeouts(timeout InternalCallTimeouts) Option {
	return func(s *Scheduler) {
		if timeout.Read > 0 {
			s.internalReadTimeout = timeout.Read
		}
		if timeout.Write > 0 {
			s.internalWriteTimeout = timeout.Write
		}
		if timeout.Lease > 0 {
			s.internalLeaseTimeout = timeout.Lease
		}
		if timeout.Upload > 0 {
			s.internalUploadTimeout = timeout.Upload
		}
	}
}

// Scheduler 负责任务配置管理、状态机推进和运行编排。
type Scheduler struct {
	mu                 sync.Mutex
	seq                int
	tasks              map[string]Task
	events             map[string][]TaskEvent
	replica            map[string]ReplicationProgress
	runner             Runner
	store              TaskStore
	metadataSourceHost string
	metadataSourcePort uint16
	cancels            map[string]context.CancelFunc
	runs               map[string]chan struct{}

	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
	// 内部依赖调用超时治理：用于 store/lease/uploader 边界，避免无界阻塞。
	internalReadTimeout   time.Duration
	internalWriteTimeout  time.Duration
	internalLeaseTimeout  time.Duration
	internalUploadTimeout time.Duration

	// cluster 相关字段：当 leaseManager != nil 时，Scheduler 进入分布式单执行语义。
	leaseManager       LeaseManager
	clusterWorkerID    string
	leaseTTL           time.Duration
	leaseRenewInterval time.Duration
	leaseGrace         time.Duration

	checkpointReader CheckpointReader
	eventStore       EventStore
	fileStore        FileStore
	dataDir          string
	fileUploader     FileUploader
	retryUploads     map[string]struct{}
	persisted        map[string]persistedTask
	retrySuccess     int64
	retryFailed      int64
	retrySkipped     int64
	retryLastTS      int64
	eventSeq         int64
	// pendingEvents are in-memory events waiting for the event store.
	// The scheduler lock is not held while they are written.
	pendingEvents []queuedEvent
	// eventLanes orders event-store writes per task. A slow insert on one
	// task does not block another task's Stop, Start, or progress.
	eventLanes map[string]*eventLane
}

// NewScheduler 创建调度器并应用可选项。
func NewScheduler(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:   make(map[string]Task),
		events:  make(map[string][]TaskEvent),
		replica: make(map[string]ReplicationProgress),
		cancels: make(map[string]context.CancelFunc),
		runs:    make(map[string]chan struct{}),

		retryBaseDelay:        time.Second,
		retryMaxDelay:         30 * time.Second,
		leaseTTL:              15 * time.Second,
		leaseRenewInterval:    5 * time.Second,
		leaseGrace:            30 * time.Second,
		retryUploads:          make(map[string]struct{}),
		persisted:             make(map[string]persistedTask),
		internalReadTimeout:   3 * time.Second,
		internalWriteTimeout:  5 * time.Second,
		internalLeaseTimeout:  2 * time.Second,
		internalUploadTimeout: 30 * time.Second,
		eventLanes:            make(map[string]*eventLane),
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

// persistedTask is the newest snapshot handed to the store for one task.
// gen increases on every persist so a write that started earlier can tell
// that a later transition already exists.
// published is the gen whose snapshot is already in the store. A read that
// sampled an older published value raced that write and must not replace memory.
type persistedTask struct {
	gen       uint64
	published uint64
	task      Task
}

func (s *Scheduler) persistTaskLocked(task Task) error {
	if s.store == nil {
		s.flushPendingEventsLocked()
		return nil
	}
	// 避免持锁执行潜在慢 I/O（DB），降低调度锁的阻塞影响。
	// 放开锁之后，更新的一次转换可能先落库。旧快照写完时如果 gen 已经前进，
	// 再把当时最新的快照写回去，避免 STOPPING 覆盖已经落库的 STOPPED。
	current := s.persisted[task.ID]
	current.gen++
	current.task = task
	s.persisted[task.ID] = current
	gen := current.gen
	snapshot := task
	for {
		batch := s.detachPendingEventsLocked()
		s.mu.Unlock()
		s.writeEventBatch(batch)
		ctx, cancel := s.withWriteTimeout(context.Background())
		err := s.store.UpsertTask(ctx, snapshot)
		cancel()
		s.mu.Lock()
		if err != nil {
			return err
		}
		latest := s.persisted[task.ID]
		if latest.gen == gen {
			latest.published = gen
			s.persisted[task.ID] = latest
			s.restorePublishedTaskLocked(snapshot)
			return nil
		}
		gen = latest.gen
		snapshot = latest.task
	}
}

// staleStoreReadLocked reports that a store snapshot was read across a publish.
// seen is persisted[id].published sampled before the read. Caller holds s.mu.
func (s *Scheduler) staleStoreReadLocked(id string, seen uint64) bool {
	return s.persisted[id].published != seen
}

// restorePublishedTaskLocked puts the snapshot just written back when a store
// read applied an older row while s.mu was released. Caller holds s.mu.
func (s *Scheduler) restorePublishedTaskLocked(snapshot Task) {
	mem, ok := s.tasks[snapshot.ID]
	if !ok {
		return
	}
	if mem.State == snapshot.State && mem.OwnerWorkerID == snapshot.OwnerWorkerID && mem.Epoch == snapshot.Epoch && mem.RunID == snapshot.RunID {
		return
	}
	s.tasks[snapshot.ID] = snapshot
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

// queuedEvent is one event-store write. ticket is that task's write order.
type queuedEvent struct {
	event  TaskEvent
	ticket uint64
	lane   *eventLane
}

// eventLane serializes event-store inserts for one task.
// Different tasks use different lanes, so one slow insert does not block another task.
type eventLane struct {
	mu     sync.Mutex
	cond   *sync.Cond
	next   uint64
	issued uint64
}

func (s *Scheduler) appendEventLocked(taskID, eventType, message, detail string) {
	// 函数名里的 Locked 表示：调用方必须已经持有 s.mu。
	// 这里会修改 eventSeq 和 events map，需要同一把锁保护。
	// 事件库写入不在这把锁里。调用方在放开 s.mu 之前要 flush，
	// 或者接着走 persistTaskLocked，由它在放开锁之后写入。
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
	if s.eventStore == nil {
		return
	}
	lane, ticket := s.issueEventTicketLocked(taskID)
	s.pendingEvents = append(s.pendingEvents, queuedEvent{event: event, ticket: ticket, lane: lane})
}

func (s *Scheduler) issueEventTicketLocked(taskID string) (*eventLane, uint64) {
	if s.eventLanes == nil {
		s.eventLanes = make(map[string]*eventLane)
	}
	lane := s.eventLanes[taskID]
	if lane == nil {
		lane = &eventLane{}
		lane.cond = sync.NewCond(&lane.mu)
		s.eventLanes[taskID] = lane
	}
	lane.issued++
	return lane, lane.issued
}

func (s *Scheduler) detachPendingEventsLocked() []queuedEvent {
	if len(s.pendingEvents) == 0 {
		return nil
	}
	batch := s.pendingEvents
	s.pendingEvents = nil
	return batch
}

// flushPendingEventsLocked writes queued events without holding s.mu.
// Caller holds s.mu and still holds it when this returns.
func (s *Scheduler) flushPendingEventsLocked() {
	batch := s.detachPendingEventsLocked()
	if len(batch) == 0 {
		return
	}
	s.mu.Unlock()
	s.writeEventBatch(batch)
	s.mu.Lock()
}

func (s *Scheduler) writeEventBatch(batch []queuedEvent) {
	store := s.eventStore
	timeout := s.internalWriteTimeout
	for _, item := range batch {
		if item.lane == nil {
			continue
		}
		item.lane.write(store, item.event, item.ticket, timeout)
	}
}

func (lane *eventLane) write(store EventStore, event TaskEvent, ticket uint64, timeout time.Duration) {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	for lane.next+1 != ticket {
		lane.cond.Wait()
	}
	if store != nil {
		// 事件落库失败不阻断主流程，保证调度与拉流优先可用。
		ctx, cancel := withTimeout(context.Background(), timeout)
		_ = store.AppendEvent(ctx, event)
		cancel()
	}
	lane.next = ticket
	lane.cond.Broadcast()
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
	seen := make(map[string]uint64, len(s.persisted))
	for id, row := range s.persisted {
		seen[id] = row.published
	}
	s.mu.Unlock()
	if store == nil {
		return nil
	}

	ctx, cancel := s.withReadTimeout(context.Background())
	list, err := store.ListTasks(ctx)
	cancel()
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// 这里采用 upsert 到内存视图，不主动清空 map：
	// 1) 兼容本进程刚创建但尚未来得及从 store 读回的任务；
	// 2) 删除路径由 DeleteTask 显式清理，避免周期 sync 误抹临时态。
	for _, task := range list {
		// 这次 List 跨过了一次落库。内存里已经是刚拿到的 owner/epoch，不能用读到的旧行盖掉。
		if s.staleStoreReadLocked(task.ID, seen[task.ID]) {
			continue
		}
		// 共享行已经要求停止时，不能用它覆盖正在跑的抄本，否则 owner/epoch 被清掉，退出时放不掉租约。
		if s.noteRemoteStopLocked(task) {
			continue
		}
		s.tasks[task.ID] = task
		if n, convErr := strconv.Atoi(task.ID); convErr == nil && n > s.seq {
			s.seq = n
		}
	}
	s.flushPendingEventsLocked()
	return nil
}

func (s *Scheduler) withReadTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return withTimeout(parent, s.internalReadTimeout)
}

func (s *Scheduler) withWriteTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return withTimeout(parent, s.internalWriteTimeout)
}

func (s *Scheduler) withLeaseTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return withTimeout(parent, s.internalLeaseTimeout)
}

func (s *Scheduler) withUploadTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return withTimeout(parent, s.internalUploadTimeout)
}

func withTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
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

func (s *Scheduler) normalizeAndValidateSourceConfig(source SourceConfig) (SourceConfig, error) {
	normalized, err := normalizeAndValidateSourceConfig(source)
	if err != nil {
		return SourceConfig{}, err
	}
	if err := s.validateMetadataSourceEndpoint(normalized); err != nil {
		return SourceConfig{}, err
	}
	return normalized, nil
}

func (s *Scheduler) validateMetadataSourceEndpoint(source SourceConfig) error {
	if s.metadataSourcePort != 0 && source.Port == s.metadataSourcePort && normalizeEndpointHost(source.Host) == s.metadataSourceHost {
		return fmt.Errorf("%w: source %s:%d is the metadata MySQL endpoint; use a separate MySQL instance", ErrInvalidSourceConfig, source.Host, source.Port)
	}
	return nil
}

// normalizeEndpointHost compares endpoint spellings without DNS resolution.
func normalizeEndpointHost(host string) string {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if isLoopbackHost(normalized) {
		return "127.0.0.1"
	}
	return normalized
}

// IsLoopbackHost reports whether host is localhost or a loopback IP literal.
// It intentionally does not resolve arbitrary hostnames.
func IsLoopbackHost(host string) bool {
	return isLoopbackHost(host)
}

// SameSourceHost reports whether two host spellings name the same source.
// Loopback aliases are one source. Other hosts match the exact stored spelling.
func SameSourceHost(a, b string) bool {
	if a == b {
		return true
	}
	return IsLoopbackHost(a) && IsLoopbackHost(b)
}

func isLoopbackHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if normalized == "localhost" {
		return true
	}
	if len(normalized) >= 2 && normalized[0] == '[' && normalized[len(normalized)-1] == ']' {
		normalized = normalized[1 : len(normalized)-1]
		if !strings.Contains(normalized, ":") {
			return false
		}
	}
	ip := net.ParseIP(normalized)
	return ip != nil && ip.IsLoopback()
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

// EffectiveLocalRetentionDays is how long a sealed file stays on local disk.
// An omitted local_retention_days uses retention_days, then the default.
func (s Storage) EffectiveLocalRetentionDays() int {
	if s.LocalRetentionDays > 0 {
		return s.LocalRetentionDays
	}
	if s.RetentionDays > 0 {
		return s.RetentionDays
	}
	return defaultRetentionDays
}

// EffectiveBucketRetentionDays is how long an uploaded object and its catalog
// row stay. An omitted bucket_retention_days uses retention_days, then the default.
func (s Storage) EffectiveBucketRetentionDays() int {
	if s.BucketRetentionDays > 0 {
		return s.BucketRetentionDays
	}
	if s.RetentionDays > 0 {
		return s.RetentionDays
	}
	return defaultRetentionDays
}

// normalizeAndValidateStorage 归一化并校验存储策略。
// 只配 retention_days 时本地和桶的有效天数相同。桶短于本地则拒绝。
func normalizeAndValidateStorage(storage Storage) (Storage, error) {
	normalized := storage
	if normalized.RetentionDays < minRetentionDays || normalized.RetentionDays > maxRetentionDays {
		return Storage{}, ErrInvalidRetentionDays
	}
	if normalized.LocalRetentionDays != 0 && (normalized.LocalRetentionDays < minRetentionDays || normalized.LocalRetentionDays > maxRetentionDays) {
		return Storage{}, fmt.Errorf("%w: storage.local_retention_days must be 1-3650", ErrInvalidRetentionDays)
	}
	if normalized.BucketRetentionDays != 0 && (normalized.BucketRetentionDays < minRetentionDays || normalized.BucketRetentionDays > maxRetentionDays) {
		return Storage{}, fmt.Errorf("%w: storage.bucket_retention_days must be 1-3650", ErrInvalidRetentionDays)
	}
	localDays := normalized.EffectiveLocalRetentionDays()
	bucketDays := normalized.EffectiveBucketRetentionDays()
	if bucketDays < localDays {
		return Storage{}, fmt.Errorf("%w: storage.bucket_retention_days (%d) is shorter than local retention (%d)", ErrInvalidRetentionDays, bucketDays, localDays)
	}
	return normalized, nil
}

// hasWhitespace 判断字符串是否包含空白字符。
func hasWhitespace(value string) bool {
	return strings.ContainsAny(value, " \t\r\n")
}
