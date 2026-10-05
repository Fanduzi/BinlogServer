// Package api provides module-level functionality for api.
// input: HTTP GET for the replay window, on-disk segments, catalog rows, and an optional object opener
// output: one ustar of the replay basenames, an empty tar when that window is empty, and an error body when a selected segment cannot be opened
// pos: HTTP coverage for the authenticated replay archive
// note: if this file changes, update this header and module README.md.
package api

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"binlog_server/internal/tasks"
)

func TestTaskAPI_ReplayArchiveMatchesReplayWindow(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	createDownloadTask(t, handler)
	createNamedDownloadTask(t, handler, "cluster-b", "cluster-b-key")

	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{
		"mysql-bin.000001":         []byte("sealed-1"),
		"mysql-bin.000002":         bytes.Repeat([]byte("b"), 600),
		"mysql-bin.000003":         []byte("sealed-3"),
		"mysql-bin.000003.open.e1": []byte("e1-dropped"),
		"mysql-bin.000003.open.e9": []byte("open-e9"),
		"notes.txt":                []byte("notes"),
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(taskDir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	before := replayTempCount(t)
	full := getReplayArchive(t, handler, "/api/tasks/1/replay/archive")
	assertReplayArchiveMatchesJSON(t, handler, "/api/tasks/1/replay", full.order)
	if got := string(full.entries["mysql-bin.000003.open.e9"]); got != "open-e9" {
		t.Fatalf("open bytes %q", got)
	}
	if got := string(full.entries["mysql-bin.000003"]); got != "sealed-3" {
		t.Fatalf("sealed bytes %q", got)
	}
	if _, ok := full.entries["mysql-bin.000003.open.e1"]; ok {
		t.Fatal("lower open epoch kept")
	}
	if _, ok := full.entries["notes.txt"]; ok {
		t.Fatal("non-binlog member")
	}
	if full.disposition != "task-1-replay.tar" {
		t.Fatalf("disposition %s", full.disposition)
	}

	window := getReplayArchive(t, handler, "/api/tasks/1/replay/archive?limit=2")
	assertReplayArchiveMatchesJSON(t, handler, "/api/tasks/1/replay?limit=2", window.order)
	if len(window.entries) != 1 || string(window.entries["mysql-bin.000003.open.e9"]) != "open-e9" {
		t.Fatalf("limit window %+v", window.entries)
	}

	empty := getReplayArchive(t, handler, "/api/tasks/2/replay/archive")
	if len(empty.entries) != 0 || empty.rawLen < 1024 || empty.rawLen%512 != 0 {
		t.Fatalf("empty tar members=%d len=%d", len(empty.entries), empty.rawLen)
	}
	jsonReplay := httptest.NewRecorder()
	handler.ServeHTTP(jsonReplay, httptest.NewRequest(http.MethodGet, "/api/tasks/2/replay", nil))
	if jsonReplay.Code != http.StatusOK || !strings.Contains(jsonReplay.Header().Get("Content-Type"), "application/json") || !strings.Contains(jsonReplay.Body.String(), `"paths":[]`) {
		t.Fatalf("json replay status=%d type=%s body=%s", jsonReplay.Code, jsonReplay.Header().Get("Content-Type"), jsonReplay.Body.String())
	}

	leftDir := t.TempDir()
	leftTask := filepath.Join(leftDir, "9")
	if err := os.MkdirAll(leftTask, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftTask, "mysql-bin.000001"), []byte("leftover"), 0o644); err != nil {
		t.Fatal(err)
	}
	left := NewServer(tasks.NewScheduler(tasks.WithDataDir(leftDir)))
	leftArchive := getReplayArchive(t, left, "/api/tasks/9/replay/archive")
	if leftArchive.disposition != "task-9-replay.tar" || string(leftArchive.entries["mysql-bin.000001"]) != "leftover" || len(leftArchive.entries) != 1 {
		t.Fatalf("leftover archive %+v name %s", leftArchive.entries, leftArchive.disposition)
	}

	assertSegmentStatus(t, handler, http.MethodGet, "/api/tasks/missing/replay/archive", http.StatusNotFound, "task not found")
	assertSegmentStatus(t, handler, http.MethodPost, "/api/tasks/1/replay/archive", http.StatusMethodNotAllowed, "method not allowed")
	one := httptest.NewRecorder()
	handler.ServeHTTP(one, httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/mysql-bin.000003.open.e9", nil))
	if one.Code != http.StatusOK || one.Header().Get("Content-Type") != "application/octet-stream" || !bytes.Equal(one.Body.Bytes(), []byte("open-e9")) {
		t.Fatalf("single download status=%d type=%s body=%q", one.Code, one.Header().Get("Content-Type"), one.Body.Bytes())
	}

	authed := NewServer(scheduler, WithAuth(AuthConfig{
		Enabled:     true,
		Mode:        AuthModeBearer,
		BearerToken: "secret-token",
		ProtectAPI:  true,
	}))
	assertSegmentStatus(t, authed, http.MethodGet, "/api/tasks/1/replay/archive", http.StatusUnauthorized, "")
	okReq := httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay/archive?limit=2", nil)
	okReq.Header.Set("Authorization", "Bearer secret-token")
	okResp := httptest.NewRecorder()
	authed.ServeHTTP(okResp, okReq)
	if okResp.Code != http.StatusOK || okResp.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("authed archive status=%d type=%s", okResp.Code, okResp.Header().Get("Content-Type"))
	}
	if replayTempCount(t) != before {
		t.Fatalf("temp archives leaked: before %d after %d", before, replayTempCount(t))
	}
}

func TestTaskAPI_ReplayArchiveObjectBytesAndNoPartial(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := []byte("catalog-path-secret")
	if err := os.WriteFile(filepath.Join(outside, "mysql-bin.000001"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	objectOne := []byte("object-sealed-one")
	objectTwo := []byte("object-should-lose")
	localTwo := []byte("local-sealed-two")
	openBody := []byte("local-open-e2")
	store := newFakeFileStore()
	store.files["1"] = []tasks.BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000001",
			FilePath:    filepath.Join(outside, "mysql-bin.000001"),
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/mysql-bin.000001",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000002",
			FilePath:    filepath.Join(outside, "mysql-bin.000002"),
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/mysql-bin.000002",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000003",
			FilePath:    filepath.Join(outside, "mysql-bin.000003"),
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/mysql-bin.000003",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000003",
			FilePath:    filepath.Join(dir, "1", "mysql-bin.000003.open.e2"),
			State:       "OPEN",
			UploadState: "LOCAL_ONLY",
		},
	}
	opener := &keyedObjectStore{bodies: map[string][]byte{
		"prefix/mysql-bin.000001": objectOne,
		"prefix/mysql-bin.000002": objectTwo,
		"prefix/mysql-bin.000003": []byte("sealed-3-object"),
	}}
	scheduler := tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(dir), tasks.WithFileUploader(opener))
	handler := NewServer(scheduler)
	createDownloadTask(t, handler)
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), localTwo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003.open.e2"), openBody, 0o644); err != nil {
		t.Fatal(err)
	}

	got := getReplayArchive(t, handler, "/api/tasks/1/replay/archive")
	assertReplayArchiveMatchesJSON(t, handler, "/api/tasks/1/replay", got.order)
	want := map[string]string{
		"mysql-bin.000001":         string(objectOne),
		"mysql-bin.000002":         string(localTwo),
		"mysql-bin.000003":         "sealed-3-object",
		"mysql-bin.000003.open.e2": string(openBody),
	}
	if len(got.entries) != len(want) {
		t.Fatalf("members %+v", got.entries)
	}
	for name, body := range want {
		if string(got.entries[name]) != body {
			t.Fatalf("%s bytes %q", name, got.entries[name])
		}
	}
	if bytes.Contains(got.raw, secret) || bytes.Contains(got.raw, objectTwo) || !bytes.Contains(got.raw, []byte("sealed-3-object")) {
		t.Fatal("archive followed the local catalog path, dropped the sealed object, or included the replaced object")
	}
	if strings.Join(opener.keys, ",") != "prefix/mysql-bin.000001,prefix/mysql-bin.000003" {
		t.Fatalf("object keys %v", opener.keys)
	}

	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay", nil))
	if replay.Code != http.StatusOK || strings.Contains(replay.Header().Get("Content-Type"), "tar") || !strings.Contains(replay.Body.String(), filepath.Join(outside, "mysql-bin.000001")) {
		t.Fatalf("replay json status=%d type=%s body=%s", replay.Code, replay.Header().Get("Content-Type"), replay.Body.String())
	}

	failDir := t.TempDir()
	refused := newFakeFileStore()
	refused.files["1"] = []tasks.BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000001",
			FilePath:    filepath.Join(outside, "mysql-bin.000001"),
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/mysql-bin.000001",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000002",
			FilePath:    filepath.Join(outside, "mysql-bin.000002"),
			State:       "SEALED",
			UploadState: "LOCAL_ONLY",
			ObjectKey:   "prefix/mysql-bin.000002",
		},
	}
	partial := &keyedObjectStore{bodies: map[string][]byte{"prefix/mysql-bin.000001": objectOne}}
	partialHandler := NewServer(tasks.NewScheduler(tasks.WithFileStore(refused), tasks.WithDataDir(failDir), tasks.WithFileUploader(partial)))
	createDownloadTask(t, partialHandler)
	before := replayTempCount(t)
	resp := httptest.NewRecorder()
	partialHandler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay/archive", nil))
	if resp.Code != http.StatusNotFound || !strings.Contains(resp.Body.String(), "segment not found on this process") || strings.Contains(resp.Header().Get("Content-Type"), "tar") || bytes.Contains(resp.Body.Bytes(), objectOne) {
		t.Fatalf("partial status=%d type=%s body=%q", resp.Code, resp.Header().Get("Content-Type"), resp.Body.Bytes())
	}
	if replayTempCount(t) != before {
		t.Fatalf("partial temp leaked")
	}

	for _, state := range []string{"UPLOAD_FAILED", "LOCAL_ONLY"} {
		row := refused.files["1"][0]
		row.UploadState = state
		only := newFakeFileStore()
		only.files["1"] = []tasks.BinlogFile{row}
		h := NewServer(tasks.NewScheduler(tasks.WithFileStore(only), tasks.WithDataDir(failDir), tasks.WithFileUploader(partial)))
		createDownloadTask(t, h)
		assertSegmentStatus(t, h, http.MethodGet, "/api/tasks/1/replay/archive", http.StatusNotFound, "segment not found on this process")
	}
	emptyKey := refused.files["1"][0]
	emptyKey.UploadState = "UPLOADED"
	emptyKey.ObjectKey = ""
	only := newFakeFileStore()
	only.files["1"] = []tasks.BinlogFile{emptyKey}
	h := NewServer(tasks.NewScheduler(tasks.WithFileStore(only), tasks.WithDataDir(failDir), tasks.WithFileUploader(partial)))
	createDownloadTask(t, h)
	assertSegmentStatus(t, h, http.MethodGet, "/api/tasks/1/replay/archive", http.StatusNotFound, "segment not found on this process")

	openOnly := newFakeFileStore()
	openOnly.files["1"] = []tasks.BinlogFile{{
		TaskID:      "1",
		FileName:    "mysql-bin.000005",
		FilePath:    filepath.Join(outside, "mysql-bin.000005.open.e1"),
		State:       "OPEN",
		UploadState: "UPLOADED",
		ObjectKey:   "prefix/mysql-bin.000005.open.e1",
	}}
	openKeys := &keyedObjectStore{bodies: map[string][]byte{"prefix/mysql-bin.000005.open.e1": []byte("invented-open")}}
	openHandler := NewServer(tasks.NewScheduler(tasks.WithFileStore(openOnly), tasks.WithDataDir(failDir), tasks.WithFileUploader(openKeys)))
	createDownloadTask(t, openHandler)
	assertSegmentStatus(t, openHandler, http.MethodGet, "/api/tasks/1/replay/archive", http.StatusNotFound, "segment not found on this process")
	if len(openKeys.keys) != 0 {
		t.Fatalf("open segment read from object %v", openKeys.keys)
	}

	plainDir := t.TempDir()
	plain := NewServer(tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(plainDir)))
	createDownloadTask(t, plain)
	assertSegmentStatus(t, plain, http.MethodGet, "/api/tasks/1/replay/archive", http.StatusNotFound, "segment not found on this process")

	broken := &brokenObjectStore{}
	brokenStore := newFakeFileStore()
	brokenStore.files["1"] = []tasks.BinlogFile{{
		TaskID:      "1",
		FileName:    "mysql-bin.000001",
		FilePath:    filepath.Join(outside, "mysql-bin.000001"),
		State:       "SEALED",
		UploadState: "UPLOADED",
		ObjectKey:   "prefix/mysql-bin.000001",
	}}
	brokenHandler := NewServer(tasks.NewScheduler(tasks.WithFileStore(brokenStore), tasks.WithDataDir(failDir), tasks.WithFileUploader(broken)))
	createDownloadTask(t, brokenHandler)
	before = replayTempCount(t)
	broke := httptest.NewRecorder()
	brokenHandler.ServeHTTP(broke, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay/archive", nil))
	if broke.Code != http.StatusInternalServerError || strings.Contains(broke.Header().Get("Content-Type"), "tar") || bytes.Contains(broke.Body.Bytes(), []byte("ustar")) || bytes.Contains(broke.Body.Bytes(), objectOne) {
		t.Fatalf("read failure status=%d type=%s body=%q", broke.Code, broke.Header().Get("Content-Type"), broke.Body.Bytes())
	}
	if replayTempCount(t) != before {
		t.Fatal("read failure left a temp archive")
	}
}

type keyedObjectStore struct {
	bodies map[string][]byte
	keys   []string
}

func (s *keyedObjectStore) UploadFile(context.Context, string, string, string) error { return nil }

func (s *keyedObjectStore) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	s.keys = append(s.keys, key)
	body, ok := s.bodies[key]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte{}, body...))), int64(len(body)), nil
}

type brokenObjectStore struct{}

func (brokenObjectStore) UploadFile(context.Context, string, string, string) error { return nil }

func (brokenObjectStore) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(errReader{}), 4, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read broke") }

func createNamedDownloadTask(t *testing.T, handler http.Handler, name, key string) {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(fmt.Sprintf(`{"name":%q,"cluster_key":%q,"source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, name, key)))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
}

type replayArchiveResponse struct {
	entries     map[string][]byte
	order       []string
	raw         []byte
	rawLen      int
	disposition string
}

func getReplayArchive(t *testing.T, handler http.Handler, path string) replayArchiveResponse {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, resp.Code, resp.Body.String())
	}
	if resp.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("%s type %s", path, resp.Header().Get("Content-Type"))
	}
	if resp.Header().Get("Content-Length") != strconv.Itoa(resp.Body.Len()) {
		t.Fatalf("%s length header=%s body=%d", path, resp.Header().Get("Content-Length"), resp.Body.Len())
	}
	_, params, err := mime.ParseMediaType(resp.Header().Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("%s disposition %q", path, resp.Header().Get("Content-Disposition"))
	}
	entries, order := readUstar(t, resp.Body.Bytes())
	return replayArchiveResponse{
		entries:     entries,
		order:       order,
		raw:         append([]byte{}, resp.Body.Bytes()...),
		rawLen:      resp.Body.Len(),
		disposition: params["filename"],
	}
}

func assertReplayArchiveMatchesJSON(t *testing.T, handler http.Handler, path string, order []string) {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%s status=%d type=%s", path, resp.Code, resp.Header().Get("Content-Type"))
	}
	body := getReplay(t, handler, path)
	want := make([]string, 0, len(body.Paths))
	for _, filePath := range body.Paths {
		want = append(want, filepath.Base(filePath))
	}
	if strings.Join(want, "\n") != strings.Join(order, "\n") {
		t.Fatalf("%s\nwant %v\ngot  %v", path, want, order)
	}
}

func readUstar(t *testing.T, raw []byte) (map[string][]byte, []string) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	out := map[string][]byte{}
	order := make([]string, 0)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Format != tar.FormatUSTAR {
			t.Fatalf("member %s format %v", hdr.Name, hdr.Format)
		}
		if hdr.Name != filepath.Base(hdr.Name) || strings.Contains(hdr.Name, "/") {
			t.Fatalf("member name %q", hdr.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := out[hdr.Name]; ok {
			t.Fatalf("duplicate %s", hdr.Name)
		}
		out[hdr.Name] = body
		order = append(order, hdr.Name)
	}
	return out, order
}

func replayTempCount(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "binlog-replay-*.tar"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}
