// Package tasks provides module-level functionality for tasks.
// input: task id, data dir, KeepLocalSegments, epoch, catalog file_path rows, ClassifySegment when state is empty, and an optional stored checkpoint
// output: the file, pos, and gtid_set the next Start continues from, including a takeover segment directory under this worker's data_dir (ownership compares absolute, symlink-resolved paths, so a relative data_dir owns its own segments), a readable epoch-0 bare OPEN file on this worker, a magic-only or header-only open file resumed from the saved checkpoint, or the name of a segment this worker cannot read; a position-4 rewind omits gtid_set
// pos: resume identity shared by the replication runner and GET /api/tasks/{id}/checkpoint
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"strings"

	"binlog_server/internal/binlog"
)

// NextResumePosition is the position the next Start uses instead of the
// configured start when takeover has not found another segment directory.
// A local complete event wins unless the task keeps adopted segments. A stored
// checkpoint is used when that event is absent. Epoch greater than 1 rewinds
// that checkpoint to position 4. ResolveTakeover replaces that rewind when the
// catalog file_path is readable, when the open file on this worker has no
// complete event, when the segment is missing, or when a sealed UPLOADED
// object already covers the checkpoint. gtid_set is copied when the
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
	// worker's {data_dir}/{task_id}. It must stay under this worker's data_dir.
	// Empty means the default directory.
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
// under this worker's data_dir continues from the last complete event in that
// directory. A readable path outside data_dir is Missing (SEGMENT_NOT_ON_WORKER):
// readability alone must not continue into another node's directory (#421 sc1).
// An OPEN row whose file is the bare source name (epoch 0) and is readable
// under data_dir continues from that file. A readable .open.eN that is only
// magic, or only a header of events whose end log_pos is 0, continues from
// the saved checkpoint. An unreadable open segment, or a sealed segment that
// is not UPLOADED, sets Missing. A checkpoint already inside a sealed
// UPLOADED object is Apply without a rewind; a local copy outside data_dir
// is materialized into this worker instead of continuing at that path.
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
		if pathUnderDataDir(dir, dataDir) {
			if file, pos, ok := binlog.DurableResumeDir(dir); ok {
				return TakeoverResume{Apply: true, Dir: dir, Checkpoint: resumeCheckpoint(file, pos, checkpoint, checkpointOK)}
			}
		} else if label := foreignUnuploadedLabel(files, dir); label != "" {
			// Readable OPEN / unuploaded tail outside data_dir (#421 sc1).
			// UPLOADED-only rows fall through to coveringUpload.
			return TakeoverResume{Missing: label}
		}
	}
	if dir, file, pos, ok := readablePlainOpen(files); ok {
		if !pathUnderDataDir(dir, dataDir) {
			return TakeoverResume{Missing: catalogPathInDir(files, dir)}
		}
		return TakeoverResume{Apply: true, Dir: dir, Checkpoint: resumeCheckpoint(file, pos, checkpoint, checkpointOK)}
	}
	if row, hasRow, covered := coveringUpload(files, checkpoint, checkpointOK); covered {
		if hasRow {
			if path := strings.TrimSpace(row.FilePath); regularSegmentFile(path) {
				if pathUnderDataDir(path, dataDir) {
					return TakeoverResume{Apply: true, Dir: filepath.Dir(path), Checkpoint: checkpoint}
				}
				// Readable but foreign: materialize the object into this data_dir.
			}
			copied := row
			return TakeoverResume{Apply: true, Checkpoint: checkpoint, Covering: &copied}
		}
		out := TakeoverResume{Apply: true, Checkpoint: checkpoint}
		if dir := readableSegmentDir(files); dir != "" && pathUnderDataDir(dir, dataDir) {
			out.Dir = dir
		}
		return out
	}
	if checkpointOK && checkpoint.File != "" && checkpoint.Pos > 4 {
		if dir := readablePreambleDir(files, checkpoint.File); dir != "" {
			if !pathUnderDataDir(dir, dataDir) {
				return TakeoverResume{Missing: catalogPathInDir(files, dir)}
			}
			return TakeoverResume{Apply: true, Dir: dir, Checkpoint: checkpoint}
		}
		return TakeoverResume{Missing: checkpoint.File}
	}
	if checkpointOK && checkpoint.File != "" && checkpoint.Pos > 0 {
		out := TakeoverResume{Apply: true, Checkpoint: checkpoint}
		if dir := readableSegmentDir(files); dir != "" && pathUnderDataDir(dir, dataDir) {
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
		named, classOK := binlog.ClassifySegment(base)
		if path == "" || !classOK || named.Open || named.Epoch >= 0 {
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

// readablePreambleDir is the directory of an OPEN .open.eN on this worker for
// checkpointFile that has no complete event. The highest epoch wins. The
// saved checkpoint is the resume position; this file is not one.
func readablePreambleDir(files []BinlogFile, checkpointFile string) string {
	checkpointFile = strings.TrimSpace(checkpointFile)
	if checkpointFile == "" {
		return ""
	}
	bestEpoch := int64(-1)
	best := ""
	for _, row := range files {
		if !catalogOpen(row) || segmentSource(row) != checkpointFile {
			continue
		}
		path := strings.TrimSpace(row.FilePath)
		named, ok := binlog.ClassifySegment(filepath.Base(path))
		if !ok || !named.Open || named.Source != checkpointFile || !regularSegmentFile(path) || !binlog.PreambleOnly(path) {
			continue
		}
		if named.Epoch >= bestEpoch {
			bestEpoch = named.Epoch
			best = filepath.Dir(path)
		}
	}
	return best
}

func segmentSource(row BinlogFile) string {
	if named, ok := binlog.ClassifySegment(strings.TrimSpace(row.FileName)); ok {
		return named.Source
	}
	if named, ok := binlog.ClassifySegment(filepath.Base(strings.TrimSpace(row.FilePath))); ok {
		return named.Source
	}
	return strings.TrimSpace(row.FileName)
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
		named, classOK := binlog.ClassifySegment(filepath.Base(path))
		if !classOK || !named.Open {
			continue
		}
		if named.Epoch >= bestEpoch {
			bestEpoch = named.Epoch
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
		if row.FileName == checkpoint.File && uint32(row.EndPos) >= checkpoint.Pos {
			return row, true, true
		}
	}
	return BinlogFile{}, false, false
}

func catalogUploaded(row BinlogFile) bool {
	return strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOADED") && strings.TrimSpace(row.ObjectKey) != ""
}

func catalogOpen(row BinlogFile) bool {
	return CatalogRowOpen(row)
}

// pathUnderDataDir reports whether path is this worker's data_dir or a path
// inside it. Both sides are made absolute (a relative data_dir is relative to
// the process working directory) and symlink-resolved when they exist, so a
// relative data_dir owns segments the catalog records by absolute path (and
// the reverse), and a shared mount reached by one symlink still counts. A readable path on the same host
// that is not under data_dir is foreign (#421 sc1).
func pathUnderDataDir(path, dataDir string) bool {
	path = strings.TrimSpace(path)
	dataDir = strings.TrimSpace(dataDir)
	if path == "" || dataDir == "" {
		return false
	}
	root, ok := resolvedAbs(dataDir)
	if !ok {
		return false
	}
	target, ok := resolvedAbs(path)
	if !ok {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolvedAbs is path made absolute (against the process working directory,
// which is what a relative data_dir is opened against) with symlinks
// resolved; a path that does not exist yet resolves through its parent.
// Ownership then compares directories, not spellings: data_dir "data" and a
// catalog path "/srv/bs/data/1/x" name the same place (#424).
func resolvedAbs(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, true
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(parent, filepath.Base(abs)), true
	}
	return abs, true
}

// catalogPathInDir names a catalog file_path under dir for SEGMENT_NOT_ON_WORKER.
// Prefer an OPEN row so the operator message points at the live segment.
func catalogPathInDir(files []BinlogFile, dir string) string {
	dir = filepath.Clean(strings.TrimSpace(dir))
	fallback := dir
	for _, row := range files {
		path := strings.TrimSpace(row.FilePath)
		if path == "" {
			continue
		}
		if filepath.Clean(filepath.Dir(path)) != dir && !pathUnderDataDir(path, dir) {
			continue
		}
		if catalogOpen(row) {
			return path
		}
		if fallback == dir {
			fallback = path
		}
	}
	return fallback
}

// foreignUnuploadedLabel is a catalog path under dir that is not an UPLOADED
// object. Those rows are the ones takeover would continue on disk; UPLOADED
// rows are resumed from the object into this worker's data_dir instead.
func foreignUnuploadedLabel(files []BinlogFile, dir string) string {
	dir = filepath.Clean(strings.TrimSpace(dir))
	fallback := ""
	for _, row := range files {
		if catalogUploaded(row) {
			continue
		}
		path := strings.TrimSpace(row.FilePath)
		if path == "" {
			label := strings.TrimSpace(row.FileName)
			if label == "" {
				continue
			}
			path = label
		} else if filepath.Clean(filepath.Dir(path)) != dir && !pathUnderDataDir(path, dir) {
			continue
		}
		if catalogOpen(row) {
			return path
		}
		if fallback == "" {
			fallback = path
		}
	}
	return fallback
}

func regularSegmentFile(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
