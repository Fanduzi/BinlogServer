// Package tasks provides module-level functionality for tasks.
// input: local data_dir and task id for a binlog segment directory
// output: sealed and open on-disk segments ordered by source generation then binlog index, WindowBinlogFilesForReplay for that same order on catalog rows, SelectReplayFiles for every sealed segment plus the highest open epoch of each source index except a sealed point-range row already covered by another sealed span of that index, ReplayLocations for local/bucket/both, ReplayClient for the mysqlbinlog or mariadb-binlog hint, leftover task ids when no task store is configured, the FILE_POS resume point at the end of the highest segment, and the next open epoch above those segments, FilePositionsForAPI for the files list (JSON null when the end is unknown, the event span when this process can read the local segment), and standalone listing positions from SegmentPositions
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

	"binlog_server/internal/binlog"
)

// listTaskBinlogFilesOnDisk reads {dataDir}/{taskID} for sealed binlog names
// and name.open.e<epoch> segments. file_name is the source name. file_path is
// the on-disk path, including .open.e<epoch> while the segment is open.
// Results follow WindowBinlogFilesForReplay: an earlier source, then index
// order inside that source. The same index lists the sealed name first, then
// open epochs ascending. limit keeps the tail of that order.
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
		source, _, epoch, state, ok := classifyBinlogSegment(name)
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
		if epoch >= 0 {
			item.Epoch = epoch
		}
		if state == "SEALED" {
			item.SealedAt = item.CreatedAt
		}
		if start, end, ok := binlog.SegmentPositions(item.FilePath); ok {
			item.StartPos = FilePos(start)
			item.EndPos = FilePos(end)
		}
		files = append(files, item)
	}
	return WindowBinlogFilesForReplay(files, limit), nil
}

// WindowBinlogFilesForReplay orders one source by index, and puts an earlier
// source before a later one. The earlier source is the one whose earliest
// CreatedAt is older, so a new primary's mysql-bin.000001 stays after the old
// primary's higher index. The same index lists the sealed name, then open
// epochs from low to high. limit keeps the tail of that order. An empty input
// is returned as-is.
func WindowBinlogFilesForReplay(files []BinlogFile, limit int) []BinlogFile {
	if len(files) == 0 {
		return files
	}
	out := make([]BinlogFile, len(files))
	copy(out, files)
	orderBinlogSegments(out)
	if limit <= 0 {
		limit = 200
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// ReplaySet is the restore argument list for one task.
// Paths are inventory file_path values, ascending: every sealed segment of a
// source index, then its highest open epoch.
// Locations is the same order: local, bucket, or both. bucket means that
// path is the catalog file_path and is not on this process; download and
// replay/archive still read the object. Do not pass a bucket path to
// mysqlbinlog until the file is downloaded.
// Client is the binary name. ClientHint names which vendor binary to run.
// source.flavor mysql → client mysqlbinlog, hint "MySQL mysqlbinlog".
// source.flavor mariadb → client and hint "mariadb-binlog".
// Any other flavor, including an empty leftover directory, leaves both empty.
type ReplaySet struct {
	Flavor     string   `json:"flavor"`
	Client     string   `json:"client"`
	ClientHint string   `json:"client_hint"`
	Paths      []string `json:"paths"`
	Locations  []string `json:"locations,omitempty"`
}

// ReplayClient maps source.flavor to the binlog client a DBA should run.
func ReplayClient(flavor string) (client, hint string) {
	switch strings.ToLower(strings.TrimSpace(flavor)) {
	case "mysql":
		return "mysqlbinlog", "MySQL mysqlbinlog"
	case "mariadb":
		return "mariadb-binlog", "mariadb-binlog"
	default:
		return "", ""
	}
}

// SelectReplayFiles keeps every sealed segment and the highest open epoch of
// each source index. A later .open.e* does not drop an earlier sealed path:
// events that exist only in that sealed segment stay in the restore list.
// A lower open epoch of the same index is dropped. A sealed row is omitted
// only when the kept open row carries the same object key, which means those
// bytes were copied into the open file, or when start_pos equals end_pos
// (both greater than 0) and another sealed row of the same index has a real
// span covering that point. That point row is a takeover re-seal: the file
// bytes repeat an already uploaded epoch, and the catalog recorded the resume
// cursor instead of a new event range. A row with end_pos 0 is kept.
// Rows that are not a binlog segment, or have an empty file_path, are dropped.
// An empty window returns an empty slice.
func SelectReplayFiles(files []BinlogFile) []BinlogFile {
	opens := make(map[string]BinlogFile)
	sealed := make([]BinlogFile, 0)
	for _, file := range files {
		key := binlogSegmentKey(file)
		if !key.ok || strings.TrimSpace(file.FilePath) == "" {
			continue
		}
		if segmentIsOpen(file) {
			id := segmentIndexKey(key)
			prev, ok := opens[id]
			if !ok || key.epoch >= binlogSegmentKey(prev).epoch {
				opens[id] = file
			}
			continue
		}
		sealed = append(sealed, file)
	}
	out := make([]BinlogFile, 0, len(sealed)+len(opens))
	for _, file := range sealed {
		if sealedCopiedIntoOpen(file, opens) || sealedPointCovered(file, sealed) {
			continue
		}
		out = append(out, file)
	}
	for _, file := range opens {
		out = append(out, file)
	}
	orderBinlogSegments(out)
	return out
}

func segmentIsOpen(file BinlogFile) bool {
	switch strings.ToUpper(strings.TrimSpace(file.State)) {
	case "SEALED":
		return false
	case "OPEN":
		return true
	}
	_, _, _, state, ok := classifyBinlogSegment(filepath.Base(file.FilePath))
	return ok && state == "OPEN"
}

func sealedCopiedIntoOpen(file BinlogFile, opens map[string]BinlogFile) bool {
	key := strings.TrimSpace(file.ObjectKey)
	if key == "" {
		return false
	}
	open, ok := opens[segmentIndexKey(binlogSegmentKey(file))]
	if !ok {
		return false
	}
	return strings.TrimSpace(open.ObjectKey) == key
}

// sealedPointCovered reports a sealed row that records no event span of its
// own (start_pos == end_pos > 0) while another sealed row of the same source
// index already covers that position. end_pos 0 is not a point and is not a
// cover: the position was not recorded, and replay still returns that segment.
func sealedPointCovered(file BinlogFile, sealed []BinlogFile) bool {
	if file.EndPos == 0 || file.StartPos != file.EndPos {
		return false
	}
	key := binlogSegmentKey(file)
	if !key.ok {
		return false
	}
	for _, other := range sealed {
		if other.FilePath == file.FilePath && other.Epoch == file.Epoch {
			continue
		}
		otherKey := binlogSegmentKey(other)
		if !otherKey.ok || otherKey.prefix != key.prefix || otherKey.seq != key.seq {
			continue
		}
		if other.EndPos <= other.StartPos {
			continue
		}
		if other.StartPos <= file.StartPos && other.EndPos >= file.EndPos {
			return true
		}
	}
	return false
}

// ReplayLocations is one location per selected replay file, same order as paths.
func ReplayLocations(files []BinlogFile) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, len(files))
	for i, file := range files {
		out[i] = file.Location
	}
	return out
}

// annotateSegmentLocations sets Location from this process's disk and the row.
// local: the basename is a regular file under {data_dir}/{task_id}, or file_path
// itself is that file. bucket: sealed UPLOADED with an object key and no local
// file. both: the local file and that object. Anything else stays empty.
func annotateSegmentLocations(dataDir string, files []BinlogFile) []BinlogFile {
	if len(files) == 0 {
		return files
	}
	out := make([]BinlogFile, len(files))
	copy(out, files)
	for i := range out {
		out[i].Location = segmentLocation(dataDir, out[i])
	}
	return out
}

func segmentLocation(dataDir string, file BinlogFile) string {
	onDisk := segmentOnDisk(dataDir, file)
	inBucket := strings.EqualFold(strings.TrimSpace(file.UploadState), "UPLOADED") && strings.TrimSpace(file.ObjectKey) != ""
	switch {
	case onDisk && inBucket:
		return "both"
	case inBucket:
		return "bucket"
	case onDisk:
		return "local"
	default:
		return ""
	}
}

func segmentOnDisk(dataDir string, file BinlogFile) bool {
	name := segmentInventoryBasename(file)
	if dir, ok := taskBinlogDir(dataDir, file.TaskID); ok && name != "" && regularFile(filepath.Join(dir, name)) {
		return true
	}
	path := strings.TrimSpace(file.FilePath)
	if path != "" && name != "" && filepath.Base(path) == name && regularFile(path) {
		return true
	}
	return false
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
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

// CatalogRowOpen is true when the catalog state is OPEN.
// An empty state uses ClassifySegment on the name. A SEALED row is not open.
func CatalogRowOpen(row BinlogFile) bool {
	switch strings.ToUpper(strings.TrimSpace(row.State)) {
	case "OPEN":
		return true
	case "SEALED":
		return false
	default:
		return binlog.OpenName(row.FileName) || binlog.OpenName(filepath.Base(row.FilePath))
	}
}

// FilePositionsForAPI is the files-list view of start_pos and end_pos.
// A local file whose catalog end is unknown or a resume-cursor point shows the
// event span when DurableCursor can read it, and null when it cannot.
// A stored span (end greater than start) is left as stored. An object-only
// row is left as stored, including a historical 0, which JSON encodes as null.
func FilePositionsForAPI(files []BinlogFile) []BinlogFile {
	if len(files) == 0 {
		return files
	}
	out := make([]BinlogFile, len(files))
	copy(out, files)
	for i := range out {
		out[i] = filePositionForAPI(out[i])
	}
	return out
}

func filePositionForAPI(file BinlogFile) BinlogFile {
	path := strings.TrimSpace(file.FilePath)
	if path == "" || !regularFile(path) {
		return file
	}
	if !binlog.SealedName(filepath.Base(path)) && !binlog.OpenName(filepath.Base(path)) {
		file.EndPos = 0
		return file
	}
	if file.EndPos > file.StartPos {
		return file
	}
	point := file.EndPos > 0 && file.StartPos == file.EndPos
	start, end, ok := binlog.SegmentPositions(path)
	if !ok {
		file.EndPos = 0
		if point {
			file.StartPos = 0
		}
		return file
	}
	file.StartPos = FilePos(start)
	file.EndPos = FilePos(end)
	return file
}

func classifyBinlogSegment(name string) (source string, seq uint64, epoch int64, state string, ok bool) {
	named, ok := binlog.ClassifySegment(name)
	if !ok {
		return "", 0, 0, "", false
	}
	state = "SEALED"
	if named.Open {
		state = "OPEN"
	}
	return named.Source, named.Seq, named.Epoch, state, true
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

// orderBinlogSegments sorts one source's files by index, and puts an earlier
// source before a later one. The earlier source is the one whose earliest
// CreatedAt is older. A VIP switch keeps the old server's mysql-bin.000009
// ahead of the new server's mysql-bin.000001.
func orderBinlogSegments(files []BinlogFile) {
	gen := segmentGenerations(files)
	sort.SliceStable(files, func(i, j int) bool {
		return segmentGenerationLess(files[i], files[j], gen)
	})
}

func segmentGenerations(files []BinlogFile) map[string]time.Time {
	gen := make(map[string]time.Time)
	for _, file := range files {
		key := binlogSegmentKey(file)
		if !key.ok || file.CreatedAt.IsZero() {
			continue
		}
		prev, ok := gen[key.prefix]
		if !ok || file.CreatedAt.Before(prev) {
			gen[key.prefix] = file.CreatedAt
		}
	}
	return gen
}

func segmentGenerationLess(a, b BinlogFile, gen map[string]time.Time) bool {
	ak := binlogSegmentKey(a)
	bk := binlogSegmentKey(b)
	if ak.ok && bk.ok && ak.prefix != bk.prefix {
		ag, aOK := gen[ak.prefix]
		bg, bOK := gen[bk.prefix]
		if aOK && bOK && !ag.Equal(bg) {
			return ag.Before(bg)
		}
		if aOK != bOK {
			return aOK
		}
		return ak.prefix < bk.prefix
	}
	return binlogSegmentLess(a, b)
}

func segmentIndexKey(key segmentSortKey) string {
	return key.prefix + "\x00" + strconv.FormatUint(key.seq, 10)
}

type segmentSortKey struct {
	prefix string
	seq    uint64
	epoch  int64
	name   string
	ok     bool
}

func binlogSegmentKey(file BinlogFile) segmentSortKey {
	name := filepath.Base(file.FilePath)
	source, seq, epoch, _, ok := classifyBinlogSegment(name)
	if !ok {
		return segmentSortKey{name: name}
	}
	prefix, _, splitOK := splitSourceIndex(source)
	if !splitOK {
		prefix = source
	}
	return segmentSortKey{prefix: prefix, seq: seq, epoch: epoch, name: name, ok: true}
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
