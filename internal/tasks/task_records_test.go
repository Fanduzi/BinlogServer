// Package tasks provides module-level functionality for tasks.
// input: a standalone data_dir with the encrypted task record (ADR 0010), a recording runner, and leftover segments
// output: #385 as tests: a restart restores every recorded task and Starts the desired-RUN ones with the source password and a start computed from disk (never the record); LATEST with nothing on disk fails RESUME_POSITION_UNKNOWN; a stopped task stays stopped; a worker FAILED survives a restart; delete leaves a tombstone (no TASK_UNADOPTED) and adopt clears it; wrong key, corrupt file and missing key are TASK_RECORDS_FAILED, restore nothing, refuse writes and leave the file as it was; a failed save refuses the action; rotation re-saves on the current key; ids are not reused; an evaluator attached before Restore leaves the previous process's TASK_RECORDS_FAILED open until EvaluateClusterNow
// pos: ADR 0010 (docs/adr/0010-standalone-task-records.md)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/taskrecords"
)

const recordPassword = "rec0rd-s3cret-pw"

// recordRunner records each task it is started with and runs until cancelled.
// hold, when set, keeps Run from returning after cancel so the scheduler's
// exit save cannot land until the test has observed the stop.
type recordRunner struct {
	mu   sync.Mutex
	seen []Task
	ch   chan Task
	hold <-chan struct{}
}

func newRecordRunner() *recordRunner { return &recordRunner{ch: make(chan Task, 16)} }

func (r *recordRunner) Run(ctx context.Context, task Task) error {
	r.mu.Lock()
	r.seen = append(r.seen, task)
	r.mu.Unlock()
	r.ch <- task
	<-ctx.Done()
	if r.hold != nil {
		<-r.hold
	}
	return ctx.Err()
}

func (r *recordRunner) next(t *testing.T) Task {
	t.Helper()
	select {
	case task := <-r.ch:
		return task
	case <-time.After(5 * time.Second):
		t.Fatal("runner was not started")
		return Task{}
	}
}

func (r *recordRunner) none(t *testing.T) {
	t.Helper()
	select {
	case task := <-r.ch:
		t.Fatalf("runner started unexpectedly with task %s", task.ID)
	case <-time.After(200 * time.Millisecond):
	}
}

func recordKey(t *testing.T, fill byte) *taskrecords.Key {
	t.Helper()
	k, err := taskrecords.ParseKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// recordProcess is one standalone process over data: it restores the
// record and resumes, as app.Run does after a restart.
func recordProcess(t *testing.T, data string, file RecordFile, keyErr error, extra ...Option) (*Scheduler, *recordRunner) {
	t.Helper()
	r := newRecordRunner()
	opts := append([]Option{WithDataDir(data), WithRunner(r), WithClusterLeaseManager(NewMemoryLease()), WithClusterWorkerID("standalone"), WithTaskRecords(file, keyErr)}, extra...)
	s := NewScheduler(opts...)
	if err := s.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.ResumeRecordedTasks()
	t.Cleanup(func() {
		s.mu.Lock()
		for _, c := range s.cancels {
			c()
		}
		s.mu.Unlock()
	})
	return s, r
}

// recordDir is t.TempDir for a scheduler data_dir. A cancelled worker exits
// on its own goroutine and may still write .tasks.enc after the test body
// returns, so removal is retried briefly instead of failing on a
// "directory not empty" race (seen in CI on LogsHoldNoSecrets).
func recordDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "task-records-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil || time.Now().After(deadline) {
				if err != nil {
					t.Errorf("remove %s: %v", dir, err)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	return dir
}

func recordSourceCfg() *SourceConfig {
	return &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: recordPassword, Flavor: "mysql", ServerID: 4242}
}

func clusterFinding(t *testing.T, s *Scheduler, subject string) (Finding, bool) {
	t.Helper()
	fs, err := s.ListFindings(context.Background(), ClusterFindingTask, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Kind == FindingTaskRecordsFailed && f.Subject == subject {
			return f, true
		}
	}
	return Finding{}, false
}

func loadRecordSnapshot(t *testing.T, store *taskrecords.Store) recordSnapshot {
	t.Helper()
	plain, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var snap recordSnapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestTaskRecords_RestartResumesFromDiskWithPassword(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	store := taskrecords.Open(data, key, nil)
	s1, r1 := recordProcess(t, data, store, nil)
	task, err := s1.CreateTaskFromSpec("orders", "orders-key", recordSourceCfg(), &StartConfig{Mode: StartModeFilePos, File: "mysql-bin.000001", Pos: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	// The run stored events; then the process is killed (no Stop).
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 1, 2)
	writeDiskSegment(t, filepath.Join(dir, "mysql-bin.000002.open.e3"), 369367)
	want, err := diskResumeStart(data, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want.File != "mysql-bin.000002" || want.Pos != 369367 {
		t.Fatalf("fixture resume point %+v", want)
	}

	raw, _ := os.ReadFile(store.Path())
	if bytes.Contains(raw, []byte(recordPassword)) || bytes.Contains(raw, []byte("orders-key")) {
		t.Fatal("record file holds plaintext")
	}
	if info, _ := os.Stat(store.Path()); info.Mode().Perm() != 0o600 {
		t.Fatalf("record file mode %04o", info.Mode().Perm())
	}

	s2, r2 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	got := r2.next(t)
	if got.ID != task.ID || got.Source.Password != recordPassword || got.Source.Host != "127.0.0.1" || got.Source.ServerID != 4242 {
		t.Fatalf("resumed with %+v", got.Source)
	}
	if got.Start != want {
		t.Fatalf("resume start %+v, want the disk position %+v (never the record's %s:4)", got.Start, want, "mysql-bin.000001")
	}
	if !got.KeepLocalSegments || got.Epoch <= 3 {
		t.Fatalf("resumed run keeps=%v epoch=%d, want adopt semantics above the leftover .open.e3", got.KeepLocalSegments, got.Epoch)
	}
	cur, err := s2.GetTask(task.ID)
	if err != nil || cur.Name != "orders" || cur.ClusterKey != "orders-key" {
		t.Fatalf("restored task %+v %v", cur, err)
	}
	// The record keeps the operator start, not the disk position.
	snap := loadRecordSnapshot(t, taskrecords.Open(data, key, nil))
	if len(snap.Tasks) != 1 || snap.Tasks[0].Start == nil || *snap.Tasks[0].Start != (StartConfig{Mode: StartModeFilePos, File: "mysql-bin.000001", Pos: 4}) {
		t.Fatalf("record start %+v", snap.Tasks[0].Start)
	}
	if snap.Tasks[0].DesiredRun != TaskDesiredRun {
		t.Fatalf("record desired %s", snap.Tasks[0].DesiredRun)
	}
	if _, ok := clusterFinding(t, s2, RecordsWrite); ok {
		t.Fatal("write finding open")
	}
}

// ADR 0010 rule 3 after #393 QA gap (b): no segment means no stored
// history, so LATEST resumes as LATEST, with an event saying so.
func TestTaskRecords_LatestWithNothingOnDiskResumesAtLatest(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	task, err := s1.CreateTaskFromSpec("idle", "idle-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	s2, r2 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	got := r2.next(t)
	if got.ID != task.ID || got.Start.Mode != StartModeLatest || got.KeepLocalSegments {
		t.Fatalf("resumed %+v keep=%v, want a LATEST run", got.Start, got.KeepLocalSegments)
	}
	cur, _ := s2.GetTask(task.ID)
	if cur.State == StateFailed || cur.LastError != "" {
		t.Fatalf("state %s last_error %q", cur.State, cur.LastError)
	}
	evs, err := s2.ListEvents(task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		found = found || e.Type == "TASK_RESUMED_AT_LATEST"
	}
	if !found {
		t.Fatalf("no TASK_RESUMED_AT_LATEST event: %+v", evs)
	}
}

// A directory with history never jumps to a new tip: segments that hold
// events but no resume point stay FAILED even for a LATEST task.
func TestTaskRecords_LatestWithUnreadableSegmentsStaysFailed(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	task, err := s1.CreateTaskFromSpec("hist", "hist-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000001.open.e1"), append([]byte{0xfe, 'b', 'i', 'n'}, bytes.Repeat([]byte{0xab}, 200)...), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, r2 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	r2.none(t)
	if got, _ := s2.GetTask(task.ID); got.State != StateFailed || !strings.HasPrefix(got.LastError, ResumePositionUnknown) {
		t.Fatalf("state %s last_error %q, want FAILED %s", got.State, got.LastError, ResumePositionUnknown)
	}
}

func TestTaskRecords_GTIDStartWithNothingOnDiskResumes(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	start := &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-5"}
	task, err := s1.CreateTaskFromSpec("g", "g-key", recordSourceCfg(), start, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	_, r2 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	if got := r2.next(t); got.Start.Mode != StartModeGTID || got.Start.GTIDSet != start.GTIDSet || got.KeepLocalSegments {
		t.Fatalf("resumed %+v keep=%v", got.Start, got.KeepLocalSegments)
	}
}

func TestTaskRecords_StoppedTaskStaysStopped(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	created, err := s1.CreateTaskFromSpec("c", "c-key", recordSourceCfg(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := s1.CreateTaskFromSpec("s", "s-key", recordSourceCfg(), &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(stopped.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	if err := s1.StopTask(stopped.ID); err != nil {
		t.Fatal(err)
	}
	s2, r2 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	r2.none(t)
	if got, _ := s2.GetTask(created.ID); got.State != StateCreated {
		t.Fatalf("created task restored as %s", got.State)
	}
	if got, _ := s2.GetTask(stopped.ID); got.State != StateStopped || got.Source.Password != recordPassword {
		t.Fatalf("stopped task restored as %s", got.State)
	}
}

func TestTaskRecords_WorkerFailureSurvivesRestart(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	task, err := s1.CreateTaskFromSpec("f", "f-key", recordSourceCfg(), &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	s1.mu.Lock()
	err = s1.markFailedLocked(task.ID, "SOURCE_ACCESS_DENIED: access denied for user repl")
	s1.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s2, r2 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	r2.none(t)
	got, _ := s2.GetTask(task.ID)
	if got.State != StateFailed || !strings.Contains(got.LastError, "SOURCE_ACCESS_DENIED") || got.FailedSpecRevision != got.SpecRevision {
		t.Fatalf("restored %s %q failed=%d spec=%d", got.State, got.LastError, got.FailedSpecRevision, got.SpecRevision)
	}
}

func TestTaskRecords_DeleteLeavesTombstoneNotUnadopted(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	task, err := s1.CreateTaskFromSpec("d", "d-key", recordSourceCfg(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDiskSegment(t, filepath.Join(dir, "mysql-bin.000001.open.e1"), 1200)
	if err := s1.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mysql-bin.000001.open.e1")); err != nil {
		t.Fatal("delete removed backup bytes")
	}
	s2, _ := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	hs := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s2, hs, EvaluatorOn)
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, hs, task.ID)[unadoptedKey]; ok {
		t.Fatal("a deliberately deleted task's directory opened TASK_UNADOPTED")
	}
	listed := false
	for _, it := range s2.ListTasks() {
		if it.ID == task.ID {
			listed = true
		}
	}
	if !listed {
		t.Fatal("tombstoned directory no longer listed read-only")
	}
	// A new task does not reuse the deleted id.
	next, err := s2.CreateTaskFromSpec("n", "n-key", recordSourceCfg(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == task.ID {
		t.Fatal("deleted id reused")
	}
	// Adopt clears the tombstone and is itself recorded.
	if _, err := s2.AdoptDiskBackup(task.ID, TaskPatch{ClusterKey: "d-again", Source: recordSourceCfg()}); err != nil {
		t.Fatal(err)
	}
	snap := loadRecordSnapshot(t, taskrecords.Open(data, key, nil))
	if len(snap.Deleted) != 0 {
		t.Fatalf("tombstone kept after adopt: %+v", snap.Deleted)
	}
	for _, rec := range snap.Tasks {
		if rec.ID == task.ID && (!rec.Adopted || rec.Start != nil) {
			t.Fatalf("adopted record %+v: the disk start must not be recorded", rec)
		}
	}
}

func TestTaskRecords_IDsNotReusedAfterDirectoryRemoved(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	a, _ := s1.CreateTaskFromSpec("a", "a-key", recordSourceCfg(), nil, nil)
	if err := s1.DeleteTask(a.ID); err != nil {
		t.Fatal(err)
	}
	s2, _ := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	b, err := s2.CreateTaskFromSpec("b", "b-key", recordSourceCfg(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == a.ID {
		t.Fatalf("id %s reused", b.ID)
	}
}

// lockedCases leave a record file that this process cannot use.
func TestTaskRecords_UnusableFileIsLoudAndUntouched(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		open    func(t *testing.T, data string) (RecordFile, error)
		mutate  func(t *testing.T, path string)
	}{
		{"wrong key", RecordsWrongKey, func(t *testing.T, data string) (RecordFile, error) {
			return taskrecords.Open(data, recordKey(t, 9), nil), nil
		}, nil},
		{"corrupt", RecordsCorrupt, func(t *testing.T, data string) (RecordFile, error) {
			return taskrecords.Open(data, recordKey(t, 1), nil), nil
		}, func(t *testing.T, path string) {
			raw, _ := os.ReadFile(path)
			i := bytes.LastIndex(raw, []byte(`"ciphertext": "`)) + len(`"ciphertext": "`) + 10
			if raw[i] == 'A' {
				raw[i] = 'B'
			} else {
				raw[i] = 'A'
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing key", RecordsKeyMissing, func(t *testing.T, data string) (RecordFile, error) {
			return taskrecords.Open(data, nil, nil), taskrecords.ErrKeyMissing
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := recordDir(t)
			key := recordKey(t, 1)
			s1, r1 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
			task, err := s1.CreateTaskFromSpec("w", "w-key", recordSourceCfg(), &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s1.StartTask(task.ID); err != nil {
				t.Fatal(err)
			}
			r1.next(t)
			dir := filepath.Join(data, task.ID)
			_ = os.MkdirAll(dir, 0o755)
			writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001.open.e1"), 1, 2)
			path := filepath.Join(data, taskrecords.FileName)
			if tc.mutate != nil {
				tc.mutate(t, path)
			}
			before, _ := os.ReadFile(path)

			file, keyErr := tc.open(t, data)
			hs := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
			s2, r2 := recordProcess(t, data, file, keyErr)
			r2.none(t)
			f, ok := clusterFinding(t, s2, tc.subject)
			if !ok {
				fs, _ := s2.ListFindings(context.Background(), ClusterFindingTask, false, 100)
				t.Fatalf("no TASK_RECORDS_FAILED/%s: %+v", tc.subject, fs)
			}
			if f.Severity != FindingCritical || !strings.Contains(f.Detail, taskrecords.FileName) {
				t.Fatalf("finding %+v", f)
			}
			if strings.Contains(f.Detail, recordPassword) {
				t.Fatal("finding detail holds the password")
			}
			// Nothing restored: the directory is unadopted, loudly.
			e := newEval(t, s2, hs, EvaluatorOn)
			if err := e.SweepNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := openOf(t, hs, task.ID)[unadoptedKey]; !ok {
				t.Fatalf("directory of an unrestored task is not TASK_UNADOPTED: %+v", openOf(t, hs, task.ID))
			}
			// Writes are refused so the file is not overwritten.
			if _, err := s2.CreateTaskFromSpec("x", "x-key", recordSourceCfg(), nil, nil); !errors.Is(err, ErrTransitionNotRecorded) || !errors.Is(err, ErrTaskRecordsLocked) {
				t.Fatalf("create while locked: %v", err)
			}
			if _, err := s2.AdoptDiskBackup(task.ID, TaskPatch{ClusterKey: "w2", Source: recordSourceCfg(), Start: &StartConfig{Mode: StartModeFilePos, File: "mysql-bin.000001", Pos: 4}}); !errors.Is(err, ErrTaskRecordsLocked) {
				t.Fatalf("adopt while locked: %v", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("an unusable record file was changed")
			}
			if len(s2.ListTasks()) != 1 {
				t.Fatalf("tasks after a refused create: %+v", s2.ListTasks())
			}
			// With the right key the next process restores and resumes.
			if tc.mutate == nil {
				_, r3 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
				if got := r3.next(t); got.ID != task.ID || got.Source.Password != recordPassword {
					t.Fatalf("resume with the right key: %+v", got)
				}
			}
		})
	}
}

func TestTaskRecords_MissingKeyWithoutFileIsVolatileAndLoud(t *testing.T) {
	data := recordDir(t)
	s, _ := recordProcess(t, data, taskrecords.Open(data, nil, nil), taskrecords.ErrKeyMissing)
	if _, ok := clusterFinding(t, s, RecordsKeyMissing); !ok {
		t.Fatal("no key and no file: no finding")
	}
	if _, err := s.CreateTaskFromSpec("v", "v-key", recordSourceCfg(), nil, nil); err != nil {
		t.Fatalf("create without a key and without a file must still work (memory only): %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, taskrecords.FileName)); !os.IsNotExist(err) {
		t.Fatal("a record file was written without a key")
	}
}

func TestTaskRecords_InvalidKeyIsKeyInvalid(t *testing.T) {
	data := recordDir(t)
	keyErr := errors.Join(taskrecords.ErrKeyInvalid, errors.New("key file mode 0644"))
	s, _ := recordProcess(t, data, taskrecords.Open(data, nil, nil), keyErr)
	if _, ok := clusterFinding(t, s, RecordsKeyInvalid); !ok {
		t.Fatal("invalid key: no key_invalid finding")
	}
}

// failingRecordFile fails Save while fail is set. A run's exit save can
// land after StopTask returns, so the flag is shared across goroutines.
type failingRecordFile struct {
	*taskrecords.Store
	fail atomic.Bool
}

func newFailingRecordFile(store *taskrecords.Store, fail bool) *failingRecordFile {
	f := &failingRecordFile{Store: store}
	f.fail.Store(fail)
	return f
}

func (f *failingRecordFile) Save(plain []byte) error {
	if f.fail.Load() {
		return errors.New("no space left on device")
	}
	return f.Store.Save(plain)
}

func TestTaskRecords_FailedSaveRefusesCreate(t *testing.T) {
	data := recordDir(t)
	file := newFailingRecordFile(taskrecords.Open(data, recordKey(t, 1), nil), false)
	s, _ := recordProcess(t, data, file, nil)
	if _, err := s.CreateTaskFromSpec("ok", "ok-key", recordSourceCfg(), nil, nil); err != nil {
		t.Fatal(err)
	}
	file.fail.Store(true)
	_, err := s.CreateTaskFromSpec("no", "no-key", recordSourceCfg(), nil, nil)
	if !errors.Is(err, ErrTransitionNotRecorded) || !errors.Is(err, ErrTaskRecordsNotSaved) {
		t.Fatalf("create with a failing save: %v", err)
	}
	if strings.Contains(err.Error(), recordPassword) {
		t.Fatal("refusal text holds the source password")
	}
	if len(s.ListTasks()) != 1 {
		t.Fatalf("memory changed by a refused create: %+v", s.ListTasks())
	}
	f, ok := clusterFinding(t, s, RecordsWrite)
	if !ok || f.Severity != FindingCritical {
		t.Fatal("failed save: no TASK_RECORDS_FAILED/write")
	}
	// The detail says how stale the file is (#393 QA P3).
	if !strings.Contains(f.Detail, "seq 1") || !strings.Contains(f.Detail, "saved_at") {
		t.Fatalf("write detail lacks the last good seq/saved_at: %q", f.Detail)
	}
	file.fail.Store(false)
	if _, err := s.CreateTaskFromSpec("again", "again-key", recordSourceCfg(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := clusterFinding(t, s, RecordsWrite); ok {
		t.Fatal("write finding still open after a successful save")
	}
}

// #393 QA P2-1: a full data_dir must never block Stop or Delete.
func TestTaskRecords_StopAndDeleteApplyWhenSaveFails(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	file := newFailingRecordFile(taskrecords.Open(data, key, nil), false)
	s, r := recordProcess(t, data, file, nil)
	run, err := s.CreateTaskFromSpec("run", "run-key", recordSourceCfg(), &StartConfig{Mode: StartModeFilePos, File: "mysql-bin.000001", Pos: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.CreateTaskFromSpec("gone", "gone-key", recordSourceCfg(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The runner's exit save runs after StopTask returns and, while the save
	// is still failing, replaces the stop detail. Hold Run so that save
	// cannot land between the flip and this assertion.
	hold := make(chan struct{})
	r.hold = hold
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	if err := s.StartTask(run.ID); err != nil {
		t.Fatal(err)
	}
	r.next(t)
	file.fail.Store(true)
	if err := s.StopTask(run.ID); err != nil {
		t.Fatalf("stop refused by a failing save: %v", err)
	}
	if got, _ := s.GetTask(run.ID); got.DesiredRun != TaskDesiredStop {
		t.Fatalf("stop not applied in memory: desired %s state %s", got.DesiredRun, got.State)
	}
	f, ok := clusterFinding(t, s, RecordsWrite)
	if !ok || !strings.Contains(f.Detail, "task "+run.ID) || !strings.Contains(f.Detail, "restart") || !strings.Contains(f.Detail, "resume") {
		t.Fatalf("write detail does not say a restart resumes task %s: %+v %v", run.ID, f, ok)
	}
	release()
	waitRunClosed(t, s, run.ID, "stop with a failing record save")
	if err := s.DeleteTask(gone.ID); err != nil {
		t.Fatalf("delete refused by a failing save: %v", err)
	}
	if _, err := s.GetTask(gone.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("delete not applied in memory: %v", err)
	}
	// Still failing: the retry keeps the finding open.
	s.RetryRecordSave()
	if _, ok := clusterFinding(t, s, RecordsWrite); !ok {
		t.Fatal("write finding closed while the save still fails")
	}
	file.fail.Store(false)
	s.RetryRecordSave()
	if _, ok := clusterFinding(t, s, RecordsWrite); ok {
		t.Fatal("write finding still open after a successful retry")
	}
	snap := loadRecordSnapshot(t, taskrecords.Open(data, key, nil))
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != run.ID || snap.Tasks[0].DesiredRun != TaskDesiredStop {
		t.Fatalf("retried snapshot tasks %+v", snap.Tasks)
	}
	if len(snap.Deleted) != 1 || snap.Deleted[0].ID != gone.ID {
		t.Fatalf("retried snapshot tombstones %+v", snap.Deleted)
	}
}

func TestTaskRecords_RotationResavesOnCurrentKey(t *testing.T) {
	data := recordDir(t)
	old, cur := recordKey(t, 1), recordKey(t, 2)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, old, nil), nil)
	task, err := s1.CreateTaskFromSpec("r", "r-key", recordSourceCfg(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := recordProcess(t, data, taskrecords.Open(data, cur, old), nil)
	if got, err := s2.GetTask(task.ID); err != nil || got.Source.Password != recordPassword {
		t.Fatalf("restore on the previous key: %+v %v", got, err)
	}
	if _, info, err := taskrecords.Open(data, cur, nil).Load(); err != nil || info.KeyID != cur.ID() {
		t.Fatalf("file not moved to the current key: %+v %v", info, err)
	}
}

// #393 QA P3: a rotation whose re-save fails keeps running on the old
// file, says so, and the retry moves it to the current key later.
func TestTaskRecords_RotationResaveFailureKeepsOldFile(t *testing.T) {
	data := recordDir(t)
	old, cur := recordKey(t, 1), recordKey(t, 2)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, old, nil), nil)
	task, err := s1.CreateTaskFromSpec("r", "r-key", recordSourceCfg(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(data, taskrecords.FileName))
	file := newFailingRecordFile(taskrecords.Open(data, cur, old), true)
	s2, _ := recordProcess(t, data, file, nil)
	if got, err := s2.GetTask(task.ID); err != nil || got.Source.Password != recordPassword {
		t.Fatalf("restore on the previous key with a failing re-save: %+v %v", got, err)
	}
	after, _ := os.ReadFile(filepath.Join(data, taskrecords.FileName))
	if !bytes.Equal(before, after) {
		t.Fatal("failed rotation changed the file")
	}
	f, ok := clusterFinding(t, s2, RecordsWrite)
	if !ok || !strings.Contains(f.Detail, old.ID()) {
		t.Fatalf("failed rotation: write finding %+v %v, want the previous key id", f, ok)
	}
	file.fail.Store(false)
	s2.RetryRecordSave()
	if _, info, err := taskrecords.Open(data, cur, nil).Load(); err != nil || info.KeyID != cur.ID() {
		t.Fatalf("retry did not move the file to the current key: %+v %v", info, err)
	}
}

// #393 QA P2-3: a GTID task resumes with the executed set built from the
// stored bytes (seed, sealed segments, open tail), not an empty one.
func TestTaskRecords_GTIDTaskResumesWithStoredSet(t *testing.T) {
	data := recordDir(t)
	key := recordKey(t, 1)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	task, err := s1.CreateTaskFromSpec("g", "g-key", recordSourceCfg(), &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-36"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeDiskSegment(t, filepath.Join(dir, "mysql-bin.000002.open.e3"), 369367)
	_, r2 := recordProcess(t, data, taskrecords.Open(data, key, nil), nil)
	got := r2.next(t)
	if got.Start.Mode != StartModeFilePos || got.Start.File != "mysql-bin.000002" || got.Start.Pos != 369367 {
		t.Fatalf("position must still come from disk: %+v", got.Start)
	}
	if got.Start.GTIDSet != gtidPITRUUID+":1-38" {
		t.Fatalf("resumed GTID set %q, want seed + sealed %s:1-38", got.Start.GTIDSet, gtidPITRUUID)
	}
}

func TestTaskRecords_OffIsTodaysBehaviour(t *testing.T) {
	data := recordDir(t)
	s := NewScheduler(WithDataDir(data))
	if _, err := s.CreateTaskFromSpec("o", "o-key", recordSourceCfg(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, taskrecords.FileName)); !os.IsNotExist(err) {
		t.Fatal("records off wrote a file")
	}
}

func TestTaskRecords_LogsHoldNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	data := recordDir(t)
	old, cur := recordKey(t, 1), recordKey(t, 2)
	s1, r1 := recordProcess(t, data, taskrecords.Open(data, old, nil), nil)
	task, err := s1.CreateTaskFromSpec("l", "l-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	r1.next(t)
	recordProcess(t, data, taskrecords.Open(data, recordKey(t, 9), nil), nil) // wrong key
	s3, r3 := recordProcess(t, data, taskrecords.Open(data, cur, old), nil)   // rotation + LATEST resume
	r3.next(t)
	// A refused write and a Stop applied without a save: their error text,
	// finding detail and log lines (#393 QA P3).
	fail := newFailingRecordFile(taskrecords.Open(recordDir(t), cur, nil), true)
	s4, _ := recordProcess(t, recordDir(t), fail, nil)
	_, refused := s4.CreateTaskFromSpec("x", "x-key", recordSourceCfg(), nil, nil)
	if refused == nil {
		t.Fatal("create with a failing save was accepted")
	}
	if err := s3.StopTask(task.ID); err != nil {
		t.Fatal(err)
	}
	f, _ := clusterFinding(t, s4, RecordsWrite)
	out := buf.String() + "\n" + refused.Error() + "\n" + f.Detail
	if !strings.Contains(out, "rotated") || !strings.Contains(out, "resuming at LATEST") || !strings.Contains(out, "key id "+cur.ID()) {
		t.Fatalf("expected rotation, the key-id line and the LATEST resume in logs:\n%s", out)
	}
	for _, secret := range []string{recordPassword, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))} {
		if strings.Contains(out, secret) {
			t.Fatalf("logs contain a secret %q", secret)
		}
	}
}

// Restore runs before app builds the evaluator; a lock raised then must still
// reach the evaluator's signal book (found by the e2e wrong-key case).
func TestTaskRecords_LockReachesLaterEvaluator(t *testing.T) {
	data := recordDir(t)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, recordKey(t, 1), nil), nil)
	if _, err := s1.CreateTaskFromSpec("e", "e-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	s2, _ := recordProcess(t, data, taskrecords.Open(data, recordKey(t, 2), nil), nil)
	e := NewEvaluator(s2, EvaluatorConfig{Mode: EvaluatorOn, NodeID: "n", Instance: "i", Debounce: time.Millisecond})
	e.mu.Lock()
	en := e.signals[ClusterFindingTask][health.SignalKey{Kind: health.Kind(FindingTaskRecordsFailed), Subject: RecordsWrongKey}]
	e.mu.Unlock()
	if en == nil || en.st.FailureSeq == 0 {
		t.Fatalf("evaluator built after Restore has no %s/%s signal", FindingTaskRecordsFailed, RecordsWrongKey)
	}
}

func TestTaskRecords_WrongKeyOpensClusterFinding(t *testing.T) {
	data := recordDir(t)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, recordKey(t, 1), nil), nil)
	if _, err := s1.CreateTaskFromSpec("w", "w-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	s2 := NewScheduler(WithDataDir(data), WithRunner(newRecordRunner()), WithClusterLeaseManager(NewMemoryLease()),
		WithClusterWorkerID("standalone"), WithTaskRecords(taskrecords.Open(data, recordKey(t, 2), nil), nil))
	e := NewEvaluator(s2, EvaluatorConfig{Mode: EvaluatorOn, Store: store, NodeID: "n", Instance: "i", Debounce: time.Millisecond})
	if err := s2.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, ClusterFindingTask)[string(FindingTaskRecordsFailed)+"/"+RecordsWrongKey]; !ok {
		t.Fatalf("no TASK_RECORDS_FAILED/wrong_key: %+v", openOf(t, store, ClusterFindingTask))
	}
	// GET /api/findings lists _cluster in standalone (found by the e2e).
	all, err := s2.OpenFindings(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range all {
		found = found || (f.TaskID == ClusterFindingTask && f.Kind == FindingTaskRecordsFailed && f.Subject == RecordsWrongKey)
	}
	if !found {
		t.Fatalf("OpenFindings has no _cluster TASK_RECORDS_FAILED/wrong_key: %+v", all)
	}
}

// A row an earlier process left open closes once the right key reads the file.
func TestTaskRecords_RightKeyClosesEarlierLockFinding(t *testing.T) {
	data := recordDir(t)
	good := recordKey(t, 1)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, good, nil), nil)
	if _, err := s1.CreateTaskFromSpec("c", "c-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	boot := func(key *taskrecords.Key) *Evaluator {
		s := NewScheduler(WithDataDir(data), WithRunner(newRecordRunner()), WithClusterLeaseManager(NewMemoryLease()),
			WithClusterWorkerID("standalone"), WithTaskRecords(taskrecords.Open(data, key, nil), nil))
		if err := s.Restore(context.Background()); err != nil { // before the evaluator, as app does without one yet
			t.Fatal(err)
		}
		e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: store, NodeID: "n", Instance: "i", Debounce: time.Millisecond})
		if _, err := e.EvaluateNow(context.Background(), ClusterFindingTask); err != nil {
			t.Fatal(err)
		}
		return e
	}
	boot(recordKey(t, 2))
	if _, ok := openOf(t, store, ClusterFindingTask)[string(FindingTaskRecordsFailed)+"/"+RecordsWrongKey]; !ok {
		t.Fatalf("wrong key did not open: %+v", openOf(t, store, ClusterFindingTask))
	}
	boot(good)
	if open := openOf(t, store, ClusterFindingTask); len(open) != 0 {
		t.Fatalf("right key left findings open: %+v", open)
	}
}

// App attaches the evaluator before Restore. The earlier process's row stays
// open until EvaluateClusterNow, which is what app.Run calls before listen.
func TestTaskRecords_EvaluateClusterNowClosesEarlierLock(t *testing.T) {
	data := recordDir(t)
	good := recordKey(t, 1)
	s1, _ := recordProcess(t, data, taskrecords.Open(data, good, nil), nil)
	if _, err := s1.CreateTaskFromSpec("n", "n-key", recordSourceCfg(), &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	cfg := EvaluatorConfig{Mode: EvaluatorOn, Store: store, NodeID: "n", Instance: "i", Debounce: time.Second}
	boot := func(key *taskrecords.Key) *Scheduler {
		s := NewScheduler(WithDataDir(data), WithRunner(newRecordRunner()), WithClusterLeaseManager(NewMemoryLease()),
			WithClusterWorkerID("standalone"), WithTaskRecords(taskrecords.Open(data, key, nil), nil))
		NewEvaluator(s, cfg) // before Restore, as app.Run does
		if err := s.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	}
	wrong := boot(recordKey(t, 2))
	if err := wrong.EvaluateClusterNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := string(FindingTaskRecordsFailed) + "/" + RecordsWrongKey
	if _, ok := openOf(t, store, ClusterFindingTask)[key]; !ok {
		t.Fatalf("wrong key did not open: %+v", openOf(t, store, ClusterFindingTask))
	}
	right := boot(good)
	if _, ok := openOf(t, store, ClusterFindingTask)[key]; !ok {
		t.Fatal("right key closed the earlier finding before EvaluateClusterNow")
	}
	if err := right.EvaluateClusterNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if open := openOf(t, store, ClusterFindingTask); len(open) != 0 {
		t.Fatalf("EvaluateClusterNow left findings open: %+v", open)
	}
	listed, err := right.OpenFindings(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range listed {
		if f.Kind == FindingTaskRecordsFailed {
			t.Fatalf("OpenFindings still has %s/%s", f.Kind, f.Subject)
		}
	}
}
