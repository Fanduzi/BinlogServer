package tasks

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"binlog_server/internal/binlog"
)

var ErrTaskNotFound = errors.New("task not found")
var ErrInvalidSourceConfig = errors.New("invalid source config")
var ErrRunnerNotConfigured = errors.New("runner is not configured")

const defaultRetentionDays = 7

type Runner interface {
	Run(ctx context.Context, task Task) error
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

	checkpointReader CheckpointReader
	eventStore       EventStore
	fileStore        FileStore
	eventSeq         int64
}

func NewScheduler(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:   make(map[string]Task),
		events:  make(map[string][]TaskEvent),
		replica: make(map[string]ReplicationProgress),
		cancels: make(map[string]context.CancelFunc),
		runs:    make(map[string]chan struct{}),

		retryBaseDelay: time.Second,
		retryMaxDelay:  30 * time.Second,
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

func (s *Scheduler) CreateTask(name string) (Task, error) {
	if name == "" {
		return Task{}, errors.New("name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	id := strconv.Itoa(s.seq)
	now := time.Now()
	task := Task{
		ID:    id,
		Name:  name,
		State: StateCreated,
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
	if source.Host == "" || source.Port == 0 || source.User == "" {
		return ErrInvalidSourceConfig
	}
	if source.Flavor == "" {
		source.Flavor = "mysql"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	if source.Password == "" {
		source.Password = task.Source.Password
	}
	task.Source = source
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_SOURCE_CONFIGURED", "source configured", source.Host)
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) ConfigureStart(id string, start StartConfig) error {
	if start.Mode == "" {
		start.Mode = StartModeLatest
	}
	if start.Mode == StartModeFilePos && (start.File == "" || start.Pos == 0) {
		return errors.New("file/pos is required")
	}
	if start.Mode == StartModeGTID && start.GTIDSet == "" {
		return errors.New("gtid_set is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	task.Start = start
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_START_CONFIGURED", "start strategy configured", string(start.Mode))
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) ConfigureStorage(id string, storage Storage) error {
	if storage.RetentionDays <= 0 {
		storage.RetentionDays = defaultRetentionDays
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	task.Storage = storage
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STORAGE_CONFIGURED", "storage configured", "")
	if err := s.persistTaskLocked(task); err != nil {
		return err
	}
	return nil
}

func (s *Scheduler) ConfigureName(id, name string) error {
	if name == "" {
		return errors.New("name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	task.Name = name
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_RENAMED", "task renamed", name)
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
	if task.State != StateCreated && task.State != StateStopped && task.State != StateRetryBackoff {
		s.mu.Unlock()
		return fmt.Errorf("cannot start from state %s", task.State)
	}
	if s.runner == nil {
		// 没有 runner 时不允许启动，避免状态被错误标记为 RUNNING。
		s.mu.Unlock()
		return ErrRunnerNotConfigured
	}
	// Scheduler 在 start 前强制校验最小 source config。
	if task.Source.Host == "" || task.Source.Port == 0 || task.Source.User == "" {
		s.mu.Unlock()
		return ErrInvalidSourceConfig
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

	go s.runTask(ctx, id, taskForRun, done)
	return nil
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
	if task.State != StateRunning && task.State != StateRetryBackoff && task.State != StateStarting {
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
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return task, nil
}

func (s *Scheduler) DeleteTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[id]; !ok {
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
			return err
		}
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
		s.mu.Lock()
		if currentDone, ok := s.runs[id]; ok && currentDone == done {
			delete(s.runs, id)
		}
		// 收到 stop 请求后，直到执行 goroutine 真退出才收敛到 STOPPED。
		if task, ok := s.tasks[id]; ok && task.State == StateStopping {
			_ = s.markStoppedLocked(id)
		}
		s.mu.Unlock()
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

func (s *Scheduler) markStoppedLocked(id string) error {
	task, ok := s.tasks[id]
	if !ok {
		return nil
	}
	if task.State == StateStopped {
		return nil
	}
	task.State = StateStopped
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
	if _, err := s.GetTask(taskID); err != nil {
		return binlog.Checkpoint{}, false, err
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
