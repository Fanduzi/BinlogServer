// Package api provides module-level functionality for api.
// input: HTTP GET /api/tasks/{id}/replay and /replay/archive with stop_gtid, including a segment that exists only as an uploaded object
// output: assertions for the stop-position command, the matching tar, plain-text 400 sentences, and the existing 404 when the segment cannot be opened
// pos: HTTP coverage for stopping replay before one MySQL GTID
// note: if this file changes, update this header and module README.md.
package api

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestTaskAPI_GTIDReplay(t *testing.T) {
	dir := t.TempDir()
	scheduler := tasks.NewScheduler(tasks.WithDataDir(dir))
	handler := NewServer(scheduler)
	create(`{"name":"mysql","cluster_key":"mysql-key","source":{"host":"127.0.0.1","port":3306,"user":"repl","password":"secret","flavor":"mysql"}}`, t, handler)
	create(`{"name":"maria","cluster_key":"maria-key","source":{"host":"127.0.0.1","port":3307,"user":"repl","password":"secret","flavor":"mariadb"}}`, t, handler)

	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2024, 1, 1, 1, 20, 0, 0, time.UTC)
	body, off := apiGTIDSegment(t, when, 7, 8)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002.open.e3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), apiGTIDSegmentBytes(t, when.Add(time.Hour), 9), 0o644); err != nil {
		t.Fatal(err)
	}

	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay?limit=1", nil))
	if plain.Code != http.StatusOK || strings.Contains(plain.Body.String(), `"command"`) {
		t.Fatalf("limit replay status=%d body=%s", plain.Code, plain.Body.String())
	}

	got := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"stop_gtid": {apiGTIDUUID + ":8"},
		"limit":     {"1"},
	})
	if len(got.Paths) != 1 || !strings.HasSuffix(got.Paths[0], "mysql-bin.000002.open.e3") {
		t.Fatalf("paths %v", got.Paths)
	}
	if !strings.Contains(got.Command, "--stop-position="+strconv.FormatUint(off, 10)) || strings.Contains(got.Command, "mysql-bin.000003") || strings.Contains(got.Command, "--stop-datetime=") || !strings.HasPrefix(got.Command, "TZ=UTC mysqlbinlog \\\n") {
		t.Fatalf("command %s", got.Command)
	}

	window := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"stop_gtid":      {apiGTIDUUID + ":8"},
		"start_datetime": {"2024-01-01 01:20:00"},
	})
	if !strings.Contains(window.Command, "--start-datetime='2024-01-01 01:20:00'") || !strings.Contains(window.Command, "--stop-position=") {
		t.Fatalf("window %s", window.Command)
	}

	archive := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{"stop_gtid": {apiGTIDUUID + ":8"}}))
	if strings.Join(archive.order, "\n") != "mysql-bin.000002.open.e3" {
		t.Fatalf("archive %v", archive.order)
	}

	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_gtid": {"not-a-gtid"}}, http.StatusBadRequest, "invalid stop_gtid")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_gtid": {""}}, http.StatusBadRequest, "invalid stop_gtid")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_gtid": {"  "}}, http.StatusBadRequest, "invalid stop_gtid")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_gtid": {"0-1-10"}}, http.StatusBadRequest, "invalid stop_gtid")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"stop_gtid":     {apiGTIDUUID + ":8"},
		"stop_datetime": {"2024-01-01 01:20:00"},
	}, http.StatusBadRequest, "stop_datetime and stop_gtid cannot both be set")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"stop_gtid":      {apiGTIDUUID + ":8"},
		"start_datetime": {"not-a-time"},
	}, http.StatusBadRequest, "invalid start_datetime")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"stop_gtid":      {apiGTIDUUID + ":8"},
		"start_datetime": {"2024-01-01 01:20:01"},
	}, http.StatusBadRequest, "start_datetime is after stop_gtid")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_gtid": {apiGTIDUUID + ":99"}}, http.StatusBadRequest, "stop_gtid is not in this task's backed-up range")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"stop_gtid": {"00000000-0000-0000-0000-000000000001:1"}}, http.StatusBadRequest, "stop_gtid is not in this task's backed-up range")
	assertPITRStatus(t, handler, "/api/tasks/2/replay", url.Values{"stop_gtid": {apiGTIDUUID + ":8"}}, http.StatusBadRequest, "stop_gtid is not supported for this flavor")
	assertPITRStatus(t, handler, "/api/tasks/2/replay", url.Values{"stop_gtid": {"0-1-10"}}, http.StatusBadRequest, "stop_gtid is not supported for this flavor")
	assertPITRStatus(t, handler, "/api/tasks/2/replay/archive", url.Values{"stop_gtid": {"0-1-10"}}, http.StatusBadRequest, "stop_gtid is not supported for this flavor")
	assertPITRStatus(t, handler, "/api/tasks/missing/replay", url.Values{"stop_gtid": {apiGTIDUUID + ":8"}}, http.StatusNotFound, "task not found")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{"start_datetime": {"2024-01-01 00:00:00"}}, http.StatusBadRequest, "stop_datetime is required")
}

func TestTaskAPI_GTIDReplayReadsBucketObject(t *testing.T) {
	dir := t.TempDir()
	when := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)
	objectBody, off := apiGTIDSegment(t, when, 4)
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

	got := getPITR(t, handler, "/api/tasks/1/replay", url.Values{"stop_gtid": {apiGTIDUUID + ":4"}})
	if len(got.Paths) != 1 || got.Paths[0] != "/data/1/mysql-bin.000004" || !strings.Contains(got.Command, "--stop-position="+strconv.FormatUint(off, 10)) {
		t.Fatalf("object gtid %+v off %d", got, off)
	}
	if len(got.Locations) != 1 || got.Locations[0] != "bucket" {
		t.Fatalf("location %+v", got.Locations)
	}
	archive := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{"stop_gtid": {apiGTIDUUID + ":4"}}))
	if string(archive.entries["mysql-bin.000004"]) != string(objectBody) {
		t.Fatal("archive bytes")
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
	assertPITRStatus(t, gone, "/api/tasks/1/replay", url.Values{"stop_gtid": {apiGTIDUUID + ":4"}}, http.StatusNotFound, "segment not found on this process")
}

const apiGTIDUUID = "3e11fa47-71ca-11e1-9e33-c80aa9429562"

func apiGTIDSegmentBytes(t *testing.T, when time.Time, seqs ...int64) []byte {
	t.Helper()
	body, _ := apiGTIDSegment(t, when, seqs...)
	return body
}

func apiGTIDSegment(t *testing.T, when time.Time, seqs ...int64) ([]byte, uint64) {
	t.Helper()
	sid, err := hex.DecodeString(strings.ReplaceAll(apiGTIDUUID, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	offset := uint64(4)
	var last uint64
	for _, seq := range seqs {
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
		size := uint32(goreplication.EventHeaderSize + len(payload))
		var hdr [19]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.Unix()))
		hdr[4] = byte(goreplication.GTID_EVENT)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], 99999)
		last = offset
		buf.Write(hdr[:])
		buf.Write(payload)
		offset += uint64(size)
	}
	return buf.Bytes(), last
}
