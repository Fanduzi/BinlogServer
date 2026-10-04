// Package tasks provides module-level functionality for tasks.
// input: task id, on-disk basename, the files inventory, and this process data_dir
// output: a size-capped reader for one sealed or .open.e* segment that the files inventory already lists, or a not-found/invalid-name error when this process cannot serve those bytes
// pos: local segment open for the authenticated download route; replay path selection stays in disk_files.go
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrInvalidSegmentName rejects a download name that is not a single basename.
var ErrInvalidSegmentName = errors.New("invalid segment name")

// ErrSegmentNotOnProcess means the name is not in the files inventory or the
// bytes are not under {data_dir}/{task_id}/ on this process.
var ErrSegmentNotOnProcess = errors.New("segment not found on this process")

var errOpenSegment = errors.New("open segment failed")

// segmentInventoryLimit keeps every catalog or disk row. The files route windows
// with the caller's limit; a download name only has to appear in that inventory.
const segmentInventoryLimit = int(^uint(0) >> 1)

// OpenTaskSegment opens one inventory segment by on-disk basename.
// The basename is the files list file_path base (sealed name or *.open.e*), or
// file_name when file_path is empty. The reader stops at the size observed at
// open, including when the task is RUNNING and the current open epoch is still
// growing. State is not a filter: an OPEN segment is not skipped.
// Bytes are read only from {data_dir}/{task_id}/{name} on this process.
func (s *Scheduler) OpenTaskSegment(taskID, name string) (io.ReadCloser, int64, error) {
	if !validSegmentBasename(name) {
		return nil, 0, ErrInvalidSegmentName
	}
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return nil, 0, err
	}
	if !inventoryIncludesBasename(files, name) {
		return nil, 0, ErrSegmentNotOnProcess
	}
	s.mu.Lock()
	dataDir := s.dataDir
	s.mu.Unlock()
	dir, ok := taskBinlogDir(dataDir, taskID)
	if !ok {
		return nil, 0, ErrSegmentNotOnProcess
	}
	full := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel != name || filepath.Base(full) != name {
		return nil, 0, ErrInvalidSegmentName
	}
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrSegmentNotOnProcess
		}
		return nil, 0, errOpenSegment
	}
	if info.IsDir() {
		return nil, 0, ErrSegmentNotOnProcess
	}
	if info.Mode()&os.ModeSymlink != 0 {
		escaped, symErr := symlinkEscapes(dir, full)
		if symErr != nil {
			if os.IsNotExist(symErr) {
				return nil, 0, ErrSegmentNotOnProcess
			}
			return nil, 0, errOpenSegment
		}
		if escaped {
			return nil, 0, ErrInvalidSegmentName
		}
	}
	file, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrSegmentNotOnProcess
		}
		return nil, 0, errOpenSegment
	}
	st, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, errOpenSegment
	}
	if !st.Mode().IsRegular() {
		_ = file.Close()
		return nil, 0, ErrSegmentNotOnProcess
	}
	size := st.Size()
	return &limitedFile{File: file, Reader: io.LimitReader(file, size)}, size, nil
}

type limitedFile struct {
	*os.File
	io.Reader
}

func (f *limitedFile) Read(p []byte) (int, error) {
	return f.Reader.Read(p)
}

func validSegmentBasename(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return false
	}
	if strings.ContainsAny(name, `/\\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	if path.Base(name) != name || filepath.Base(name) != name {
		return false
	}
	return true
}

func inventoryIncludesBasename(files []BinlogFile, name string) bool {
	for _, file := range files {
		if segmentInventoryBasename(file) == name {
			return true
		}
	}
	return false
}

func segmentInventoryBasename(file BinlogFile) string {
	if strings.TrimSpace(file.FilePath) != "" {
		base, ok := anyPathBase(file.FilePath)
		if !ok {
			return ""
		}
		return base
	}
	base, ok := anyPathBase(file.FileName)
	if !ok {
		return ""
	}
	return base
}

func anyPathBase(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	slash := strings.TrimRight(strings.ReplaceAll(raw, `\`, `/`), "/")
	if slash == "" {
		return "", false
	}
	base := path.Base(slash)
	if base == "." || base == ".." || base == "/" || strings.ContainsAny(base, `/\\`) {
		return "", false
	}
	return base, true
}

func symlinkEscapes(dir, full string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return false, err
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return true, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true, nil
	}
	return false, nil
}
