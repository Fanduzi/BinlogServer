// Package tasks proves a TLS verify failure on a leftover dump is terminal.
// input: a stopped task with a pending Binlog Dump id and a KILL that returns an x509 error
// output: FAILED, last_error contains the x509 text, and the runner does not start
// pos: unit coverage for a verify-identity change applied while a dump is still open
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
)

func TestStartTLSVerifyFailureIsTerminal(t *testing.T) {
	runner := &fenceGateRunner{started: make(chan struct{}, 1)}
	s := NewScheduler()
	s.SetRunner(runner)
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateStopped
	item.DesiredRun = TaskDesiredStop
	s.tasks[task.ID] = item
	s.rememberPendingDumpLocked(task.ID, DumpCleanup{ConnectionID: 302, Host: "10.0.0.8", Port: 3306})
	s.mu.Unlock()
	s.dumpKiller = func(SourceConfig, uint32) error {
		return errors.New("writeAuthHandshake: x509: certificate is valid for mysql.tls.test, not wrong.example")
	}
	if err := s.startTask(task.ID, taskStart{resetBudget: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-runner.started:
		t.Fatal("runner started after a TLS verify failure")
	case <-time.After(50 * time.Millisecond):
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed {
		t.Fatalf("state %s last_error %s", got.State, got.LastError)
	}
	if !strings.Contains(got.LastError, "x509") || strings.Contains(got.LastError, "may still be open") {
		t.Fatalf("last_error %q", got.LastError)
	}
}

func TestStartTLSVerifyWarnsOnce(t *testing.T) {
	obs := captureZap(t, zapcore.WarnLevel)
	runner := &fenceGateRunner{started: make(chan struct{}, 1)}
	s := NewScheduler()
	s.SetRunner(runner)
	task := s.mustTask(t)
	s.mu.Lock()
	item := s.tasks[task.ID]
	item.State = StateStopped
	item.DesiredRun = TaskDesiredStop
	s.tasks[task.ID] = item
	s.rememberPendingDumpLocked(task.ID, DumpCleanup{ConnectionID: 303, Host: "10.0.0.8", Port: 3306})
	s.mu.Unlock()
	s.dumpKiller = func(SourceConfig, uint32) error {
		return errors.New("writeAuthHandshake: x509: certificate signed by unknown authority")
	}
	if err := s.startTask(task.ID, taskStart{resetBudget: true}); err != nil {
		t.Fatalf("start: %v", err)
	}
	warns := 0
	for _, e := range obs.All() {
		if e.Level != zapcore.WarnLevel {
			continue
		}
		if e.Message == "binlog dump cleanup tls verify failed" {
			t.Fatal("killLeftover logged a second x509 warn")
		}
		if e.Message == "source tls verify failed" {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("source tls verify warns=%d, logs=%v", warns, obs.All())
	}
}

func TestUploadTLSClearsAfterSuccessfulRetry(t *testing.T) {
	const tlsLine = `object storage probe: Get "https://127.0.0.1:9000": x509: certificate signed by unknown authority`
	name := "mysql-bin.000092"
	objectKey := "prefix/c/u/" + name
	store := newRetryTestFileStore()
	s := NewScheduler(WithFileStore(store), WithFileUploader(&retryTestUploader{}))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	store.files[task.ID] = []BinlogFile{{
		TaskID: task.ID, FileName: name, FilePath: writeRetryTestFile(t, t.TempDir(), name),
		SealedAt: time.Now(), UploadState: "UPLOAD_FAILED", ObjectKey: objectKey,
	}}
	s.SetVerifyStorageDetail(tlsLine)
	h := s.HealthSummary(context.Background())
	if h.Status != HealthDegraded || !strings.Contains(strings.Join(h.Reasons, " "), "UPLOAD_TLS") {
		t.Fatalf("before %+v", h)
	}
	s.SetVerifyStorageDetail("")
	h = s.HealthSummary(context.Background())
	if h.Status != HealthOK {
		t.Fatalf("re-probe %+v", h)
	}
	s.SetVerifyStorageDetail(tlsLine)
	if _, err := s.RetryFailedUploads(task.ID, 10); err != nil {
		t.Fatal(err)
	}
	h = s.HealthSummary(context.Background())
	if h.Status != HealthOK || strings.Contains(strings.Join(h.Reasons, " "), "UPLOAD_TLS") {
		t.Fatalf("after retry %+v", h)
	}
	s.SetVerifyStorageDetail("object storage probe: NoSuchBucket bucket binlog endpoint 127.0.0.1:9000")
	s.clearStickyUploadTLS()
	h = s.HealthSummary(context.Background())
	joined := strings.Join(h.Detail, " ")
	if h.Status != HealthOK || !strings.Contains(joined, "NoSuchBucket") {
		t.Fatalf("non-tls probe %+v", h)
	}
}

func TestHealthSummary_TLSTransportDegrades(t *testing.T) {
	s := NewScheduler()
	s.SetVerifyStorageDetail("object storage probe: Get \"https://127.0.0.1:9000\": x509: certificate signed by unknown authority")
	h := s.HealthSummary(context.Background())
	if h.Status != HealthDegraded || !strings.Contains(strings.Join(h.Reasons, " "), "UPLOAD_TLS") {
		t.Fatalf("tls probe %+v", h)
	}
	s.SetVerifyStorageDetail("object storage probe: NoSuchBucket bucket binlog endpoint 127.0.0.1:9000")
	s.clearUploadTLS()
	h = s.HealthSummary(context.Background())
	if h.Status != HealthOK {
		t.Fatalf("NoSuchBucket degraded %+v", h)
	}
	s.noteUploadTLS("put object: x509: certificate signed by unknown authority")
	h = s.HealthSummary(context.Background())
	if h.Status != HealthDegraded || !strings.Contains(strings.Join(h.Reasons, " "), "UPLOAD_TLS") {
		t.Fatalf("retry tls %+v", h)
	}
}

// TestBackgroundUploadRetryTLSDegradesAndWarns drives the background retry,
// not the startup probe. Dropping the TLS record or the retry WARN goes red.
func TestBackgroundUploadRetryTLSDegradesAndWarns(t *testing.T) {
	obs := captureZap(t, zapcore.WarnLevel)
	name := "mysql-bin.000091"
	objectKey := "prefix/c/u/" + name
	const tlsErr = `Put "https://127.0.0.1:9000/binlog": tls: failed to verify certificate: x509: certificate signed by unknown authority`
	store := newRetryTestFileStore()
	uploader := &retryTestUploader{errByObject: map[string]error{objectKey: errors.New(tlsErr)}}
	s := NewScheduler(WithFileStore(store), WithFileUploader(uploader))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	store.files[task.ID] = []BinlogFile{{
		TaskID: task.ID, FileName: name, FilePath: writeRetryTestFile(t, t.TempDir(), name),
		SealedAt: time.Now(), UploadState: "UPLOAD_FAILED", ObjectKey: objectKey,
	}}
	before := UploadTLSFailures()
	s.retryKnownFailedUploads()
	h := s.HealthSummary(context.Background())
	reasons := strings.Join(h.Reasons, " ")
	if h.Status != HealthDegraded || !strings.Contains(reasons, "UPLOAD_TLS") || !strings.Contains(reasons, "x509") {
		t.Fatalf("health %+v", h)
	}
	if !sawLevel(obs, "background upload retry tls transport failed", zapcore.WarnLevel) {
		t.Fatalf("missing retry WARN, logs=%v", obs.All())
	}
	if got := UploadTLSFailures() - before; got != 1 {
		t.Fatalf("upload tls failures +%d, want 1", got)
	}
}
