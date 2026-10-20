// Package api provides module-level functionality for api.
// input: a running verifier, a catalog row whose local file is missing or whose object bytes differ, and the HTTP API
// output: proof that the worker's finding is returned by GET /api/tasks/{id}/findings and degrades GET /api/health, and that the verify series are on /metrics
// pos: the CI check for production-readiness A4 (go test ./internal/api, part of go test ./...)
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/tasks"
)

type verifyFileStore struct {
	files map[string][]tasks.BinlogFile
}

func (f *verifyFileStore) UpsertBinlogFile(context.Context, tasks.BinlogFile) error { return nil }

func (f *verifyFileStore) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	items := f.files[taskID]
	if limit > 0 && limit < len(items) {
		items = items[len(items)-limit:]
	}
	out := make([]tasks.BinlogFile, len(items))
	copy(out, items)
	return out, nil
}

type verifyMemObjects struct {
	obj map[string][]byte
}

func (m verifyMemObjects) Head(_ context.Context, key string) (int64, error) {
	b, ok := m.obj[key]
	if !ok {
		return 0, os.ErrNotExist
	}
	return int64(len(b)), nil
}

func (m verifyMemObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := m.obj[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func newAPIVerifier(t *testing.T, objects assurance.ObjectStore) (dir, id string, files *verifyFileStore, s *tasks.Scheduler, v *tasks.Verifier) {
	t.Helper()
	dir = t.TempDir()
	files = &verifyFileStore{files: map[string][]tasks.BinlogFile{}}
	s = tasks.NewScheduler(tasks.WithDataDir(dir), tasks.WithFileStore(files), tasks.WithFindingStore(tasks.NewFileFindingStore(filepath.Join(dir, tasks.FindingFileName))))
	task, err := s.CreateTask("verify", "verify-key")
	if err != nil {
		t.Fatal(err)
	}
	id = task.ID
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	v = tasks.NewVerifier(s, tasks.VerifierConfig{DataDir: dir, BudgetBytesPerSec: -1, Objects: objects})
	s.SetVerifier(v)
	return dir, id, files, s, v
}

func TestVerifier_APIReportsMissingLocalAndCorruptObject(t *testing.T) {
	t.Run("missing local", func(t *testing.T) {
		dir, id, files, s, v := newAPIVerifier(t, nil)
		files.files[id] = []tasks.BinlogFile{{
			TaskID: id, FileName: "mysql-bin.000001", FilePath: filepath.Join(dir, id, "mysql-bin.000001"),
			State: "SEALED", UploadState: "PENDING", SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}}
		v.Pass(context.Background(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
		assertVerifierFindingAndHealth(t, s, id, assurance.KindLocalMissing)
		body := scrapeVerifierMetrics(t, s)
		if !strings.Contains(body, `binlog_server_finding_open{kind="I6_MISSING_LOCAL",severity="CRITICAL",task_id="`+id+`"}`) {
			t.Fatalf("metrics missing the finding:\n%s", body)
		}
		if !strings.Contains(body, `binlog_server_task_last_verified_timestamp{task_id="`+id+`"}`) {
			t.Fatalf("metrics missing last_verified:\n%s", body)
		}
	})
	t.Run("corrupt object", func(t *testing.T) {
		body := bytes.Repeat([]byte{7}, 32)
		sum := sha256.Sum256(body)
		bad := append([]byte(nil), body...)
		bad[0] ^= 1
		key := "bucket/mysql-bin.000009"
		dir, id, files, s, v := newAPIVerifier(t, verifyMemObjects{obj: map[string][]byte{key: bad}})
		now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
		removed := now.Add(-time.Hour)
		name := "mysql-bin.000009"
		files.files[id] = []tasks.BinlogFile{{
			TaskID: id, FileName: name, FilePath: filepath.Join(dir, id, name), State: "SEALED",
			SizeBytes: 32, SHA256: hex.EncodeToString(sum[:]), UploadState: "UPLOADED", ObjectKey: key,
			UploadedAt: now.Add(-time.Minute), SealedAt: now.Add(-48 * time.Hour), RetentionRemovedAt: &removed,
		}}
		v.Pass(context.Background(), now)
		assertVerifierFindingAndHealth(t, s, id, assurance.KindObjectCorrupt)
		text := scrapeVerifierMetrics(t, s)
		if !strings.Contains(text, `binlog_server_verify_bytes_total{source="object",task_id="`+id+`"}`) {
			t.Fatalf("metrics missing object bytes:\n%s", text)
		}
	})
}

func assertVerifierFindingAndHealth(t *testing.T, s *tasks.Scheduler, id, kind string) {
	t.Helper()
	handler := NewServer(s)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/"+id+"/findings", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("findings status %d body %s", resp.Code, resp.Body.String())
	}
	var items []tasks.Finding
	if err := json.Unmarshal(resp.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range items {
		if string(f.Kind) == kind && f.Open() && f.Severity == tasks.FindingCritical {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET findings missing %s: %+v", kind, items)
	}
	healthResp := httptest.NewRecorder()
	handler.ServeHTTP(healthResp, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if healthResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status %d body %s", healthResp.Code, healthResp.Body.String())
	}
	if !strings.Contains(healthResp.Body.String(), `"degraded"`) || !strings.Contains(healthResp.Body.String(), kind) {
		t.Fatalf("health body %s", healthResp.Body.String())
	}
}

func scrapeVerifierMetrics(t *testing.T, s *tasks.Scheduler) string {
	t.Helper()
	handler := NewServer(s)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("metrics status %d", resp.Code)
	}
	return resp.Body.String()
}
