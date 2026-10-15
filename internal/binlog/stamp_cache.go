// Package binlog provides module-level functionality for binlog.
// input: a segment file's os.FileInfo and a value computed from its bytes
// output: FileStamp (size, mtime and, where the OS has it, ctime) and stampCache, a bounded per-path cache that is valid only while the file's stamp is unchanged; touch -r keeps the mtime but not the ctime, so an in-place edit is read again (#380 QA)
// pos: shared by the ledger contents, sealed-corrupt, damage and positions caches and the health evaluator's directory fingerprint
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"os"
	"sync"
	"time"
)

// FileStamp is what a cached read of a file is valid for.
type FileStamp struct {
	Size  int64
	MTime time.Time
	CTime int64 // nanoseconds; 0 where the OS does not report it
}

// StampOf is the stamp of info.
func StampOf(info os.FileInfo) FileStamp {
	return FileStamp{Size: info.Size(), MTime: info.ModTime(), CTime: changeTime(info)}
}

// Equal reports the same size, mtime and ctime.
func (s FileStamp) Equal(o FileStamp) bool {
	return s.Size == o.Size && s.MTime.Equal(o.MTime) && s.CTime == o.CTime
}

// stampCacheCap bounds each cache. A full cache is cleared rather than
// tracked per entry: segments that retention removed would otherwise stay
// forever, and refilling costs one read per live segment.
const stampCacheCap = 1 << 14

type stampEntry[T any] struct {
	stamp FileStamp
	v     T
}

type stampCache[T any] struct {
	mu sync.Mutex
	m  map[string]stampEntry[T]
}

func (c *stampCache[T]) get(key string, st FileStamp) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || !e.stamp.Equal(st) {
		var zero T
		return zero, false
	}
	return e.v, true
}

func (c *stampCache[T]) put(key string, st FileStamp, v T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= stampCacheCap {
		c.m = map[string]stampEntry[T]{}
	}
	c.m[key] = stampEntry[T]{stamp: st, v: v}
}

func (c *stampCache[T]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
