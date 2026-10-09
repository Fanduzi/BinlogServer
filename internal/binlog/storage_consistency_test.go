// Package binlog provides module-level functionality for binlog.
// input: segment bytes, a checkpoint gtid_set, and a start gtid_set
// output: assertions that a stray rotate, a backwards event position, and the case-8 re-dump layout are reported, and that out-of-order commits, interleaved UUIDs, and a holed start set are not; the case-8 report names the damaged segment, the missing GTIDs, the earlier valid segment, and the restart set
// pos: unit coverage for detecting a backup damaged by a stale GTID re-dump
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

const storageUUID = "51ca62d9-1111-1111-1111-111111111111"

func TestDetectStorageProblem(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), nil, 37, 38, 39)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-39", "", "mysql"); found {
			t.Fatal("start set below the first stored event is not a problem")
		}
	})
	t.Run("out of order across segments", func(t *testing.T) {
		dir := t.TempDir()
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000001"), storageUUID, nil, []int64{39, 41}, []uint32{200, 400})
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000002"), storageUUID, nil, []int64{40}, []uint32{300})
		if _, found := DetectStorageProblem(dir, storageUUID+":1-41", "", "mysql"); found {
			t.Fatal("A:40 committing in the next file is replica commit order, not a re-dump")
		}
	})
	t.Run("interleaved uuids", func(t *testing.T) {
		dir := t.TempDir()
		const other = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		writeMixedGTIDs(t, filepath.Join(dir, "mysql-bin.000001"), []mixedGTID{
			{storageUUID, 5, 200},
			{other, 1, 300},
			{storageUUID, 4, 400},
			{other, 2, 500},
		})
		checkpoint := storageUUID + ":1-5," + other + ":1-2"
		if _, found := DetectStorageProblem(dir, checkpoint, "", "mysql"); found {
			t.Fatal("interleaved UUIDs and an out-of-order commit are not a re-dump")
		}
	})
	t.Run("start set hole inside range", func(t *testing.T) {
		dir := t.TempDir()
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000001"), storageUUID, nil, []int64{10, 11, 12, 14, 15}, nil)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-12:14-15", "", "mysql"); found {
			t.Fatal("a start-set hole inside the segment range is not a dropped transaction")
		}
	})
	t.Run("checkpoint past last event", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000001"), nil, 1, 2, 3)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-9", "", "mysql"); found {
			t.Fatal("a checkpoint past the last stored event is a deleted newer file, not this corruption")
		}
	})
	t.Run("retention gap", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000001"), nil, 1, 2, 3)
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000003"), nil, 7, 8, 9)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-9", "", "mysql"); found {
			t.Fatal("000002 is gone; the hole between the files that remain is not this corruption")
		}
	})
	t.Run("front expiry", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000003"), nil, 7, 8, 9)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-9", "", "mysql"); found {
			t.Fatal("events expired from the front are still in the checkpoint and are not this corruption")
		}
	})
	t.Run("start set has a hole", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000001"), nil, 1, 2, 3, 5, 6, 7, 8, 9)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-3:5-9", "", "mysql"); found {
			t.Fatal("a hole the checkpoint also omits is the start set, not a dropped transaction")
		}
	})
	t.Run("switchover chain", func(t *testing.T) {
		dir := t.TempDir()
		const oldServer = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		const newServer = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		writeNamedGTIDs(t, filepath.Join(dir, oldServer+".mysql-bin.000010"), oldServer, nil, []int64{10, 11, 12}, nil)
		writeNamedGTIDs(t, filepath.Join(dir, newServer+".mysql-bin.000003"), newServer, nil, []int64{1, 2, 3}, nil)
		checkpoint := oldServer + ":1-12," + newServer + ":1-3"
		if _, found := DetectStorageProblem(dir, checkpoint, "", "mysql"); found {
			t.Fatal("a second server uuid and the previous server's unstored prefix are not this corruption")
		}
	})
	t.Run("case-8 redump", func(t *testing.T) {
		dir := t.TempDir()
		const stored = "51ca62d9-c2c4-11f1-a149-822b383dbcd0"
		const other = "4bf78e6c-c2c4-11f1-bd7c-822b383dbcd0"
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000005"), stored, []string{"mysql-bin.000006"}, []int64{37}, nil)
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000006"), stored, []string{"mysql-bin.000006"}, []int64{38}, nil)
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000006.open.e2"), stored, []string{"mysql-bin.000006"}, []int64{38, 39, 43, 44}, nil)
		checkpoint := other + ":1-13," + stored + ":1-44"
		problem, found := DetectStorageProblem(dir, checkpoint, "", "mysql")
		if !found || !strings.Contains(problem.Message, "mysql-bin.000006") || !strings.Contains(problem.Message, "Create a new task") {
			t.Fatalf("problem %+v found=%v", problem, found)
		}
	})
	t.Run("case-8 names segment, missing gtids, and valid segments", func(t *testing.T) {
		dir := t.TempDir()
		const stored = "51ca62d9-c2c4-11f1-a149-822b383dbcd0"
		const other = "4bf78e6c-c2c4-11f1-bd7c-822b383dbcd0"
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000005"), stored, []string{"mysql-bin.000006"}, []int64{37}, nil)
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000006"), stored, []string{"mysql-bin.000006"}, []int64{38}, nil)
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000006.open.e2"), stored, []string{"mysql-bin.000006"}, []int64{38, 39, 43, 44}, nil)
		checkpoint := other + ":1-13," + stored + ":1-44"
		problem, found := DetectStorageProblem(dir, checkpoint, stored+":1-36", "mysql")
		if !found {
			t.Fatal("damage not found")
		}
		if problem.Segment != "mysql-bin.000006" {
			t.Fatalf("segment %q", problem.Segment)
		}
		if len(problem.Valid) != 1 || problem.Valid[0] != "mysql-bin.000005" {
			t.Fatalf("valid %v", problem.Valid)
		}
		if want := other + ":1-13," + stored + ":40-42"; problem.Missing != want {
			t.Fatalf("missing %q want %q", problem.Missing, want)
		}
		if want := stored + ":1-37"; problem.Restart != want {
			t.Fatalf("restart %q want %q", problem.Restart, want)
		}
		for _, part := range []string{"mysql-bin.000006", stored + ":40-42", "(mysql-bin.000005)", "still restores", "Create a new task from a GTID the source still has", "gtid_set " + stored + ":1-37"} {
			if !strings.Contains(problem.Message, part) {
				t.Fatalf("message lacks %q: %s", part, problem.Message)
			}
		}
	})
	t.Run("restart unknown without a start set or header", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), []string{"mysql-bin.000006"}, 37, 38, 39)
		problem, found := DetectStorageProblem(dir, "", "", "mysql")
		if !found || problem.Restart != "" || len(problem.Valid) != 0 || !strings.Contains(problem.Message, "No segment before mysql-bin.000006") {
			t.Fatalf("problem %+v found=%v", problem, found)
		}
	})
	t.Run("gtid order alone", func(t *testing.T) {
		dir := t.TempDir()
		writeNamedGTIDs(t, filepath.Join(dir, "mysql-bin.000006"), storageUUID, nil, []int64{39, 37}, []uint32{200, 300})
		if _, found := DetectStorageProblem(dir, storageUUID+":1-39", "", "mysql"); found {
			t.Fatal("a GTID sequence that goes backwards without a rotate or a position regression is replica commit order")
		}
	})
	t.Run("position regresses", func(t *testing.T) {
		dir := t.TempDir()
		var buf []byte
		buf = append(buf, 0xfe, 'b', 'i', 'n')
		buf = append(buf, eventBytes(byte(goreplication.QUERY_EVENT), 500, []byte("ok"))...)
		buf = append(buf, eventBytes(byte(goreplication.QUERY_EVENT), 120, []byte("back"))...)
		if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000006"), buf, 0o644); err != nil {
			t.Fatal(err)
		}
		problem, found := DetectStorageProblem(dir, "", "", "mysql")
		if !found || !strings.Contains(problem.Message, "end pos 120") {
			t.Fatalf("problem %+v found=%v", problem, found)
		}
	})
	t.Run("switch below min", func(t *testing.T) {
		dir := t.TempDir()
		seqs := make([]int64, 0, 34)
		for n := int64(6); n <= 39; n++ {
			seqs = append(seqs, n)
		}
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000001"), nil, seqs...)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-39", "", "mysql"); found {
			t.Fatal("sequences below the first stored event are the previous-GTIDs header, not a hole")
		}
	})
	t.Run("no events", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000001"), nil)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-36", "", "mysql"); found {
			t.Fatal("a uuid with no stored events is not a phantom")
		}
	})
	t.Run("mariadb skips gtid compare", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), nil, 37, 38, 39)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-44", "", "mariadb"); found {
			t.Fatal("mariadb does not compare mysql gtid intervals")
		}
	})
	t.Run("stray rotate", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), []string{"mysql-bin.000006"}, 37, 38, 39)
		problem, found := DetectStorageProblem(dir, storageUUID+":1-39", "", "mysql")
		if !found || !strings.Contains(problem.Message, "rotate to mysql-bin.000006") {
			t.Fatalf("problem %+v found=%v", problem, found)
		}
	})
	t.Run("forward rotate", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000005"), []string{"mysql-bin.000006"}, 37)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-37", "", "mysql"); found {
			t.Fatal("a rotate to the next file is not stray")
		}
	})
	t.Run("identity prefix", func(t *testing.T) {
		dir := t.TempDir()
		const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		writeGTIDSegment(t, filepath.Join(dir, id+".mysql-bin.000006"), []string{"mysql-bin.000006"})
		if _, found := DetectStorageProblem(dir, "", "", "mysql"); !found {
			t.Fatal("a prefixed segment that rotates to its own file is stray")
		}
	})
	t.Run("ignores notes", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), nil, 37)
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, found := DetectStorageProblem(dir, storageUUID+":1-37", "", "mysql"); found {
			t.Fatal("a non-segment file is not a problem")
		}
	})
}

type mixedGTID struct {
	uuid string
	seq  int64
	pos  uint32
}

func writeMixedGTIDs(t *testing.T, path string, events []mixedGTID) {
	t.Helper()
	var buf []byte
	buf = append(buf, 0xfe, 'b', 'i', 'n')
	for _, ev := range events {
		sid, err := hex.DecodeString(strings.ReplaceAll(ev.uuid, "-", ""))
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(ev.seq))
		buf = append(buf, eventBytes(byte(goreplication.GTID_EVENT), ev.pos, payload)...)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGTIDSegment(t *testing.T, path string, rotates []string, seqs ...int64) {
	t.Helper()
	writeNamedGTIDs(t, path, storageUUID, rotates, seqs, nil)
}

func writeNamedGTIDs(t *testing.T, path, uuid string, rotates []string, seqs []int64, positions []uint32) {
	t.Helper()
	var buf []byte
	buf = append(buf, 0xfe, 'b', 'i', 'n')
	sid, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	for i, seq := range seqs {
		pos := uint32(100 + seq)
		if i < len(positions) {
			pos = positions[i]
		}
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
		buf = append(buf, eventBytes(byte(goreplication.GTID_EVENT), pos, payload)...)
	}
	for _, name := range rotates {
		body := make([]byte, 8+len(name))
		binary.LittleEndian.PutUint64(body[:8], 4)
		copy(body[8:], name)
		buf = append(buf, eventBytes(byte(goreplication.ROTATE_EVENT), 1758, body)...)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventBytes(kind byte, logPos uint32, body []byte) []byte {
	hdr := make([]byte, goreplication.EventHeaderSize)
	hdr[4] = kind
	binary.LittleEndian.PutUint32(hdr[9:13], uint32(len(hdr)+len(body)))
	binary.LittleEndian.PutUint32(hdr[13:17], logPos)
	return append(hdr, body...)
}

func TestPlainBinlogStripsStintPrefix(t *testing.T) {
	const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	for _, name := range []string{"mysql-bin.000004", id + ".mysql-bin.000004", id + "~2.mysql-bin.000004", id + "~13.mysql-bin.000004"} {
		base, seq, ok := plainBinlog(name)
		if !ok || base != "mysql-bin" || seq != 4 {
			t.Fatalf("%s: %s %d %v", name, base, seq, ok)
		}
	}
	if base, _, ok := plainBinlog(id + "~x.mysql-bin.000004"); ok && base == "mysql-bin" {
		t.Fatal("~x is not a stint number")
	}
}
