// Package tasks provides module-level functionality for tasks.
// input: on-disk and catalog segments, including a chain that exists only as uploaded objects, plus mysql and mariadb flavors
// output: assertions for a continuous recoverable window, a missing source index, a GTID hole, checksum mismatch, a segment that is not durable off-host, and agreement with PITR replay
// pos: regression coverage for the retained-chain window and its breaks
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"

	"github.com/go-mysql-org/go-mysql/mysql"
)

func TestRecoveryWindow_ContinuousAgreesWithReplay(t *testing.T) {
	dir := t.TempDir()
	scheduler := NewScheduler(WithDataDir(dir))
	if _, err := scheduler.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := clock(t, "2024-01-01 01:00:00")
	second := clock(t, "2024-01-01 01:30:00")
	third := clock(t, "2024-01-01 02:00:00")
	writeWindowSegment(t, filepath.Join(taskDir, "mysql-bin.000001"), first, "", 1)
	writeWindowSegment(t, filepath.Join(taskDir, "mysql-bin.000002"), second, gtidPITRUUID+":1", 2)
	writeWindowSegment(t, filepath.Join(taskDir, "mysql-bin.000003"), third, gtidPITRUUID+":1-2", 3)

	got, err := scheduler.RecoveryWindow("1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Continuous || len(got.Breaks) != 0 {
		t.Fatalf("window %+v", got)
	}
	if got.Earliest == nil || !got.Earliest.Equal(first) || got.Latest == nil || !got.Latest.Equal(third) {
		t.Fatalf("span earliest=%v latest=%v", got.Earliest, got.Latest)
	}
	if !gtidContains(t, got.GTIDSet, gtidPITRUUID+":1-3") || gtidContains(t, got.GTIDSet, gtidPITRUUID+":4") {
		t.Fatalf("gtid_set %s", got.GTIDSet)
	}

	inside, err := scheduler.PITRReplay("1", nil, third.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if inside.Command == "" || len(inside.Paths) != 3 {
		t.Fatalf("inside replay %+v", inside)
	}
	before, err := scheduler.PITRReplay("1", nil, first)
	if err != nil {
		t.Fatal(err)
	}
	if before.Command != "" || len(before.Paths) != 0 {
		t.Fatalf("stop at earliest should be empty, got %+v", before)
	}
	earlier, err := scheduler.PITRReplay("1", nil, first.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if earlier.Command != "" {
		t.Fatalf("before earliest command %s", earlier.Command)
	}

	if err := os.Remove(filepath.Join(taskDir, "mysql-bin.000002")); err != nil {
		t.Fatal(err)
	}
	broken, err := scheduler.RecoveryWindow("1")
	if err != nil {
		t.Fatal(err)
	}
	if broken.Continuous || !breakMentions(broken, "missing source file mysql-bin.000002 between mysql-bin.000001 and mysql-bin.000003") {
		t.Fatalf("gap window %+v", broken)
	}
	if !breakMentions(broken, "gtid hole between mysql-bin.000001 and mysql-bin.000003") {
		t.Fatalf("expected gtid hole, %+v", broken)
	}
	across, err := scheduler.PITRReplay("1", nil, third.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(across.Paths, "\n")
	if strings.Contains(joined, "mysql-bin.000002") || across.Command == "" {
		t.Fatalf("replay across the gap invented or dropped the chain: %+v", across)
	}
}

func TestRecoveryWindow_DurabilityChecksumAndMariaDB(t *testing.T) {
	dir := t.TempDir()
	store := newFakeFileStore()
	when := clock(t, "2024-01-01 03:00:00")
	scheduler := NewScheduler(WithDataDir(dir), WithFileStore(store), WithFileUploader(uploadOnlyStub{}))
	created, err := scheduler.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, created.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), windowSegment(t, when, "", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), windowSegment(t, when.Add(time.Minute), gtidPITRUUID+":1", 2), 0o644); err != nil {
		t.Fatal(err)
	}
	store.files[created.ID] = []BinlogFile{
		{
			TaskID:      created.ID,
			FileName:    "mysql-bin.000001",
			FilePath:    filepath.Join(taskDir, "mysql-bin.000001"),
			State:       "SEALED",
			UploadState: "LOCAL_ONLY",
		},
		{
			TaskID:      created.ID,
			FileName:    "mysql-bin.000002",
			FilePath:    filepath.Join(taskDir, "mysql-bin.000002"),
			State:       "SEALED",
			UploadState: "UPLOAD_FAILED",
			Checksum:    ChecksumMismatch,
		},
	}
	got, err := scheduler.RecoveryWindow(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Continuous || !breakMentions(got, "segment is not durable off-host (LOCAL_ONLY)") || !breakMentions(got, "checksum mismatch") {
		t.Fatalf("durability %+v", got)
	}
	if breakMentions(got, "segment is not durable off-host (UPLOAD_FAILED)") {
		t.Fatalf("mismatch should not also be reported as not durable: %+v", got)
	}

	store.files[created.ID][0].UploadState = "UPLOADED"
	store.files[created.ID][0].Checksum = ChecksumMatch
	store.files[created.ID][1].UploadState = "UPLOADED"
	store.files[created.ID][1].Checksum = ChecksumMatch
	clean, err := scheduler.RecoveryWindow(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !clean.Continuous || len(clean.Breaks) != 0 {
		t.Fatalf("uploaded chain %+v", clean)
	}
}

func TestRecoveryWindow_BucketOnlyAndUnreadable(t *testing.T) {
	dir := t.TempDir()
	firstAt := clock(t, "2024-01-01 04:00:00")
	secondAt := clock(t, "2024-01-01 04:30:00")
	first := windowSegment(t, firstAt, "", 1)
	second := windowSegment(t, secondAt, gtidPITRUUID+":1", 2)
	store := newFakeFileStore()
	store.files["1"] = []BinlogFile{
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000010",
			FilePath:    "/catalog/1/mysql-bin.000010",
			State:       "SEALED",
			UploadState: "UPLOADED",
			Checksum:    ChecksumMatch,
			ObjectKey:   "obj/mysql-bin.000010",
		},
		{
			TaskID:      "1",
			FileName:    "mysql-bin.000011",
			FilePath:    "/catalog/1/mysql-bin.000011",
			State:       "SEALED",
			UploadState: "UPLOADED",
			Checksum:    ChecksumMatch,
			ObjectKey:   "obj/mysql-bin.000011",
		},
	}
	opener := windowObjects{bodies: map[string][]byte{
		"obj/mysql-bin.000010": first,
		"obj/mysql-bin.000011": second,
	}}
	scheduler := NewScheduler(WithDataDir(dir), WithFileStore(store), WithFileUploader(opener))
	if _, err := scheduler.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.RecoveryWindow("1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Continuous || got.Earliest == nil || !got.Earliest.Equal(firstAt) || got.Latest == nil || !got.Latest.Equal(secondAt) {
		t.Fatalf("bucket window %+v", got)
	}
	if !gtidContains(t, got.GTIDSet, gtidPITRUUID+":1-2") {
		t.Fatalf("gtid_set %s", got.GTIDSet)
	}

	store.files["1"] = append(store.files["1"], BinlogFile{
		TaskID:      "1",
		FileName:    "mysql-bin.000012",
		FilePath:    "/catalog/1/mysql-bin.000012",
		State:       "SEALED",
		UploadState: "LOCAL_ONLY",
	})
	broken, err := scheduler.RecoveryWindow("1")
	if err != nil {
		t.Fatal(err)
	}
	if broken.Continuous || !breakMentions(broken, "segment is not durable off-host (LOCAL_ONLY)") || !breakMentions(broken, "segment is not readable") {
		t.Fatalf("local-only missing bytes %+v", broken)
	}
}

func TestRecoveryWindow_MariaDBSkipsGTIDHole(t *testing.T) {
	dir := t.TempDir()
	scheduler := NewScheduler(WithDataDir(dir))
	if _, err := scheduler.CreateTaskFromSpec("maria", "maria-key", &SourceConfig{Host: "127.0.0.1", Port: 3307, User: "repl", Password: "secret", Flavor: "mariadb"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	when := clock(t, "2024-01-01 05:00:00")
	writeWindowSegment(t, filepath.Join(taskDir, "mysql-bin.000001"), when, "", 1)
	writeWindowSegment(t, filepath.Join(taskDir, "mysql-bin.000003"), when.Add(time.Minute), "", 4)
	got, err := scheduler.RecoveryWindow("1")
	if err != nil {
		t.Fatal(err)
	}
	if got.GTIDSet != "" || breakMentions(got, "gtid hole") {
		t.Fatalf("mariadb gtid %+v", got)
	}
	if got.Continuous || !breakMentions(got, "missing source file mysql-bin.000002 between mysql-bin.000001 and mysql-bin.000003") {
		t.Fatalf("mariadb index %+v", got)
	}
}

func TestRecoveryWindow_OpenSegmentIsNotADurabilityBreak(t *testing.T) {
	dir := t.TempDir()
	store := newFakeFileStore()
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	when := clock(t, "2024-01-01 06:00:00")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001.open.e1"), windowSegment(t, when, "", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	store.files["1"] = []BinlogFile{{
		TaskID:      "1",
		FileName:    "mysql-bin.000001",
		FilePath:    filepath.Join(taskDir, "mysql-bin.000001.open.e1"),
		State:       "OPEN",
		UploadState: "LOCAL_ONLY",
	}}
	scheduler := NewScheduler(WithDataDir(dir), WithFileStore(store), WithFileUploader(uploadOnlyStub{}))
	if _, err := scheduler.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.RecoveryWindow("1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Continuous || len(got.Breaks) != 0 || got.Earliest == nil {
		t.Fatalf("open window %+v", got)
	}
}

func TestRecoveryWindow_MissingTask(t *testing.T) {
	scheduler := NewScheduler()
	if _, err := scheduler.RecoveryWindow("missing"); !errorsIsNotFound(err) {
		t.Fatalf("err %v", err)
	}
}

func errorsIsNotFound(err error) bool {
	return errors.Is(err, ErrTaskNotFound)
}

func breakMentions(window RecoveryWindow, text string) bool {
	for _, br := range window.Breaks {
		if strings.Contains(br.Reason, text) {
			return true
		}
		for _, name := range br.Files {
			if name == text {
				return true
			}
		}
	}
	return false
}

func gtidContains(t *testing.T, got, want string) bool {
	t.Helper()
	left, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, got)
	if err != nil {
		t.Fatal(err)
	}
	right, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, want)
	if err != nil {
		t.Fatal(err)
	}
	return left.Contain(right)
}

func writeWindowSegment(t *testing.T, path string, when time.Time, previous string, seqs ...int64) {
	t.Helper()
	if err := os.WriteFile(path, windowSegment(t, when, previous, seqs...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func windowSegment(t *testing.T, when time.Time, previous string, seqs ...int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	if previous != "" {
		set, err := mysql.ParseMysqlGTIDSet(previous)
		if err != nil {
			t.Fatal(err)
		}
		writeWindowEvent(&buf, when, goreplication.PREVIOUS_GTIDS_EVENT, set.Encode())
	}
	sid, err := hex.DecodeString(strings.ReplaceAll(gtidPITRUUID, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range seqs {
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
		writeWindowEvent(&buf, when, goreplication.GTID_EVENT, payload)
	}
	return buf.Bytes()
}

func writeWindowEvent(buf *bytes.Buffer, when time.Time, kind goreplication.EventType, payload []byte) {
	size := uint32(goreplication.EventHeaderSize + len(payload))
	var hdr [19]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.Unix()))
	hdr[4] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[9:13], size)
	buf.Write(hdr[:])
	buf.Write(payload)
}

type windowObjects struct {
	bodies map[string][]byte
}

func (windowObjects) UploadFile(context.Context, string, string, string) error { return nil }

func (o windowObjects) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	body, ok := o.bodies[key]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}
