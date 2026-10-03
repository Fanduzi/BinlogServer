// Package tasks provides module-level functionality for tasks.
// input: local data_dir and task id for a binlog segment directory
// output: sealed and open on-disk segments in ascending binlog index order, leftover task ids when no task store is configured, the FILE_POS resume point at the end of the highest segment, and the next open epoch above those segments
// pos: disk listing and standalone leftover-directory discovery when the file catalog or task row is missing
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const binlogOpenEpochMark = ".open.e"

// listTaskBinlogFilesOnDisk reads {dataDir}/{taskID} for sealed binlog names
// and name.open.e<epoch> segments. file_name is the source name. file_path is
// the on-disk path, including .open.e<epoch> while the segment is open.
// Results are ascending by source index. The same index lists the sealed name
// first, then open epochs ascending. limit keeps the highest indexes.
func listTaskBinlogFilesOnDisk(dataDir, taskID string, limit int) ([]BinlogFile, error) {
	dir, ok := taskBinlogDir(dataDir, taskID)
	if !ok {
		return []BinlogFile{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BinlogFile{}, nil
		}
		return nil, err
	}

	files := make([]BinlogFile, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		source, _, _, state, ok := classifyBinlogSegment(name)
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		item := BinlogFile{
			TaskID:      taskID,
			FileName:    source,
			FilePath:    filepath.Join(dir, name),
			State:       state,
			SizeBytes:   info.Size(),
			UploadState: "LOCAL_ONLY",
			CreatedAt:   info.ModTime().UTC(),
		}
		if state == "SEALED" {
			item.SealedAt = item.CreatedAt
		}
		files = append(files, item)
	}
	sort.SliceStable(files, func(i, j int) bool {
		return binlogSegmentLess(files[i], files[j])
	})
	if limit <= 0 {
		limit = 200
	}
	if len(files) > limit {
		files = files[len(files)-limit:]
	}
	return files, nil
}

func taskBinlogDir(dataDir, taskID string) (string, bool) {
	dataDir = strings.TrimSpace(dataDir)
	taskID = strings.TrimSpace(taskID)
	if dataDir == "" || taskID == "" || taskID == "." || taskID == ".." {
		return "", false
	}
	if taskID != filepath.Base(taskID) {
		return "", false
	}
	return filepath.Join(dataDir, taskID), true
}

func classifyBinlogSegment(name string) (source string, seq uint64, epoch int64, state string, ok bool) {
	if name == "" || strings.HasPrefix(name, ".") {
		return "", 0, 0, "", false
	}
	state = "SEALED"
	epoch = -1
	source = name
	if idx := strings.LastIndex(name, binlogOpenEpochMark); idx > 0 {
		epochText := name[idx+len(binlogOpenEpochMark):]
		if epochText == "" || strings.ContainsAny(epochText, "./\\") {
			return "", 0, 0, "", false
		}
		n, err := strconv.ParseInt(epochText, 10, 64)
		if err != nil || n < 0 {
			return "", 0, 0, "", false
		}
		source = name[:idx]
		epoch = n
		state = "OPEN"
	}
	prefix, seq, ok := splitBinlogIndex(source)
	if !ok || prefix == "" {
		return "", 0, 0, "", false
	}
	return source, seq, epoch, state, true
}

func splitBinlogIndex(name string) (prefix string, seq uint64, ok bool) {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(name[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name[:i], seq, true
}

func binlogSegmentLess(a, b BinlogFile) bool {
	ak := binlogSegmentKey(a)
	bk := binlogSegmentKey(b)
	if ak.ok && bk.ok && ak.seq != bk.seq {
		return ak.seq < bk.seq
	}
	if ak.ok != bk.ok {
		return ak.ok
	}
	if ak.epoch != bk.epoch {
		return ak.epoch < bk.epoch
	}
	return ak.name < bk.name
}

type segmentSortKey struct {
	seq   uint64
	epoch int64
	name  string
	ok    bool
}

func binlogSegmentKey(file BinlogFile) segmentSortKey {
	name := filepath.Base(file.FilePath)
	_, seq, epoch, _, ok := classifyBinlogSegment(name)
	if !ok {
		return segmentSortKey{name: name}
	}
	return segmentSortKey{seq: seq, epoch: epoch, name: name, ok: true}
}

// listDiskBackupTasks returns data_dir children that still contain sealed or
// .open.e<epoch> segments and are not already in known. Ids already in memory
// stay the live task. Hidden names and symlinks are skipped.
// ponytail: full directory scan, cache if dashboard polls contend with start/stop.
func listDiskBackupTasks(dataDir string, known map[string]struct{}) ([]Task, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Task, 0)
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		if _, ok := known[name]; ok {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		task, found, err := lookupDiskBackupTask(dataDir, name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool {
		return LessTaskID(out[i].ID, out[j].ID)
	})
	return out, nil
}

func lookupDiskBackupTask(dataDir, taskID string) (Task, bool, error) {
	dir, ok := taskBinlogDir(dataDir, taskID)
	if !ok || strings.HasPrefix(taskID, ".") {
		return Task{}, false, nil
	}
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Task{}, false, nil
		}
		return Task{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Task{}, false, nil
	}
	files, err := listTaskBinlogFilesOnDisk(dataDir, taskID, 1)
	if err != nil {
		return Task{}, false, err
	}
	if len(files) == 0 {
		return Task{}, false, nil
	}
	return diskBackupTask(taskID, info.ModTime().UTC()), true, nil
}

func diskBackupTask(id string, updated time.Time) Task {
	return Task{
		ID:        id,
		Name:      id,
		State:     StateStopped,
		UpdatedAt: updated,
	}
}

func maxNumericDiskBackupID(dataDir string) (int, error) {
	found, err := listDiskBackupTasks(dataDir, nil)
	if err != nil {
		return 0, err
	}
	maxID := 0
	for _, task := range found {
		n, convErr := strconv.Atoi(task.ID)
		if convErr != nil || n < 0 {
			continue
		}
		if n > maxID {
			maxID = n
		}
	}
	return maxID, nil
}

func readOnlyDiskBackup(store TaskStore, dataDir, id string) error {
	if store != nil {
		return nil
	}
	_, found, err := lookupDiskBackupTask(dataDir, id)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return ErrDiskBackupReadOnly
}

// diskResumeStart is FILE_POS at the byte size of the highest sealed or
// .open.e* segment. The file is the source name, not the .open.e suffix.
func diskResumeStart(dataDir, taskID string) (StartConfig, error) {
	files, err := listTaskBinlogFilesOnDisk(dataDir, taskID, 1)
	if err != nil {
		return StartConfig{}, err
	}
	if len(files) == 0 {
		return StartConfig{}, ErrTaskNotFound
	}
	file := files[len(files)-1]
	if file.FileName == "" || file.SizeBytes <= 0 || file.SizeBytes > int64(^uint32(0)) {
		return StartConfig{}, ErrDiskResumePosition
	}
	return StartConfig{
		Mode: StartModeFilePos,
		File: file.FileName,
		Pos:  uint32(file.SizeBytes),
	}, nil
}

// diskNextOpenEpoch is one higher than any .open.e* epoch in the task directory, and at least 1.
func diskNextOpenEpoch(dataDir, taskID string) (int64, error) {
	dir, ok := taskBinlogDir(dataDir, taskID)
	if !ok {
		return 1, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}
	maxEpoch := int64(-1)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		_, _, epoch, state, ok := classifyBinlogSegment(entry.Name())
		if !ok || state != "OPEN" || epoch < 0 {
			continue
		}
		if epoch > maxEpoch {
			maxEpoch = epoch
		}
	}
	next := maxEpoch + 1
	if next < 1 {
		next = 1
	}
	return next, nil
}
