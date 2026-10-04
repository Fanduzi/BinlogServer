// Package api provides module-level functionality for api.
// input: HTTP GET for one task segment basename, auth headers, on-disk or catalog inventory, and an optional object opener
// output: raw segment bytes with the basename, object bytes when a sealed UPLOADED row has no local file, 400 for traversal names, and 404 when the task or bytes are absent
// pos: HTTP coverage for authenticated inventory segment download
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"context"
	"io"
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

type scriptedObjectStore struct {
	bodies map[string][]byte
	keys   []string
}

func (s *scriptedObjectStore) UploadFile(context.Context, string, string, string) error {
	return nil
}

func (s *scriptedObjectStore) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	s.keys = append(s.keys, key)
	body, ok := s.bodies[key]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	payload := append(append([]byte{}, body...), []byte("EXTRA-NOT-IN-SIZE")...)
	return io.NopCloser(bytes.NewReader(payload)), int64(len(body)), nil
}

func TestTaskAPI_DownloadUploadedSegmentWhenLocalMissing(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := []byte("catalog-path-secret")
	if err := os.WriteFile(filepath.Join(outside, "mysql-bin.000001"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	objectBody := []byte("object-sealed-bytes")
	store := newFakeFileStore()
	store.files["1"] = []tasks.BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000001",
			FilePath:    filepath.Join(outside, "mysql-bin.000001"),
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/cluster/uuid/mysql-bin.000001",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000002",
			FilePath:    filepath.Join(outside, "mysql-bin.000002"),
			State:       "SEALED",
			UploadState: "LOCAL_ONLY",
			ObjectKey:   "prefix/cluster/uuid/mysql-bin.000002",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000003",
			FilePath:    filepath.Join(outside, "mysql-bin.000003"),
			State:       "SEALED",
			UploadState: "UPLOAD_FAILED",
			ObjectKey:   "prefix/cluster/uuid/mysql-bin.000003",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000004",
			FilePath:    filepath.Join(outside, "mysql-bin.000004"),
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000005",
			FilePath:    filepath.Join(outside, "mysql-bin.000005.open.e2"),
			State:       "OPEN",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/cluster/uuid/mysql-bin.000005.open.e2",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000006",
			FilePath:    filepath.Join(outside, "mysql-bin.000006"),
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/cluster/uuid/mysql-bin.000006",
		},
	}
	opener := &scriptedObjectStore{bodies: map[string][]byte{
		"prefix/cluster/uuid/mysql-bin.000001": objectBody,
		"prefix/cluster/uuid/mysql-bin.000006": []byte("object-should-lose-to-local"),
	}}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(dir), tasks.WithFileUploader(opener))
	handler := NewServer(scheduler)
	createDownloadTask(t, handler)
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	local := []byte("local-sealed")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000006"), local, 0o644); err != nil {
		t.Fatal(err)
	}

	assertSegmentDownload(t, handler, "/api/tasks/1/files/mysql-bin.000001", "mysql-bin.000001", objectBody)
	assertSegmentDownload(t, handler, "/api/tasks/1/files/mysql-bin.000006", "mysql-bin.000006", local)
	if len(opener.keys) != 1 || opener.keys[0] != "prefix/cluster/uuid/mysql-bin.000001" {
		t.Fatalf("object keys %v", opener.keys)
	}
	for _, path := range []string{
		"/api/tasks/1/files/mysql-bin.000002",
		"/api/tasks/1/files/mysql-bin.000003",
		"/api/tasks/1/files/mysql-bin.000004",
		"/api/tasks/1/files/mysql-bin.000005.open.e2",
	} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "segment not found on this process") || bytes.Contains(resp.Body.Bytes(), secret) || bytes.Contains(resp.Body.Bytes(), objectBody) {
			t.Fatalf("%s status=%d body=%q", path, resp.Code, resp.Body.Bytes())
		}
	}
	if len(opener.keys) != 1 {
		t.Fatalf("object store called for a refused row: %v", opener.keys)
	}
	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/1/files/../secret", http.StatusBadRequest, "invalid segment name")
	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/missing/files/mysql-bin.000001", http.StatusNotFound, "task not found")

	filesResp := httptest.NewRecorder()
	handler.ServeHTTP(filesResp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/files", nil))
	if filesResp.Code != http.StatusOK || strings.Contains(filesResp.Header().Get("Content-Type"), "octet-stream") || !strings.Contains(filesResp.Body.String(), `"upload_state":"UPLOADED"`) || !strings.Contains(filesResp.Body.String(), "mysql-bin.000001") {
		t.Fatalf("files status=%d type=%s body=%s", filesResp.Code, filesResp.Header().Get("Content-Type"), filesResp.Body.String())
	}
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay", nil))
	if replay.Code != http.StatusOK || strings.Contains(replay.Header().Get("Content-Type"), "octet-stream") || !strings.Contains(replay.Body.String(), filepath.Join(outside, "mysql-bin.000001")) || strings.Contains(replay.Body.String(), string(objectBody)) {
		t.Fatalf("replay status=%d type=%s body=%s", replay.Code, replay.Header().Get("Content-Type"), replay.Body.String())
	}

	authed := NewServer(scheduler, WithAuth(AuthConfig{
		Enabled:     true,
		Mode:        AuthModeBearer,
		BearerToken: "secret-token",
		ProtectAPI:  true,
	}))
	assertSegmentStatus(t, authed, http.MethodGet, "/api/tasks/1/files/mysql-bin.000001", http.StatusUnauthorized, "")
	okReq := httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/mysql-bin.000001", nil)
	okReq.Header.Set("Authorization", "Bearer secret-token")
	okResp := httptest.NewRecorder()
	authed.ServeHTTP(okResp, okReq)
	if okResp.Code != http.StatusOK || !bytes.Equal(okResp.Body.Bytes(), objectBody) {
		t.Fatalf("authed object status=%d body=%q", okResp.Code, okResp.Body.Bytes())
	}

	plain := NewServer(tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(dir)))
	createDownloadTask(t, plain)
	assertSegmentStatus(t, plain, http.MethodGet, "/api/tasks/1/files/mysql-bin.000001", http.StatusNotFound, "segment not found on this process")

	uploadOnly := NewServer(tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(dir), tasks.WithFileUploader(&fakeRetryUploader{})))
	createDownloadTask(t, uploadOnly)
	assertSegmentStatus(t, uploadOnly, http.MethodGet, "/api/tasks/1/files/mysql-bin.000001", http.StatusNotFound, "segment not found on this process")

	planeOpener := &scriptedObjectStore{bodies: map[string][]byte{
		"prefix/cluster/uuid/mysql-bin.000001": objectBody,
	}}
	plane := NewServer(tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithFileUploader(planeOpener)))
	createDownloadTask(t, plane)
	assertSegmentDownload(t, plane, "/api/tasks/1/files/mysql-bin.000001", "mysql-bin.000001", objectBody)
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
