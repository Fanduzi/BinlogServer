// Package binlog provides module-level functionality for binlog.
// input: a temp task directory and real MySQL 8.0 / MariaDB 11.4 sealed segments
// output: the seal checksum ledger round-trips with the last line of a name winning, skips a torn line and compacts away names no longer on disk; a sealed segment with a flipped CRC32-covered byte is ErrCorruptEventSize wrapping ErrEventChecksum while an open read of the same bytes is not (#380 QA)
// pos: ADR 0009 phase 2 P2-3 QA round 1
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealSumsLedger(t *testing.T) {
	dir := t.TempDir()
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, r := range []struct {
		name string
		size int64
		sha  string
	}{{"mysql-bin.000001", 10, a}, {"mysql-bin.000002", 20, a}, {"mysql-bin.000001", 11, b}} {
		if err := RecordSealSum(dir, r.name, r.size, r.sha); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.OpenFile(filepath.Join(dir, SealSumsFileName), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("deadbeef 3 mysql-bin.00") // torn last line
	f.Close()
	sums, err := LoadSealSums(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 || sums["mysql-bin.000001"] != (SealSum{b, 11}) || sums["mysql-bin.000002"] != (SealSum{a, 20}) {
		t.Fatalf("sums %+v", sums)
	}
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000002"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compactSealSums(dir); err != nil {
		t.Fatal(err)
	}
	if sums, _ := LoadSealSums(dir); len(sums) != 1 || sums["mysql-bin.000002"].SHA256 != a {
		t.Fatalf("compacted %+v", sums)
	}
}

func TestSealedReadChecksCRC32(t *testing.T) {
	for _, p := range []string{"../tasks/testdata/real-mysql80/mysql-bin.000004", "../tasks/testdata/real-mariadb114/mysql-bin.000003"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSealedSegmentContents(bytes.NewReader(raw)); err != nil {
			t.Fatalf("%s clean: %v", p, err)
		}
		raw[len(raw)-6] ^= 0xff // inside the last event, under its CRC32
		_, err = ReadSealedSegmentContents(bytes.NewReader(raw))
		if !errors.Is(err, ErrCorruptEventSize) || !errors.Is(err, ErrEventChecksum) {
			t.Fatalf("%s flipped: %v", p, err)
		}
		if _, err := ReadSegmentContents(bytes.NewReader(raw)); err != nil {
			t.Fatalf("%s open read must not check CRC: %v", p, err)
		}
	}
}

func TestStampCacheIsBounded(t *testing.T) {
	var c stampCache[int]
	st := FileStamp{Size: 1}
	for i := 0; i <= stampCacheCap; i++ {
		c.put(strings.Repeat("p", 1)+string(rune(i)), st, i)
	}
	if n := c.len(); n > stampCacheCap || n == 0 {
		t.Fatalf("cache holds %d entries, cap %d", n, stampCacheCap)
	}
	if _, ok := c.get("x", FileStamp{Size: 2}); ok {
		t.Fatal("hit on a missing key")
	}
	c.put("x", st, 7)
	if v, ok := c.get("x", st); !ok || v != 7 {
		t.Fatal("miss on a fresh key")
	}
	if _, ok := c.get("x", FileStamp{Size: 1, CTime: 9}); ok {
		t.Fatal("hit after the ctime changed")
	}
}
