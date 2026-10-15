// Package binlog provides module-level functionality for binlog.
// input: a file stamp
// output: stub of the bounded stamp cache (P2-3 QA red commit): never bounded, ignores ctime
// pos: ADR 0009 P2-3 QA
// note: if this file changes, update this header and module README.md.
package binlog

import "time"

// FileStamp is what a cached read of a file is valid for.
type FileStamp struct {
	Size  int64
	MTime time.Time
	CTime int64
}

const stampCacheCap = 1 << 14

type stampCache[T any] struct{ m map[string]T }

func (c *stampCache[T]) get(key string, st FileStamp) (T, bool) {
	v, ok := c.m[key]
	return v, ok
}

func (c *stampCache[T]) put(key string, st FileStamp, v T) {
	if c.m == nil {
		c.m = map[string]T{}
	}
	c.m[key] = v
}

func (c *stampCache[T]) len() int { return len(c.m) }
