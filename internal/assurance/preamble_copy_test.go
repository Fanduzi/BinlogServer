package assurance

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
)

// preambleOf is the magic plus the file's leading header events (format
// description, previous GTIDs): what a resumed open file of the same source
// file holds before its first transaction.
func preambleOf(t *testing.T, raw []byte) []byte {
	t.Helper()
	pos := 4
	for pos+19 <= len(raw) {
		typ := raw[pos+4]
		size := int(binary.LittleEndian.Uint32(raw[pos+9 : pos+13]))
		if typ != 15 && typ != 35 && typ != 4 { // FORMAT_DESCRIPTION, PREVIOUS_GTIDS, ROTATE
			break
		}
		pos += size
	}
	if pos <= 4 || pos >= len(raw) {
		t.Fatalf("no preamble in %d bytes", len(raw))
	}
	return raw[:pos]
}

// QA #404 fbup cluster: after the legacy failback repair the resumed
// A~2.mysql-bin.000004.open.e2 held only the preamble of that source file,
// which is byte-for-byte the start of A~2.mysql-bin.000004.sealed.e1. The
// byte-prefix rule called it a second copy, so storage_alert was
// STORAGE_INCONSISTENT and replay stopped until the next transaction
// arrived (indefinitely on an idle source). A preamble is not content.
func TestPreambleOnlyOpenIsNotACopy(t *testing.T) {
	f := live(t)
	last := f.in.Order[len(f.in.Order)-1]
	if err := os.Remove(filepath.Join(f.dir, last)); err != nil {
		t.Fatal(err)
	}
	f.in.Order = f.in.Order[:len(f.in.Order)-1]
	src := f.in.Order[len(f.in.Order)-1]
	raw, err := os.ReadFile(filepath.Join(f.dir, src))
	if err != nil {
		t.Fatal(err)
	}
	open := src + ".open.e9"
	if err := os.WriteFile(filepath.Join(f.dir, open), preambleOf(t, raw), 0o644); err != nil {
		t.Fatal(err)
	}
	f.in.Order = append(f.in.Order, open)
	f.in.Local = binlog.LedgerAt(f.dir)
	if v, found := ChainOf(f.in).Health(); found {
		t.Fatalf("preamble-only open file became a verdict: %+v", v)
	}
	if vs := ChainOf(f.in).Check(testNow); has(vs, I4Overlap, "") {
		t.Fatalf("check %s", kinds(vs))
	}
	// A real copy (preamble plus a transaction) is still a copy.
	if err := os.WriteFile(filepath.Join(f.dir, open), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f.in.Local = binlog.LedgerAt(f.dir)
	if v, found := ChainOf(f.in).Health(); !found || v.Kind != I4Overlap {
		t.Fatalf("full open copy: %+v %v", v, found)
	}
}
