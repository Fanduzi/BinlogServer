// Package replication provides module-level functionality for replication.
// input: a GTID dump and a file/pos dump whose first read is MySQL 1236, each led by an artificial Rotate with no CRC, plus CRC32 format-description and query events
// output: proof that sealed segments pass checksum verification, that task-N.binlog is not sealed or uploaded, and that a file/pos rotate still seals a checksum-valid segment
// pos: regression coverage for issue 205
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// mysql8EventHeaderLengths is the post-header length array from a MySQL 8.0
// format description. Index 1 is QUERY_EVENT (13). The checksum algorithm
// byte follows this array.
var mysql8EventHeaderLengths = []byte{
	0x38, 0x0d, 0x00, 0x08, 0x00, 0x12, 0x00, 0x04, 0x04, 0x04, 0x04, 0x12, 0x00, 0x00, 0x5c, 0x00,
	0x04, 0x1a, 0x08, 0x00, 0x00, 0x00, 0x08, 0x08, 0x08, 0x02, 0x00, 0x00, 0x00, 0x0a, 0x0a, 0x0a,
	0x19, 0x19, 0x00, 0x12, 0x34, 0x00, 0x0a, 0x28, 0x00,
}

func TestGTIDStart_SealedSegmentsVerifyAndSkipPlaceholder(t *testing.T) {
	const firstSQL = "INSERT INTO checksum_probe VALUES ('GTID-START')"
	const nextSQL = "INSERT INTO checksum_probe VALUES ('GTID-NEXT')"
	rotateIn := artificialRotateNoCRC("mysql-bin.000010")
	rotateOut := artificialRotateNoCRC("mysql-bin.000011")
	events := checksumStream(t, 4, rotateIn, firstSQL, rotateOut, nextSQL)

	dir, catalog, uploads := runChecksumDump(t, checksumDump{
		start: tasks.StartConfig{
			Mode:    tasks.StartModeGTID,
			GTIDSet: sampleGTID + ":1-5",
		},
		events: events,
	})

	assertNoPlaceholder(t, dir, catalog, uploads)
	sealed := filepath.Join(dir, "mysql-bin.000010")
	opened := filepath.Join(dir, "mysql-bin.000011")
	assertChecksumSegment(t, sealed, firstSQL, rotateIn.RawData)
	assertChecksumSegment(t, opened, nextSQL, rotateOut.RawData)
	if uploads.of("mysql-bin.000010") != 1 || uploads.of("mysql-bin.000011") != 0 {
		t.Fatalf("uploads %+v", uploads.names())
	}
	if catalog.state("mysql-bin.000010") != "SEALED" || catalog.state("mysql-bin.000011") != "OPEN" {
		t.Fatalf("catalog %+v", catalog.rows)
	}
}

func TestFilePos1236_DoesNotSealEmptyOrUnchecksummedSegment(t *testing.T) {
	const resumedSQL = "INSERT INTO checksum_probe VALUES ('ALREADY-ON-DISK')"
	const nextSQL = "INSERT INTO checksum_probe VALUES ('AFTER-1236')"
	// A file/pos resume whose local segment already has events. The 1236
	// fallback's artificial rotate must not be appended, and the sealed
	// bytes must still verify.
	t.Run("existing segment", func(t *testing.T) {
		rotateOut := artificialRotateNoCRC("mysql-bin.000012")
		body, endPos := checksumPrefix(t, resumedSQL)
		dir, catalog, uploads := runChecksumDump(t, checksumDump{
			start: tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000008", Pos: 4},
			checkpoint: &binlogCheckpoint{
				File: "mysql-bin.000008", Pos: endPos, GTIDSet: sampleGTID + ":1-20",
			},
			preload: map[string][]byte{"mysql-bin.000008": body},
			events:  checksumStream(t, 4, rotateOut, nextSQL, artificialRotateNoCRC("mysql-bin.000013"), ""),
			purged:  true,
		})
		assertNoPlaceholder(t, dir, catalog, uploads)
		sealed := filepath.Join(dir, "mysql-bin.000008")
		assertChecksumSegment(t, sealed, resumedSQL, rotateOut.RawData)
		if uploads.of("mysql-bin.000008") != 1 {
			t.Fatalf("uploads %+v", uploads.names())
		}
		next := filepath.Join(dir, "mysql-bin.000012")
		assertChecksumSegment(t, next, nextSQL, nil)
	})

	// The purged name was opened as magic only. It must not be sealed or uploaded.
	t.Run("magic only", func(t *testing.T) {
		rotateIn := artificialRotateNoCRC("mysql-bin.000012")
		const sql = "INSERT INTO checksum_probe VALUES ('MAGIC-FALLBACK')"
		events := checksumStream(t, 4, rotateIn, sql, artificialRotateNoCRC("mysql-bin.000013"), "")
		dir, catalog, uploads := runChecksumDump(t, checksumDump{
			start: tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000008", Pos: 400},
			checkpoint: &binlogCheckpoint{
				File: "mysql-bin.000008", Pos: 400, GTIDSet: sampleGTID + ":1-20",
			},
			events: events,
			purged: true,
		})
		assertNoPlaceholder(t, dir, catalog, uploads)
		if _, err := os.Stat(filepath.Join(dir, "mysql-bin.000008")); !os.IsNotExist(err) {
			t.Fatalf("magic-only purged file still present: %v", err)
		}
		if catalog.has("mysql-bin.000008") || uploads.of("mysql-bin.000008") != 0 {
			t.Fatalf("catalog %+v uploads %+v", catalog.rows, uploads.names())
		}
		assertChecksumSegment(t, filepath.Join(dir, "mysql-bin.000012"), sql, rotateIn.RawData)
		if catalog.state("mysql-bin.000012") != "SEALED" {
			t.Fatalf("catalog %+v", catalog.rows)
		}
	})
}

func TestFilePosRotate_SealedSegmentVerifies(t *testing.T) {
	const sql = "INSERT INTO checksum_probe VALUES ('FILE-POS')"
	ts := uint32(1_700_000_000)
	pos := uint32(4)
	fde, pos := crcFormatDescription(ts, pos)
	query, pos := crcQueryEvent("t", sql, ts+1, pos)
	rotate, _ := crcRotateEvent("mysql-bin.000002", 4, ts+2, pos)
	dir, catalog, uploads := runChecksumDump(t, checksumDump{
		start: tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 4},
		events: []*goreplication.BinlogEvent{
			fde, query, rotate,
		},
	})
	assertNoPlaceholder(t, dir, catalog, uploads)
	sealed := filepath.Join(dir, "mysql-bin.000001")
	assertChecksumSegment(t, sealed, sql, nil)
	body, err := os.ReadFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, rotate.RawData) {
		t.Fatal("real rotate was dropped from the sealed file/pos segment")
	}
	if uploads.of("mysql-bin.000001") != 1 {
		t.Fatalf("uploads %+v", uploads.names())
	}
}

type binlogCheckpoint struct {
	File    string
	Pos     uint32
	GTIDSet string
}

type checksumDump struct {
	start      tasks.StartConfig
	checkpoint *binlogCheckpoint
	preload    map[string][]byte
	events     []*goreplication.BinlogEvent
	purged     bool
}

type sealLog struct {
	rows []tasks.BinlogFile
}

func (s *sealLog) handle(_ context.Context, file tasks.BinlogFile) error {
	s.rows = append(s.rows, file)
	return nil
}

func (s *sealLog) of(name string) int {
	n := 0
	for _, row := range s.rows {
		if row.FileName == name || strings.Contains(row.ObjectKey, name) {
			n++
		}
	}
	return n
}

func (s *sealLog) names() []string {
	out := make([]string, 0, len(s.rows))
	for _, row := range s.rows {
		out = append(out, row.FileName)
	}
	return out
}

func (c *takeoverCatalog) state(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range c.rows {
		if row.FileName == name {
			return row.State
		}
	}
	return ""
}

func (c *takeoverCatalog) has(name string) bool {
	return c.state(name) != ""
}

func runChecksumDump(t *testing.T, dump checksumDump) (string, *takeoverCatalog, *sealLog) {
	t.Helper()
	base := t.TempDir()
	task := newRunnerTask(dump.start)
	taskDir := filepath.Join(base, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range dump.preload {
		if err := os.WriteFile(filepath.Join(taskDir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := &fakeRunnerCheckpointStore{}
	if dump.checkpoint != nil {
		store.loadOK = true
		store.loadCheckpoint = binlog.Checkpoint{
			File: dump.checkpoint.File, Pos: dump.checkpoint.Pos, GTIDSet: dump.checkpoint.GTIDSet,
		}
	}
	results := make([]streamResult, 0, len(dump.events)+1)
	for _, ev := range dump.events {
		if ev == nil || ev.Header == nil {
			continue
		}
		results = append(results, streamResult{event: ev})
	}
	results = append(results, streamResult{err: context.Canceled})
	var syncer binlogSyncer
	if dump.purged {
		purged := &gomysql.MyError{Code: 1236, State: "HY000", Message: "Could not find first log file name in binary log index file"}
		syncer = &purgeFileSyncer{
			streamer:     &fakeStreamer{results: []streamResult{{err: purged}}},
			gtidStreamer: &fakeStreamer{results: results},
		}
	} else {
		syncer = &fakeSyncer{streamer: &fakeStreamer{results: results}}
	}
	catalog := &takeoverCatalog{}
	uploads := &sealLog{}
	runner := NewMySQLRunner(base,
		WithCheckpointStore(store),
		WithFileMetaStore(catalog),
		WithSealedHandler(uploads.handle, "prefix"),
	)
	runner.fetcher = &fakeSourceMetaFetcher{serverUUID: "11111111-1111-1111-1111-111111111111"}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return taskDir, catalog, uploads
}

func assertNoPlaceholder(t *testing.T, dir string, catalog *takeoverCatalog, uploads *sealLog) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "task-") {
			t.Fatalf("placeholder segment %s", entry.Name())
		}
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	for _, row := range catalog.rows {
		if strings.HasPrefix(row.FileName, "task-") {
			t.Fatalf("catalog placeholder %+v", row)
		}
	}
	for _, row := range uploads.rows {
		if strings.HasPrefix(row.FileName, "task-") || strings.Contains(filepath.Base(row.ObjectKey), "task-") {
			t.Fatalf("uploaded placeholder %+v", row)
		}
	}
}

func assertChecksumSegment(t *testing.T, path, sql string, absent []byte) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if sql != "" && !bytes.Contains(body, []byte(sql)) {
		t.Fatalf("%s missing %q (%d bytes)", path, sql, len(body))
	}
	if len(absent) > 0 && bytes.Contains(body, absent) {
		t.Fatalf("%s contains the no-CRC artificial rotate (%d bytes)", path, len(body))
	}
	parser := goreplication.NewBinlogParser()
	parser.SetVerifyChecksum(true)
	if err := parser.ParseFile(path, 0, func(*goreplication.BinlogEvent) error { return nil }); err != nil {
		t.Fatalf("checksum %s: %v", path, err)
	}
	// mysqlbinlog is not in the unit-test image. When it is on PATH, this is the
	// same check a DBA runs. The parser above is the CRC32 check that always runs.
	bin, lookErr := exec.LookPath("mysqlbinlog")
	if lookErr != nil {
		return
	}
	cmd := exec.Command(bin, "--verify-binlog-checksum", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mysqlbinlog --verify-binlog-checksum %s: %v\n%s", path, err, out)
	}
}

func checksumPrefix(t *testing.T, sql string) ([]byte, uint32) {
	t.Helper()
	ts := uint32(1_700_000_000)
	pos := uint32(4)
	fde, pos := crcFormatDescription(ts, pos)
	query, pos := crcQueryEvent("t", sql, ts+1, pos)
	var buf bytes.Buffer
	buf.Write(binlogMagic)
	buf.Write(fde.RawData)
	buf.Write(query.RawData)
	return buf.Bytes(), pos
}

// checksumStream is an artificial rotate, then one query in that file, then a
// rotate that seals it, then an optional query in the next file.
func checksumStream(t *testing.T, start uint32, rotateIn *goreplication.BinlogEvent, sql string, rotateOut *goreplication.BinlogEvent, nextSQL string) []*goreplication.BinlogEvent {
	t.Helper()
	ts := uint32(1_700_000_000)
	pos := start
	fde, pos := crcFormatDescription(ts, pos)
	query, pos := crcQueryEvent("t", sql, ts+1, pos)
	out := []*goreplication.BinlogEvent{rotateIn, fde, query, rotateOut}
	if nextSQL != "" {
		// The next file starts at position 4. Its format description is the
		// event MySQL sends after the rotate.
		nextFDE, nextPos := crcFormatDescription(ts+2, 4)
		nextQuery, _ := crcQueryEvent("t", nextSQL, ts+3, nextPos)
		out = append(out, nextFDE, nextQuery)
	}
	return out
}

func artificialRotateNoCRC(next string) *goreplication.BinlogEvent {
	name := []byte(next)
	body := make([]byte, 8+len(name))
	binary.LittleEndian.PutUint64(body[:8], 4)
	copy(body[8:], name)
	size := uint32(goreplication.EventHeaderSize + len(body))
	raw := make([]byte, size)
	raw[4] = byte(goreplication.ROTATE_EVENT)
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint16(raw[17:19], goreplication.LOG_EVENT_ARTIFICIAL_F)
	copy(raw[goreplication.EventHeaderSize:], body)
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.ROTATE_EVENT,
			ServerID:  1,
			EventSize: size,
			Flags:     goreplication.LOG_EVENT_ARTIFICIAL_F,
		},
		Event:   &goreplication.RotateEvent{Position: 4, NextLogName: append([]byte(nil), name...)},
		RawData: raw,
	}
}

func crcFormatDescription(ts, start uint32) (*goreplication.BinlogEvent, uint32) {
	body := make([]byte, 57+len(mysql8EventHeaderLengths)+1)
	binary.LittleEndian.PutUint16(body[0:2], 4)
	copy(body[2:52], []byte("8.0.36"))
	binary.LittleEndian.PutUint32(body[52:56], ts)
	body[56] = byte(goreplication.EventHeaderSize)
	copy(body[57:], mysql8EventHeaderLengths)
	body[len(body)-1] = byte(goreplication.BINLOG_CHECKSUM_ALG_CRC32)
	ev, end := crcEvent(goreplication.FORMAT_DESCRIPTION_EVENT, body, start, ts, 0)
	ev.Event = &goreplication.FormatDescriptionEvent{
		ChecksumAlgorithm: goreplication.BINLOG_CHECKSUM_ALG_CRC32,
		ServerVersion:     "8.0.36",
	}
	return ev, end
}

func crcQueryEvent(schema, query string, ts, start uint32) (*goreplication.BinlogEvent, uint32) {
	body := make([]byte, 13+len(schema)+1+len(query))
	body[8] = byte(len(schema))
	copy(body[13:], schema)
	body[13+len(schema)] = 0
	copy(body[14+len(schema):], query)
	ev, end := crcEvent(goreplication.QUERY_EVENT, body, start, ts, 0)
	ev.Event = &goreplication.QueryEvent{Schema: []byte(schema), Query: []byte(query)}
	return ev, end
}

func crcRotateEvent(next string, position uint64, ts, start uint32) (*goreplication.BinlogEvent, uint32) {
	name := []byte(next)
	body := make([]byte, 8+len(name))
	binary.LittleEndian.PutUint64(body[:8], position)
	copy(body[8:], name)
	ev, end := crcEvent(goreplication.ROTATE_EVENT, body, start, ts, 0)
	ev.Event = &goreplication.RotateEvent{Position: position, NextLogName: append([]byte(nil), name...)}
	return ev, end
}

func crcEvent(eventType goreplication.EventType, body []byte, start, ts uint32, flags uint16) (*goreplication.BinlogEvent, uint32) {
	size := uint32(goreplication.EventHeaderSize + len(body) + 4)
	end := start + size
	raw := make([]byte, size)
	binary.LittleEndian.PutUint32(raw[0:4], ts)
	raw[4] = byte(eventType)
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint32(raw[13:17], end)
	binary.LittleEndian.PutUint16(raw[17:19], flags)
	copy(raw[goreplication.EventHeaderSize:], body)
	binary.LittleEndian.PutUint32(raw[len(raw)-4:], crc32.ChecksumIEEE(raw[:len(raw)-4]))
	return &goreplication.BinlogEvent{
		RawData: raw,
		Header: &goreplication.EventHeader{
			Timestamp: ts,
			EventType: eventType,
			ServerID:  1,
			EventSize: size,
			LogPos:    end,
			Flags:     flags,
		},
	}, end
}
