// Package tasks provides module-level functionality for tasks.
// input: event time spans, UTC datetime text, and on-disk binlog segments
// output: assertions for the point-in-time path filter, the client command, and flavor mapping
// pos: regression coverage for the datetime replay window
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePITRDatetime(t *testing.T) {
	want := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{
		"2024-06-01 12:00:00",
		" 2024-06-01T12:00:00 ",
		"2024-06-01T12:00:00Z",
		"2024-06-01T04:00:00-08:00",
	} {
		got, err := ParsePITRDatetime(raw)
		if err != nil || !got.Equal(want) {
			t.Fatalf("%q got %s err %v", raw, got, err)
		}
	}
	if _, err := ParsePITRDatetime("yesterday"); err == nil {
		t.Fatal("accepted garbage")
	}
	if _, err := ParsePITRDatetime("2024-13-01 00:00:00"); err == nil {
		t.Fatal("accepted month 13")
	}
}

func TestFilterPITRFiles(t *testing.T) {
	files := []BinlogFile{
		{FilePath: "/data/1/mysql-bin.000001"},
		{FilePath: "/data/1/mysql-bin.000002.open.e8"},
		{FilePath: "/data/1/mysql-bin.000003"},
	}
	spans := []EventSpan{
		{First: clock(t, "2024-01-01 00:00:00"), Last: clock(t, "2024-01-01 00:30:00")},
		{First: clock(t, "2024-01-01 01:00:00"), Last: clock(t, "2024-01-01 01:40:00")},
		{First: clock(t, "2024-01-01 02:00:00"), Last: clock(t, "2024-01-01 02:30:00")},
	}
	stop := clock(t, "2024-01-01 01:20:00")
	got := FilterPITRFiles(files, spans, nil, stop)
	if pathsOf(got) != "/data/1/mysql-bin.000001\n/data/1/mysql-bin.000002.open.e8" {
		t.Fatalf("stop-only %s", pathsOf(got))
	}
	start := clock(t, "2024-01-01 01:20:00")
	stop = clock(t, "2024-01-01 02:10:00")
	got = FilterPITRFiles(files, spans, &start, stop)
	if pathsOf(got) != "/data/1/mysql-bin.000002.open.e8\n/data/1/mysql-bin.000003" {
		t.Fatalf("window %s", pathsOf(got))
	}
	got = FilterPITRFiles(files, spans, nil, clock(t, "2020-01-01 00:00:00"))
	if len(got) != 0 {
		t.Fatalf("early stop %+v", got)
	}
	same := clock(t, "2024-01-01 01:20:00")
	got = FilterPITRFiles(files, spans, &same, same)
	if len(got) != 0 {
		t.Fatalf("equal window %s", pathsOf(got))
	}
	got = FilterPITRFiles(files, spans, nil, clock(t, "2024-01-01 02:00:00"))
	if pathsOf(got) != "/data/1/mysql-bin.000001\n/data/1/mysql-bin.000002.open.e8" {
		t.Fatalf("stop touch %s", pathsOf(got))
	}
	got = FilterPITRFiles(files, []EventSpan{{}, {}, {}}, nil, stop)
	if len(got) != 0 {
		t.Fatal("untimed segments")
	}
}

func TestFormatPITRCommand(t *testing.T) {
	stop := clock(t, "2024-01-01 01:20:00")
	got := FormatPITRCommand("mysqlbinlog", []string{"/data/1/mysql-bin.000001", "/data/1/mysql-bin.000002.open.e8"}, nil, stop)
	want := strings.Join([]string{
		"TZ=UTC mysqlbinlog \\",
		"  --stop-datetime='2024-01-01 01:20:00' \\",
		"  /data/1/mysql-bin.000001 \\",
		"  /data/1/mysql-bin.000002.open.e8",
	}, "\n")
	if got != want {
		t.Fatalf("stop-only\n%s", got)
	}
	start := clock(t, "2024-01-01 01:20:00")
	stop = clock(t, "2024-01-01 02:10:00")
	got = FormatPITRCommand("mariadb-binlog", []string{"/data/1/mysql-bin.000003"}, &start, stop)
	if !strings.Contains(got, "TZ=UTC mariadb-binlog \\\n") || !strings.Contains(got, "--start-datetime='2024-01-01 01:20:00'") || !strings.Contains(got, "--stop-datetime='2024-01-01 02:10:00'") || !strings.HasSuffix(got, "/data/1/mysql-bin.000003") {
		t.Fatalf("start+stop\n%s", got)
	}
	if FormatPITRCommand("mysqlbinlog", nil, nil, stop) != "" {
		t.Fatal("empty paths")
	}
	quoted := FormatPITRCommand("", []string{"/data/my bin/mysql-bin.000001"}, nil, stop)
	if !strings.Contains(quoted, "--stop-datetime='2024-01-01 02:10:00'") || !strings.Contains(quoted, "'/data/my bin/mysql-bin.000001'") || strings.Contains(quoted, "TZ=UTC") {
		t.Fatalf("quoted\n%s", quoted)
	}
}

func TestPITRReplay_DiskSegments(t *testing.T) {
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
	writeTimed := func(name string, stamps ...string) {
		t.Helper()
		times := make([]time.Time, len(stamps))
		for i, stamp := range stamps {
			times[i] = clock(t, stamp)
		}
		if err := os.WriteFile(filepath.Join(taskDir, name), timedSegment(times...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTimed("mysql-bin.000001", "2024-01-01 00:00:00", "2024-01-01 00:30:00")
	writeTimed("mysql-bin.000002", "2024-01-01 01:00:00", "2024-01-01 01:30:00")
	writeTimed("mysql-bin.000002.open.e1", "2024-01-01 01:10:00")
	writeTimed("mysql-bin.000002.open.e8", "2024-01-01 01:00:00", "2024-01-01 01:40:00")
	writeTimed("mysql-bin.000003", "2024-01-01 02:00:00", "2024-01-01 02:30:00")

	stop := clock(t, "2024-01-01 01:20:00")
	got, err := scheduler.PITRReplay("1", nil, stop)
	if err != nil {
		t.Fatal(err)
	}
	if got.Client != "mysqlbinlog" || got.ClientHint != "MySQL mysqlbinlog" {
		t.Fatalf("client %+v", got)
	}
	want := []string{
		filepath.Join(taskDir, "mysql-bin.000001"),
		filepath.Join(taskDir, "mysql-bin.000002.open.e8"),
	}
	if strings.Join(got.Paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("paths %v", got.Paths)
	}
	if !strings.Contains(got.Command, "--stop-datetime='2024-01-01 01:20:00'") || strings.Contains(got.Command, "--start-datetime=") {
		t.Fatalf("command %s", got.Command)
	}
	if strings.Contains(got.Command, "mysql-bin.000002 ") || strings.Contains(got.Command, "open.e1") {
		t.Fatalf("command kept a dropped epoch %s", got.Command)
	}

	start := clock(t, "2024-01-01 01:20:00")
	stop = clock(t, "2024-01-01 02:10:00")
	got, err = scheduler.PITRReplay("1", &start, stop)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		filepath.Join(taskDir, "mysql-bin.000002.open.e8"),
		filepath.Join(taskDir, "mysql-bin.000003"),
	}
	if strings.Join(got.Paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("window %v", got.Paths)
	}
	if !strings.Contains(got.Command, "--start-datetime='2024-01-01 01:20:00'") || !strings.Contains(got.Command, "--stop-datetime='2024-01-01 02:10:00'") {
		t.Fatalf("window command %s", got.Command)
	}

	empty, err := scheduler.PITRReplay("1", nil, clock(t, "2020-01-01 00:00:00"))
	if err != nil || len(empty.Paths) != 0 || empty.Command != "" || empty.Client != "mysqlbinlog" {
		t.Fatalf("empty %+v err %v", empty, err)
	}
	same := clock(t, "2024-01-01 01:20:00")
	equal, err := scheduler.PITRReplay("1", &same, same)
	if err != nil || len(equal.Paths) != 0 || equal.Command != "" || equal.Client != "mysqlbinlog" || equal.Flavor != "mysql" {
		t.Fatalf("equal window %+v err %v", equal, err)
	}

	mariaDir := filepath.Join(dir, "2")
	if err := os.MkdirAll(mariaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mariaDir, "mysql-bin.000010"), timedSegment(clock(t, "2024-01-01 00:00:00")), 0o644); err != nil {
		t.Fatal(err)
	}
	maria, err := scheduler.PITRReplay("2", nil, clock(t, "2024-01-01 00:30:00"))
	if err != nil {
		t.Fatal(err)
	}
	if maria.Client != "mariadb-binlog" || maria.ClientHint != "mariadb-binlog" || !strings.HasPrefix(maria.Command, "TZ=UTC mariadb-binlog \\\n") {
		t.Fatalf("maria %+v", maria)
	}
}

func clock(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := ParsePITRDatetime(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func pathsOf(files []BinlogFile) string {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.FilePath
	}
	return strings.Join(paths, "\n")
}

func timedSegment(times ...time.Time) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	pos := uint32(4)
	for _, ts := range times {
		const payload = 1
		size := uint32(19 + payload)
		pos += size
		var hdr [19]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(ts.Unix()))
		hdr[4] = 2
		binary.LittleEndian.PutUint32(hdr[5:9], 1)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], pos)
		buf.Write(hdr[:])
		buf.WriteByte(0)
	}
	return buf.Bytes()
}
