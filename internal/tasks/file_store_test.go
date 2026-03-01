// Package tasks provides module-level functionality for tasks.
// input: task commands/events, runner callbacks, store/lease/uploader dependencies
// output: task state transitions, scheduling decisions, and execution coordination
// pos: core domain orchestration layer governing backup task lifecycle and policies
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"testing"
	"time"
)

type fakeFileStore struct {
	files map[string][]BinlogFile
}

// newFakeFileStore 实现对应功能逻辑。
func newFakeFileStore() *fakeFileStore {
	return &fakeFileStore{files: make(map[string][]BinlogFile)}
}

// UpsertBinlogFile 实现对应功能逻辑。
func (f *fakeFileStore) UpsertBinlogFile(_ context.Context, meta BinlogFile) error {
	items := f.files[meta.TaskID]
	for i := range items {
		if items[i].FileName == meta.FileName {
			items[i] = meta
			f.files[meta.TaskID] = items
			return nil
		}
	}
	f.files[meta.TaskID] = append(items, meta)
	return nil
}

// ListBinlogFiles 实现对应功能逻辑。
func (f *fakeFileStore) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]BinlogFile, error) {
	items := f.files[taskID]
	if limit <= 0 || limit >= len(items) {
		out := make([]BinlogFile, len(items))
		copy(out, items)
		return out, nil
	}
	out := make([]BinlogFile, limit)
	copy(out, items[len(items)-limit:])
	return out, nil
}

// TestScheduler_ListFilesFromStore 验证相关行为。
func TestScheduler_ListFilesFromStore(t *testing.T) {
	store := newFakeFileStore()
	store.files["1"] = []BinlogFile{
		{
			TaskID:    "1",
			FileName:  "mysql-bin.000001",
			FilePath:  "/tmp/mysql-bin.000001",
			SizeBytes: 1024,
			StartPos:  4,
			EndPos:    1200,
			CreatedAt: time.Now().Add(-time.Hour),
			SealedAt:  time.Now(),
		},
	}
	s := NewScheduler(WithFileStore(store))
	if _, err := s.CreateTask("cluster-a", "cluster-a-key"); err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	files, err := s.ListFiles("1", 10)
	if err != nil {
		t.Fatalf("ListFiles returned error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].FileName != "mysql-bin.000001" {
		t.Fatalf("unexpected file name: %s", files[0].FileName)
	}
}
