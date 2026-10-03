// Package tasks provides module-level functionality for tasks.
// input: local data_dir and task id for a binlog segment directory
// output: sealed and open on-disk segments in ascending binlog index order
// pos: disk listing used when the file catalog is missing or empty
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
