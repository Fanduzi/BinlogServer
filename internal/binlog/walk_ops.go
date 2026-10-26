// Package binlog provides module-level functionality for binlog.
// input: paths the segment walk stats, opens or lists
// output: Stat, Lstat, Open, OpenFile, ReadDir, ReadFile and Glob, each counted in WalkOps together with every StampOf; ResetWalkOps zeroes that count
// pos: the filesystem seam the cold segment walk uses, so a nested stat, open, list, read or glob cannot skip the count by calling os directly
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"os"
	"path/filepath"
	"sync/atomic"
)

// walkOps counts Stat, Lstat, Open, OpenFile, ReadFile, ReadDir, Glob, a ReadDir entry's Info, and StampOf.
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

// Lstat is os.Lstat, counted as one walk operation.
func Lstat(name string) (os.FileInfo, error) {
	noteWalk()
	return os.Lstat(name)
}

// Open is os.Open, counted as one walk operation.
func Open(name string) (*os.File, error) {
	noteWalk()
	return os.Open(name)
}

// OpenFile is os.OpenFile, counted as one walk operation.
// A write uses it too, so a directory read cannot sit next to an uncounted open.
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	noteWalk()
	return os.OpenFile(name, flag, perm)
}

// ReadFile is os.ReadFile, counted as one walk operation.
func ReadFile(name string) ([]byte, error) {
	noteWalk()
	return os.ReadFile(name)
}

// Glob is filepath.Glob, counted as one walk operation.
func Glob(pattern string) ([]string, error) {
	noteWalk()
	return filepath.Glob(pattern)
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
