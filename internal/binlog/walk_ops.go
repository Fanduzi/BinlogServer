// Package binlog provides module-level functionality for binlog.
// input: paths the segment walk stats, opens or lists
// output: Stat, Open and ReadDir, each counted in WalkOps together with every StampOf; ResetWalkOps zeroes that count
// pos: the filesystem seam the cold segment walk uses, so a nested stat, open or stamp cannot skip the count by avoiding the call in Chain.Segments
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"os"
	"path/filepath"
	"sync/atomic"
)

// walkOps counts Stat, Open, ReadDir, a ReadDir entry's Info, and StampOf.
// The evaluator's cold pass goes through these, so a nested walk that skips
// the old per-segment counter still counts.
var walkOps atomic.Int64

func noteWalk() { walkOps.Add(1) }

// WalkOps is the number of filesystem and stamp operations since the last reset.
func WalkOps() int64 { return walkOps.Load() }

// ResetWalkOps zeroes the walk counter.
func ResetWalkOps() { walkOps.Store(0) }

// Stat is os.Stat, counted as one walk operation.
func Stat(name string) (os.FileInfo, error) {
	noteWalk()
	return os.Stat(name)
}

// Open is os.Open, counted as one walk operation.
func Open(name string) (*os.File, error) {
	noteWalk()
	return os.Open(name)
}

// ReadDir is os.ReadDir, counted as one walk operation. Info on a returned
// entry is counted as well, then reads the entry's own info.
func ReadDir(name string) ([]os.DirEntry, error) {
	noteWalk()
	entries, err := os.ReadDir(name)
	if err != nil {
		return nil, err
	}
	out := make([]os.DirEntry, len(entries))
	for i, entry := range entries {
		out[i] = countedEntry{DirEntry: entry, path: filepath.Join(name, entry.Name())}
	}
	return out, nil
}

type countedEntry struct {
	os.DirEntry
	path string
}

func (e countedEntry) Info() (os.FileInfo, error) {
	noteWalk()
	return e.DirEntry.Info()
}
