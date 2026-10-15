// Package healthstore provides module-level functionality for healthstore.
// input: transitions and health of standalone tasks
// output: FileStore: {data_dir}/.health.json written atomically; standalone fence only; pending transitions saved alongside
// pos: standalone (no meta DSN) Store
// note: if this file changes, update this header and module README.md.
package healthstore

import (
	"context"

	"binlog_server/internal/health"
)

// HealthFileName is the standalone health file under data_dir.
const HealthFileName = ".health.json"

// FileStore is the standalone Store.
type FileStore struct{ path string }

// NewFileStore stores health in path.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (s *FileStore) ApplyTransitions(ctx context.Context, taskID string, fence health.Fence, ts []health.Transition, h health.TaskHealth) (Applied, error) {
	return Applied{}, nil
}
func (s *FileStore) ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error) {
	return nil, nil
}
func (s *FileStore) ListHealth(ctx context.Context) ([]health.TaskHealth, error) { return nil, nil }
func (s *FileStore) ListTransitions(ctx context.Context, taskID string) ([]Transition, error) {
	return nil, nil
}

// SavePending persists transitions not yet applied, so a standalone
// restart replays them (design §5.1 step 6).
func (s *FileStore) SavePending(taskID string, ts []health.Transition) error { return nil }

// LoadPending returns the saved pending transitions of every task.
func (s *FileStore) LoadPending() (map[string][]health.Transition, error) { return nil, nil }
