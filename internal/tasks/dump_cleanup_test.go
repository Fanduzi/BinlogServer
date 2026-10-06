// Package tasks provides module-level functionality for tasks.
// input: KILL results for one saved connection id
// output: pending, repeat, clear, and deleted-task cases of the cleanup state machine
// pos: unit coverage for an unreachable Stop that still becomes STOPPED
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestApplyDumpCleanup(t *testing.T) {
	refused := errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")
	pending := DumpCleanup{ConnectionID: 42, Host: "10.0.0.8", Port: 3306}
	other := DumpCleanup{ConnectionID: 99, Host: "10.0.0.8", Port: 3306}

	tests := []struct {
		name      string
		current   DumpCleanup
		attempted DumpCleanup
		killErr   error
		want      DumpCleanup
		event     string
	}{
		{name: "unreachable sets pending", attempted: pending, killErr: refused, want: pending, event: dumpCleanupPending},
		{name: "same failure does not emit again", current: pending, attempted: pending, killErr: refused, want: pending},
		{name: "new id replaces pending", current: pending, attempted: other, killErr: refused, want: other, event: dumpCleanupPending},
		{name: "success clears", current: pending, attempted: DumpCleanup{ConnectionID: 42}, want: DumpCleanup{}, event: dumpCleanupCleared},
		{name: "success with nothing pending is quiet", attempted: DumpCleanup{ConnectionID: 42}},
		{name: "success for a different id keeps pending", current: pending, attempted: DumpCleanup{ConnectionID: 7}, want: pending},
		{name: "zero id failure keeps current", current: pending, killErr: refused, want: pending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, event := ApplyDumpCleanup(tt.current, tt.attempted, tt.killErr)
			if !got.same(tt.want) || event != tt.event {
				t.Fatalf("got %+v event %q, want %+v event %q", got, event, tt.want, tt.event)
			}
		})
	}
	if got := DecodeDumpCleanup(EncodeDumpCleanup(pending)); !got.same(pending) || got.Warning != DumpCleanupWarning(42) {
		t.Fatalf("round trip %+v", got)
	}
	if DecodeDumpCleanup("").ConnectionID != 0 || DecodeDumpCleanup("{").ConnectionID != 0 {
		t.Fatal("blank or broken marker must be empty")
	}
}

func TestNoteDumpCleanupEmitsOnePendingAndOneCleared(t *testing.T) {
	s := NewScheduler()
	task := s.mustTask(t)
	src := SourceConfig{Host: "10.0.0.8", Port: 3306, User: "repl", Password: "secret"}
	refused := errors.New("dial tcp 10.0.0.8:3306: connect: connection refused")

	s.noteDumpCleanup(task.ID, src, 42, refused)
	s.noteDumpCleanup(task.ID, src, 42, refused)
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 42 {
		t.Fatalf("pending %+v", got.PendingDumpCleanup)
	}
	if got.PendingDumpCleanup.Warning != DumpCleanupWarning(42) {
		t.Fatalf("warning %q", got.PendingDumpCleanup.Warning)
	}
	assertDumpCleanupEvents(t, s, task.ID, 1, 0)

	s.noteDumpCleanup(task.ID, src, 42, nil)
	got, err = s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask after clear: %v", err)
	}
	if got.PendingDumpCleanup != nil {
		t.Fatalf("still pending %+v", got.PendingDumpCleanup)
	}
	assertDumpCleanupEvents(t, s, task.ID, 1, 1)

	s.noteDumpCleanup(task.ID, src, 42, nil)
	assertDumpCleanupEvents(t, s, task.ID, 1, 1)
}

func TestDumpCleanupRetryUsesCurrentPasswordThenStopsWhenDeleted(t *testing.T) {
	s := NewScheduler()
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateStopped
	item.Source.Password = "old"
	pending := DumpCleanup{ConnectionID: 7, Host: "10.0.0.8", Port: 3306}.warned()
	item.PendingDumpCleanup = &pending
	s.tasks[task.ID] = item
	s.mu.Unlock()

	var passwords []string
	s.dumpKiller = func(source SourceConfig, id uint32) error {
		if id != 7 || source.Host != "10.0.0.8" || source.Port != 3306 {
			t.Fatalf("kill target %+v id %d", source, id)
		}
		passwords = append(passwords, source.Password)
		if source.Password != "new" {
			return errors.New("access denied")
		}
		return nil
	}
	if !s.retryPendingDumpCleanups() {
		t.Fatal("old password should stay pending")
	}
	s.mu.Lock()
	item = s.tasks[task.ID]
	item.Source.Password = "new"
	s.tasks[task.ID] = item
	s.mu.Unlock()
	if s.retryPendingDumpCleanups() {
		t.Fatal("new password should clear")
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.PendingDumpCleanup != nil {
		t.Fatalf("pending after success %+v", got.PendingDumpCleanup)
	}
	if len(passwords) != 2 || passwords[0] != "old" || passwords[1] != "new" {
		t.Fatalf("passwords %v", passwords)
	}

	s.mu.Lock()
	item = s.tasks[task.ID]
	again := DumpCleanup{ConnectionID: 7, Host: "10.0.0.8", Port: 3306}.warned()
	item.PendingDumpCleanup = &again
	item.State = StateStopped
	s.tasks[task.ID] = item
	s.mu.Unlock()
	if err := s.DeleteTask(task.ID); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	called := false
	s.dumpKiller = func(SourceConfig, uint32) error {
		called = true
		return nil
	}
	if s.retryPendingDumpCleanups() || called {
		t.Fatal("deleted task must not be killed")
	}
}

func TestRunDumpCleanupRetryStops(t *testing.T) {
	s := NewScheduler()
	s.dumpKiller = func(SourceConfig, uint32) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunDumpCleanupRetry(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry loop did not stop")
	}
}

func TestSchema3PendingDumpSurvivesStopped(t *testing.T) {
	store := newColumnlessStore()
	s := NewScheduler(WithStore(store))
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateRetryBackoff
	s.tasks[task.ID] = item
	done := make(chan struct{})
	s.runs[task.ID] = done
	s.mu.Unlock()

	src := SourceConfig{Host: "10.0.0.8", Port: 3306, User: "repl", Password: "secret"}
	refused := errors.New("dial tcp 10.0.0.8:3306: connect: connection refused")
	s.noteDumpCleanup(task.ID, src, 1101, refused)

	s.mu.Lock()
	if err := s.markStoppedLocked(task.ID); err != nil {
		s.mu.Unlock()
		t.Fatalf("markStopped: %v", err)
	}
	if currentDone, ok := s.runs[task.ID]; ok && currentDone == done {
		delete(s.runs, task.ID)
	}
	close(done)
	if err := s.reloadIdleTaskFromStoreLocked(task.ID); err != nil {
		s.mu.Unlock()
		t.Fatalf("reload: %v", err)
	}
	wiped := s.tasks[task.ID]
	wiped.PendingDumpCleanup = nil
	s.tasks[task.ID] = wiped
	s.mu.Unlock()

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("state %s", got.State)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 1101 {
		t.Fatalf("pending %+v", got.PendingDumpCleanup)
	}
	if !got.PendingDumpCleanup.ProcessLocal || !strings.Contains(got.PendingDumpCleanup.Warning, "this process") {
		t.Fatalf("warning %+v", got.PendingDumpCleanup)
	}
	if !strings.Contains(got.PendingDumpCleanup.Warning, "may still be open") {
		t.Fatalf("warning %q", got.PendingDumpCleanup.Warning)
	}

	other := NewScheduler(WithStore(store))
	if err := other.Restore(context.Background()); err != nil {
		t.Fatalf("other restore: %v", err)
	}
	otherGot, err := other.GetTask(task.ID)
	if err != nil {
		t.Fatalf("other GetTask: %v", err)
	}
	if otherGot.PendingDumpCleanup != nil {
		t.Fatalf("schema 3 marker leaked to another process: %+v", otherGot.PendingDumpCleanup)
	}
	otherCalled := false
	other.dumpKiller = func(SourceConfig, uint32) error {
		otherCalled = true
		return nil
	}
	if other.retryPendingDumpCleanups() || otherCalled {
		t.Fatal("another process must not KILL a schema 3 marker")
	}

	s.mu.Lock()
	s.runs[task.ID] = make(chan struct{})
	s.mu.Unlock()
	killedWhileOpen := false
	s.dumpKiller = func(SourceConfig, uint32) error {
		killedWhileOpen = true
		return nil
	}
	if s.retryPendingDumpCleanups() || killedWhileOpen {
		t.Fatal("open runner must not be killed by the pending retry")
	}
	s.mu.Lock()
	close(s.runs[task.ID])
	delete(s.runs, task.ID)
	s.mu.Unlock()

	var passwords []string
	s.dumpKiller = func(source SourceConfig, id uint32) error {
		if id != 1101 || source.Host != "10.0.0.8" || source.Port != 3306 {
			t.Fatalf("kill target %+v id %d", source, id)
		}
		passwords = append(passwords, source.Password)
		if source.Password != "rotated" {
			return errors.New("access denied")
		}
		return nil
	}
	if !s.retryPendingDumpCleanups() {
		t.Fatal("old password should stay pending after STOPPED")
	}
	stored, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("stored task: %v", err)
	}
	stored.Source.Password = "rotated"
	if err := store.UpsertTask(context.Background(), stored); err != nil {
		t.Fatalf("rotate password: %v", err)
	}
	if s.retryPendingDumpCleanups() {
		t.Fatal("rotated password should clear")
	}
	got, err = s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask after clear: %v", err)
	}
	if got.PendingDumpCleanup != nil {
		t.Fatalf("still pending %+v", got.PendingDumpCleanup)
	}
	if len(passwords) != 2 || passwords[0] != "secret" || passwords[1] != "rotated" {
		t.Fatalf("passwords %v", passwords)
	}
	assertDumpCleanupEvents(t, s, task.ID, 1, 1)
}

func TestSchema4PendingDumpSurvivesRestart(t *testing.T) {
	store := newPendingColumnStore()
	s := NewScheduler(WithStore(store))
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateRunning
	s.tasks[task.ID] = item
	s.mu.Unlock()

	src := SourceConfig{Host: "10.0.0.8", Port: 3306, User: "repl", Password: "secret"}
	refused := errors.New("dial tcp 10.0.0.8:3306: connect: connection refused")
	s.noteDumpCleanup(task.ID, src, 42, refused)
	s.mu.Lock()
	if err := s.markStoppedLocked(task.ID); err != nil {
		s.mu.Unlock()
		t.Fatalf("markStopped: %v", err)
	}
	s.mu.Unlock()

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateStopped || got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 42 || got.PendingDumpCleanup.ProcessLocal {
		t.Fatalf("before restart %+v state %s", got.PendingDumpCleanup, got.State)
	}

	restarted := NewScheduler(WithStore(store))
	if err := restarted.Restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err = restarted.GetTask(task.ID)
	if err != nil {
		t.Fatalf("restored GetTask: %v", err)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 42 || got.PendingDumpCleanup.ProcessLocal {
		t.Fatalf("restored pending %+v", got.PendingDumpCleanup)
	}
	restarted.dumpKiller = func(source SourceConfig, id uint32) error {
		if id != 42 || source.Password != "secret" {
			t.Fatalf("kill %+v id %d", source, id)
		}
		return nil
	}
	if restarted.retryPendingDumpCleanups() {
		t.Fatal("schema 4 restart should clear on KILL")
	}
	got, err = restarted.GetTask(task.ID)
	if err != nil {
		t.Fatalf("cleared GetTask: %v", err)
	}
	if got.PendingDumpCleanup != nil {
		t.Fatalf("still pending %+v", got.PendingDumpCleanup)
	}
}

func TestSchema4ClearDropsAnotherProcessRegistry(t *testing.T) {
	store := newPendingColumnStore()
	holder := NewScheduler(WithStore(store))
	task := holder.mustTask(t)
	src := SourceConfig{Host: "10.0.0.8", Port: 3306, User: "repl", Password: "secret"}
	refused := errors.New("dial tcp 10.0.0.8:3306: connect: connection refused")
	holder.noteDumpCleanup(task.ID, src, 178, refused)

	reader := NewScheduler(WithStore(store))
	got, err := reader.GetTask(task.ID)
	if err != nil {
		t.Fatalf("reader GetTask: %v", err)
	}
	if got.PendingDumpCleanup == nil || got.PendingDumpCleanup.ConnectionID != 178 || got.PendingDumpCleanup.ProcessLocal {
		t.Fatalf("reader pending %+v", got.PendingDumpCleanup)
	}

	holder.noteDumpCleanup(task.ID, src, 178, nil)
	got, err = reader.GetTask(task.ID)
	if err != nil {
		t.Fatalf("reader after clear: %v", err)
	}
	if got.PendingDumpCleanup != nil {
		t.Fatalf("stale registry %+v", got.PendingDumpCleanup)
	}
}

type columnlessStore struct {
	*fakeStore
}

func newColumnlessStore() *columnlessStore {
	return &columnlessStore{fakeStore: newFakeStore()}
}

func (s *columnlessStore) UpsertTask(ctx context.Context, task Task) error {
	task.PendingDumpCleanup = nil
	return s.fakeStore.UpsertTask(ctx, task)
}

func (s *columnlessStore) GetTask(ctx context.Context, taskID string) (Task, error) {
	task, err := s.fakeStore.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	task.PendingDumpCleanup = nil
	return task, nil
}

func (s *columnlessStore) ListTasks(ctx context.Context) ([]Task, error) {
	list, err := s.fakeStore.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].PendingDumpCleanup = nil
	}
	return list, nil
}

type pendingColumnStore struct {
	*fakeStore
	pending map[string]string
}

func newPendingColumnStore() *pendingColumnStore {
	return &pendingColumnStore{fakeStore: newFakeStore(), pending: map[string]string{}}
}

func (s *pendingColumnStore) PendingDumpColumn() bool { return true }

func (s *pendingColumnStore) SavePendingDumpCleanup(_ context.Context, taskID, previous, next string) (bool, error) {
	if s.pending[taskID] != previous {
		return false, nil
	}
	s.pending[taskID] = next
	return true, nil
}

func (s *pendingColumnStore) decorate(task Task) Task {
	task.PendingDumpCleanup = nil
	if decoded := DecodeDumpCleanup(s.pending[task.ID]); decoded.ConnectionID != 0 {
		task.PendingDumpCleanup = &decoded
	}
	return task
}

func (s *pendingColumnStore) GetTask(ctx context.Context, taskID string) (Task, error) {
	task, err := s.fakeStore.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	return s.decorate(task), nil
}

func (s *pendingColumnStore) ListTasks(ctx context.Context) ([]Task, error) {
	list, err := s.fakeStore.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i] = s.decorate(list[i])
	}
	return list, nil
}

func (s *Scheduler) mustTask(t *testing.T) Task {
	t.Helper()
	task, err := s.CreateTask("cleanup", "cleanup-key")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.ConfigureSource(task.ID, SourceConfig{Host: "10.0.0.8", Port: 3306, User: "repl", Password: "secret"}); err != nil {
		t.Fatalf("ConfigureSource: %v", err)
	}
	return task
}

func assertDumpCleanupEvents(t *testing.T, s *Scheduler, id string, pending, cleared int) {
	t.Helper()
	events, err := s.ListEvents(id, 20)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	gotPending, gotCleared := 0, 0
	for _, event := range events {
		switch event.Type {
		case "DUMP_CLEANUP_PENDING":
			gotPending++
		case "DUMP_CLEANUP_CLEARED":
			gotCleared++
		}
	}
	if gotPending != pending || gotCleared != cleared {
		t.Fatalf("pending=%d cleared=%d events=%v", gotPending, gotCleared, events)
	}
}
