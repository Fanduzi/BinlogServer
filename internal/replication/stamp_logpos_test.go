// Package replication provides module-level functionality for replication.
// input: RawData whose end_log_pos is 0 and a filled-in Header LogPos
// output: proof that stampLogPosInRaw writes that LogPos into the bytes (and CRC when asked), and that MariaDB syncer config enables FillZeroLogPos
// pos: unit coverage for the MariaDB 11.4 LogPos=0 path (#359)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"encoding/binary"
	"hash/crc32"
	"testing"

	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestStampLogPosInRaw_PatchesZeroEndLogPos(t *testing.T) {
	raw := make([]byte, 30)
	raw[4] = byte(goreplication.TABLE_MAP_EVENT)
	binary.LittleEndian.PutUint32(raw[9:13], 30)
	got := stampLogPosInRaw(raw, 4096, false)
	if binary.LittleEndian.Uint32(got[13:17]) != 4096 {
		t.Fatalf("logpos %d", binary.LittleEndian.Uint32(got[13:17]))
	}
	if binary.LittleEndian.Uint32(raw[13:17]) != 0 {
		t.Fatal("input mutated")
	}
	same := stampLogPosInRaw(got, 8192, false)
	if binary.LittleEndian.Uint32(same[13:17]) != 4096 {
		t.Fatal("non-zero logpos must stay")
	}
}

func TestStampLogPosInRaw_RewritesCRC(t *testing.T) {
	raw := make([]byte, 34)
	raw[4] = byte(goreplication.WRITE_ROWS_EVENTv1)
	binary.LittleEndian.PutUint32(raw[9:13], 34)
	binary.LittleEndian.PutUint32(raw[len(raw)-4:], crc32.ChecksumIEEE(raw[:len(raw)-4]))
	got := stampLogPosInRaw(raw, 5000, true)
	want := crc32.ChecksumIEEE(got[:len(got)-4])
	if binary.LittleEndian.Uint32(got[len(got)-4:]) != want {
		t.Fatalf("crc not rewritten")
	}
	if binary.LittleEndian.Uint32(got[13:17]) != 5000 {
		t.Fatal("logpos")
	}
}

// go-mysql's FillZeroLogPos also fills the mid-file format description
// MariaDB sends with log_pos=0 and no ARTIFICIAL flag (start+252), which
// pushed the stored cursor past the next GTID: STREAM_REGRESSION (#362 QA).
// We ask for ANNOTATE_ROWS ourselves and fill positions in the runner.
func TestBuildSyncerConfig_MariaDBAnnotateWithoutFillZeroLogPos(t *testing.T) {
	cfg := buildSyncerConfig(tasks.Task{
		ID:     "1",
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mariadb", ServerID: 99},
	})
	if cfg.FillZeroLogPos {
		t.Fatal("MariaDB must not use go-mysql FillZeroLogPos: it fills the mid-file FDE")
	}
	if cfg.DumpCommandFlag&goreplication.BINLOG_SEND_ANNOTATE_ROWS_EVENT == 0 {
		t.Fatal("MariaDB must ask for ANNOTATE_ROWS so positions have no holes")
	}
	cfg = buildSyncerConfig(tasks.Task{
		ID:     "1",
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql", ServerID: 99},
	})
	if cfg.FillZeroLogPos || cfg.DumpCommandFlag != 0 {
		t.Fatalf("MySQL config changed: fill=%v flag=%d", cfg.FillZeroLogPos, cfg.DumpCommandFlag)
	}
}

func TestFilledEndPos(t *testing.T) {
	hdr := func(kind goreplication.EventType, logPos uint32, flags uint16) *goreplication.EventHeader {
		return &goreplication.EventHeader{EventType: kind, LogPos: logPos, EventSize: 100, Flags: flags}
	}
	cases := []struct {
		name   string
		flavor string
		h      *goreplication.EventHeader
		want   uint32
	}{
		{"mid-file FDE", "mariadb", hdr(goreplication.FORMAT_DESCRIPTION_EVENT, 0, 0), 0},
		{"rotate", "mariadb", hdr(goreplication.ROTATE_EVENT, 0, 0), 0},
		{"gtid list", "mariadb", hdr(goreplication.MARIADB_GTID_LIST_EVENT, 0, 0), 0},
		{"binlog checkpoint", "mariadb", hdr(goreplication.MARIADB_BINLOG_CHECKPOINT_EVENT, 0, 0), 0},
		{"heartbeat", "mariadb", hdr(goreplication.HEARTBEAT_EVENT, 0, 0), 0},
		{"artificial", "mariadb", hdr(goreplication.TABLE_MAP_EVENT, 0, goreplication.LOG_EVENT_ARTIFICIAL_F), 0},
		{"annotate", "mariadb", hdr(goreplication.MARIADB_ANNOTATE_ROWS_EVENT, 0, 0), 1100},
		{"table map", "mariadb", hdr(goreplication.TABLE_MAP_EVENT, 0, 0), 1100},
		{"write rows", "mariadb", hdr(goreplication.WRITE_ROWS_EVENTv1, 0, 0), 1100},
		{"source pos not refilled", "mariadb", hdr(goreplication.TABLE_MAP_EVENT, 777, 0), 0},
		{"mysql untouched", "mysql", hdr(goreplication.TABLE_MAP_EVENT, 0, 0), 0},
	}
	for _, c := range cases {
		if got := filledEndPos(c.flavor, c.h, 1000); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	if got := filledEndPos("mariadb", hdr(goreplication.TABLE_MAP_EVENT, 0, 0), 0); got != 0 {
		t.Errorf("no cursor yet: got %d want 0", got)
	}
}
