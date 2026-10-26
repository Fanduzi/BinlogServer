// Package tasks provides module-level functionality for tasks.
// input: findings from the sink in standalone mode (no metadata DSN)
// output: {data_dir}/.findings.json written atomically (temp file, fsync, rename), so open findings survive a restart
// pos: standalone FindingStore adapter for ADR 0009
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"binlog_server/internal/binlog"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// FindingFileName is the standalone findings file under data_dir. It starts
// with a dot so the on-disk backup listing never treats it as a task.
const FindingFileName = ".findings.json"

const maxFileFindings = 2000

type fileFindingStore struct {
	mu   sync.Mutex
	path string
}

// NewFileFindingStore persists findings in one JSON file.
func NewFileFindingStore(path string) FindingStore {
	return &fileFindingStore{path: path}
}

func (f *fileFindingStore) load() (map[findingKey]Finding, error) {
	out := map[findingKey]Finding{}
	raw, err := binlog.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Finding
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
	}
	for _, it := range items {
		out[findingKey{it.TaskID, it.Kind, it.Subject}] = it
	}
	return out, nil
}

func (f *fileFindingStore) save(rows map[findingKey]Finding) error {
	items := make([]Finding, 0, len(rows))
	for _, it := range rows {
		items = append(items, it)
	}
	sortFindings(items)
	if len(items) > maxFileFindings {
		items = items[:maxFileFindings] // open rows sort first; the oldest resolved rows go
	}
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".findings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmpName))
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmpName))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmpName))
	}
	return os.Rename(tmpName, f.path)
}

func (f *fileFindingStore) UpsertFinding(_ context.Context, it Finding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows, err := f.load()
	if err != nil {
		return err
	}
	key := findingKey{it.TaskID, it.Kind, it.Subject}
	if prev, ok := rows[key]; ok && prev.FirstSeen.Before(it.FirstSeen) {
		it.FirstSeen = prev.FirstSeen
	}
	rows[key] = it
	return f.save(rows)
}

func (f *fileFindingStore) list(match func(Finding) bool, limit int) ([]Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows, err := f.load()
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, it := range rows {
		if match(it) {
			out = append(out, it)
		}
	}
	sortFindings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fileFindingStore) ListFindings(_ context.Context, taskID string, includeResolved bool, limit int) ([]Finding, error) {
	return f.list(func(it Finding) bool { return it.TaskID == taskID && (includeResolved || it.Open()) }, limit)
}

func (f *fileFindingStore) ListOpenFindings(_ context.Context, limit int) ([]Finding, error) {
	return f.list(Finding.Open, limit)
}

// LoadOpenFindings seeds the in-process book from the store at startup, so a
// restarted process still shows (and can resolve) findings it had open.
func (s *Scheduler) LoadOpenFindings(ctx context.Context) error {
	b := s.findings
	b.mu.Lock()
	store := b.store
	b.mu.Unlock()
	if store == nil {
		return nil
	}
	items, err := store.ListOpenFindings(ctx, 1000)
	if errors.Is(err, ErrFindingStoreAbsent) {
		// Schema 6: findings stay in memory, and finding_store_active is 0 from now on.
		log.Printf("%v", err)
		b.mu.Lock()
		b.store = nil
		b.mu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, it := range items {
		key := findingKey{it.TaskID, it.Kind, it.Subject}
		if _, ok := b.open[key]; !ok {
			b.open[key] = it
		}
	}
	return nil
}

// CountOpenFindings counts open findings per task (FindingCounter).
func (f *fileFindingStore) CountOpenFindings(context.Context) (map[string]int, error) {
	open, err := f.list(Finding.Open, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	for _, it := range open {
		out[it.TaskID]++
	}
	return out, nil
}
