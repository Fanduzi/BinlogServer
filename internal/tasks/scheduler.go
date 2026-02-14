package tasks

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

var ErrTaskNotFound = errors.New("task not found")

type Scheduler struct {
	mu    sync.Mutex
	seq   int
	tasks map[string]Task
}

func NewScheduler() *Scheduler {
	return &Scheduler{
		tasks: make(map[string]Task),
	}
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
		ID:        id,
		Name:      name,
		State:     StateCreated,
		UpdatedAt: now,
	}
	s.tasks[id] = task
	return task, nil
}

func (s *Scheduler) StartTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	if task.State != StateCreated && task.State != StateStopped && task.State != StateRetryBackoff {
		return fmt.Errorf("cannot start from state %s", task.State)
	}

	task.State = StateStarting
	task.UpdatedAt = time.Now()
	task.State = StateRunning
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
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
	return nil
}

func (s *Scheduler) StopTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	if task.State != StateRunning && task.State != StateRetryBackoff && task.State != StateStarting {
		return fmt.Errorf("cannot stop from state %s", task.State)
	}

	task.State = StateStopping
	task.UpdatedAt = time.Now()
	task.State = StateStopped
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
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
