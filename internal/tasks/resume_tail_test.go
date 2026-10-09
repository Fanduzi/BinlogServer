// Package tasks provides module-level functionality for tasks.
// input: a GTID task, a stored checkpoint that lags the open segment, and that segment on disk
// output: assertions that ResumePosition adds the open segment's complete transactions to gtid_set, reports the cut of an unfinished trailing transaction, and keeps updated_at from the stored row
// pos: GET /checkpoint shows the gtid_set Start will resume from
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestResumePosition_AddsOpenTailGTIDs(t *testing.T) {
	dir := t.TempDir()
	reader := &schedulerCheckpointReader{checkpoints: map[string]binlog.Checkpoint{}}
	s := NewScheduler(WithDataDir(dir), WithCheckpointReader(reader))
	task, err := s.CreateTaskFromSpec("gtid", "gtid-key", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-10"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seg, commitEnd, end := tailTestSegment(t)
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001.open.e1"), seg, 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 8, 1, 2, 3, 0, time.UTC)
	reader.checkpoints[task.ID] = binlog.Checkpoint{File: "mysql-bin.000001", Pos: 4, GTIDSet: gtidPITRUUID + ":1-10", UpdatedAt: at}
	cp, ok, err := s.ResumePosition(context.Background(), task.ID)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if want := gtidPITRUUID + ":1-11"; !strings.EqualFold(cp.GTIDSet, want) {
		t.Fatalf("gtid_set %q want %q", cp.GTIDSet, want)
	}
	if cp.File != "mysql-bin.000001" || cp.Pos != commitEnd {
		t.Fatalf("resume %s:%d want cut %d (end %d)", cp.File, cp.Pos, commitEnd, end)
	}
	if !cp.UpdatedAt.Equal(at) {
		t.Fatalf("updated_at %v", cp.UpdatedAt)
	}
	info, err := os.Stat(filepath.Join(taskDir, "mysql-bin.000001.open.e1"))
	if err != nil || info.Size() != int64(len(seg)) {
		t.Fatalf("GET must not truncate: %v size=%d", err, info.Size())
	}
}

// tailTestSegment is GTID 11 + XID (complete) then GTID 12 without a commit.
func tailTestSegment(t *testing.T) ([]byte, uint32, uint32) {
	t.Helper()
	sid, err := hex.DecodeString(strings.ReplaceAll(gtidPITRUUID, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	buf := []byte{0xfe, 'b', 'i', 'n'}
	pos := uint32(4)
	add := func(kind goreplication.EventType, payload []byte) {
		size := uint32(goreplication.EventHeaderSize + len(payload))
		pos += size
		hdr := make([]byte, goreplication.EventHeaderSize)
		hdr[4] = byte(kind)
		binary.LittleEndian.PutUint32(hdr[9:13], size)
		binary.LittleEndian.PutUint32(hdr[13:17], pos)
		buf = append(buf, hdr...)
		buf = append(buf, payload...)
	}
	gtid := func(seq uint64) {
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], seq)
		add(goreplication.GTID_EVENT, payload)
	}
	gtid(11)
	add(goreplication.XID_EVENT, make([]byte, 8))
	commitEnd := pos
	gtid(12)
	return buf, commitEnd, pos
}
