// Package binlog provides module-level functionality for binlog.
// input: replication events, checkpoint state, local filesystem dependencies
// output: binlog file writing, file rotation metadata, and checkpoint progression
// pos: binlog persistence primitives used by replication runtime and recovery flows
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"sync"
	"time"
)

type syncFile interface {
	Write(p []byte) (n int, err error)
	Sync() error
}

// Writer 把 event 写入文件，并在 fsync 成功后推进 checkpoint。
type Writer struct {
	mu         sync.Mutex
	file       syncFile
	current    Checkpoint
	pending    Checkpoint
	hasPending bool
}

// NewWriter 创建一个带初始 checkpoint 的 Writer。
func NewWriter(file syncFile, initial Checkpoint) *Writer {
	return &Writer{
		file:    file,
		current: initial,
	}
}

// Append 先写入 event，再暂存对应的 next checkpoint。
func (w *Writer) Append(event []byte, next Checkpoint) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.file.Write(event); err != nil {
		return err
	}
	w.pending = next
	w.hasPending = true
	return nil
}

// FlushAndCheckpoint 执行 fsync，并在成功后提交 pending checkpoint。
func (w *Writer) FlushAndCheckpoint() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.file.Sync(); err != nil {
		return err
	}
	if w.hasPending {
		next := w.pending
		next.UpdatedAt = time.Now()
		w.current = next
		w.hasPending = false
	}
	return nil
}

// CurrentCheckpoint 返回当前已提交的 checkpoint。
func (w *Writer) CurrentCheckpoint() Checkpoint {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current
}
