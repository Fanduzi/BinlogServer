// Package api provides module-level functionality for api.
// input: HTTP GET /api/tasks/{id}/replay and /replay/archive with start_gtid_set, including segments that exist only as uploaded objects
// output: assertions for --exclude-gtids, the covered note, plain-text 400 sentences, and bucket bytes in the tar
// pos: HTTP coverage for rolling forward from a restored backup's executed GTID set
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

	"github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestTaskAPI_ExecutedGTIDReplay(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), apiExecutedSegment(t, when, "", 1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	body, offs := apiExecutedSegmentAt(t, when, apiGTIDUUID+":1-2", 3, 4)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/tasks/1/replay?limit=1", nil))
	if plain.Code != http.StatusOK || strings.Contains(plain.Body.String(), `"command"`) {
		t.Fatalf("limit replay status=%d body=%s", plain.Code, plain.Body.String())
	}

	got := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
		"limit":          {"1"},
	})
	if len(got.Paths) != 1 || !strings.HasSuffix(got.Paths[0], "mysql-bin.000002") || got.Note != "" {
		t.Fatalf("paths %+v", got)
	}
	if !strings.Contains(got.Command, "--exclude-gtids="+apiGTIDUUID+":1-2") || !strings.Contains(got.Command, "--stop-position="+strconv.FormatUint(offs[4], 10)) || strings.Contains(got.Command, "mysql-bin.000001") || strings.Contains(got.Command, "--start-datetime=") {
		t.Fatalf("command %s", got.Command)
	}
	archive := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}))
	if strings.Join(archive.order, "\n") != "mysql-bin.000002" {
		t.Fatalf("archive %v", archive.order)
	}

	covered := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-5"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	})
	if len(covered.Paths) != 0 || covered.Command != "" || covered.Note != "every transaction up to the stop is already in start_gtid_set" {
		t.Fatalf("covered %+v", covered)
	}
	emptyArchive := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-5"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}))
	if len(emptyArchive.order) != 0 {
		t.Fatalf("empty archive %v", emptyArchive.order)
	}

	stop := time.Date(2024, 1, 1, 1, 30, 0, 0, time.UTC)
	timed := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_datetime":  {stop.Format("2006-01-02 15:04:05")},
	})
	if !strings.Contains(timed.Command, "--exclude-gtids=") || !strings.Contains(timed.Command, "--stop-datetime=") || strings.Contains(timed.Command, "--stop-position=") {
		t.Fatalf("timed %s", timed.Command)
	}

	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {"not-a-set"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}, http.StatusBadRequest, "invalid start_gtid_set")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {"  "},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}, http.StatusBadRequest, "invalid start_gtid_set")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {"0-1-10"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}, http.StatusBadRequest, "invalid start_gtid_set")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
	}, http.StatusBadRequest, "stop_datetime or stop_gtid is required")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"start_datetime": {"2024-01-01 01:00:00"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}, http.StatusBadRequest, "start_gtid_set and start_datetime cannot both be set")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {"not-a-set"},
		"start_datetime": {"2024-01-01 01:00:00"},
		"stop_datetime":  {"2024-01-01 02:00:00"},
	}, http.StatusBadRequest, "start_gtid_set and start_datetime cannot both be set")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
		"stop_datetime":  {"2024-01-01 01:20:00"},
	}, http.StatusBadRequest, "stop_datetime and stop_gtid cannot both be set")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_datetime": {"2024-01-01 00:00:00"},
	}, http.StatusBadRequest, "stop_datetime is required")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_gtid":      {"not-a-gtid"},
	}, http.StatusBadRequest, "invalid stop_gtid")
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-5"},
		"stop_gtid":      {apiGTIDUUID + ":99"},
	}, http.StatusBadRequest, "stop_gtid is not in this task's backed-up range")
	assertPITRStatus(t, handler, "/api/tasks/2/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}, http.StatusBadRequest, "start_gtid_set is not supported for this flavor")
	assertPITRStatus(t, handler, "/api/tasks/2/replay/archive", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_datetime":  {"2024-01-01 02:00:00"},
	}, http.StatusBadRequest, "start_gtid_set is not supported for this flavor")
	assertPITRStatus(t, handler, "/api/tasks/missing/replay", url.Values{
		"start_gtid_set": {"not-a-set"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}, http.StatusBadRequest, "invalid start_gtid_set")
	assertPITRStatus(t, handler, "/api/tasks/missing/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-2"},
		"stop_gtid":      {apiGTIDUUID + ":4"},
	}, http.StatusNotFound, "task not found")

	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), apiExecutedSegment(t, when, "", 10, 11), 0o644); err != nil {
		t.Fatal(err)
	}
	assertPITRStatus(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-5"},
		"stop_gtid":      {apiGTIDUUID + ":11"},
	}, http.StatusBadRequest, "start_gtid_set has a gap before this task's backed-up range")
}

func TestTaskAPI_ExecutedGTIDReplayReadsBucketObject(t *testing.T) {
	dir := t.TempDir()
	when := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)
	first := apiExecutedSegment(t, when, "", 1)
	second, offs := apiExecutedSegmentAt(t, when, apiGTIDUUID+":1-1", 2, 3)
	store := newFakeFileStore()
	store.files["1"] = []tasks.BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000001",
			FilePath:    "/data/1/mysql-bin.000001",
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/mysql-bin.000001",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000002",
			FilePath:    "/data/1/mysql-bin.000002",
			State:       "SEALED",
			UploadState: "UPLOADED",
			ObjectKey:   "prefix/mysql-bin.000002",
		},
	}
	opener := &keyedObjectStore{bodies: map[string][]byte{
		"prefix/mysql-bin.000001": first,
		"prefix/mysql-bin.000002": second,
	}}
	handler := NewServer(tasks.NewScheduler(tasks.WithFileStore(store), tasks.WithDataDir(dir), tasks.WithFileUploader(opener)))
	createDownloadTask(t, handler)

	got := getPITR(t, handler, "/api/tasks/1/replay", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-1"},
		"stop_gtid":      {apiGTIDUUID + ":3"},
	})
	if len(got.Paths) != 1 || got.Paths[0] != "/data/1/mysql-bin.000002" {
		t.Fatalf("paths %v", got.Paths)
	}
	if len(got.Locations) != 1 || got.Locations[0] != "bucket" {
		t.Fatalf("location %+v", got.Locations)
	}
	if !strings.Contains(got.Command, "--exclude-gtids="+apiGTIDUUID+":1") || !strings.Contains(got.Command, "--stop-position="+strconv.FormatUint(offs[3], 10)) {
		t.Fatalf("command %s", got.Command)
	}
	archive := getReplayArchive(t, handler, pitrPath(t, "/api/tasks/1/replay/archive", url.Values{
		"start_gtid_set": {apiGTIDUUID + ":1-1"},
		"stop_gtid":      {apiGTIDUUID + ":3"},
	}))
	if string(archive.entries["mysql-bin.000002"]) != string(second) || len(archive.order) != 1 {
		t.Fatalf("archive %v", archive.order)
	}
}

func apiExecutedSegment(t *testing.T, when time.Time, previous string, seqs ...int64) []byte {
	t.Helper()
	body, _ := apiExecutedSegmentAt(t, when, previous, seqs...)
	return body
}

func apiExecutedSegmentAt(t *testing.T, when time.Time, previous string, seqs ...int64) ([]byte, map[int64]uint64) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	offs := map[int64]uint64{}
	if previous != "" {
		set, err := mysql.ParseMysqlGTIDSet(previous)
		if err != nil {
			t.Fatal(err)
		}
		apiWriteExecutedEvent(&buf, when, goreplication.PREVIOUS_GTIDS_EVENT, set.Encode())
	}
	sid, err := hex.DecodeString(strings.ReplaceAll(apiGTIDUUID, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range seqs {
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
		offs[seq] = uint64(buf.Len())
		apiWriteExecutedEvent(&buf, when, goreplication.GTID_EVENT, payload)
	}
	return buf.Bytes(), offs
}

func apiWriteExecutedEvent(buf *bytes.Buffer, when time.Time, kind goreplication.EventType, payload []byte) {
	size := uint32(goreplication.EventHeaderSize + len(payload))
	var hdr [19]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.Unix()))
	hdr[4] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[9:13], size)
	buf.Write(hdr[:])
	buf.Write(payload)
}
