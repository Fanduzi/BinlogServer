// Package tasks provides module-level functionality for tasks.
// input: task id, data dir, KeepLocalSegments, epoch, catalog file_path rows, and an optional stored checkpoint
// output: the file, pos, and gtid_set the next Start continues from, including a takeover segment directory, a readable epoch-0 bare OPEN file on this worker, or the name of a segment this worker cannot read; a position-4 rewind omits gtid_set
// pos: resume identity shared by the replication runner and GET /api/tasks/{id}/checkpoint
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"binlog_server/internal/binlog"
)

// NextResumePosition is the position the next Start uses instead of the
// configured start when takeover has not found another segment directory.
// A local complete event wins unless the task keeps adopted segments. A stored
// checkpoint is used when that event is absent. Epoch greater than 1 rewinds
// that checkpoint to position 4. ResolveTakeover replaces that rewind when the
// catalog file_path is readable, when the segment is missing, or when a sealed
// UPLOADED object already covers the checkpoint. gtid_set is copied when the
// file and pos are the stored checkpoint's. A rewind to position 4 changes
// pos, so the gtid_set from the later position is left off. This does not
// contact the source.
func NextResumePosition(dataDir string, task Task, checkpoint binlog.Checkpoint, checkpointOK bool) (binlog.Checkpoint, bool) {
	if !task.KeepLocalSegments {
		if file, pos, ok := binlog.DurableResume(dataDir, task.ID); ok {
			out := binlog.Checkpoint{File: file, Pos: pos}
			if checkpointOK && checkpoint.File == file && checkpoint.Pos == pos {
				out.GTIDSet = checkpoint.GTIDSet
				out.UpdatedAt = checkpoint.UpdatedAt
			}
			return out, true
		}
	}
	if !checkpointOK || checkpoint.File == "" || checkpoint.Pos == 0 {
		return binlog.Checkpoint{}, false
	}
	pos := checkpoint.Pos
	if task.Epoch > 1 && pos > 4 {
		pos = 4
	}
	out := binlog.Checkpoint{
		File:      checkpoint.File,
		Pos:       pos,
		UpdatedAt: checkpoint.UpdatedAt,
	}
	// The executed set belongs to the stored position. Position 4 is not that position.
	if pos == checkpoint.Pos {
		out.GTIDSet = checkpoint.GTIDSet
	}
	return out, true
}

// TakeoverResume is how an epoch greater than 1 continues when this worker's
// own data directory has no complete event. The lease moves. The segment
// directory does not. file_path on the catalog row is that directory.
type TakeoverResume struct {
	// Apply replaces the position-4 rewind.
	Apply bool
	// Dir is the segment directory to keep writing into when it is not this
	// worker's {data_dir}/{task_id}. Empty means the default directory.
	Dir string
	// Checkpoint is the position to continue from when Apply is set.
	Checkpoint binlog.Checkpoint
	// Missing is the segment path a DBA has to make readable. Start must fail.
	Missing string
	// Covering is a sealed UPLOADED object that already holds the checkpoint.
	// The local file is absent, so the caller reads the object instead of
	// rewinding to position 4.
	Covering *BinlogFile
}

// ResolveTakeover decides a lease takeover when this worker's data directory
// has no complete event in a .open.eN file. A readable catalog file_path
// continues from the last complete event in that directory. An OPEN row whose
// file is the bare source name (epoch 0) and is readable here continues from
// that file. An unreadable open segment, or a sealed segment that is not
// UPLOADED, sets Missing. A checkpoint already inside a sealed UPLOADED object
// is Apply without a rewind.
func ResolveTakeover(dataDir string, task Task, checkpoint binlog.Checkpoint, checkpointOK bool, files []BinlogFile) TakeoverResume {
	if task.KeepLocalSegments || task.Epoch <= 1 {
		return TakeoverResume{}
	}
	if _, _, ok := binlog.DurableResume(dataDir, task.ID); ok {
		return TakeoverResume{}
	}
	if label := unreadableTail(files); label != "" {
		return TakeoverResume{Missing: label}
	}
	if dir := readableSegmentDir(files); dir != "" {
		if file, pos, ok := binlog.DurableResumeDir(dir); ok {
			return TakeoverResume{Apply: true, Dir: dir, Checkpoint: resumeCheckpoint(file, pos, checkpoint, checkpointOK)}
		}
	}
	if dir, file, pos, ok := readablePlainOpen(files); ok {
		return TakeoverResume{Apply: true, Dir: dir, Checkpoint: resumeCheckpoint(file, pos, checkpoint, checkpointOK)}
	}
	if row, hasRow, covered := coveringUpload(files, checkpoint, checkpointOK); covered {
		if hasRow {
			if path := strings.TrimSpace(row.FilePath); regularSegmentFile(path) {
				return TakeoverResume{Apply: true, Dir: filepath.Dir(path), Checkpoint: checkpoint}
			}
			copied := row
			return TakeoverResume{Apply: true, Checkpoint: checkpoint, Covering: &copied}
		}
		out := TakeoverResume{Apply: true, Checkpoint: checkpoint}
		if dir := readableSegmentDir(files); dir != "" {
			out.Dir = dir
		}
		return out
	}
	if checkpointOK && checkpoint.File != "" && checkpoint.Pos > 4 {
		return TakeoverResume{Missing: checkpoint.File}
	}
	if checkpointOK && checkpoint.File != "" && checkpoint.Pos > 0 {
		out := TakeoverResume{Apply: true, Checkpoint: checkpoint}
		if dir := readableSegmentDir(files); dir != "" {
			out.Dir = dir
		}
		return out
	}
	return TakeoverResume{}
}

func resumeCheckpoint(file string, pos uint32, checkpoint binlog.Checkpoint, checkpointOK bool) binlog.Checkpoint {
	out := binlog.Checkpoint{File: file, Pos: pos}
	if checkpointOK && checkpoint.File == file && checkpoint.Pos == pos {
		out.GTIDSet = checkpoint.GTIDSet
		out.UpdatedAt = checkpoint.UpdatedAt
	}
	return out
}

func unreadableTail(files []BinlogFile) string {
	sealed := ""
	for _, row := range files {
		if catalogUploaded(row) {
			continue
		}
		path := strings.TrimSpace(row.FilePath)
		if path != "" && regularSegmentFile(path) {
			continue
		}
		label := path
		if label == "" {
			label = strings.TrimSpace(row.FileName)
		}
		if label == "" {
			continue
		}
		if catalogOpen(row) {
			return label
		}
		if sealed == "" {
			sealed = label
		}
	}
	return sealed
}

// readablePlainOpen is an OPEN catalog row stored under the bare source name.
// Epoch 0 writes that name. A sealed row with the same spelling is not open.
func readablePlainOpen(files []BinlogFile) (dir, file string, pos uint32, ok bool) {
	for _, row := range files {
		if !strings.EqualFold(strings.TrimSpace(row.State), "OPEN") {
			continue
		}
		path := strings.TrimSpace(row.FilePath)
		base := filepath.Base(path)
		if path == "" || base == "." || base == ".." || strings.Contains(base, ".open.e") || strings.Contains(base, ".sealed.e") {
			continue
		}
		if !regularSegmentFile(path) {
			continue
		}
		endPos, _, _, found := binlog.DurableCursor(path)
		if !found {
			continue
		}
		name := strings.TrimSpace(row.FileName)
		if name == "" {
			name = base
		}
		return filepath.Dir(path), name, endPos, true
	}
	return "", "", 0, false
}

func readableSegmentDir(files []BinlogFile) string {
	bestEpoch := int64(-1)
	best := ""
	fallback := ""
	for _, row := range files {
		path := strings.TrimSpace(row.FilePath)
		if !regularSegmentFile(path) {
			continue
		}
		dir := filepath.Dir(path)
		if fallback == "" {
			fallback = dir
		}
		if !catalogOpen(row) {
			continue
		}
		epoch := openEpoch(filepath.Base(path))
		if epoch >= bestEpoch {
			bestEpoch = epoch
			best = dir
		}
	}
	if best != "" {
		return best
	}
	return fallback
}

func coveringUpload(files []BinlogFile, checkpoint binlog.Checkpoint, checkpointOK bool) (BinlogFile, bool, bool) {
	if !checkpointOK || checkpoint.File == "" || checkpoint.Pos == 0 {
		return BinlogFile{}, false, false
	}
	for _, row := range files {
		if !catalogUploaded(row) {
			return BinlogFile{}, false, false
		}
	}
	if checkpoint.Pos <= 4 {
		return BinlogFile{}, false, true
	}
	for _, row := range files {
		if row.FileName == checkpoint.File && row.EndPos >= checkpoint.Pos {
			return row, true, true
		}
	}
	return BinlogFile{}, false, false
}

func catalogUploaded(row BinlogFile) bool {
	return strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOADED") && strings.TrimSpace(row.ObjectKey) != ""
}

func catalogOpen(row BinlogFile) bool {
	if strings.EqualFold(strings.TrimSpace(row.State), "OPEN") {
		return true
	}
	return strings.Contains(row.FileName, ".open.e") || strings.Contains(filepath.Base(row.FilePath), ".open.e")
}

func regularSegmentFile(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func openEpoch(name string) int64 {
	const mark = ".open.e"
	idx := strings.LastIndex(name, mark)
	if idx <= 0 {
		return 0
	}
	n, err := strconv.ParseInt(name[idx+len(mark):], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
