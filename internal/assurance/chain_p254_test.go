// Package assurance provides module-level functionality for assurance.
// input: a sealed catalog row, an object listing and whether local retention already removed the local file
// output: proof that a retention marker suppresses I6_ORPHAN_ROW, that a listed-and-missing object on that row is I6_MISSING_OBJECT, and that an incomplete prefix judges neither (design 0009 §26.4)
// pos: P2-5.4 red tests for the retention marker on I6
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"os"
	"path/filepath"
	"testing"

	"binlog_server/internal/binlog"
)

func p254Chain(t *testing.T, onChain, marked, listObject, prefixOK bool) []Violation {
	t.Helper()
	dir := t.TempDir()
	const name = "mysql-bin.000005"
	const key = "p/c/u/mysql-bin.000005"
	row := Row{Name: name, Source: name, Epoch: 1, Sealed: true, Size: 100,
		UploadState: "UPLOADED", Checksum: "match", ObjectKey: key, SealedAt: testNow,
		RetentionRemoved: marked}
	in := Inputs{
		Local:         binlog.LedgerAt(dir),
		Rows:          []Row{row},
		Objects:       map[string]int64{},
		ObjectStorage: true,
	}
	if onChain {
		in.Order = []string{name}
	}
	if listObject {
		in.Objects[key] = 100
	}
	if !prefixOK {
		in.ObjectPrefixes = map[string]bool{ObjectPrefixOf(key): false}
	}
	return ChainOf(in).Check(testNow)
}

func TestP254RetentionMarkerIsNotAnOrphan(t *testing.T) {
	for _, onChain := range []bool{true, false} {
		vs := p254Chain(t, onChain, false, false, true)
		if !has(vs, I6OrphanRow, "mysql-bin.000005") {
			t.Errorf("onChain=%v: unmarked missing copies: %s", onChain, kinds(vs))
		}
		vs = p254Chain(t, onChain, true, false, true)
		if has(vs, I6OrphanRow, "mysql-bin.000005") {
			t.Errorf("onChain=%v: retention-removed row raised ORPHAN: %s", onChain, kinds(vs))
		}
		if !has(vs, I6MissingObject, "mysql-bin.000005") {
			t.Errorf("onChain=%v: listed-missing object after retention: %s", onChain, kinds(vs))
		}
		vs = p254Chain(t, onChain, true, true, true)
		if has(vs, I6OrphanRow, "mysql-bin.000005") || has(vs, I6MissingObject, "mysql-bin.000005") {
			t.Errorf("onChain=%v: object still present: %s", onChain, kinds(vs))
		}
		vs = p254Chain(t, onChain, false, false, false)
		if has(vs, I6OrphanRow, "mysql-bin.000005") || has(vs, I6MissingObject, "mysql-bin.000005") {
			t.Errorf("onChain=%v: incomplete prefix judged the row: %s", onChain, kinds(vs))
		}
	}
}

func TestP254MarkerDoesNotSilenceAPresentLocalCopy(t *testing.T) {
	dir := t.TempDir()
	const name = "mysql-bin.000005"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Inputs{
		Order: []string{name},
		Local: binlog.LedgerAt(dir),
		Rows: []Row{{Name: name, Source: name, Epoch: 1, Sealed: true, Size: 100,
			UploadState: "UPLOADED", Checksum: "match", ObjectKey: "p/c/u/" + name,
			RetentionRemoved: true}},
		Objects:       map[string]int64{},
		ObjectStorage: true,
	}
	vs := ChainOf(in).Check(testNow)
	if has(vs, I6OrphanRow, name) {
		t.Fatalf("local file is back: %s", kinds(vs))
	}
	if !has(vs, I6MissingObject, name) {
		t.Fatalf("object still missing while the file is on disk: %s", kinds(vs))
	}
}
