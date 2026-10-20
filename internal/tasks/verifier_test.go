// Package tasks provides module-level functionality for tasks.
// input: a temp data_dir, a fake catalog, and the verifier worker driven pass by pass
// output: proof that a forced segment is read once per period, that LOCAL_UNREADABLE escalates and closes only after the detail is fed back, and that SealedAt reaches the clock-skew check
// pos: worker feedback tests for ADR 0009 phase 3 (docs/design/0009-phase3-verifier.md §10)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/assurance"
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
