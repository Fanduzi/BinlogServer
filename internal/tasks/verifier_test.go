// Package tasks provides module-level functionality for tasks.
// input: a temp data_dir, a fake catalog, and the verifier worker driven pass by pass
// output: proof that a forced segment is read once per period, that LastVerified stops the next read, that LOCAL_UNREADABLE escalates and stays CRITICAL when that severity is read back, that both byte caps stop a read, that an Allows error skips the task, that .verify.json is saved and loaded, that an object subject round-trips through the file store and closes, that gone segments and tasks leave the progress file, that a deadline keeps the tasks already checked and returns within its own timeout, that a clean second pass does not rewrite .verify.json, that a failed rename leaves no temp and is retried, that a cancelled pass does not mark the task verified and keeps partial progress, that pass_deadline_sec 0 uses every_sec, that an empty scheduler drops stale tasks, that a startup HEAD tells a missing key from auth, a missing bucket, and a network error, and that SealedAt reaches the clock-skew check
// pos: worker feedback tests for ADR 0009 phase 3 (docs/design/0009-phase3-verifier.md §10)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/health"
)

func newTestVerifier(t *testing.T, objects assurance.ObjectStore) (dir, id string, files *fakeFileStore, s *Scheduler, v *Verifier) {
	t.Helper()
	dir = t.TempDir()
	files = newFakeFileStore()
	s = NewScheduler(WithDataDir(dir), WithFileStore(files), WithFindingStore(NewFileFindingStore(filepath.Join(dir, FindingFileName))))
	task, err := s.CreateTask("verify", "verify-key")
	if err != nil {
		t.Fatal(err)
	}
	id = task.ID
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	v = NewVerifier(s, VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1, Objects: objects})
	s.SetVerifier(v)
	return dir, id, files, s, v
}

func putSealed(files *fakeFileStore, id, name, dir string, row BinlogFile) {
	row.TaskID = id
	row.FileName = name
	if row.FilePath == "" {
		row.FilePath = filepath.Join(dir, id, name)
	}
	if row.State == "" {
		row.State = "SEALED"
	}
	files.files[id] = append(files.files[id], row)
}

func openKind(s *Scheduler, id, kind string) (Finding, bool) {
	for _, f := range s.openBookFindings(id) {
		if string(f.Kind) == kind {
			return f, true
		}
	}
	return Finding{}, false
}

func flushFindings(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.StartFindingSink(ctx, time.Hour)
	cancel()
	if !s.WaitFindingSink(5 * time.Second) {
		t.Fatal("finding sink did not flush")
	}
}

// TestVerifier_ForcedSegmentOncePerPeriod: a segment an open finding names
// is parsed once per period when Report.Read is written back, and again in
// the next period.
func TestVerifier_ForcedSegmentOncePerPeriod(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	name := "mysql-bin.000001"
	// Magic plus a zero event size: a sealed segment that does not parse, so
	// the open VERIFY_FAILED forces the next reads. Bytes that are not the
	// binlog magic scan as an empty log and open nothing.
	body := make([]byte, 64)
	copy(body[:4], []byte{0xfe, 'b', 'i', 'n'})
	if err := os.WriteFile(filepath.Join(dir, id, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: 64, UploadState: "PENDING", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	if m := s.VerifyMetrics().Tasks[id]; m.LocalBytes != 64 {
		t.Fatalf("first pass local bytes %d", m.LocalBytes)
	}
	if _, ok := openKind(s, id, assurance.KindVerifyFailed); !ok {
		t.Fatal("parse failure did not open VERIFY_FAILED")
	}
	v.Pass(context.Background(), now.Add(time.Hour))
	if m := s.VerifyMetrics().Tasks[id]; m.LocalBytes != 64 {
		t.Fatalf("same period read the segment again: %d bytes", m.LocalBytes)
	}
	v.Pass(context.Background(), now.Add(25*time.Hour))
	if m := s.VerifyMetrics().Tasks[id]; m.LocalBytes != 128 {
		t.Fatalf("next period local bytes %d, want 128", m.LocalBytes)
	}
}

// TestVerifier_LocalUnreadableFeedsSeverityAndDetail: two consecutive
// unreadable passes go WARNING then CRITICAL, the first clean read is
// recovering, and the second closes. The recovering detail is what the
// second clean read sees, including after it has been through the file store.
func TestVerifier_LocalUnreadableFeedsSeverityAndDetail(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	taskDir := filepath.Join(dir, id)
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002"} {
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		putSealed(files, id, name, dir, BinlogFile{SizeBytes: 1, UploadState: "PENDING", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	}
	if err := os.Chmod(taskDir, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(taskDir, 0o755) })
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	got, ok := openKind(s, id, assurance.KindLocalUnreadable)
	if !ok || got.Severity != FindingWarning || got.Subject != "" || got.Detail != assurance.DetailUnreadable {
		t.Fatalf("first pass: %+v %v", got, ok)
	}
	v.Pass(context.Background(), now.Add(time.Hour))
	got, ok = openKind(s, id, assurance.KindLocalUnreadable)
	if !ok || got.Severity != FindingCritical || got.Detail != assurance.DetailUnreadable {
		t.Fatalf("second pass: %+v %v", got, ok)
	}
	if err := os.Chmod(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	v.Pass(context.Background(), now.Add(2*time.Hour))
	got, ok = openKind(s, id, assurance.KindLocalUnreadable)
	if !ok || got.Detail != assurance.DetailUnreadableRecovering || got.Severity != FindingCritical {
		t.Fatalf("first clean pass: %+v %v", got, ok)
	}
	flushFindings(t, s)
	s2 := NewScheduler(WithDataDir(dir), WithFileStore(files), WithFindingStore(NewFileFindingStore(filepath.Join(dir, FindingFileName))))
	// The leftover {data_dir}/{id} already owns this id, so a new CreateTask
	// would allocate the next one. The restarted process keeps the same row.
	prev, err := s.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	s2.mu.Lock()
	s2.tasks[id] = prev
	s2.mu.Unlock()
	if err := s2.LoadOpenFindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, ok := openKind(s2, id, assurance.KindLocalUnreadable)
	if !ok || loaded.Severity != FindingCritical || loaded.Detail != assurance.DetailUnreadableRecovering {
		t.Fatalf("file store dropped severity or detail: %+v %v", loaded, ok)
	}
	v2 := NewVerifier(s2, VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
	s2.SetVerifier(v2)
	v2.Pass(context.Background(), now.Add(3*time.Hour))
	if _, ok := openKind(s2, id, assurance.KindLocalUnreadable); ok {
		t.Fatalf("still open after the second clean read: %+v", s2.openBookFindings(id))
	}
}

// TestVerifier_SealedAtFeedsClockSkew: a future sealed_at on the catalog
// row is UPLOAD_CLOCK_SKEW. A zero SealedAt would not be.
func TestVerifier_SealedAtFeedsClockSkew(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	name := "mysql-bin.000003"
	if err := os.WriteFile(filepath.Join(dir, id, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: 1, UploadState: "PENDING", SealedAt: now.Add(72 * time.Hour)})
	v.Pass(context.Background(), now)
	if _, ok := openKind(s, id, assurance.KindUploadClockSkew); !ok {
		t.Fatalf("no clock skew: %+v", s.openBookFindings(id))
	}
}

func copyLedger(t *testing.T, dir, id, name string) int64 {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "binlog", "testdata", "ledger", "live", "mysql-bin.000003"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return int64(len(body))
}

func verifierCfg(dir string, extra VerifierConfig) VerifierConfig {
	extra.DataDir = dir
	if extra.BudgetBytesPerSec == 0 {
		extra.BudgetBytesPerSec = -1
	}
	if extra.RotationPeriods == 0 {
		extra.RotationPeriods = -1
	}
	return extra
}

type scriptObjects struct {
	obj         map[string][]byte
	blockOpen   bool
	blockHead   bool
	headStarted chan struct{}
}

func (s *scriptObjects) Head(ctx context.Context, key string) (int64, error) {
	if s.blockHead {
		if s.headStarted != nil {
			select {
			case <-s.headStarted:
			default:
				close(s.headStarted)
			}
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	b, ok := s.obj[key]
	if !ok {
		return 0, os.ErrNotExist
	}
	return int64(len(b)), nil
}

func (s *scriptObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.blockOpen {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b, ok := s.obj[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func readVerifyState(t *testing.T, dir string) verifyStateFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, VerifyStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var st verifyStateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func restartVerifier(t *testing.T, dir string, files *fakeFileStore, prev *Scheduler, cfg VerifierConfig) (*Scheduler, *Verifier) {
	t.Helper()
	flushFindings(t, prev)
	s := NewScheduler(WithDataDir(dir), WithFileStore(files), WithFindingStore(NewFileFindingStore(filepath.Join(dir, FindingFileName))))
	prev.mu.Lock()
	copied := make(map[string]Task, len(prev.tasks))
	for id, task := range prev.tasks {
		copied[id] = task
	}
	prev.mu.Unlock()
	s.mu.Lock()
	for id, task := range copied {
		s.tasks[id] = task
	}
	s.mu.Unlock()
	if err := s.LoadOpenFindings(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(s, cfg)
	s.SetVerifier(v)
	return s, v
}

func localBytes(s *Scheduler, id string) int64 {
	return s.VerifyMetrics().Tasks[id].LocalBytes
}

func TestVerifier_AllowsFalseSkips(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	putSealed(files, id, "mysql-bin.000001", dir, BinlogFile{UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	v.cfg.Allows = func(context.Context, string) (bool, error) { return false, nil }
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	if got := s.openBookFindings(id); len(got) != 0 {
		t.Fatalf("skipped task opened %+v", got)
	}
	if _, ok := s.VerifyMetrics().Tasks[id]; ok {
		t.Fatal("skipped task updated metrics")
	}
}

// TestVerifier_LastVerifiedStopsTheNextRead is W1: Report.VerifiedAt has to
// become Options.LastVerified or the next pass reads the segment again.
func TestVerifier_LastVerifiedStopsTheNextRead(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	if localBytes(s, id) != size {
		t.Fatalf("first pass read %d, want %d", localBytes(s, id), size)
	}
	v.Pass(context.Background(), now.Add(time.Hour))
	if localBytes(s, id) != size {
		t.Fatalf("second pass read %d, want the first pass only (%d)", localBytes(s, id), size)
	}
}

// TestVerifier_CriticalSeverityReadBack is W4: CRITICAL has to come back
// from the file store into the next pass, or a later failure drops to WARNING.
func TestVerifier_CriticalSeverityReadBack(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	taskDir := filepath.Join(dir, id)
	for _, name := range []string{"mysql-bin.000001", "mysql-bin.000002"} {
		if err := os.WriteFile(filepath.Join(taskDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		putSealed(files, id, name, dir, BinlogFile{SizeBytes: 1, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	}
	if err := os.Chmod(taskDir, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(taskDir, 0o755) })
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	v.Pass(context.Background(), now.Add(time.Hour))
	if err := os.Chmod(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	v.Pass(context.Background(), now.Add(2*time.Hour))
	s, v = restartVerifier(t, dir, files, s, v.cfg)
	got, ok := openKind(s, id, assurance.KindLocalUnreadable)
	if !ok || got.Severity != FindingCritical || got.Detail != assurance.DetailUnreadableRecovering {
		t.Fatalf("file store: %+v %v", got, ok)
	}
	if err := os.Chmod(taskDir, 0o444); err != nil {
		t.Fatal(err)
	}
	v.Pass(context.Background(), now.Add(3*time.Hour))
	got, ok = openKind(s, id, assurance.KindLocalUnreadable)
	if !ok || got.Severity != FindingCritical {
		t.Fatalf("severity was not read back: %+v %v", got, ok)
	}
}

// TestVerifier_BudgetBytesPerSecLimitsReads is W6.
func TestVerifier_BudgetBytesPerSecLimitsReads(t *testing.T) {
	dir, id, files, s, _ := newTestVerifier(t, nil)
	v := NewVerifier(s, verifierCfg(dir, VerifierConfig{BudgetBytesPerSec: 1, PassDeadline: time.Second}))
	s.SetVerifier(v)
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	if got := localBytes(s, id); got != 0 {
		t.Fatalf("budget let %d bytes through a 1 B/s pass", got)
	}
}

// TestVerifier_PassMaxBytesLimitsReads is W7.
func TestVerifier_PassMaxBytesLimitsReads(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{PassMaxBytes: 100})
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	if got := localBytes(s, id); got != 0 {
		t.Fatalf("pass_max_bytes let %d bytes through a 100 byte pass", got)
	}
}

// TestVerifier_AllowsErrorIsNotAllowed is W8.
func TestVerifier_AllowsErrorIsNotAllowed(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	putSealed(files, id, "mysql-bin.000001", dir, BinlogFile{UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	v.cfg.Allows = func(context.Context, string) (bool, error) { return true, errors.New("ownership unknown") }
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	if got := s.openBookFindings(id); len(got) != 0 {
		t.Fatalf("Allows error opened %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, VerifyStateFileName)); !os.IsNotExist(err) {
		t.Fatalf(".verify.json err=%v", err)
	}
}

// TestVerifier_ProgressSurvivesRestart is W10.
func TestVerifier_ProgressSurvivesRestart(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	st := readVerifyState(t, dir)
	if _, ok := st.Tasks[id].LastVerified[name]; !ok {
		t.Fatalf("saved progress %+v", st)
	}
	s2, v2 := restartVerifier(t, dir, files, s, v.cfg)
	v2.Pass(context.Background(), now.Add(time.Hour))
	if got := localBytes(s2, id); got != 0 {
		t.Fatalf("restart read %d bytes; progress was not loaded", got)
	}
}

// TestVerifier_ObjectSubjectRoundTripsAndCloses is W13.
func TestVerifier_ObjectSubjectRoundTripsAndCloses(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}}
	dir, id, files, s, v := newTestVerifier(t, objs)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	v.cfg.Objects = objs
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	key := "bucket/" + name
	body, err := os.ReadFile(filepath.Join(dir, id, name))
	if err != nil {
		t.Fatal(err)
	}
	putSealed(files, id, name, dir, BinlogFile{
		SizeBytes: size, UploadState: "UPLOADED", ObjectKey: key, State: "SEALED",
		SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), UploadedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	got, ok := openKind(s, id, assurance.KindObjectMissing)
	if !ok || got.Subject != health.ObjectSubject(name) {
		t.Fatalf("stored subject %+v %v", got, ok)
	}
	s, v = restartVerifier(t, dir, files, s, v.cfg)
	v.cfg.Objects = objs
	v.Pass(context.Background(), now.Add(time.Hour))
	got, ok = openKind(s, id, assurance.KindObjectMissing)
	if !ok || got.Subject != health.ObjectSubject(name) {
		t.Fatalf("after reload %+v %v", got, ok)
	}
	objs.obj[key] = body
	v.Pass(context.Background(), now.Add(2*time.Hour))
	if _, ok := openKind(s, id, assurance.KindObjectMissing); ok {
		t.Fatalf("object restored, finding still open: %+v", s.openBookFindings(id))
	}
}

func TestVerifier_PruneGoneSegmentsAndTasks(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	kept, gone := "mysql-bin.000003", "mysql-bin.000004"
	size := copyLedger(t, dir, id, kept)
	if err := os.WriteFile(filepath.Join(dir, id, gone), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The gone name is not a real binlog; give it a catalog row the first
	// pass can record, then drop the row.
	putSealed(files, id, kept, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	v.mu.Lock()
	st := v.state.Tasks[id]
	if st.LastVerified == nil {
		st.LastVerified = map[string]time.Time{}
	}
	st.LastVerified[gone] = now
	v.state.Tasks[id] = st
	v.dirty = true
	v.mu.Unlock()
	v.save()
	files.files[id] = files.files[id][:1]
	os.Remove(filepath.Join(dir, id, gone))
	v.Pass(context.Background(), now.Add(time.Hour))
	if _, ok := readVerifyState(t, dir).Tasks[id].LastVerified[gone]; ok {
		t.Fatal("gone segment still in .verify.json")
	}
	if _, ok := readVerifyState(t, dir).Tasks[id].LastVerified[kept]; !ok {
		t.Fatal("live segment was pruned")
	}

	other, err := s.CreateTask("other", "other-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, other.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	copyLedger(t, dir, other.ID, kept)
	putSealed(files, other.ID, kept, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	v.Pass(context.Background(), now.Add(2*time.Hour))
	s.mu.Lock()
	delete(s.tasks, other.ID)
	s.mu.Unlock()
	// ListTasks also returns a data_dir child that still holds a segment.
	if err := os.RemoveAll(filepath.Join(dir, other.ID)); err != nil {
		t.Fatal(err)
	}
	v.Pass(context.Background(), now.Add(3*time.Hour))
	if _, ok := readVerifyState(t, dir).Tasks[other.ID]; ok {
		t.Fatal("gone task still in .verify.json")
	}
	if _, ok := readVerifyState(t, dir).Tasks[id]; !ok {
		t.Fatal("live task was pruned")
	}
}

func TestVerifier_DeadlinePersistsPartialProgress(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, blockOpen: true}
	dir, id, files, s, v := newTestVerifier(t, objs)
	v.cfg = verifierCfg(dir, VerifierConfig{PassDeadline: 200 * time.Millisecond})
	v.cfg.Objects = objs
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	objs.obj["k"] = bytes.Repeat([]byte{7}, int(size))
	putSealed(files, id, name, dir, BinlogFile{
		SizeBytes: size, UploadState: "UPLOADED", ObjectKey: "k", State: "SEALED",
		SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	second, err := s.CreateTask("second", "second-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, second.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	putSealed(files, second.ID, "mysql-bin.000009", dir, BinlogFile{UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	passWithin(t, v, context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	st := readVerifyState(t, dir)
	if _, ok := st.Tasks[id]; !ok {
		t.Fatalf("partial pass did not save the task it checked: %+v", st.Tasks)
	}
	if _, ok := st.Tasks[second.ID]; ok {
		t.Fatal("deadline still checked the later task")
	}
	if len(s.openBookFindings(second.ID)) != 0 {
		t.Fatalf("later task was verified: %+v", s.openBookFindings(second.ID))
	}
}

// passWithin fails the test when Pass does not return. A missing deadline
// used to block until go test's own timeout.
func passWithin(t *testing.T, v *Verifier, ctx context.Context, now time.Time) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		v.Pass(ctx, now)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pass did not return")
	}
}

func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("file stat has no inode")
	}
	return st.Ino
}

func verifyTemps(dir string) ([]string, error) {
	return filepath.Glob(filepath.Join(dir, ".verify-*.tmp"))
}

// TestVerifier_CleanPassDoesNotRewriteProgress is WV08. A second pass that
// checks nothing must not rewrite .verify.json.
func TestVerifier_CleanPassDoesNotRewriteProgress(t *testing.T) {
	dir, id, files, _, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	path := filepath.Join(dir, VerifyStateFileName)
	ino := fileInode(t, path)
	v.cfg.Allows = func(context.Context, string) (bool, error) { return false, nil }
	v.Pass(context.Background(), now.Add(time.Hour))
	if got := fileInode(t, path); got != ino {
		t.Fatalf("clean pass rewrote .verify.json (inode %d -> %d)", ino, got)
	}
}

// TestVerifier_RenameFailureRetriesAndDropsTemp is the leaked temp and WV14.
// A non-empty directory at .verify.json makes rename fail. The temp is
// removed, dirty stays set, and the next pass writes once the directory is gone.
func TestVerifier_RenameFailureRetriesAndDropsTemp(t *testing.T) {
	dir, id, files, _, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	blocked := filepath.Join(dir, VerifyStateFileName)
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	temps, err := verifyTemps(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("rename failure left %v", temps)
	}
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	v.cfg.Allows = func(context.Context, string) (bool, error) { return false, nil }
	v.Pass(context.Background(), now.Add(time.Hour))
	st := readVerifyState(t, dir)
	if _, ok := st.Tasks[id].LastVerified[name]; !ok {
		t.Fatalf("failed write was not retried: %+v", st.Tasks)
	}
	temps, err = verifyTemps(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("retry left %v", temps)
	}
}

func uploadedLedger(t *testing.T, objs *scriptObjects) (dir, id string, s *Scheduler, v *Verifier) {
	t.Helper()
	var files *fakeFileStore
	dir, id, files, s, v = newTestVerifier(t, objs)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	v.cfg.Objects = objs
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{
		SizeBytes: size, UploadState: "UPLOADED", ObjectKey: "k", State: "SEALED",
		SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	return dir, id, s, v
}

// TestVerifier_CancelDoesNotMarkVerified is WV11. Cancelling during HEAD
// must not set last_verified.
func TestVerifier_CancelDoesNotMarkVerified(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, blockHead: true, headStarted: make(chan struct{})}
	_, id, s, v := uploadedLedger(t, objs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		v.Pass(ctx, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
		close(done)
	}()
	select {
	case <-objs.headStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("HEAD did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled pass did not return")
	}
	if m := s.VerifyMetrics().Tasks[id]; m.HaveLastVerified {
		t.Fatal("cancelled pass marked the task verified")
	}
}

// TestVerifier_CancelKeepsPartialProgress is WV16. The same cancel still
// writes the task it had already applied.
func TestVerifier_CancelKeepsPartialProgress(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, blockHead: true, headStarted: make(chan struct{})}
	dir, id, _, v := uploadedLedger(t, objs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		v.Pass(ctx, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
		close(done)
	}()
	select {
	case <-objs.headStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("HEAD did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled pass did not return")
	}
	st := readVerifyState(t, dir)
	if _, ok := st.Tasks[id]; !ok {
		t.Fatalf("cancelled pass dropped the task: %+v", st.Tasks)
	}
}

// TestVerifier_ZeroDeadlineUsesEvery is WV12. pass_deadline_sec 0 uses every_sec.
func TestVerifier_ZeroDeadlineUsesEvery(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, blockOpen: true}
	dir, id, files, s, v := newTestVerifier(t, objs)
	v.cfg = verifierCfg(dir, VerifierConfig{Every: 200 * time.Millisecond})
	v.cfg.Objects = objs
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	objs.obj["k"] = bytes.Repeat([]byte{7}, int(size))
	putSealed(files, id, name, dir, BinlogFile{
		SizeBytes: size, UploadState: "UPLOADED", ObjectKey: "k", State: "SEALED",
		SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	second, err := s.CreateTask("second", "second-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, second.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	putSealed(files, second.ID, "mysql-bin.000009", dir, BinlogFile{UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	passWithin(t, v, context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	if _, ok := readVerifyState(t, dir).Tasks[second.ID]; ok {
		t.Fatal("a zero pass deadline still checked the later task")
	}
}

// TestVerifier_EmptySchedulerDropsStaleTasks is WV15.
func TestVerifier_EmptySchedulerDropsStaleTasks(t *testing.T) {
	dir, id, files, s, v := newTestVerifier(t, nil)
	v.cfg = verifierCfg(dir, VerifierConfig{})
	name := "mysql-bin.000003"
	size := copyLedger(t, dir, id, name)
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: size, UploadState: "PENDING", State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	s.mu.Lock()
	delete(s.tasks, id)
	s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(dir, id)); err != nil {
		t.Fatal(err)
	}
	v.Pass(context.Background(), now.Add(time.Hour))
	if _, ok := readVerifyState(t, dir).Tasks[id]; ok {
		t.Fatal("empty scheduler left the task in .verify.json")
	}
}

type probeHead struct{ err error }

func (p probeHead) HeadObject(context.Context, string) (int64, error) { return 0, p.err }
func (p probeHead) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(nil)), 0, nil
}

func TestProbeObjectHead_ClassifiesErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		fail bool
	}{
		{name: "auth", err: errors.New("AccessDenied"), fail: true},
		{name: "missing-bucket", err: errors.New("NoSuchBucket"), fail: true},
		{name: "network", err: errors.New("connection refused"), fail: true},
		{name: "missing-key", err: os.ErrNotExist, fail: false},
		{name: "present", err: nil, fail: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ProbeObjectHead(context.Background(), probeHead{err: tc.err})
			if tc.fail {
				if err == nil || !strings.Contains(err.Error(), tc.err.Error()) {
					t.Fatalf("probe err %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := ProbeObjectHead(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("nil client: %v", err)
	}
}
