// Package tasks provides module-level functionality for tasks.
// input: a temp data_dir, a fake catalog, and the verifier worker driven pass by pass
// output: proof that a forced segment is read once per period, that LastVerified stops the next read, that LOCAL_UNREADABLE escalates and stays CRITICAL when that severity is read back, that both byte caps stop a read, that an Allows error skips the task, that .verify.json is saved and loaded, that an object subject round-trips through the file store and closes, that gone segments and tasks leave the progress file, that a deadline keeps the tasks already checked and returns within its own timeout, that a clean second pass does not rewrite .verify.json, that a failed rename leaves no temp and is retried, that a cancelled pass does not mark the task verified and keeps partial progress, that pass_deadline_sec 0 uses every_sec, that an empty scheduler drops stale tasks, that a finished object pass resets the strike streak, that a pass with no tasks does not, that deleting the last task clears object detail without zeroing the streak and the next failure is consecutive 4, that an all-skipped pass neither increments nor clears the streak and keeps a detail that is already set, that the next real failure after an empty or all-skipped pass is consecutive 3, that an all-skipped pass keeps a detail already set so the next real failure is consecutive 4, that an empty pass logs tasks=0 skipped=0, that a mixed pass logs skipped equal to the skipped tasks, that a deadline does not count unreached tasks as skipped, that the object-incomplete warn's consecutive matches the streak, that health detail side_kill and lock_detail stay on both summary returns, that an Allows skip and a catalog-list failure are not checked tasks and the pass log counts them as skipped, that a progress temp name contains the pid, that startup sweep removes both the pid form and the old form and leaves unrelated names, that each object-incomplete marker is enough on its own and an empty object or bucket field is omitted, that stale-temp cleanup unlinks a symlink without touching its target and keeps going after a remove error, that catalog, retention, verify, progress-read, and encode failures are zap warn, that a startup HEAD tells a missing key from auth, a missing bucket, and a network error, and that SealedAt reaches the clock-skew check
// pos: worker feedback tests for ADR 0009 phase 3 (docs/design/0009-phase3-verifier.md §10)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/health"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
	headErr     error
	headStarted chan struct{}
}

func (s *scriptObjects) Head(ctx context.Context, key string) (int64, error) {
	if s.headErr != nil {
		return 0, s.headErr
	}
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

func captureZap(t *testing.T, level zapcore.Level) *observer.ObservedLogs {
	t.Helper()
	core, obs := observer.New(level)
	prev := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(prev) })
	return obs
}

func withVerifyFileFault(t *testing.T, which string) {
	t.Helper()
	prev := verifyFiles
	t.Cleanup(func() { verifyFiles = prev })
	boom := errors.New(which + " failed")
	switch which {
	case "write":
		verifyFiles.write = func(*os.File, []byte) (int, error) { return 0, boom }
	case "sync":
		verifyFiles.sync = func(*os.File) error { return boom }
	case "close":
		verifyFiles.close = func(*os.File) error { return boom }
	default:
		t.Fatalf("unknown fault %s", which)
	}
}

func assertFileFaultDropsTemp(t *testing.T, which string) {
	t.Helper()
	dir := t.TempDir()
	withVerifyFileFault(t, which)
	err := writeAtomic(filepath.Join(dir, VerifyStateFileName), []byte("{}\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	temps, gerr := verifyTemps(dir)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(temps) != 0 {
		t.Fatalf("%s failure left %v", which, temps)
	}
}

func TestVerifier_WriteErrorDropsTemp(t *testing.T) { assertFileFaultDropsTemp(t, "write") }
func TestVerifier_SyncErrorDropsTemp(t *testing.T)  { assertFileFaultDropsTemp(t, "sync") }
func TestVerifier_CloseErrorDropsTemp(t *testing.T) { assertFileFaultDropsTemp(t, "close") }

func TestVerifier_WriteFailureLogsWarn(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir))
	v := NewVerifier(s, VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
	v.mu.Lock()
	v.dirty = true
	v.state.Tasks["t"] = verifyTaskState{}
	v.mu.Unlock()
	withVerifyFileFault(t, "write")
	v.save()
	v.mu.Lock()
	dirty := v.dirty
	v.mu.Unlock()
	if !dirty {
		t.Fatal("failed write cleared dirty")
	}
	var saw bool
	for _, e := range obs.All() {
		if !strings.Contains(e.Message, "verifier: write progress") {
			continue
		}
		saw = true
		if e.Level != zapcore.WarnLevel {
			t.Fatalf("write failure level %s, want warn", e.Level)
		}
	}
	if !saw {
		t.Fatal("write failure was not logged")
	}
}

func TestVerifier_FindingsOpenedCountsNewOnly(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	dir, id, files, s, v := newTestVerifier(t, nil)
	name := "mysql-bin.000001"
	body := make([]byte, 64)
	copy(body[:4], []byte{0xfe, 'b', 'i', 'n'})
	if err := os.WriteFile(filepath.Join(dir, id, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
	putSealed(files, id, name, dir, BinlogFile{SizeBytes: 64, UploadState: "PENDING", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	want := [][2]int{{1, 1}, {0, 1}, {0, 1}}
	for i := 0; i < 3; i++ {
		from := len(obs.All())
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
		opened, open, ok := lastPassCounts(obs.All()[from:])
		if !ok {
			t.Fatalf("pass %d did not log", i+1)
		}
		if opened != want[i][0] || open != want[i][1] {
			t.Fatalf("pass %d opened=%d open=%d, want %d %d", i+1, opened, open, want[i][0], want[i][1])
		}
	}
	if _, ok := openKind(s, id, assurance.KindVerifyFailed); !ok {
		t.Fatal("VERIFY_FAILED was not left open")
	}
}

func lastPassCounts(entries []observer.LoggedEntry) (opened, open int, ok bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		msg := entries[i].Message
		if !strings.Contains(msg, "verifier: pass ") {
			continue
		}
		if entries[i].Level != zapcore.InfoLevel {
			return 0, 0, false
		}
		var tasks, skipped, closed int
		var bytes int64
		var cut, incomplete bool
		var dur string
		_, err := fmt.Sscanf(msg, "verifier: pass tasks=%d skipped=%d bytes=%d findings_opened=%d findings_open=%d findings_closed=%d cut_short=%t object_incomplete=%t duration=%s",
			&tasks, &skipped, &bytes, &opened, &open, &closed, &cut, &incomplete, &dur)
		if err != nil {
			return 0, 0, false
		}
		return opened, open, true
	}
	return 0, 0, false
}

func TestVerifier_ObjectIncompleteDegradesThenClears(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	dir, id, s, v := uploadedLedger(t, objs)
	v.cfg.ObjectBucket = "binlogs"
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
		h := s.HealthSummary(context.Background())
		if h.Status != HealthOK || strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
			t.Fatalf("pass %d health %+v", i, h)
		}
	}
	v.Pass(context.Background(), now.Add(3*time.Minute))
	h := s.HealthSummary(context.Background())
	joined := strings.Join(h.Detail, " ")
	if h.Status != HealthOK || !detailHasConsecutive(h, 3) || !strings.Contains(joined, "connection refused") || !strings.Contains(joined, "task "+id+" object k bucket binlogs") {
		t.Fatalf("after 3: %+v", h)
	}
	if got := warnConsecutives(obs.All()); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("warn consecutive %v", got)
	}
	if !sawLevel(obs, "verifier: object pass incomplete", zapcore.WarnLevel) {
		t.Fatal("object incomplete was not logged at warn")
	}
	if !passFlag(obs.All(), "object_incomplete=true") {
		t.Fatal("info line did not mark the object pass incomplete")
	}
	body, err := os.ReadFile(filepath.Join(dir, id, "mysql-bin.000003"))
	if err != nil {
		t.Fatal(err)
	}
	objs.headErr = nil
	objs.obj["k"] = body
	from := len(obs.All())
	v.Pass(context.Background(), now.Add(4*time.Minute))
	h = s.HealthSummary(context.Background())
	if h.Status != HealthOK || len(h.Detail) != 0 {
		t.Fatalf("cleared health %+v", h)
	}
	if !passFlag(obs.All()[from:], "object_incomplete=false") {
		t.Fatal("recovery pass still marked object_incomplete")
	}
}

func warnConsecutives(entries []observer.LoggedEntry) []int64 {
	var out []int64
	for _, e := range entries {
		if e.Level != zapcore.WarnLevel || !strings.Contains(e.Message, "verifier: object pass incomplete") {
			continue
		}
		for _, f := range e.Context {
			if f.Key == "consecutive" {
				out = append(out, f.Integer)
			}
		}
	}
	return out
}

func sawLevel(obs *observer.ObservedLogs, msg string, level zapcore.Level) bool {
	for _, e := range obs.All() {
		if strings.Contains(e.Message, msg) && e.Level == level {
			return true
		}
	}
	return false
}

func lastPassTasks(entries []observer.LoggedEntry) (tasks, skipped int, ok bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		msg := entries[i].Message
		if !strings.Contains(msg, "verifier: pass ") || entries[i].Level != zapcore.InfoLevel {
			continue
		}
		var bytes int64
		var opened, open, closed int
		var cut, incomplete bool
		var dur string
		_, err := fmt.Sscanf(msg, "verifier: pass tasks=%d skipped=%d bytes=%d findings_opened=%d findings_open=%d findings_closed=%d cut_short=%t object_incomplete=%t duration=%s",
			&tasks, &skipped, &bytes, &opened, &open, &closed, &cut, &incomplete, &dur)
		if err != nil {
			return 0, 0, false
		}
		return tasks, skipped, true
	}
	return 0, 0, false
}

func passFlag(entries []observer.LoggedEntry, flag string) bool {
	for i := len(entries) - 1; i >= 0; i-- {
		if strings.Contains(entries[i].Message, "verifier: pass ") && strings.Contains(entries[i].Message, flag) {
			return true
		}
	}
	return false
}

func TestVerifier_StartupDropsStaleTemps(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".verify-123.tmp")
	nested := filepath.Join(taskDir, ".verify-9.tmp")
	other := filepath.Join(dir, "notes.tmp")
	for _, name := range []string{stale, nested, other} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := NewScheduler(WithDataDir(dir))
	NewVerifier(s, VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp remains: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
}

func TestHealthSummary_KeepsProbeDetailWhenDegraded(t *testing.T) {
	s := NewScheduler(WithDataDir(t.TempDir()))
	s.Report(Finding{TaskID: "1", Kind: FindingTaskPersistFailed, Severity: FindingCritical, Detail: "disk"})
	s.SetVerifyStorageDetail("object storage probe: NoSuchBucket bucket binlog endpoint 127.0.0.1:9000")
	h := s.HealthSummary(context.Background())
	if h.Status != HealthDegraded {
		t.Fatalf("status %s reasons %v", h.Status, h.Reasons)
	}
	if !strings.Contains(strings.Join(h.Detail, "\n"), "NoSuchBucket") {
		t.Fatalf("detail dropped while degraded: %+v", h)
	}
}

// TestVerifier_CompletedPassResetsStreak is S03. Two storage failures, one
// finished object pass, then one more failure must not set health detail.
// Clearing the detail without zeroing the streak would make that last
// failure the third strike.
func TestVerifier_CompletedPassResetsStreak(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	dir, id, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	body, err := os.ReadFile(filepath.Join(dir, id, "mysql-bin.000003"))
	if err != nil {
		t.Fatal(err)
	}
	objs.headErr = nil
	objs.obj["k"] = body
	v.Pass(context.Background(), now.Add(3*time.Minute))
	if h := s.HealthSummary(context.Background()); h.Status != HealthOK || len(h.Detail) != 0 {
		t.Fatalf("finished pass left detail %+v", h)
	}
	objs.headErr = errors.New("connection refused")
	objs.obj = map[string][]byte{}
	v.Pass(context.Background(), now.Add(4*time.Minute))
	h := s.HealthSummary(context.Background())
	if h.Status != HealthOK || strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
		t.Fatalf("one failure after a finished pass set detail %+v", h)
	}
}

// TestVerifier_EmptyPassKeepsStreak is S05. A pass with zero tasks is not an
// object pass: it does not reset the streak. Two failures, an empty
// scheduler, then one failure is the third strike.
func TestVerifier_EmptyPassKeepsStreak(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	dir, id, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	s.mu.Lock()
	task := s.tasks[id]
	delete(s.tasks, id)
	s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(dir, id)); err != nil {
		t.Fatal(err)
	}
	v.Pass(context.Background(), now.Add(3*time.Minute))
	if h := s.HealthSummary(context.Background()); strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
		t.Fatalf("empty pass set detail %+v", h)
	}
	s.mu.Lock()
	s.tasks[id] = task
	s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	copyLedger(t, dir, id, "mysql-bin.000003")
	v.Pass(context.Background(), now.Add(4*time.Minute))
	h := s.HealthSummary(context.Background())
	if h.Status != HealthOK || !strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
		t.Fatalf("empty pass reset the streak: %+v", h)
	}
}

// TestVerifier_ObjectErrAloneIsIncomplete is S06. ObjectErr with no
// MethodError coverage row is still an incomplete object pass. With no
// per-object row the object field is omitted, and an empty bucket is too.
func TestVerifier_ObjectErrAloneIsIncomplete(t *testing.T) {
	cause := objectPassCause("1", "binlogs", nil, assurance.Report{ObjectErr: errors.New("Access Denied.")})
	if cause != "task 1 bucket binlogs: Access Denied." {
		t.Fatalf("ObjectErr alone: %q", cause)
	}
	if strings.Contains(cause, "object  ") || strings.Contains(cause, "bucket  ") || strings.Contains(cause, "object bucket") {
		t.Fatalf("empty field: %q", cause)
	}
	nobucket := objectPassCause("1", "", nil, assurance.Report{ObjectErr: errors.New("Access Denied.")})
	if nobucket != "task 1: Access Denied." || strings.Contains(nobucket, "bucket") || strings.Contains(nobucket, "object ") {
		t.Fatalf("empty bucket: %q", nobucket)
	}
}

// TestVerifier_MethodErrorAloneIsIncomplete is S07. A MethodError coverage
// row with a nil ObjectErr is still an incomplete object pass, and it names
// the object key.
func TestVerifier_MethodErrorAloneIsIncomplete(t *testing.T) {
	rep := assurance.Report{Coverage: map[string]assurance.Coverage{
		"mysql-bin.000003": {Object: assurance.Check{Method: assurance.MethodError, Reason: "Access Denied."}},
	}}
	rows := []assurance.VerifyRow{{Name: "mysql-bin.000003", ObjectKey: "k"}}
	cause := objectPassCause("1", "binlogs", rows, rep)
	if !strings.Contains(cause, "Access Denied.") || !strings.Contains(cause, "object k") || !strings.Contains(cause, "bucket binlogs") {
		t.Fatalf("MethodError alone: %q", cause)
	}
	emptyKey := objectPassCause("1", "binlogs", []assurance.VerifyRow{{Name: "mysql-bin.000003"}}, rep)
	if !strings.Contains(emptyKey, "object mysql-bin.000003") || strings.Contains(emptyKey, "object  ") || strings.Contains(emptyKey, "bucket  ") {
		t.Fatalf("empty object key: %q", emptyKey)
	}
}

// TestVerifier_SweepDoesNotFollowSymlink is C05. The data-dir temp is a
// symlink; cleanup unlinks it and leaves the target outside the data dir.
func TestVerifier_SweepDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(target, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".verify-link.tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	NewVerifier(NewScheduler(WithDataDir(dir)), VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("symlink remains: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "safe" {
		t.Fatalf("target %q err %v", got, err)
	}
}

func sweepFaultDir(t *testing.T) (string, *Verifier) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{".verify-a.tmp", ".verify-b.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte(`{"tasks":{"kept":{"last_verified":{"mysql-bin.000001":"2026-01-01T00:00:00Z"}}}}`)
	if err := os.WriteFile(filepath.Join(dir, VerifyStateFileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	prev := verifyFiles.remove
	t.Cleanup(func() { verifyFiles.remove = prev })
	verifyFiles.remove = func(name string) error {
		if filepath.Base(name) == ".verify-a.tmp" {
			return errors.New("remove failed")
		}
		return os.Remove(name)
	}
	v := NewVerifier(NewScheduler(WithDataDir(dir)), VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
	return dir, v
}

// TestVerifier_SweepRemoveErrorStillLoads is C06. A remove error does not
// skip loading .verify.json.
func TestVerifier_SweepRemoveErrorStillLoads(t *testing.T) {
	_, v := sweepFaultDir(t)
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.state.Tasks["kept"]; !ok {
		t.Fatalf("progress not loaded: %+v", v.state.Tasks)
	}
}

// TestVerifier_SweepRemoveErrorContinues is C07. One remove error does not
// leave the later temp in place.
func TestVerifier_SweepRemoveErrorContinues(t *testing.T) {
	dir, _ := sweepFaultDir(t)
	if _, err := os.Stat(filepath.Join(dir, ".verify-b.tmp")); !os.IsNotExist(err) {
		t.Fatalf("later temp remains: %v", err)
	}
}

// TestVerifier_SweepRemoveErrorLogsWarn is C08. The remove error is a zap warn.
func TestVerifier_SweepRemoveErrorLogsWarn(t *testing.T) {
	obs := captureZap(t, zapcore.WarnLevel)
	sweepFaultDir(t)
	if !sawLevel(obs, "verifier: remove stale temp", zapcore.WarnLevel) {
		t.Fatal("remove error was not logged at warn")
	}
}

// TestVerifier_ErrorPathsLogWarn: a level=warn filter sees a corrupt
// progress file, a directory at that name, a catalog-list error, a
// retention-marker error, a per-task Verify error, and an encode error.
func TestVerifier_ErrorPathsLogWarn(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		obs := captureZap(t, zapcore.WarnLevel)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, VerifyStateFileName), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		NewVerifier(NewScheduler(WithDataDir(dir)), VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
		if !sawLevel(obs, "is not progress", zapcore.WarnLevel) {
			t.Fatal("corrupt progress was not logged at warn")
		}
	})
	t.Run("directory", func(t *testing.T) {
		obs := captureZap(t, zapcore.WarnLevel)
		dir := t.TempDir()
		path := filepath.Join(dir, VerifyStateFileName)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "x"), []byte("y"), 0o644); err != nil {
			t.Fatal(err)
		}
		NewVerifier(NewScheduler(WithDataDir(dir)), VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
		if !sawLevel(obs, "verifier: read ", zapcore.WarnLevel) {
			t.Fatal("directory progress was not logged at warn")
		}
	})
	t.Run("list", func(t *testing.T) {
		obs := captureZap(t, zapcore.WarnLevel)
		_, _, files, _, v := newTestVerifier(t, nil)
		files.listErr = errors.New("catalog down")
		v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
		if !sawLevel(obs, "verifier: list catalog", zapcore.WarnLevel) {
			t.Fatal("catalog list was not logged at warn")
		}
	})
	t.Run("markers", func(t *testing.T) {
		obs := captureZap(t, zapcore.WarnLevel)
		_, _, files, _, v := newTestVerifier(t, nil)
		files.markerErr = errors.New("markers down")
		v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
		if !sawLevel(obs, "verifier: retention markers", zapcore.WarnLevel) {
			t.Fatal("retention markers were not logged at warn")
		}
	})
	t.Run("verify", func(t *testing.T) {
		obs := captureZap(t, zapcore.WarnLevel)
		_, _, _, _, v := newTestVerifier(t, nil)
		v.cfg.RecentSharePercent = 101
		v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
		if !sawLevel(obs, "verifier: task=", zapcore.WarnLevel) {
			t.Fatal("verify error was not logged at warn")
		}
	})
	t.Run("encode", func(t *testing.T) {
		obs := captureZap(t, zapcore.WarnLevel)
		dir := t.TempDir()
		v := NewVerifier(NewScheduler(WithDataDir(dir)), VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
		prev := verifyEncode
		t.Cleanup(func() { verifyEncode = prev })
		verifyEncode = func(any, string, string) ([]byte, error) {
			return nil, errors.New("encode failed")
		}
		v.mu.Lock()
		v.dirty = true
		v.mu.Unlock()
		v.save()
		v.mu.Lock()
		dirty := v.dirty
		v.mu.Unlock()
		if !dirty {
			t.Fatal("encode failure cleared dirty")
		}
		if !sawLevel(obs, "verifier: encode progress", zapcore.WarnLevel) {
			t.Fatal("encode failure was not logged at warn")
		}
	})
}

func denyAllows(context.Context, string) (bool, error) { return false, nil }

// TestVerifier_AllSkippedDoesNotIncrementStreak is N03. Two object failures
// leave the streak at 2. Skipping the only task must not make it the third.
func TestVerifier_AllSkippedDoesNotIncrementStreak(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	_, _, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	v.cfg.Allows = denyAllows
	v.Pass(context.Background(), now.Add(3*time.Minute))
	if h := s.HealthSummary(context.Background()); strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
		t.Fatalf("all-skipped pass incremented the streak: %+v", h)
	}
}

// TestVerifier_AllSkippedDoesNotClearStreak is N04. The same skip must not
// zero the streak: one real failure afterwards is the third strike.
func TestVerifier_AllSkippedDoesNotClearStreak(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	_, _, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	v.cfg.Allows = denyAllows
	v.Pass(context.Background(), now.Add(3*time.Minute))
	v.cfg.Allows = nil
	v.Pass(context.Background(), now.Add(4*time.Minute))
	h := s.HealthSummary(context.Background())
	if h.Status != HealthOK || !strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
		t.Fatalf("all-skipped pass cleared the streak: %+v", h)
	}
}

// TestVerifier_AllowsSkipIsNotChecked is N05. A task Allows refuses is
// skipped, not checked.
func TestVerifier_AllowsSkipIsNotChecked(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	_, _, _, v := uploadedLedger(t, &scriptObjects{obj: map[string][]byte{}})
	v.cfg.Allows = denyAllows
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	tasks, skipped, ok := lastPassTasks(obs.All())
	if !ok || tasks != 0 || skipped != 1 {
		t.Fatalf("tasks=%d skipped=%d ok=%t", tasks, skipped, ok)
	}
}

// TestVerifier_CatalogListFailureIsNotChecked is N07. A catalog-list failure
// is not a checked task.
func TestVerifier_CatalogListFailureIsNotChecked(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	_, _, files, _, v := newTestVerifier(t, nil)
	files.listErr = errors.New("catalog down")
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	tasks, skipped, ok := lastPassTasks(obs.All())
	if !ok || tasks != 0 || skipped != 1 {
		t.Fatalf("tasks=%d skipped=%d ok=%t", tasks, skipped, ok)
	}
}

// TestVerifier_TempNameIncludesPid is N08. The progress temp name contains
// this process id between the dots, not only somewhere in the random suffix.
func TestVerifier_TempNameIncludesPid(t *testing.T) {
	dir := t.TempDir()
	var name string
	prev := verifyFiles.write
	t.Cleanup(func() { verifyFiles.write = prev })
	verifyFiles.write = func(f *os.File, p []byte) (int, error) {
		name = f.Name()
		return prev(f, p)
	}
	if err := writeAtomic(filepath.Join(dir, VerifyStateFileName), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(".verify-%d-", os.Getpid())
	base := filepath.Base(name)
	if !strings.Contains(base, want) || !strings.HasSuffix(base, ".tmp") {
		t.Fatalf("temp %q", base)
	}
}

// TestVerifier_SweepRemovesPidAndOldTemps is N09. Startup removes both the
// pid form and the older .verify-123.tmp form.
func TestVerifier_SweepRemovesPidAndOldTemps(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		filepath.Join(dir, fmt.Sprintf(".verify-%d-abc.tmp", os.Getpid())),
		filepath.Join(dir, ".verify-123.tmp"),
	}
	for _, name := range names {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	NewVerifier(NewScheduler(WithDataDir(dir)), VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
	for _, name := range names {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", filepath.Base(name), err)
		}
	}
}

// TestVerifier_SweepKeepsUnrelatedNames is N11 and N12. Names that are not
// .verify-*.tmp stay, including ones a wider glob would take.
func TestVerifier_SweepKeepsUnrelatedNames(t *testing.T) {
	dir := t.TempDir()
	keep := []string{".verify-foo", ".verify.json.bak", "other.tmp", ".verifyX.tmp"}
	for _, name := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	real := filepath.Join(dir, ".verify-123.tmp")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	NewVerifier(NewScheduler(WithDataDir(dir)), VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1})
	if _, err := os.Stat(real); !os.IsNotExist(err) {
		t.Fatalf("real temp remains: %v", err)
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestVerifier_LastTaskDeletedClearsDetail: three object failures set the
// detail. Deleting the last task clears that detail and leaves the streak,
// so the next failure after the task returns sets the detail again.
func TestVerifier_LastTaskDeletedClearsDetail(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	dir, id, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	if h := s.HealthSummary(context.Background()); !strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
		t.Fatalf("three failures left %+v", h)
	}
	s.mu.Lock()
	task := s.tasks[id]
	delete(s.tasks, id)
	s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(dir, id)); err != nil {
		t.Fatal(err)
	}
	v.Pass(context.Background(), now.Add(4*time.Minute))
	if h := s.HealthSummary(context.Background()); strings.Contains(strings.Join(h.Detail, " "), "consecutive") {
		t.Fatalf("last task deleted left detail %+v", h)
	}
	s.mu.Lock()
	s.tasks[id] = task
	s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	copyLedger(t, dir, id, "mysql-bin.000003")
	v.Pass(context.Background(), now.Add(5*time.Minute))
	if h := s.HealthSummary(context.Background()); !detailHasConsecutive(h, 4) {
		t.Fatalf("streak was cleared with the detail: %+v", h)
	}
}

func detailHasConsecutive(h HealthSummary, n int) bool {
	return strings.Contains(strings.Join(h.Detail, " "), fmt.Sprintf("%d consecutive", n))
}

func lastPassLine(entries []observer.LoggedEntry) (tasks, skipped int, cut, incomplete bool, ok bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		msg := entries[i].Message
		if !strings.Contains(msg, "verifier: pass ") || entries[i].Level != zapcore.InfoLevel {
			continue
		}
		var bytes int64
		var opened, open, closed int
		var dur string
		_, err := fmt.Sscanf(msg, "verifier: pass tasks=%d skipped=%d bytes=%d findings_opened=%d findings_open=%d findings_closed=%d cut_short=%t object_incomplete=%t duration=%s",
			&tasks, &skipped, &bytes, &opened, &open, &closed, &cut, &incomplete, &dur)
		if err != nil {
			return 0, 0, false, false, false
		}
		return tasks, skipped, cut, incomplete, true
	}
	return 0, 0, false, false, false
}

func dropTask(t *testing.T, dir, id string, s *Scheduler) Task {
	t.Helper()
	s.mu.Lock()
	task := s.tasks[id]
	delete(s.tasks, id)
	s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(dir, id)); err != nil {
		t.Fatal(err)
	}
	return task
}

func restoreTask(t *testing.T, dir, id string, s *Scheduler, task Task) {
	t.Helper()
	s.mu.Lock()
	s.tasks[id] = task
	s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	copyLedger(t, dir, id, "mysql-bin.000003")
}

// TestVerifier_EmptyPassDoesNotBumpStreak is P03. Two object failures, a
// pass with no tasks, then one real failure is consecutive 3. Adding one
// during the empty pass would make it 4.
func TestVerifier_EmptyPassDoesNotBumpStreak(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	dir, id, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	task := dropTask(t, dir, id, s)
	v.Pass(context.Background(), now.Add(3*time.Minute))
	restoreTask(t, dir, id, s, task)
	v.Pass(context.Background(), now.Add(4*time.Minute))
	if h := s.HealthSummary(context.Background()); !detailHasConsecutive(h, 3) {
		t.Fatalf("empty pass changed the streak: %+v", h)
	}
}

// TestVerifier_EmptyPassLogsZeroSkipped is the zero-task info line:
// tasks=0 skipped=0, and the pass is not cut short.
func TestVerifier_EmptyPassLogsZeroSkipped(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	dir, id, s, v := uploadedLedger(t, &scriptObjects{obj: map[string][]byte{}})
	dropTask(t, dir, id, s)
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	tasks, skipped, cut, incomplete, ok := lastPassLine(obs.All())
	if !ok || tasks != 0 || skipped != 0 || cut || incomplete {
		t.Fatalf("tasks=%d skipped=%d cut=%t incomplete=%t ok=%t", tasks, skipped, cut, incomplete, ok)
	}
}

// TestVerifier_AllSkippedDoesNotBumpStreak is the silent N03. Two object
// failures, a pass that skips the only task, then one real failure is
// consecutive 3.
func TestVerifier_AllSkippedDoesNotBumpStreak(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	_, _, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 2; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	v.cfg.Allows = denyAllows
	v.Pass(context.Background(), now.Add(3*time.Minute))
	v.cfg.Allows = nil
	v.Pass(context.Background(), now.Add(4*time.Minute))
	if h := s.HealthSummary(context.Background()); !detailHasConsecutive(h, 3) {
		t.Fatalf("all-skipped pass changed the streak: %+v", h)
	}
}

// TestVerifier_AllSkippedKeepsDetail is P04. Three strikes set the detail.
// Skipping the only task must leave that detail in place, and the next real
// failure is consecutive 4.
func TestVerifier_AllSkippedKeepsDetail(t *testing.T) {
	objs := &scriptObjects{obj: map[string][]byte{}, headErr: errors.New("connection refused")}
	_, _, s, v := uploadedLedger(t, objs)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		v.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
	}
	if h := s.HealthSummary(context.Background()); !detailHasConsecutive(h, 3) {
		t.Fatalf("three failures left %+v", h)
	}
	v.cfg.Allows = denyAllows
	v.Pass(context.Background(), now.Add(4*time.Minute))
	if h := s.HealthSummary(context.Background()); !detailHasConsecutive(h, 3) {
		t.Fatalf("all-skipped pass wiped the detail: %+v", h)
	}
	v.cfg.Allows = nil
	v.Pass(context.Background(), now.Add(5*time.Minute))
	if h := s.HealthSummary(context.Background()); !detailHasConsecutive(h, 4) {
		t.Fatalf("all-skipped pass changed the streak: %+v", h)
	}
}

// TestVerifier_MixedPassCountsSkipped is P07. One allowed task and two
// Allows skips log tasks=1 skipped=2.
func TestVerifier_MixedPassCountsSkipped(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	dir, _, _, s, v := newTestVerifier(t, nil)
	second, err := s.CreateTask("second", "second-key")
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.CreateTask("third", "third-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{second, third} {
		if err := os.MkdirAll(filepath.Join(dir, task.ID), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	v.cfg.Allows = func(_ context.Context, taskID string) (bool, error) {
		return taskID == second.ID, nil
	}
	v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	tasks, skipped, cut, incomplete, ok := lastPassLine(obs.All())
	if !ok || tasks != 1 || skipped != 2 || cut || incomplete {
		t.Fatalf("tasks=%d skipped=%d cut=%t incomplete=%t ok=%t", tasks, skipped, cut, incomplete, ok)
	}
}

// TestVerifier_DeadlineDoesNotCountUnreachedAsSkipped is P08. The task the
// deadline never reaches is cut_short, not skipped.
func TestVerifier_DeadlineDoesNotCountUnreachedAsSkipped(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
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
	tasks, skipped, cut, _, ok := lastPassLine(obs.All())
	if !ok || tasks != 1 || skipped != 0 || !cut {
		t.Fatalf("tasks=%d skipped=%d cut=%t ok=%t", tasks, skipped, cut, ok)
	}
}

// TestVerifier_CanceledPassIsCutShortNotSkipped is the pass that reaches no
// task. It is cut_short, and the unreached task is not skipped.
func TestVerifier_CanceledPassIsCutShortNotSkipped(t *testing.T) {
	obs := captureZap(t, zapcore.InfoLevel)
	_, _, _, _, v := newTestVerifier(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v.Pass(ctx, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	tasks, skipped, cut, incomplete, ok := lastPassLine(obs.All())
	if !ok || tasks != 0 || skipped != 0 || !cut || incomplete {
		t.Fatalf("tasks=%d skipped=%d cut=%t incomplete=%t ok=%t", tasks, skipped, cut, incomplete, ok)
	}
}

type healthFieldStore struct {
	*fakeStore
	side string
	lock string
}

func (s healthFieldStore) SessionDegraded() (bool, string) { return false, "" }
func (s healthFieldStore) SideKill() string                { return s.side }
func (s healthFieldStore) LockDetail() string              { return s.lock }

func newHealthFieldScheduler(t *testing.T) *Scheduler {
	t.Helper()
	s := NewScheduler(WithDataDir(t.TempDir()), WithStore(healthFieldStore{fakeStore: newFakeStore(), side: "off", lock: "reduced"}))
	s.SetVerifyStorageDetail("object storage probe: NoSuchBucket bucket binlog endpoint 127.0.0.1:9000")
	return s
}

func healthFieldsHeld(t *testing.T, h HealthSummary, status string) {
	t.Helper()
	if h.Status != status {
		t.Fatalf("status %s reasons %v", h.Status, h.Reasons)
	}
	if !strings.Contains(strings.Join(h.Detail, "\n"), "NoSuchBucket") {
		t.Fatalf("detail %+v", h)
	}
	if h.SideKill != "off" || h.LockDetail != "reduced" {
		t.Fatalf("side_kill %q lock_detail %q", h.SideKill, h.LockDetail)
	}
}

// TestHealthSummary_PinsFieldsWhenOK is the healthy return. Detail,
// side_kill, and lock_detail are all present, and status stays ok.
func TestHealthSummary_PinsFieldsWhenOK(t *testing.T) {
	h := newHealthFieldScheduler(t).HealthSummary(context.Background())
	healthFieldsHeld(t, h, HealthOK)
	if len(h.Reasons) != 0 {
		t.Fatalf("reasons %v", h.Reasons)
	}
}

// TestHealthSummary_PinsFieldsWhenDegraded is P12, P17b, and P17c. A
// degraded summary keeps detail, side_kill, lock_detail, and the reasons.
func TestHealthSummary_PinsFieldsWhenDegraded(t *testing.T) {
	s := newHealthFieldScheduler(t)
	s.Report(Finding{TaskID: "1", Kind: FindingTaskPersistFailed, Severity: FindingCritical, Detail: "disk"})
	h := s.HealthSummary(context.Background())
	healthFieldsHeld(t, h, HealthDegraded)
	if !strings.Contains(strings.Join(h.Reasons, " "), "TASK_PERSIST_FAILED") {
		t.Fatalf("reasons %v", h.Reasons)
	}
}

// TestVerifyAllows_LeaseHolderOnly is the ADR 0011 seam phase 3 §10 binds:
// the holder verifies, every other node skips. Epoch 0 and no worker id
// are "not held" too.
func TestVerifyAllows_LeaseHolderOnly(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(WithDataDir(dir), WithFileStore(newFakeFileStore()), WithClusterWorkerID("worker-a"))
	task, err := s.CreateTask("v", "k")
	if err != nil {
		t.Fatal(err)
	}
	allows := func() bool {
		ok, err := s.VerifyAllows(context.Background(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if allows() {
		t.Fatal("unheld task allowed")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-a", Epoch: 3}
	s.mu.Unlock()
	if !allows() {
		t.Fatal("holder not allowed")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-b", Epoch: 3}
	s.mu.Unlock()
	if allows() {
		t.Fatal("another holder allowed")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-a"}
	s.mu.Unlock()
	if allows() {
		t.Fatal("epoch 0 allowed")
	}
	plain := NewScheduler(WithDataDir(t.TempDir()), WithFileStore(newFakeFileStore()))
	if ok, _ := plain.VerifyAllows(context.Background(), task.ID); ok {
		t.Fatal("no worker id allowed")
	}
}

// TestVerifier_ClusterOwnerVerifies wires the real gate through a pass:
// a held task is checked (metrics update), an unheld one is skipped.
func TestVerifier_ClusterOwnerVerifies(t *testing.T) {
	dir := t.TempDir()
	files := newFakeFileStore()
	s := NewScheduler(WithDataDir(dir), WithFileStore(files), WithFindingStore(NewFileFindingStore(filepath.Join(dir, FindingFileName))), WithClusterWorkerID("worker-a"))
	task, err := s.CreateTask("verify", "verify-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, task.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	putSealed(files, task.ID, "mysql-bin.000001", dir, BinlogFile{State: "SEALED", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	v := NewVerifier(s, VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1, Allows: s.VerifyAllows})
	s.SetVerifier(v)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	v.Pass(context.Background(), now)
	if _, ok := s.VerifyMetrics().Tasks[task.ID]; ok {
		t.Fatal("unheld task was checked")
	}
	s.mu.Lock()
	s.tasks[task.ID] = Task{ID: task.ID, OwnerWorkerID: "worker-a", Epoch: 3}
	s.mu.Unlock()
	v.Pass(context.Background(), now)
	if _, ok := s.VerifyMetrics().Tasks[task.ID]; !ok {
		t.Fatal("held task was not checked")
	}
}
