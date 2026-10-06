// Package replication provides module-level functionality for replication.
// input: cluster scheduler stop/start under a concurrent GET /api/tasks/{id} poll, in-memory lease, and a scripted binlog stream
// output: proof that each cycle ends RUNNING, renews the lease, writes mysql-bin.000001.open.eN, and does not report SEGMENT_NOT_ON_WORKER
// pos: operator loop for the epoch-0 start race under task polling
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

type pollTaskStore struct {
	mu    sync.Mutex
	tasks map[string]tasks.Task
}

func (s *pollTaskStore) UpsertTask(_ context.Context, task tasks.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = map[string]tasks.Task{}
	}
	s.tasks[task.ID] = task
	return nil
}

func (s *pollTaskStore) GetTask(_ context.Context, taskID string) (tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return tasks.Task{}, tasks.ErrTaskNotFound
	}
	return task, nil
}

func (s *pollTaskStore) ListTasks(context.Context) ([]tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]tasks.Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task)
	}
	return out, nil
}

func (s *pollTaskStore) ListTasksPage(_ context.Context, filter tasks.TaskListFilter) ([]tasks.Task, int, error) {
	items, err := s.ListTasks(context.Background())
	if err != nil {
		return nil, 0, err
	}
	page, total := tasks.PageTasks(items, filter)
	return page, total, nil
}

func (s *pollTaskStore) ListStartingUnownedTasks(context.Context) ([]tasks.Task, error) {
	items, err := s.ListTasks(context.Background())
	if err != nil {
		return nil, err
	}
	return tasks.StartingUnownedTasks(items), nil
}

func (s *pollTaskStore) DeleteTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
	return nil
}

type countingLease struct {
	inner *tasks.MemoryLease
	mu    sync.Mutex
	renew int
}

func (l *countingLease) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	return l.inner.Acquire(ctx, taskID, workerID, ttl)
}

func (l *countingLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	ok, err := l.inner.Renew(ctx, taskID, workerID, epoch, now, ttl)
	if err == nil && ok && epoch > 0 {
		l.mu.Lock()
		l.renew++
		l.mu.Unlock()
	}
	return ok, err
}

func (l *countingLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return l.inner.Release(ctx, taskID, workerID, epoch)
}

func (l *countingLease) Verify(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return l.inner.Verify(ctx, taskID, workerID, epoch)
}

func (l *countingLease) renewCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renew
}

func TestStopStartUnderPollKeepsOpenEpoch(t *testing.T) {
	dir := t.TempDir()
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	phases := make([][]*goreplication.BinlogEvent, 4)
	for i := range phases {
		marker := "INSERT INTO poll_epoch VALUES (" + strconv.Itoa(i) + ")"
		ev, _ := chainBinlogEvents(4, eventAt, []namedEvent{
			{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", marker)},
		})
		phases[i] = ev
	}
	store := &pollTaskStore{}
	leases := &countingLease{inner: tasks.NewMemoryLease()}
	syncer := &stopResumeSyncer{phases: phases}
	runner := NewMySQLRunner(dir, WithCheckpointStore(&memCheckpointStore{}))
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000001", Pos: 4},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	scheduler := tasks.NewScheduler(
		tasks.WithDataDir(dir),
		tasks.WithStore(store),
		tasks.WithRunner(runner),
		tasks.WithClusterLeaseManager(leases),
		tasks.WithClusterWorkerID("all-in-one"),
		tasks.WithClusterLease(time.Minute, 20*time.Millisecond, time.Minute),
	)
	task, err := scheduler.CreateTaskFromSpec("poll", "poll-key", &tasks.SourceConfig{
		Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql",
	}, &tasks.StartConfig{Mode: tasks.StartModeLatest}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stopPoll := make(chan struct{})
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		for {
			select {
			case <-stopPoll:
				return
			default:
				_, _ = scheduler.GetTask(task.ID)
				_ = scheduler.ListTasks()
			}
		}
	}()
	t.Cleanup(func() {
		close(stopPoll)
		pollWG.Wait()
	})

	for i := 0; i < len(phases); i++ {
		before := leases.renewCount()
		if err := scheduler.StartTask(task.ID); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		waitTaskState(t, scheduler, task.ID, tasks.StateRunning)
		got, err := scheduler.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != tasks.StateRunning || got.OwnerWorkerID != "all-in-one" || got.Epoch <= 0 {
			t.Fatalf("cycle %d task state=%s owner=%q epoch=%d", i, got.State, got.OwnerWorkerID, got.Epoch)
		}
		if strings.Contains(got.LastError, "SEGMENT_NOT_ON_WORKER") {
			t.Fatalf("cycle %d last_error=%s", i, got.LastError)
		}
		deadline := time.Now().Add(2 * time.Second)
		for leases.renewCount() <= before {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d did not renew the lease", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
		held, err := leases.Verify(context.Background(), task.ID, "all-in-one", got.Epoch)
		if err != nil || !held {
			t.Fatalf("cycle %d lease epoch %d held=%v err=%v", i, got.Epoch, held, err)
		}
		assertOpenEpochFile(t, dir, task.ID, got.Epoch)
		if err := scheduler.StopTask(task.ID); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		waitTaskState(t, scheduler, task.ID, tasks.StateStopped)
	}
}

func assertOpenEpochFile(t *testing.T, dir, taskID string, epoch int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var matches []string
	for {
		var err error
		matches, err = filepath.Glob(filepath.Join(dir, taskID, "mysql-bin.000001*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(matches) != 1 {
		t.Fatalf("segments=%v, want one .open.e file", matches)
	}
	base := filepath.Base(matches[0])
	want := "mysql-bin.000001.open.e" + strconv.FormatInt(epoch, 10)
	if base != want {
		t.Fatalf("segment %s, want %s", base, want)
	}
}
