// Package tasks provides module-level functionality for tasks.
// input: on-disk binlog segments that contain MySQL GTID events, plus MariaDB and MySQL task flavors
// output: assertions for the stop-position command, sealed and open segments, a later file left out, start_datetime, and the 400 sentences
// pos: regression coverage for stopping replay before one GTID
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

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

const gtidPITRUUID = "3e11fa47-71ca-11e1-9e33-c80aa9429562"

func TestParseMySQLStopGTID(t *testing.T) {
	got, err := ParseMySQLStopGTID(" 3E11FA47-71CA-11E1-9E33-C80AA9429562:15 ")
	if err != nil || got.UUID != gtidPITRUUID || got.Seq != 15 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, raw := range []string{"", "yesterday", gtidPITRUUID + ":0", gtidPITRUUID + ":1-5", gtidPITRUUID + ":01", "0-1-10", gtidPITRUUID + ":1," + gtidPITRUUID + ":2"} {
		if _, err := ParseMySQLStopGTID(raw); err != ErrInvalidStopGTID {
			t.Fatalf("%q err %v", raw, err)
		}
	}
	if !MariaDBStopGTID("0-1-10") || MariaDBStopGTID(gtidPITRUUID+":10") {
		t.Fatal("mariadb form")
	}
}

func TestFormatGTIDCommand(t *testing.T) {
	start := clock(t, "2024-01-01 01:20:00")
	got := FormatGTIDCommand("mysqlbinlog", []string{"/data/1/mysql-bin.000001", "/data/1/mysql-bin.000002.open.e8"}, &start, 154)
	want := strings.Join([]string{
		"TZ=UTC mysqlbinlog \\",
		"  --start-datetime='2024-01-01 01:20:00' \\",
		"  --stop-position=154 \\",
		"  /data/1/mysql-bin.000001 \\",
		"  /data/1/mysql-bin.000002.open.e8",
	}, "\n")
	if got != want {
		t.Fatalf("command\n%s", got)
	}
	if FormatGTIDCommand("mysqlbinlog", nil, nil, 4) != "" {
		t.Fatal("empty paths")
	}
}

func TestGTIDReplay_SealedOpenAndLaterFile(t *testing.T) {
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
	before := clock(t, "2024-01-01 01:00:00")
	when := clock(t, "2024-01-01 01:20:00")
	later := clock(t, "2024-01-01 02:00:00")
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), gtidSegment(t, before, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000002"), gtidSegment(t, when, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	openBody, openOff := gtidSegmentAt(t, when, 3, 4)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000003.open.e1"), openBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000004"), gtidSegment(t, later, 5), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := scheduler.GTIDReplay("1", gtidPITRUUID+":4", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(taskDir, "mysql-bin.000001"),
		filepath.Join(taskDir, "mysql-bin.000002"),
		filepath.Join(taskDir, "mysql-bin.000003.open.e1"),
	}
	if strings.Join(got.Paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("paths %v", got.Paths)
	}
	if !strings.Contains(got.Command, "--stop-position="+strconv.FormatUint(openOff, 10)) || strings.Contains(got.Command, "mysql-bin.000004") || strings.Contains(got.Command, "--stop-datetime=") {
		t.Fatalf("command %s", got.Command)
	}
	if got.Client != "mysqlbinlog" || got.Flavor != "mysql" {
		t.Fatalf("client %+v", got)
	}

	sealed, err := scheduler.GTIDReplay("1", strings.ToUpper(gtidPITRUUID)+":2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sealed.Paths, "\n") != strings.Join([]string{
		filepath.Join(taskDir, "mysql-bin.000001"),
		filepath.Join(taskDir, "mysql-bin.000002"),
	}, "\n") || strings.Contains(sealed.Command, "open.e1") {
		t.Fatalf("sealed paths %v command %s", sealed.Paths, sealed.Command)
	}

	start := when
	window, err := scheduler.GTIDReplay("1", gtidPITRUUID+":4", &start)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(window.Paths, "\n") != strings.Join(want[1:], "\n") || !strings.Contains(window.Command, "--start-datetime='2024-01-01 01:20:00'") || !strings.Contains(window.Command, "--stop-position=") {
		t.Fatalf("window paths %v command %s", window.Paths, window.Command)
	}
	oneSecond := clock(t, "2024-01-01 01:20:01")
	if _, err := scheduler.GTIDReplay("1", gtidPITRUUID+":4", &oneSecond); err != ErrStartAfterStopGTID {
		t.Fatalf("start after %v", err)
	}

	if _, err := scheduler.GTIDReplay("1", gtidPITRUUID+":99", nil); err != ErrStopGTIDNotInRange {
		t.Fatalf("seq %v", err)
	}
	if _, err := scheduler.GTIDReplay("1", "00000000-0000-0000-0000-000000000001:1", nil); err != ErrStopGTIDNotInRange {
		t.Fatalf("uuid %v", err)
	}
	if _, err := scheduler.GTIDReplay("1", "0-1-10", nil); err != ErrInvalidStopGTID {
		t.Fatalf("maria form on mysql %v", err)
	}
	if _, err := scheduler.GTIDReplay("2", gtidPITRUUID+":1", nil); err != ErrStopGTIDUnsupported {
		t.Fatalf("flavor %v", err)
	}
	if _, err := scheduler.GTIDReplay("2", "0-1-10", nil); err != ErrStopGTIDUnsupported {
		t.Fatalf("maria flavor %v", err)
	}
}

func gtidSegment(t *testing.T, when time.Time, seqs ...int64) []byte {
	t.Helper()
	body, _ := gtidSegmentAt(t, when, seqs...)
	return body
}

func gtidSegmentAt(t *testing.T, when time.Time, seqs ...int64) ([]byte, uint64) {
	t.Helper()
	sid, err := hex.DecodeString(strings.ReplaceAll(gtidPITRUUID, "-", ""))
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
