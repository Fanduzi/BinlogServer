// Package binlog provides module-level functionality for binlog.
// input: segment bytes and a checkpoint gtid_set
// output: assertions that a stray rotate or a checkpoint ahead of the stored events is reported, and that a healthy chain, a switch, and MariaDB are not
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
		if _, found := DetectStorageProblem(dir, storageUUID+":1-39", "mysql"); found {
			t.Fatal("start set below the first stored event is not a problem")
		}
	})
	t.Run("checkpoint ahead", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), nil, 37, 38, 39, 43, 44)
		problem, found := DetectStorageProblem(dir, storageUUID+":1-44", "")
		if !found || !strings.Contains(problem.Message, storageUUID+":40") || !strings.Contains(problem.Message, "Create a new task") {
			t.Fatalf("problem %+v found=%v", problem, found)
		}
	})
	t.Run("max plus one", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), nil, 37, 38, 39)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-40", "mysql"); !found {
			t.Fatal("checkpoint claims the next sequence, which is not stored")
		}
	})
	t.Run("switch below min", func(t *testing.T) {
		dir := t.TempDir()
		seqs := make([]int64, 0, 34)
		for n := int64(6); n <= 39; n++ {
			seqs = append(seqs, n)
		}
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000001"), nil, seqs...)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-39", "mysql"); found {
			t.Fatal("sequences below the first stored event are the previous-GTIDs header, not a hole")
		}
	})
	t.Run("no events", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000001"), nil)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-36", "mysql"); found {
			t.Fatal("a uuid with no stored events is not a phantom")
		}
	})
	t.Run("mariadb skips gtid compare", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), nil, 37, 38, 39)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-44", "mariadb"); found {
			t.Fatal("mariadb does not compare mysql gtid intervals")
		}
	})
	t.Run("stray rotate", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), []string{"mysql-bin.000006"}, 37, 38, 39)
		problem, found := DetectStorageProblem(dir, storageUUID+":1-39", "mysql")
		if !found || !strings.Contains(problem.Message, "rotate to mysql-bin.000006") {
			t.Fatalf("problem %+v found=%v", problem, found)
		}
	})
	t.Run("forward rotate", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000005"), []string{"mysql-bin.000006"}, 37)
		if _, found := DetectStorageProblem(dir, storageUUID+":1-37", "mysql"); found {
			t.Fatal("a rotate to the next file is not stray")
		}
	})
	t.Run("identity prefix", func(t *testing.T) {
		dir := t.TempDir()
		const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		writeGTIDSegment(t, filepath.Join(dir, id+".mysql-bin.000006"), []string{"mysql-bin.000006"})
		if _, found := DetectStorageProblem(dir, "", "mysql"); !found {
			t.Fatal("a prefixed segment that rotates to its own file is stray")
		}
	})
	t.Run("ignores notes", func(t *testing.T) {
		dir := t.TempDir()
		writeGTIDSegment(t, filepath.Join(dir, "mysql-bin.000006"), nil, 37)
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, found := DetectStorageProblem(dir, storageUUID+":1-37", "mysql"); found {
			t.Fatal("a non-segment file is not a problem")
		}
	})
}

func writeGTIDSegment(t *testing.T, path string, rotates []string, seqs ...int64) {
	t.Helper()
	var buf []byte
	buf = append(buf, 0xfe, 'b', 'i', 'n')
	sid, err := hex.DecodeString(strings.ReplaceAll(storageUUID, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range seqs {
		payload := make([]byte, 1+goreplication.SidLength+8)
		copy(payload[1:], sid)
		binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
		buf = append(buf, eventBytes(byte(goreplication.GTID_EVENT), uint32(100+seq), payload)...)
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
