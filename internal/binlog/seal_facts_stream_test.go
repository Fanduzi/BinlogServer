// Package binlog provides module-level functionality for binlog.
// input: a real sealed segment followed by 64 MiB of padding
// output: assertions that ReadSealFacts streams: it allocates well under the file size, and its digest and span match the bytes
// pos: regression for #351 QA P2 (a 1 GiB seal cost about 1 GiB of memory)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadSealFactsStreams(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "ledger", "live", "mysql-bin.000003"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mysql-bin.000003")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const pad = 64 << 20
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if _, err := w.Write(src); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(w, zeroReader{}, pad); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(h.Sum(nil))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := ReadSealFacts(path)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4<<20 {
		t.Fatalf("ReadSealFacts allocated %d bytes for a %d byte file; it must stream", alloc, len(src)+pad)
	}
	if got.SHA256 != want {
		t.Fatalf("sha %s want %s", got.SHA256, want)
	}
	small, err := ReadSealFacts(filepath.Join("testdata", "ledger", "live", "mysql-bin.000003"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasSpan || !got.First.Equal(small.First) || !got.Last.Equal(small.Last) {
		t.Fatalf("span %+v want %+v", got, small)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
