// Package tasks provides module-level functionality for tasks.
// input: task id, on-disk basename, the files inventory, ClassifySegment, this process data_dir, and the configured object uploader when a sealed row is already UPLOADED
// output: a size-capped reader for one inventory segment from local disk, or from object storage when the local file is missing and the sealed row is UPLOADED with an object key
// pos: segment open for the authenticated download route; replay path selection stays in disk_files.go
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"binlog_server/internal/binlog"
)

// ErrInvalidSegmentName rejects a download name that is not a single basename.
var ErrInvalidSegmentName = errors.New("invalid segment name")

// ErrSegmentNotOnProcess means the name is not in the files inventory, or this
// process cannot serve its bytes from local disk or an already uploaded sealed object.
var ErrSegmentNotOnProcess = errors.New("segment not found on this process")

var errOpenSegment = errors.New("open segment failed")

// errLocalSegmentAbsent means this process has no file at {data_dir}/{task_id}/{name}.
// Callers may then read a sealed UPLOADED object. It is not an HTTP response.
var errLocalSegmentAbsent = errors.New("local segment absent")

// objectOpener reads one object already stored by the configured uploader.
// FileUploader stays upload-only. A process with no upload config, or an
// uploader that cannot read, keeps the local-only 404.
type objectOpener interface {
	OpenObject(ctx context.Context, objectKey string) (io.ReadCloser, int64, error)
}

// segmentInventoryLimit keeps every catalog or disk row. The files route windows
// with the caller's limit; a download name only has to appear in that inventory.
const segmentInventoryLimit = int(^uint(0) >> 1)

// OpenTaskSegment opens one inventory segment by on-disk basename.
// The basename is the files list file_path base (sealed name or *.open.e*), or
// file_name when file_path is empty. The reader stops at the size observed at
// open, including when the task is RUNNING and the current open epoch is still
// growing. State is not a filter for a local file: an OPEN segment is not skipped.
// Local {data_dir}/{task_id}/{name} wins. When that file is absent, a sealed
// catalog row with upload_state UPLOADED and a non-empty object_key is read
// from the configured object store. Catalog file_path is never opened.
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
	rc, size, err := s.openLocalTaskSegment(taskID, name)
	if err == nil || !errors.Is(err, errLocalSegmentAbsent) {
		return rc, size, err
	}
	return s.openUploadedTaskSegment(files, name)
}

func (s *Scheduler) openLocalTaskSegment(taskID, name string) (io.ReadCloser, int64, error) {
	s.mu.Lock()
	dataDir := s.dataDir
	s.mu.Unlock()
	dir, ok := taskBinlogDir(dataDir, taskID)
	if !ok {
		return nil, 0, errLocalSegmentAbsent
	}
	full := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel != name || filepath.Base(full) != name {
		return nil, 0, ErrInvalidSegmentName
	}
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, errLocalSegmentAbsent
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
				return nil, 0, errLocalSegmentAbsent
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
			return nil, 0, errLocalSegmentAbsent
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

func (s *Scheduler) openUploadedTaskSegment(files []BinlogFile, name string) (io.ReadCloser, int64, error) {
	key, ok := sealedUploadedObjectKey(files, name)
	if !ok {
		return nil, 0, ErrSegmentNotOnProcess
	}
	s.mu.Lock()
	uploader := s.fileUploader
	s.mu.Unlock()
	opener, ok := uploader.(objectOpener)
	if !ok {
		return nil, 0, ErrSegmentNotOnProcess
	}
	// The handler keeps the body for the response, so this context stays open.
	rc, size, err := opener.OpenObject(context.Background(), key)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrSegmentNotOnProcess
		}
		return nil, 0, err
	}
	if rc == nil {
		return nil, 0, ErrSegmentNotOnProcess
	}
	if size < 0 {
		_ = rc.Close()
		return nil, 0, errOpenSegment
	}
	return &limitReadCloser{Closer: rc, Reader: io.LimitReader(rc, size)}, size, nil
}

// sealedUploadedObjectKey is the catalog object key for a sealed UPLOADED row
// whose inventory basename is name. Open segments are refused even when a row
// carries UPLOADED and an object key. An empty key is refused.
func sealedUploadedObjectKey(files []BinlogFile, name string) (string, bool) {
	if !binlog.SealedName(name) {
		return "", false
	}
	for _, file := range files {
		if segmentInventoryBasename(file) != name {
			continue
		}
		if rowIsOpenSegment(file) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(file.UploadState), "UPLOADED") {
			continue
		}
		key := strings.TrimSpace(file.ObjectKey)
		if key == "" {
			continue
		}
		return key, true
	}
	return "", false
}

func rowIsOpenSegment(file BinlogFile) bool {
	return CatalogRowOpen(file)
}

type limitReadCloser struct {
	io.Closer
	io.Reader
}

func (f *limitReadCloser) Read(p []byte) (int, error) {
	return f.Reader.Read(p)
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
