// Package tasks provides module-level functionality for tasks.
// input: on-disk binlog segments with MySQL GTID events and a previous-GTIDs event, plus a MariaDB task flavor
// output: assertions for segment omission, --exclude-gtids, the covered note, the gap sentence, and stop_datetime
// pos: regression coverage for rolling forward from a restored backup's executed GTID set
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestParseStartGTIDSet(t *testing.T) {
	got, err := ParseStartGTIDSet(" " + gtidPITRUUID + ":1-5 ")
	if err != nil || got.String() != gtidPITRUUID+":1-5" {
		t.Fatalf("%v %v", got, err)
	}
	other := "11111111-1111-1111-1111-111111111111"
	raw := gtidPITRUUID + ":1-5,\n" + other + ":1-3\r\n"
	got, err = ParseStartGTIDSet(raw)
	if err != nil {
		t.Fatal(err)
	}
	want, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, gtidPITRUUID+":1-5,"+other+":1-3")
	if err != nil || !got.Equal(want) {
		t.Fatalf("normalized %s", got)
	}
	for _, raw := range []string{"", "  ", "\n", "0-1-10", "not-a-set", gtidPITRUUID + ":0", gtidPITRUUID + ":0-5"} {
		if _, err := ParseStartGTIDSet(raw); err != ErrInvalidStartGTIDSet {
			t.Fatalf("%q err %v", raw, err)
		}
	}
}

func TestFormatExecutedGTIDCommand(t *testing.T) {
	stop := clock(t, "2024-01-01 01:20:00")
	other := "11111111-1111-1111-1111-111111111111"
	got := FormatExecutedGTIDCommand("mysqlbinlog", []string{"/data/1/mysql-bin.000002"}, gtidPITRUUID+":1-2,"+other+":1-3", 154, true, &stop)
	want := strings.Join([]string{
		"TZ=UTC mysqlbinlog \\",
		"  --exclude-gtids='" + gtidPITRUUID + ":1-2," + other + ":1-3' \\",
		"  --stop-datetime='2024-01-01 01:20:00' \\",
		"  --stop-position=154 \\",
		"  /data/1/mysql-bin.000002",
	}, "\n")
	if got != want {
		t.Fatalf("command\n%s", got)
	}
	plain := FormatExecutedGTIDCommand("mysqlbinlog", []string{"/data/1/a"}, gtidPITRUUID+":1-2", 0, false, nil)
	if strings.Contains(plain, "--stop-position=") || strings.Contains(plain, "--stop-datetime=") || strings.Contains(plain, "--start-datetime=") || !strings.Contains(plain, "--exclude-gtids="+gtidPITRUUID+":1-2") {
		t.Fatalf("plain %s", plain)
	}
	if FormatExecutedGTIDCommand("mysqlbinlog", nil, gtidPITRUUID+":1-2", 4, true, nil) != "" {
		t.Fatal("empty paths")
	}
}

func TestExecutedGTIDReplay_OmitsCoveredSegment(t *testing.T) {
	scheduler, taskDir := executedFixture(t)
	when := clock(t, "2024-01-01 01:20:00")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), executedSegment(t, when, "", 1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	body, offs := executedSegmentAt(t, when, gtidPITRUUID+":1-2", 3, 4)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-2", gtidPITRUUID+":4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || !strings.HasSuffix(got.Paths[0], "mysql-bin.000002") {
		t.Fatalf("paths %v", got.Paths)
	}
	if strings.Contains(got.Command, "mysql-bin.000001") || !strings.Contains(got.Command, "--exclude-gtids="+gtidPITRUUID+":1-2") || !strings.Contains(got.Command, "--stop-position="+strconv.FormatUint(offs[4], 10)) || strings.Contains(got.Command, "--stop-datetime=") || strings.Contains(got.Command, "--start-datetime=") {
		t.Fatalf("command %s", got.Command)
	}
	if got.Note != "" {
		t.Fatalf("note %q", got.Note)
	}
}

func TestExecutedGTIDReplay_CoveredNoteAndEmptyStop(t *testing.T) {
	scheduler, taskDir := executedFixture(t)
	when := clock(t, "2024-01-01 01:20:00")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), executedSegment(t, when, "", 1, 2, 3, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-5", gtidPITRUUID+":4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 0 || got.Command != "" || got.Note != executedGTIDCoveredNote {
		t.Fatalf("%+v", got)
	}
	empty, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-100", gtidPITRUUID+":1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Paths) != 0 || empty.Command != "" || empty.Note != "" {
		t.Fatalf("empty stop %+v", empty)
	}
}

func TestExecutedGTIDReplay_GapAndContinuation(t *testing.T) {
	scheduler, taskDir := executedFixture(t)
	when := clock(t, "2024-01-01 01:20:00")
	body, _ := executedSegmentAt(t, when, "", 10, 11, 12)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-5", gtidPITRUUID+":12", nil); err != ErrStartGTIDSetGap {
		t.Fatalf("inferred gap %v", err)
	}

	os.Remove(filepath.Join(taskDir, "mysql-bin.000001"))
	cont, offs := executedSegmentAt(t, when, gtidPITRUUID+":1-9", 10, 11)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), cont, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-9", gtidPITRUUID+":11", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || !strings.Contains(got.Command, "--exclude-gtids="+gtidPITRUUID+":1-9") || !strings.Contains(got.Command, "--stop-position="+strconv.FormatUint(offs[11], 10)) {
		t.Fatalf("continuation %+v", got)
	}

	hole, _ := executedSegmentAt(t, when, gtidPITRUUID+":1-9", 10, 11, 12)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003"), hole, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-5,"+gtidPITRUUID+":10", gtidPITRUUID+":12", nil); err != ErrStartGTIDSetGap {
		t.Fatalf("hole %v", err)
	}
}

func TestExecutedGTIDReplay_StopDatetimeAndDroppedStopFile(t *testing.T) {
	scheduler, taskDir := executedFixture(t)
	early := clock(t, "2024-01-01 01:00:00")
	late := clock(t, "2024-01-01 02:00:00")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), executedSegment(t, early, gtidPITRUUID+":1-2", 3), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), executedSegment(t, late, gtidPITRUUID+":1-3", 4), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := clock(t, "2024-01-01 01:30:00")
	got, err := scheduler.ExecutedGTIDReplay("1", " "+gtidPITRUUID+":1-2 ", "", &stop)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || !strings.HasSuffix(got.Paths[0], "mysql-bin.000001") || !strings.Contains(got.Command, "--stop-datetime='2024-01-01 01:30:00'") || strings.Contains(got.Command, "--stop-position=") || strings.Contains(got.Command, "--start-datetime=") || !strings.Contains(got.Command, "--exclude-gtids="+gtidPITRUUID+":1-2") {
		t.Fatalf("datetime %+v", got)
	}

	full, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-2", gtidPITRUUID+":4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Paths) != 1 || !strings.HasSuffix(full.Paths[0], "mysql-bin.000001") || strings.Contains(full.Command, "--stop-position=") {
		t.Fatalf("dropped stop file %+v", full)
	}
	if _, err := scheduler.ExecutedGTIDReplay("1", gtidPITRUUID+":1-2", gtidPITRUUID+":99", nil); err != ErrStopGTIDNotInRange {
		t.Fatalf("missing stop %v", err)
	}
	if _, err := scheduler.ExecutedGTIDReplay("2", gtidPITRUUID+":1-2", gtidPITRUUID+":4", nil); err != ErrStartGTIDSetUnsupported {
		t.Fatalf("flavor %v", err)
	}
}

func executedFixture(t *testing.T) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	scheduler := NewScheduler(WithDataDir(dir))
	if _, err := scheduler.CreateTaskFromSpec("mysql", "mysql-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.CreateTaskFromSpec("maria", "maria-key", &SourceConfig{Host: "127.0.0.1", Port: 3307, User: "repl", Password: "secret", Flavor: "mariadb"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, "1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return scheduler, taskDir
}

func executedSegment(t *testing.T, when time.Time, previous string, seqs ...int64) []byte {
	t.Helper()
	body, _ := executedSegmentAt(t, when, previous, seqs...)
	return body
}

func executedSegmentAt(t *testing.T, when time.Time, previous string, seqs ...int64) ([]byte, map[int64]uint64) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	offs := map[int64]uint64{}
	if previous != "" {
		set, err := mysql.ParseMysqlGTIDSet(previous)
		if err != nil {
			t.Fatal(err)
		}
		writeExecutedEvent(&buf, when, goreplication.PREVIOUS_GTIDS_EVENT, set.Encode())
	}
	sid, err := hex.DecodeString(strings.ReplaceAll(gtidPITRUUID, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range seqs {
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
		offs[seq] = uint64(buf.Len())
		writeExecutedEvent(&buf, when, goreplication.GTID_EVENT, payload)
	}
	return buf.Bytes(), offs
}

func writeExecutedEvent(buf *bytes.Buffer, when time.Time, kind goreplication.EventType, payload []byte) {
	size := uint32(goreplication.EventHeaderSize + len(payload))
	var hdr [19]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(when.Unix()))
	hdr[4] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[9:13], size)
	buf.Write(hdr[:])
	buf.Write(payload)
}
