// Package api provides module-level functionality for api.
// input: HTTP GET /api/tasks/{id}/replay and /replay/archive with stop_datetime and start_datetime
// output: assertions for the point-in-time paths, command flags, 400 datetime errors, flavor client, and the matching tar
// pos: HTTP coverage for the datetime restore window on the existing replay routes
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

func TestTaskAPI_PITRReplay(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	create(`{"name":"mysql","cluster_key":"mysql-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	create(`{"name":"maria","cluster_key":"maria-key","source":{"host":"127.0.0.1","port":3307,"user":"repl","password":"secret","flavor":"mariadb"}}`, t, handler)

	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, stamps ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(taskDir, name), apiTimedSegment(stamps...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mysql-bin.000001", "2024-01-01 00:00:00", "2024-01-01 00:30:00")
	write("mysql-bin.000002", "2024-01-01 01:00:00", "2024-01-01 01:30:00")
	write("mysql-bin.000002.open.e1", "2024-01-01 01:10:00")
	write("mysql-bin.000002.open.e8", "2024-01-01 01:00:00", "2024-01-01 01:40:00")
	write("mysql-bin.000003", "2024-01-01 02:00:00", "2024-01-01 02:30:00")

	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay?limit=1", nil))
	if plain.Code != http.StatusOK || strings.Contains(plain.Body.String(), `"command"`) || !strings.Contains(plain.Body.String(), "mysql-bin.000003") || strings.Contains(plain.Body.String(), "mysql-bin.000001") {
		t.Fatalf("limit replay status=%d body=%s", plain.Code, plain.Body.String())
	}

	stopOnly := getPITR(t, handler, "/api/tasks/1/replay", url.Values{"stop_datetime": {"2024-01-01 01:20:00"}, "limit": {"1"}})
	if stopOnly.Client != "mysqlbinlog" || stopOnly.ClientHint != "MySQL mysqlbinlog" {
		t.Fatalf("client %+v", stopOnly)
	}
	wantStop := []string{
		filepath.Join(taskDir, "mysql-bin.000001"),
		filepath.Join(taskDir, "mysql-bin.000002"),
		filepath.Join(taskDir, "mysql-bin.000002.open.e8"),
	}
	if strings.Join(stopOnly.Paths, "\n") != strings.Join(wantStop, "\n") {
		t.Fatalf("stop-only paths %v", stopOnly.Paths)
	}
	if !strings.Contains(stopOnly.Command, "--stop-datetime='2024-01-01 01:20:00'") || strings.Contains(stopOnly.Command, "--start-datetime=") || !strings.HasPrefix(stopOnly.Command, "TZ=UTC mysqlbinlog \\\n") {
		t.Fatalf("stop-only command %s", stopOnly.Command)
	}
	for _, path := range wantStop {
		if !strings.Contains(stopOnly.Command, path) {
			t.Fatalf("command missing %s\n%s", path, stopOnly.Command)
		}
	}
	if strings.Contains(stopOnly.Command, "open.e1") || strings.Contains(stopOnly.Command, "mysql-bin.000003") {
		t.Fatalf("command %s", stopOnly.Command)
	}

	window := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"start_datetime": {"2024-01-01T01:20:00Z"},
		"stop_datetime":  {"2024-01-01 02:10:00"},
	})
	wantWindow := []string{
		filepath.Join(taskDir, "mysql-bin.000002"),
		filepath.Join(taskDir, "mysql-bin.000002.open.e8"),
		filepath.Join(taskDir, "mysql-bin.000003"),
	}
	if strings.Join(window.Paths, "\n") != strings.Join(wantWindow, "\n") {
		t.Fatalf("window paths %v", window.Paths)
	}
	if !strings.Contains(window.Command, "--start-datetime='2024-01-01 01:20:00'") || !strings.Contains(window.Command, "--stop-datetime='2024-01-01 02:10:00'") {
		t.Fatalf("window command %s", window.Command)
	}

	empty := getPITR(t, handler, "/api/tasks/1/replay", url.Values{"stop_datetime": {"2020-01-01 00:00:00"}})
	if len(empty.Paths) != 0 || empty.Command != "" || empty.Client != "mysqlbinlog" {
		t.Fatalf("empty %+v", empty)
	}
	equal := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"start_datetime": {"2024-01-01T01:20:00Z"},
		"stop_datetime":  {"2024-01-01 01:20:00"},
	})
	if len(equal.Paths) != 0 || equal.Command != "" || equal.Flavor != "mysql" || equal.Client != "mysqlbinlog" || equal.ClientHint != "MySQL mysqlbinlog" {
		t.Fatalf("equal window %+v", equal)
	}

	mariaDir := filepath.Join(dir, "2")
	if err := os.MkdirAll(mariaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mariaDir, "mysql-bin.000010"), apiTimedSegment("2024-01-01 00:00:00"), 0o644); err != nil {
		t.Fatal(err)
	}
	maria := getPITR(t, handler, "/api/tasks/2/replay", url.Values{"stop_datetime": {"2024-01-01 00:30:00"}})
	if maria.Flavor != "mariadb" || maria.Client != "mariadb-binlog" || maria.ClientHint != "mariadb-binlog" || !strings.HasPrefix(maria.Command, "TZ=UTC mariadb-binlog \\\n") {
		t.Fatalf("maria %+v", maria)
	}

	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_datetime": {"yesterday"}}, http.StatusBadRequest, "invalid stop_datetime")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_datetime": {""}}, http.StatusBadRequest, "invalid stop_datetime")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"start_datetime": {"2024-01-01 00:00:00"}}, http.StatusBadRequest, "stop_datetime is required")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_datetime": {"not-a-time"},
		"stop_datetime":  {"2024-01-01 00:00:00"},
	}, http.StatusBadRequest, "invalid start_datetime")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_datetime": {"2024-01-02 00:00:00"},
		"stop_datetime":  {"2024-01-01 00:00:00"},
	}, http.StatusBadRequest, "start_datetime is after stop_datetime")
	assertPITRStatus(t, handler, "/api/tasks/missing/replay", url.Values{"stop_datetime": {"2024-01-01 00:00:00"}}, http.StatusNotFound, "task not found")

	archive := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{"stop_datetime": {"2024-01-01 01:20:00"}}))
	if strings.Join(archive.order, "\n") != "mysql-bin.000001\nmysql-bin.000002\nmysql-bin.000002.open.e8" {
		t.Fatalf("archive order %v", archive.order)
	}
	early := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{"stop_datetime": {"2020-01-01 00:00:00"}}))
	if len(early.entries) != 0 || len(early.order) != 0 {
		t.Fatalf("empty archive %+v", early.order)
	}
}

func TestTaskAPI_PITRArchiveUsesUploadedObject(t *testing.T) {
	dir := t.TempDir()
	objectBody := apiTimedSegment("2024-01-01 03:00:00", "2024-01-01 03:30:00")
	store := newFakeFileStore()
	store.files["1"] = []tasks.BinlogFile{{
		TaskID:      "1",
		FileName:    "mysql-bin.000004",
		FilePath:    "/data/1/mysql-bin.000004",
		State:       "SEALED",
		UploadState: "UPLOADED",
		ObjectKey:   "prefix/mysql-bin.000004",
	}}
	opener := &keyedObjectStore{bodies: map[string][]byte{"prefix/mysql-bin.000004": objectBody}}
	handler := NewServer(tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(dir), tasks.WithFileUploader(opener)))
	createDownloadTask(t, handler)

	got := getPITR(t, handler, "/api/tasks/1/replay", url.Values{"stop_datetime": {"2024-01-01 03:20:00"}})
	if len(got.Paths) != 1 || got.Paths[0] != "/data/1/mysql-bin.000004" || !strings.Contains(got.Command, "--stop-datetime='2024-01-01 03:20:00'") {
		t.Fatalf("object pitr %+v", got)
	}
	archive := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{"stop_datetime": {"2024-01-01 03:20:00"}}))
	if string(archive.entries["mysql-bin.000004"]) != string(objectBody) {
		t.Fatalf("archive bytes %q", archive.entries["mysql-bin.000004"])
	}

	missing := newFakeFileStore()
	missing.files["1"] = []tasks.BinlogFile{{
		TaskID:      "1",
		FileName:    "mysql-bin.000004",
		FilePath:    "/data/1/mysql-bin.000004",
		State:       "SEALED",
		UploadState: "LOCAL_ONLY",
	}}
	gone := NewServer(tasks.NewScheduler(tasks.WithFileStore(missing), tasks.WithDataDir(t.TempDir())))
	createDownloadTask(t, gone)
	assertPITRStatus(t, gone, "/api/tasks/1/replay", url.Values{"stop_datetime": {"2024-01-01 03:20:00"}}, http.StatusNotFound, "segment not found on this process")
}

type pitrBody struct {
	Flavor     string   `json:"flavor"`
	Client     string   `json:"client"`
	ClientHint string   `json:"client_hint"`
	Paths      []string `json:"paths"`
	Command    string   `json:"command"`
}

func getPITR(t *testing.T, handler http.Handler, path string, query url.Values) pitrBody {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, pitrPath(t, path, query), nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, resp.Code, resp.Body.String())
	}
	var body pitrBody
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Paths == nil {
		t.Fatalf("nil paths %s", resp.Body.String())
	}
	return body
}

func assertPITRStatus(t *testing.T, handler http.Handler, path string, query url.Values, status int, text string) {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, pitrPath(t, path, query), nil))
	if resp.Code != status || !strings.Contains(resp.Body.String(), text) {
		t.Fatalf("%s status=%d body=%s", path, resp.Code, resp.Body.String())
	}
}

func pitrPath(t *testing.T, path string, query url.Values) string {
	t.Helper()
	return path + "?" + query.Encode()
}

func create(body string, t *testing.T, handler http.Handler) {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func apiTimedSegment(stamps ...string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	pos := uint32(4)
	for _, stamp := range stamps {
		parsed, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, time.UTC)
		if err != nil {
			panic(err)
		}
		const payload = 1
		size := uint32(19 + payload)
		pos += size
		var hdr [19]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(parsed.Unix()))
		hdr[4] = 2
		binary.LittleEndian.PutUint32(hdr[5:9], 1)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], pos)
		buf.Write(hdr[:])
		buf.WriteByte(0)
	}
	return buf.Bytes()
}
