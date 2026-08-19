// Package meta provides module-level functionality for meta.
// input: standalone data_dir, task/checkpoint/event/file snapshots
// output: fsynced control-plane files that survive process kill -9
// pos: file-backed metadata store used when meta_dsn is empty
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

const (
	fileMetaRoot        = ".meta"
	fileTasksDir        = "tasks"
	fileEventsDir       = "events"
	fileFilesDir        = "files"
	checkpointFileName  = "checkpoint.json"
	openEpochNameMarker = ".open.e"
)

// FileTaskStore persists tasks, checkpoints, events, and file metadata under data_dir.
type FileTaskStore struct {
	dataDir string
	mu      sync.Mutex
}

// NewFileTaskStore creates a standalone file-backed metadata store.
func NewFileTaskStore(dataDir string) (*FileTaskStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "./data"
	}
	store := &FileTaskStore{dataDir: dataDir}
	if err := os.MkdirAll(store.tasksDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(store.eventsDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(store.filesMetaDir(), 0o755); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *FileTaskStore) tasksDir() string {
	return filepath.Join(s.dataDir, fileMetaRoot, fileTasksDir)
}

func (s *FileTaskStore) eventsDir() string {
	return filepath.Join(s.dataDir, fileMetaRoot, fileEventsDir)
}

func (s *FileTaskStore) filesMetaDir() string {
	return filepath.Join(s.dataDir, fileMetaRoot, fileFilesDir)
}

func (s *FileTaskStore) taskPath(taskID string) string {
	return filepath.Join(s.tasksDir(), taskID+".json")
}

func (s *FileTaskStore) eventsPath(taskID string) string {
	return filepath.Join(s.eventsDir(), taskID+".jsonl")
}

func (s *FileTaskStore) filesPath(taskID string) string {
	return filepath.Join(s.filesMetaDir(), taskID+".json")
}

func (s *FileTaskStore) checkpointPath(taskID string) string {
	return filepath.Join(s.dataDir, taskID, checkpointFileName)
}

func (s *FileTaskStore) taskDataDir(taskID string) string {
	return filepath.Join(s.dataDir, taskID)
}

// Close implements the optional closer used by MySQL store; file store has no handles.
func (s *FileTaskStore) Close() error {
	return nil
}

// UpsertTask fsyncs the task snapshot.
func (s *FileTaskStore) UpsertTask(_ context.Context, task tasks.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSONAtomic(s.taskPath(task.ID), task)
}

// ListTasks reads all persisted task snapshots.
func (s *FileTaskStore) ListTasks(_ context.Context) ([]tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.tasksDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []tasks.Task{}, nil
		}
		return nil, err
	}
	out := make([]tasks.Task, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var task tasks.Task
		if err := readJSONFile(filepath.Join(s.tasksDir(), entry.Name()), &task); err != nil {
			return nil, err
		}
		if task.ID == "" {
			task.ID = strings.TrimSuffix(entry.Name(), ".json")
		}
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool {
		left, leftErr := strconv.Atoi(out[i].ID)
		right, rightErr := strconv.Atoi(out[j].ID)
		if leftErr == nil && rightErr == nil {
			return left < right
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteTask removes control-plane records; binlog files are left for operator inspection.
func (s *FileTaskStore) DeleteTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(s.taskPath(taskID))
	_ = os.Remove(s.eventsPath(taskID))
	_ = os.Remove(s.filesPath(taskID))
	_ = os.Remove(s.checkpointPath(taskID))
	return nil
}

// UpsertCheckpoint fsyncs the durable position under {data_dir}/{task_id}/checkpoint.json.
func (s *FileTaskStore) UpsertCheckpoint(_ context.Context, taskID string, checkpoint binlog.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if checkpoint.UpdatedAt.IsZero() {
		checkpoint.UpdatedAt = time.Now()
	}
	if err := os.MkdirAll(s.taskDataDir(taskID), 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(s.checkpointPath(taskID), checkpoint)
}

// LoadCheckpoint reads the last fsynced position.
func (s *FileTaskStore) LoadCheckpoint(_ context.Context, taskID string) (binlog.Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var checkpoint binlog.Checkpoint
	if err := readJSONFile(s.checkpointPath(taskID), &checkpoint); err != nil {
		if os.IsNotExist(err) {
			return binlog.Checkpoint{}, false, nil
		}
		return binlog.Checkpoint{}, false, err
	}
	if checkpoint.File == "" || checkpoint.Pos == 0 {
		return binlog.Checkpoint{}, false, nil
	}
	return checkpoint, true, nil
}

// AppendEvent appends a JSONL event record.
func (s *FileTaskStore) AppendEvent(_ context.Context, event tasks.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.eventsDir(), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.eventsPath(event.TaskID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ListEvents returns the latest events (newest last, then truncated from the head).
func (s *FileTaskStore) ListEvents(_ context.Context, taskID string, limit int) ([]tasks.TaskEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.eventsPath(taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return []tasks.TaskEvent{}, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]tasks.TaskEvent, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event tasks.TaskEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

// UpsertBinlogFile persists file metadata and is merged with a disk scan on list.
func (s *FileTaskStore) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.readFileMetaLocked(meta.TaskID)
	if err != nil {
		return err
	}
	replaced := false
	for i, item := range files {
		if item.FileName == meta.FileName || item.FilePath == meta.FilePath {
			files[i] = meta
			replaced = true
			break
		}
	}
	if !replaced {
		files = append(files, meta)
	}
	return writeJSONAtomic(s.filesPath(meta.TaskID), files)
}

// ListBinlogFiles returns persisted metadata plus currently open/sealed files on disk.
func (s *FileTaskStore) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.readFileMetaLocked(taskID)
	if err != nil {
		return nil, err
	}
	files = mergeDiskBinlogFiles(files, s.taskDataDir(taskID), taskID)
	sort.Slice(files, func(i, j int) bool {
		if files[i].CreatedAt.Equal(files[j].CreatedAt) {
			return files[i].FileName < files[j].FileName
		}
		return files[i].CreatedAt.After(files[j].CreatedAt)
	})
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}

func (s *FileTaskStore) readFileMetaLocked(taskID string) ([]tasks.BinlogFile, error) {
	var files []tasks.BinlogFile
	if err := readJSONFile(s.filesPath(taskID), &files); err != nil {
		if os.IsNotExist(err) {
			return []tasks.BinlogFile{}, nil
		}
		return nil, err
	}
	return files, nil
}

func mergeDiskBinlogFiles(existing []tasks.BinlogFile, dir, taskID string) []tasks.BinlogFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return existing
	}
	byName := make(map[string]int, len(existing))
	for i, item := range existing {
		byName[item.FileName] = i
		if item.FilePath != "" {
			byName[filepath.Base(item.FilePath)] = i
		}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == checkpointFileName || strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sourceName := displayBinlogFileName(name)
		if idx, ok := byName[sourceName]; ok {
			if existing[idx].SizeBytes == 0 {
				existing[idx].SizeBytes = info.Size()
			}
			if existing[idx].FilePath == "" {
				existing[idx].FilePath = filepath.Join(dir, name)
			}
			continue
		}
		if idx, ok := byName[name]; ok {
			if existing[idx].SizeBytes == 0 {
				existing[idx].SizeBytes = info.Size()
			}
			continue
		}
		existing = append(existing, tasks.BinlogFile{
			TaskID:      taskID,
			FileName:    sourceName,
			FilePath:    filepath.Join(dir, name),
			SizeBytes:   info.Size(),
			CreatedAt:   info.ModTime(),
			UploadState: "LOCAL_ONLY",
		})
		byName[sourceName] = len(existing) - 1
	}
	return existing
}

func displayBinlogFileName(name string) string {
	if idx := strings.LastIndex(name, openEpochNameMarker); idx > 0 {
		return name[:idx]
	}
	return name
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
