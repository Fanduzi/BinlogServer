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

func TestBuildSyncerConfig_MariaDBSetsFillZeroLogPos(t *testing.T) {
	cfg := buildSyncerConfig(tasks.Task{
		ID:     "1",
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mariadb", ServerID: 99},
	})
	if !cfg.FillZeroLogPos {
		t.Fatal("MariaDB must set FillZeroLogPos (#359)")
	}
	cfg = buildSyncerConfig(tasks.Task{
		ID:     "1",
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql", ServerID: 99},
	})
	if cfg.FillZeroLogPos {
		t.Fatal("MySQL must not set FillZeroLogPos")
	}
}
