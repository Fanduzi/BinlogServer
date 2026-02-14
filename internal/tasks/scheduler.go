package tasks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

var ErrTaskNotFound = errors.New("task not found")
var ErrInvalidSourceConfig = errors.New("invalid source config")

type Runner interface {
	Run(ctx context.Context, task Task) error
}

type TaskStore interface {
	UpsertTask(ctx context.Context, task Task) error
	ListTasks(ctx context.Context) ([]Task, error)
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

type Scheduler struct {
	mu      sync.Mutex
	seq     int
	tasks   map[string]Task
	runner  Runner
	store   TaskStore
	cancels map[string]context.CancelFunc
}

func NewScheduler(opts ...Option) *Scheduler {
	s := &Scheduler{
		tasks:   make(map[string]Task),
		cancels: make(map[string]context.CancelFunc),
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
		UpdatedAt: now,
	}
	s.tasks[id] = task
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
	err := s.runner.Run(ctx, task)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.tasks[id]
	if !ok {
		return
	}
	if current.State == StateStopped || current.State == StateStopping {
		return
	}
	current.State = StateRetryBackoff
	current.LastError = err.Error()
	current.UpdatedAt = time.Now()
	s.tasks[id] = current
	_ = s.persistTaskLocked(current)
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
