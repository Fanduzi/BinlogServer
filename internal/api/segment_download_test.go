// Package api provides module-level functionality for api.
// input: HTTP GET for one task segment basename, auth headers, and on-disk or catalog inventory
// output: raw segment bytes with the on-disk filename, 400 for traversal names, and 404 when the task or local file is absent
// pos: HTTP coverage for authenticated inventory segment download
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"binlog_server/internal/tasks"
)

func TestTaskAPI_DownloadSegment(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	createDownloadTask(t, handler)

	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealed := []byte("sealed-seg")
	openBody := []byte("open-seg-e1")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), sealed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000004.open.e1"), openBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "notes.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "secret")
	if err := os.WriteFile(outside, []byte("secret-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(taskDir, "mysql-bin.000008")); err != nil {
		t.Fatal(err)
	}

	assertSegmentDownload(t, handler, "/api/tasks/1/files/mysql-bin.000003", "mysql-bin.000003", sealed)
	assertSegmentDownload(t, handler, "/api/tasks/1/files/mysql-bin.000004.open.e1", "mysql-bin.000004.open.e1", openBody)

	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/1/files/mysql-bin.000004", http.StatusNotFound, "segment not found on this process")
	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/1/files/notes.txt", http.StatusNotFound, "segment not found on this process")
	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/missing/files/mysql-bin.000003", http.StatusNotFound, "task not found")
	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/1/files/mysql-bin.000008", http.StatusBadRequest, "invalid segment name")
	assertSegmentStatus(t, handler, http.MethodPost, "/api/tasks/1/files/mysql-bin.000003", http.StatusMethodNotAllowed, "method not allowed")
	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/1/files/retry-upload", http.StatusMethodNotAllowed, "method not allowed")

	for _, raw := range []string{
		"/api/tasks/1/files/..",
		"/api/tasks/1/files/../secret",
		"/api/tasks/1/files/%2e%2e",
		"/api/tasks/1/files/foo/bar",
		"/api/tasks/1/files/" + url.PathEscape(`..\secret`),
		"/api/tasks/1/files/" + url.PathEscape("mysql-bin.000003/../secret"),
	} {
		assertSegmentStatus(t, handler, http.MethodGet, raw, http.StatusBadRequest, "invalid segment name")
	}

	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay", nil))
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "mysql-bin.000004.open.e1") || strings.Contains(replay.Header().Get("Content-Type"), "octet-stream") {
		t.Fatalf("replay status=%d type=%s body=%s", replay.Code, replay.Header().Get("Content-Type"), replay.Body.String())
	}

	authed := NewServer(scheduler, WithAuth(AuthConfig{
		Enabled:     true,
		Mode:        AuthModeBearer,
		BearerToken: "secret-token",
		ProtectAPI:  true,
	}))
	assertSegmentStatus(t, authed, http.MethodGet, "/api/tasks/1/files/mysql-bin.000003", http.StatusUnauthorized, "")
	okReq := httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/mysql-bin.000004.open.e1", nil)
	okReq.Header.Set("Authorization", "Bearer secret-token")
	okResp := httptest.NewRecorder()
	authed.ServeHTTP(okResp, okReq)
	if okResp.Code != http.StatusOK || !bytes.Equal(okResp.Body.Bytes(), openBody) {
		t.Fatalf("authed download status=%d body=%q", okResp.Code, okResp.Body.Bytes())
	}
}

func TestTaskAPI_DownloadSegmentControlPlaneHasNoLocalBytes(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := []byte("do-not-serve-catalog-bytes")
	if err := os.WriteFile(filepath.Join(outside, "mysql-bin.000001"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	store := newFakeFileStore()
	store.files["1"] = []tasks.BinlogFile{{
		TaskID:   "1",
		FileName: "mysql-bin.000001",
		FilePath: filepath.Join(outside, "mysql-bin.000001"),
		State:    "SEALED",
	}, {
		TaskID:   "1",
		FileName: "mysql-bin.000002",
		FilePath: filepath.Join(outside, "mysql-bin.000002.open.e4"),
		State:    "OPEN",
	}}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	createDownloadTask(t, handler)

	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000099"), []byte("extra-on-disk"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/api/tasks/1/files/mysql-bin.000001",
		"/api/tasks/1/files/mysql-bin.000002.open.e4",
		"/api/tasks/1/files/mysql-bin.000099",
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "segment not found on this process") || bytes.Contains(resp.Body.Bytes(), secret) {
			t.Fatalf("%s status=%d body=%q", path, resp.Code, resp.Body.Bytes())
		}
	}

	local := []byte("local-open-epoch")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002.open.e4"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	assertSegmentDownload(t, handler, "/api/tasks/1/files/mysql-bin.000002.open.e4", "mysql-bin.000002.open.e4", local)

	plane := NewServer(tasks.NewScheduler(tasks.WithFileStore(store)))
	createDownloadTask(t, plane)
	assertSegmentStatus(t, plane, http.MethodGet, "/api/tasks/1/files/mysql-bin.000001", http.StatusNotFound, "segment not found on this process")
}

func createDownloadTask(t *testing.T, handler http.Handler) {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(`{"name":"cluster-a","cluster_key":"cluster-a-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func assertSegmentDownload(t *testing.T, handler http.Handler, path, name string, want []byte) {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, resp.Code, resp.Body.String())
	}
	if resp.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("%s type %s", path, resp.Header().Get("Content-Type"))
	}
	if resp.Header().Get("Content-Length") != strconv.Itoa(len(want)) || resp.Body.Len() != len(want) {
		t.Fatalf("%s length header=%s body=%d", path, resp.Header().Get("Content-Length"), resp.Body.Len())
	}
	if !bytes.Equal(resp.Body.Bytes(), want) {
		t.Fatalf("%s body %q", path, resp.Body.Bytes())
	}
	_, params, err := mime.ParseMediaType(resp.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != name {
		t.Fatalf("%s disposition %q err %v", path, resp.Header().Get("Content-Disposition"), err)
	}
}

func assertSegmentStatus(t *testing.T, handler http.Handler, method, raw string, status int, bodyPart string) {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(method, raw, nil))
	if resp.Code != status || (bodyPart != "" && !strings.Contains(resp.Body.String(), bodyPart)) {
		t.Fatalf("%s %s status=%d body=%q want %d %q", method, raw, resp.Code, resp.Body.String(), status, bodyPart)
	}
}
