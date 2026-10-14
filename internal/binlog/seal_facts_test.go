// Package binlog provides module-level functionality for binlog.
// input: the ledger testdata segments, a magic-only file and a missing path
// output: assertions that ReadSealFacts matches EventTimeSpan and sha256 of the bytes, has no span for a magic-only file, and fails for a missing file
// pos: unit coverage for the catalog seal facts (ADR 0009 phase 1, P1-4)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSealFacts(t *testing.T) {
	for _, name := range []string{"live/mysql-bin.000003", "live/mysql-bin.000006", "failback/mysql-bin.000003.sealed.e1"} {
		path := filepath.Join("testdata", "ledger", name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		first, last, ok, err := EventTimeSpan(bytes.NewReader(raw))
		if err != nil || !ok {
			t.Fatalf("%s: span %v %v", name, ok, err)
		}
		sum := sha256.Sum256(raw)
		got, err := ReadSealFacts(path)
		if err != nil {
			t.Fatal(err)
		}
		if !got.HasSpan || !got.First.Equal(first) || !got.Last.Equal(last) || got.First.Location().String() != "UTC" {
			t.Fatalf("%s: span %+v want %v..%v", name, got, first, last)
		}
		if got.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s: sha %q", name, got.SHA256)
		}
	}
	magic := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(magic, []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSealFacts(magic)
	if err != nil || got.HasSpan || got.SHA256 == "" {
		t.Fatalf("magic-only %+v %v", got, err)
	}
	if _, err := ReadSealFacts(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file read")
	}
}
