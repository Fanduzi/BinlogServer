// Package binlog provides module-level functionality for binlog.
// input: a sealed segment's directory, base name, size and sha256 at seal; the task directory when reading them back
// output: the seal checksum ledger {task dir}/.seal-sha256: one "sha256 size name" line appended per seal (the last line of a name wins), LoadSealSums to read it, SealSumAt for one name (sealSumAt caches it per ledger stamp for the unreadable check, #381), and compaction that drops names no longer on disk once the ledger passes 1 MiB; it is the expected checksum the health rehasher compares with when no catalog row carries one (standalone without meta, or meta without migration 000008)
// pos: ADR 0009 P2-3 rehash source (#380 QA): the bytes at seal, recorded next to the segments they describe
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// SealSumsFileName is the ledger file in a task directory. It is not a
// segment name, so directory listings and the replay order skip it.
const SealSumsFileName = ".seal-sha256"

// sealSumsCompactAt is the ledger size that triggers a compaction.
const sealSumsCompactAt = 1 << 20

// SealSum is one segment's checksum and size at seal.
type SealSum struct {
	SHA256 string
	Size   int64
}

var sealSumsMu sync.Mutex // one writer per ledger in this process

// RecordSealSum appends name's checksum to dir's ledger and fsyncs it.
func RecordSealSum(dir, name string, size int64, sha string) error {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if dir == "" || name == "" || sha == "" || strings.ContainsAny(name, " \n") {
		return nil
	}
	sealSumsMu.Lock()
	defer sealSumsMu.Unlock()
	path := filepath.Join(dir, SealSumsFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s %d %s\n", sha, size, name); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	info, statErr := f.Stat()
	if err := f.Close(); err != nil {
		return err
	}
	if statErr == nil && info.Size() > sealSumsCompactAt {
		return compactSealSums(dir)
	}
	return nil
}

// LoadSealSums reads dir's ledger. A missing ledger is an empty map.
func LoadSealSums(dir string) (map[string]SealSum, error) {
	f, err := os.Open(filepath.Join(dir, SealSumsFileName))
	if os.IsNotExist(err) {
		return map[string]SealSum{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]SealSum{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) != 3 || len(parts[0]) != 64 {
			continue // a torn last line
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		out[parts[2]] = SealSum{SHA256: parts[0], Size: size}
	}
	return out, sc.Err()
}

var sealSumsCache stampCache[map[string]SealSum] // ledger path -> LoadSealSums

// sealSumAt is name's checksum and size at seal in dir's ledger, read once
// per ledger size and mtime. A missing or unreadable ledger has no entry.
// SealSumAt is name's last ledger line in dir, cached per ledger stamp.
func SealSumAt(dir, name string) (SealSum, bool) { return sealSumAt(dir, name) }

func sealSumAt(dir, name string) (SealSum, bool) {
	path := filepath.Join(dir, SealSumsFileName)
	info, err := Stat(path)
	if err != nil {
		return SealSum{}, false
	}
	st := StampOf(info)
	sums, ok := sealSumsCache.get(path, st)
	if !ok {
		if sums, err = LoadSealSums(dir); err != nil {
			return SealSum{}, false
		}
		sealSumsCache.put(path, st, sums)
	}
	sum, ok := sums[name]
	return sum, ok
}

// compactSealSums rewrites the ledger with one line per name still on disk.
func compactSealSums(dir string) error {
	sums, err := LoadSealSums(dir)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, SealSumsFileName+".tmp-*")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tmp)
	for name, s := range sums {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			continue
		}
		fmt.Fprintf(w, "%s %d %s\n", s.SHA256, s.Size, name)
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, SealSumsFileName))
}

// RecordSealFacts records the checksum of the sealed file at path.
func RecordSealFacts(path string, f SealFacts) error {
	return RecordSealSum(filepath.Dir(path), filepath.Base(path), f.Size, f.SHA256)
}
