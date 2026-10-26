// Package api proves UPLOAD_TLS on GET /api/health follows the current transport.
// input: a scheduler whose startup probe detail is an x509 error, then a retry that stores the object
// output: 503 with UPLOAD_TLS while the probe line is current, then 200 after a checksum-matching retry succeeds (resolveFinding, not the empty-checksum report)
// pos: HTTP proof that a fixed CA or endpoint does not leave /api/health failed until restart
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

func TestAPIHealth_UploadTLSClearsWhenRetrySucceeds(t *testing.T) {
	const tlsLine = `object storage probe: Get "https://127.0.0.1:9000": x509: certificate signed by unknown authority`
	dir := t.TempDir()
	name := "mysql-bin.000093"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &uploadTLSFiles{}
	s := tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithFileUploader(uploadTLSPut{}))
	task, err := s.CreateTask("cluster-a", "cluster-a-key")
	if err != nil {
		t.Fatal(err)
	}
	store.files = []tasks.BinlogFile{{
		TaskID: task.ID, FileName: name, FilePath: path,
		SealedAt: time.Now(), UploadState: "UPLOAD_FAILED", ObjectKey: "prefix/c/u/" + name,
	}}
	s.SetVerifyStorageDetail(tlsLine)
	handler := NewServer(s)

	before := getHealth(t, handler)
	if before.code != http.StatusServiceUnavailable || before.status != "degraded" || !strings.Contains(strings.Join(before.reasons, " "), "UPLOAD_TLS") {
		t.Fatalf("before %+v", before)
	}
	if _, err := s.RetryFailedUploads(task.ID, 10); err != nil {
		t.Fatal(err)
	}
	after := getHealth(t, handler)
	if after.code != http.StatusOK || after.status != "ok" || strings.Contains(strings.Join(after.reasons, " "), "UPLOAD_TLS") {
		t.Fatalf("after %+v", after)
	}
}

type healthBody struct {
	code    int
	status  string
	reasons []string
}

func getHealth(t *testing.T, handler http.Handler) healthBody {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var body struct {
		Status  string   `json:"status"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return healthBody{code: resp.Code, status: body.Status, reasons: body.Reasons}
}

type uploadTLSFiles struct {
	files []tasks.BinlogFile
}

func (s *uploadTLSFiles) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	for i := range s.files {
		if s.files[i].FileName == meta.FileName {
			s.files[i] = meta
			return nil
		}
	}
	s.files = append(s.files, meta)
	return nil
}

func (s *uploadTLSFiles) ListBinlogFiles(context.Context, string, int) ([]tasks.BinlogFile, error) {
	out := make([]tasks.BinlogFile, len(s.files))
	copy(out, s.files)
	return out, nil
}

func (s *uploadTLSFiles) ListFailedUploadBinlogFiles(_ context.Context, _ string, limit int) ([]tasks.BinlogFile, error) {
	return tasks.FailedUploadFiles(s.files, limit), nil
}

type uploadTLSPut struct{}

func (uploadTLSPut) UploadFile(context.Context, string, string, string) error { return nil }

func (uploadTLSPut) SealedObjectMatches(context.Context, string, string) (bool, error) {
	return true, nil
}
