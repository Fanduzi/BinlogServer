// Package replication provides module-level functionality for replication.
// input: a runner bound to a recording finding sink, with failing catalog writes and event appends
// output: proof that S5 (retention could not record an unverified upload), S6 (RETENTION_REMOVED event lost) and the post-seal S8/S9 upload outcomes reach the finding sink
// pos: fault-injection tests for the runner half of ADR 0009 phase 0
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"binlog_server/internal/tasks"
)

type recordedFindings struct {
	mu   sync.Mutex
	open map[string]tasks.Finding
}

func bindRecorder(r *MySQLRunner) *recordedFindings {
	rec := &recordedFindings{open: map[string]tasks.Finding{}}
	r.BindFindings(func(f tasks.Finding) {
		rec.mu.Lock()
		rec.open[f.TaskID+"/"+string(f.Kind)+"/"+f.Subject] = f
		rec.mu.Unlock()
	}, func(task string, kind tasks.FindingKind, subject string) {
		rec.mu.Lock()
		delete(rec.open, task+"/"+string(kind)+"/"+subject)
		rec.mu.Unlock()
	})
	return rec
}

func (r *recordedFindings) has(task string, kind tasks.FindingKind, subject string) (tasks.Finding, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.open[task+"/"+string(kind)+"/"+subject]
	return f, ok
}

type failingRetentionMeta struct {
	upsertErr error
	appendErr error
}

func (m *failingRetentionMeta) UpsertBinlogFile(context.Context, tasks.BinlogFile) error {
	return m.upsertErr
}
func (m *failingRetentionMeta) ListBinlogFiles(context.Context, string, int) ([]tasks.BinlogFile, error) {
	return nil, nil
}
func (m *failingRetentionMeta) DeleteBinlogFile(context.Context, string, string, int64) error {
	return nil
}
func (m *failingRetentionMeta) AppendEvent(context.Context, tasks.TaskEvent) error {
	return m.appendErr
}

// S5: retention could not mark an unverified upload UPLOAD_FAILED.
func TestS5_RetentionRecordFailureIsFinding(t *testing.T) {
	meta := &failingRetentionMeta{upsertErr: errors.New("read-only")}
	r := NewMySQLRunner(t.TempDir(), WithFileMetaStore(meta))
	rec := bindRecorder(r)
	objects, err := r.retentionObjects(context.Background(), tasks.Task{ID: "5", ClusterKey: "c"}, "uuid")
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.note(tasks.BinlogFile{TaskID: "5", FileName: "mysql-bin.000001", UploadState: "UPLOAD_FAILED"}); err == nil {
		t.Fatalf("expected note error")
	}
	if _, ok := rec.has("5", tasks.FindingRetentionRecordFailed, "mysql-bin.000001"); !ok {
		t.Fatalf("no RETENTION_RECORD_FAILED: %+v", rec.open)
	}
}

// S6: the RETENTION_REMOVED event for a deleted local file was not written.
func TestS6_RetentionEventLossIsFinding(t *testing.T) {
	meta := &failingRetentionMeta{appendErr: errors.New("insert task_events: timeout")}
	r := NewMySQLRunner(t.TempDir(), WithFileMetaStore(meta))
	rec := bindRecorder(r)
	r.announceRetentionRemoved(context.Background(), "6", "mysql-bin.000009", "UPLOADED")
	if _, ok := rec.has("6", tasks.FindingRetentionRecordFailed, "mysql-bin.000009"); !ok {
		t.Fatalf("no RETENTION_RECORD_FAILED for the lost event: %+v", rec.open)
	}
}

// S8/S9 on the post-seal path: the runner's one upload caller reports both.
func TestS8S9_PostSealUploadOutcomeIsFinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000004")
	if err := os.WriteFile(path, []byte("seg"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := &failingRetentionMeta{}
	r := NewMySQLRunner(dir, WithFileMetaStore(meta), WithUploader(&fileStateUploader{}, "p"))
	rec := bindRecorder(r)
	file := tasks.BinlogFile{TaskID: "8", FileName: "mysql-bin.000004", FilePath: path, ObjectKey: "p/c/u/mysql-bin.000004"}
	if err := r.sealedHandler(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	if f, ok := rec.has("8", tasks.FindingUploadUnverified, "mysql-bin.000004"); !ok || f.Severity != tasks.FindingWarning {
		t.Fatalf("no UPLOAD_UNVERIFIED: %+v", rec.open)
	}
	meta.upsertErr = errors.New("server has gone away")
	if err := r.sealedHandler(context.Background(), file); err == nil {
		t.Fatalf("expected record error")
	}
	if _, ok := rec.has("8", tasks.FindingUploadRecordFailed, "mysql-bin.000004"); !ok {
		t.Fatalf("no UPLOAD_RECORD_FAILED: %+v", rec.open)
	}
}
