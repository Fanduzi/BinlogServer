package binlog

import (
	"sync"
	"time"
)

type syncFile interface {
	Write(p []byte) (n int, err error)
	Sync() error
}

type Writer struct {
	mu         sync.Mutex
	file       syncFile
	current    Checkpoint
	pending    Checkpoint
	hasPending bool
}

func NewWriter(file syncFile, initial Checkpoint) *Writer {
	return &Writer{
		file:    file,
		current: initial,
	}
}

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

func (w *Writer) CurrentCheckpoint() Checkpoint {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.current
}
