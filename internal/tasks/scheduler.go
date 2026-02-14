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

const defaultRetentionDays = 7

type Runner interface {
	Run(ctx context.Context, task Task) error
}

type TaskStore interface {
	UpsertTask(ctx context.Context, task Task) error
	ListTasks(ctx context.Context) ([]Task, error)
	DeleteTask(ctx context.Context, taskID string) error
}

type CheckpointReader interface {
	LoadCheckpoint(ctx context.Context, taskID string) (binlog.Checkpoint, bool, error)
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

type Scheduler struct {
	mu      sync.Mutex
	seq     int
	tasks   map[string]Task
	events  map[string][]TaskEvent
	runner  Runner
	store   TaskStore
	cancels map[string]context.CancelFunc

	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration

	checkpointReader CheckpointReader
	eventSeq         int64
}

func NewScheduler(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:   make(map[string]Task),
		events:  make(map[string][]TaskEvent),
		cancels: make(map[string]context.CancelFunc),

		retryBaseDelay: time.Second,
		retryMaxDelay:  30 * time.Second,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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
	if s.runner != nil {
		if task.Source.Host == "" || task.Source.Port == 0 || task.Source.User == "" {
			s.mu.Unlock()
			return ErrInvalidSourceConfig
		}
	}

	task.State = StateStarting
	task.UpdatedAt = time.Now()
	task.State = StateRunning
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STARTED", "task started", "")
	if err := s.persistTaskLocked(task); err != nil {
		s.mu.Unlock()
		return err
	}
	runner := s.runner
	s.mu.Unlock()

	if runner != nil {
		ctx, cancel := context.WithCancel(context.Background())

		s.mu.Lock()
		if oldCancel, ok := s.cancels[id]; ok {
			oldCancel()
		}
		s.cancels[id] = cancel
		taskForRun := s.tasks[id]
		s.mu.Unlock()

		go s.runTask(ctx, id, taskForRun)
	}
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

	if cancel, ok := s.cancels[id]; ok {
		cancel()
		delete(s.cancels, id)
	}

	task.State = StateStopping
	task.UpdatedAt = time.Now()
	task.State = StateStopped
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	s.appendEventLocked(id, "TASK_STOPPED", "task stopped", "")
	if err := s.persistTaskLocked(task); err != nil {
		s.mu.Unlock()
		return err
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
	delete(s.tasks, id)
	delete(s.events, id)
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

func (s *Scheduler) runTask(ctx context.Context, id string, task Task) {
	attempt := 0
	for {
		err := s.runner.Run(ctx, task)
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
		current.State = StateRunning
		current.LastError = ""
		current.UpdatedAt = time.Now()
		s.tasks[id] = current
		s.appendEventLocked(id, "TASK_RETRY_RECOVERED", "task recovered after retry", "")
		_ = s.persistTaskLocked(current)
		task = current
		s.mu.Unlock()
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

func (s *Scheduler) appendEventLocked(taskID, eventType, message, detail string) {
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
}
