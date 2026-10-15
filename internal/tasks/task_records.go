// Package tasks provides module-level functionality for tasks.
// input: a standalone task record file (RecordFile, the encrypted {data_dir}/.tasks.enc of ADR 0010), the key problem found at startup, the in-memory tasks, and the task directories under data_dir
// output: the record snapshot (spec, source password, desired state, terminal failure, id seq, delete tombstones; never a position) saved before memory changes at the two operator commit points and best effort when a worker failure changes desired state; Restore of standalone tasks from that file with the start computed fresh from disk (diskResumeStart, adopt semantics) and RESUME_POSITION_UNKNOWN instead of a guessed position; ResumeRecordedTasks that Starts every desired-RUN task after a restart; TASK_RECORDS_FAILED (key_missing, key_invalid, wrong_key, corrupt, write) and refused record-changing actions while the file cannot be used
// pos: ADR 0010 (#385): standalone auto-resume after restart; memory stays the runtime authority (ADR 0005), the file is a write-through journal read once at startup
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/taskrecords"
)

// Subjects of TASK_RECORDS_FAILED.
const (
	RecordsKeyMissing = "key_missing"
	RecordsKeyInvalid = "key_invalid"
	RecordsWrongKey   = "wrong_key"
	RecordsCorrupt    = "corrupt"
	RecordsWrite      = "write"
)

// ErrTaskRecordsLocked: the record file exists but cannot be read, so a
// change that would overwrite it is refused.
var ErrTaskRecordsLocked = errors.New("standalone task records are locked")

// ResumePositionUnknown prefixes last_error when a restored task has no
// position on disk to continue from and its start would skip events.
const ResumePositionUnknown = "RESUME_POSITION_UNKNOWN"

// RecordFile is the encrypted record file (taskrecords.Store).
type RecordFile interface {
	Load() ([]byte, taskrecords.LoadInfo, error)
	Save(plain []byte) error
	Path() string
	KeyID() string
	HasKey() bool
	RemoveStaleTemps() error
}

// WithTaskRecords turns on the standalone task record (ADR 0010). keyErr
// is the problem found loading the key (nil when the key is usable). It
// has no effect when a task store is configured.
func WithTaskRecords(file RecordFile, keyErr error) Option {
	return func(s *Scheduler) {
		s.records = &recordState{file: file, keyErr: keyErr, saved: map[string]string{}, tombstones: map[string]time.Time{}}
	}
}

// recordState is the scheduler's view of the record file. Guarded by s.mu.
type recordState struct {
	file   RecordFile
	keyErr error
	// volatile: no key and no file. Tasks work but are not saved.
	volatile bool
	// locked: a file exists that cannot be read; nothing may overwrite it.
	locked       bool
	lockedReason string
	lockedDetail string
	// saved is the JSON of each task's record as last saved.
	saved      map[string]string
	tombstones map[string]time.Time
	loaded     bool
	// resume lists the ids Restore found with desired RUN.
	resume []string
	// failed holds restore failures to commit after Restore.
	failed map[string]string
	// open holds the TASK_RECORDS_FAILED subjects raised and not yet
	// cleared (subject -> detail). Restore runs before the evaluator exists,
	// so NewEvaluator replays them (and cleared) through replayRecordSignals.
	open map[string]string
	// cleared holds the subjects this process found healthy, replayed as
	// successes so a row left open by an earlier process closes.
	cleared map[string]bool
}

type recordSource struct {
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Flavor   string `json:"flavor"`
	ServerID uint32 `json:"server_id"`
	SemiSync bool   `json:"semi_sync,omitempty"`
}

type recordFailure struct {
	SpecRevision int64  `json:"spec_revision"`
	LastError    string `json:"last_error"`
}

type taskRecord struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	ClusterKey   string         `json:"cluster_key"`
	Source       recordSource   `json:"source"`
	Start        *StartConfig   `json:"start,omitempty"`
	Storage      Storage        `json:"storage"`
	DesiredRun   string         `json:"desired_run"`
	SpecRevision int64          `json:"spec_revision"`
	Adopted      bool           `json:"adopted,omitempty"`
	Failed       *recordFailure `json:"failed,omitempty"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type tombstone struct {
	ID        string    `json:"id"`
	DeletedAt time.Time `json:"deleted_at"`
}

type recordSnapshot struct {
	Seq     int          `json:"seq"`
	SavedAt time.Time    `json:"saved_at"`
	Tasks   []taskRecord `json:"tasks"`
	Deleted []tombstone  `json:"deleted,omitempty"`
}

// recordsActiveLocked: standalone with records configured.
func (s *Scheduler) recordsActiveLocked() bool {
	return s.records != nil && s.store == nil
}

func recordOf(t Task) taskRecord {
	r := taskRecord{
		ID: t.ID, Name: t.Name, ClusterKey: t.ClusterKey,
		Source: recordSource{Host: t.Source.Host, Port: t.Source.Port, User: t.Source.User, Password: t.Source.Password,
			Flavor: t.Source.Flavor, ServerID: t.Source.ServerID, SemiSync: t.Source.SemiSync},
		Storage: t.Storage, DesiredRun: effectiveDesired(t), SpecRevision: t.SpecRevision,
		Adopted: t.KeepLocalSegments, UpdatedAt: t.UpdatedAt.UTC(),
	}
	if t.StartFromDisk {
		if t.OperatorStart != nil {
			st := *t.OperatorStart
			r.Start = &st
		}
	} else {
		st := t.Start
		r.Start = &st
	}
	if t.State == StateFailed && t.FailedSpecRevision > 0 && t.FailedSpecRevision == t.SpecRevision {
		r.Failed = &recordFailure{SpecRevision: t.FailedSpecRevision, LastError: t.LastError}
	}
	return r
}

func recordJSON(r taskRecord) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// snapshotLocked is the record of memory with next applied (put) or id
// removed (deleted). Caller holds s.mu.
func (s *Scheduler) snapshotLocked(next *Task, deleted string) recordSnapshot {
	snap := recordSnapshot{Seq: s.seq, SavedAt: time.Now().UTC()}
	ids := make([]string, 0, len(s.tasks)+1)
	for id := range s.tasks {
		ids = append(ids, id)
	}
	if next != nil {
		if _, ok := s.tasks[next.ID]; !ok {
			ids = append(ids, next.ID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == deleted {
			continue
		}
		t := s.tasks[id]
		if next != nil && next.ID == id {
			t = *next
		}
		snap.Tasks = append(snap.Tasks, recordOf(t))
	}
	tombs := map[string]time.Time{}
	for id, at := range s.records.tombstones {
		tombs[id] = at
	}
	if deleted != "" {
		tombs[deleted] = snap.SavedAt
	}
	if next != nil {
		delete(tombs, next.ID)
	}
	for id, at := range tombs {
		snap.Deleted = append(snap.Deleted, tombstone{ID: id, DeletedAt: at})
	}
	sort.Slice(snap.Deleted, func(i, j int) bool { return snap.Deleted[i].ID < snap.Deleted[j].ID })
	if n := idNumber(deleted); n > snap.Seq {
		snap.Seq = n
	}
	if next != nil {
		if n := idNumber(next.ID); n > snap.Seq {
			snap.Seq = n
		}
	}
	return snap
}

func idNumber(id string) int {
	n, err := strconv.Atoi(id)
	if err != nil {
		return 0
	}
	return n
}

// saveRecordsLocked writes snap. A locked file is refused; a volatile
// state (no key, no file) saves nothing. Caller holds s.mu.
func (s *Scheduler) saveRecordsLocked(snap recordSnapshot) error {
	r := s.records
	if r.locked {
		return fmt.Errorf("%w (%s): %s", ErrTaskRecordsLocked, r.lockedReason, r.lockedDetail)
	}
	if r.volatile || r.file == nil || !r.file.HasKey() {
		return nil
	}
	plain, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err := r.file.Save(plain); err != nil {
		s.recordSignalLocked(RecordsWrite, "the standalone task record "+r.file.Path()+" was not saved: "+err.Error()+"; the change was not applied, and tasks changed since the last save will not survive a restart")
		return fmt.Errorf("save task records: %w", err)
	}
	s.recordClearLocked(RecordsWrite)
	r.saved = map[string]string{}
	for _, t := range snap.Tasks {
		r.saved[t.ID] = recordJSON(t)
	}
	r.tombstones = map[string]time.Time{}
	for _, d := range snap.Deleted {
		r.tombstones[d.ID] = d.DeletedAt
	}
	return nil
}

// recordPutLocked saves memory with next applied, before the caller puts
// next in memory. Caller holds s.mu.
func (s *Scheduler) recordPutLocked(next Task) error {
	if !s.recordsActiveLocked() {
		return nil
	}
	return s.saveRecordsLocked(s.snapshotLocked(&next, ""))
}

// recordDeleteLocked saves memory without id and with its tombstone.
func (s *Scheduler) recordDeleteLocked(id string) error {
	if !s.recordsActiveLocked() {
		return nil
	}
	return s.saveRecordsLocked(s.snapshotLocked(nil, id))
}

// recordFollowLocked saves best effort when a worker-side transition
// changed what the record holds for task (FAILED sets desired STOP). A
// failure leaves TASK_RECORDS_FAILED/write open; the next save carries it.
func (s *Scheduler) recordFollowLocked(task Task) {
	if !s.recordsActiveLocked() || s.records.locked || s.records.volatile {
		return
	}
	if _, ok := s.tasks[task.ID]; !ok {
		return
	}
	if s.records.saved[task.ID] == recordJSON(recordOf(task)) {
		return
	}
	if err := s.saveRecordsLocked(s.snapshotLocked(&task, "")); err != nil {
		log.Printf("task records: save after %s of task %s failed: %v", task.State, task.ID, err)
	}
}

// recordTombstonedLocked: the directory belongs to a task deleted while
// records were on; it is not unadopted. Caller holds s.mu.
func (s *Scheduler) recordTombstonedLocked(id string) bool {
	if !s.recordsActiveLocked() {
		return false
	}
	_, ok := s.records.tombstones[id]
	return ok
}

func (s *Scheduler) lockRecordsLocked(reason, detail string) {
	r := s.records
	r.locked, r.lockedReason, r.lockedDetail = true, reason, detail
	s.recordSignalLocked(reason, detail)
}

// restoreRecordsLocked loads the record file into memory. Caller holds s.mu.
func (s *Scheduler) restoreRecordsLocked() {
	r := s.records
	if r.loaded {
		return
	}
	r.loaded = true
	if r.file == nil {
		return
	}
	if err := r.file.RemoveStaleTemps(); err != nil {
		log.Printf("task records: remove stale temp files: %v", err)
	}
	path := r.file.Path()
	plain, info, err := r.file.Load()
	keyReason := RecordsKeyMissing
	if errors.Is(r.keyErr, taskrecords.ErrKeyInvalid) {
		keyReason = RecordsKeyInvalid
	}
	switch {
	case errors.Is(err, taskrecords.ErrNoFile):
		if r.keyErr != nil {
			r.volatile = true
			s.recordSignalLocked(keyReason, "standalone task records are on but "+r.keyErr.Error()+": tasks are kept in memory only and will not resume after a restart; configure "+taskrecords.KeyEnv+" or standalone.task_key_file and restart")
			log.Printf("task records: no file at %s and no usable key (%v); tasks will not survive a restart", path, r.keyErr)
			return
		}
		s.recordReadableLocked()
		log.Printf("task records: no file at %s yet; key id %s", path, r.file.KeyID())
		return
	case errors.Is(err, taskrecords.ErrKeyMissing):
		reason := keyReason
		cause := "no task key is configured"
		if r.keyErr != nil {
			cause = r.keyErr.Error()
		}
		s.lockRecordsLocked(reason, path+" holds the standalone task records (key id "+info.KeyID+") but "+cause+
			": no task was restored, every task directory is unadopted, and changes are refused so the file is not overwritten; configure the key and restart")
		log.Printf("task records: %s exists (key id %s) but no usable key: %v; nothing restored", path, info.KeyID, r.keyErr)
		return
	case errors.Is(err, taskrecords.ErrWrongKey):
		s.lockRecordsLocked(RecordsWrongKey, path+" was written with key id "+info.KeyID+", not the configured key id "+r.file.KeyID()+
			" (or the previous key): no task was restored and changes are refused so the file is not overwritten; configure the right key (or set it as the previous key to rotate) and restart")
		log.Printf("task records: %s is on key id %s, configured key id %s; nothing restored", path, info.KeyID, r.file.KeyID())
		return
	case err != nil:
		s.lockRecordsLocked(RecordsCorrupt, err.Error()+": no task was restored and changes are refused so the file is not overwritten; copy the file aside, restore it from a backup of data_dir (or move it away to start with no records) and restart")
		log.Printf("task records: %v; nothing restored", err)
		return
	}
	var snap recordSnapshot
	if err := json.Unmarshal(plain, &snap); err != nil {
		s.lockRecordsLocked(RecordsCorrupt, path+" decrypted but its content is not a task record snapshot: "+err.Error()+"; changes are refused so the file is not overwritten")
		log.Printf("task records: %s content: %v; nothing restored", path, err)
		return
	}
	s.recordReadableLocked()
	log.Printf("task records: loaded %s key_id=%s saved_at=%s tasks=%d tombstones=%d", path, info.KeyID, snap.SavedAt.Format(time.RFC3339), len(snap.Tasks), len(snap.Deleted))
	s.applySnapshotLocked(snap)
	if info.Previous {
		if err := s.saveRecordsLocked(s.snapshotLocked(nil, "")); err != nil {
			log.Printf("task records: re-save on the current key id %s failed: %v (the file stays on key id %s)", r.file.KeyID(), err, info.KeyID)
		} else {
			log.Printf("task records: rotated %s from key id %s to key id %s", path, info.KeyID, r.file.KeyID())
		}
	}
}

func (s *Scheduler) applySnapshotLocked(snap recordSnapshot) {
	r := s.records
	if snap.Seq > s.seq {
		s.seq = snap.Seq
	}
	r.failed = map[string]string{}
	for _, d := range snap.Deleted {
		if dir, ok := taskBinlogDir(s.dataDir, d.ID); ok {
			if _, err := os.Lstat(dir); err == nil {
				r.tombstones[d.ID] = d.DeletedAt
			}
		}
	}
	for _, rec := range snap.Tasks {
		t, failure := s.restoredTask(rec)
		s.tasks[t.ID] = t
		r.saved[t.ID] = recordJSON(rec)
		if n := idNumber(t.ID); n > s.seq {
			s.seq = n
		}
		switch {
		case failure != "":
			r.failed[t.ID] = failure
		case t.State == StateStopped && t.DesiredRun == TaskDesiredRun:
			r.resume = append(r.resume, t.ID)
		}
	}
	sort.Strings(r.resume)
}

// restoredTask builds the memory task of rec. The start is computed from
// the task directory, never read from the record as a position. failure is
// set when the task cannot continue safely and must be FAILED.
func (s *Scheduler) restoredTask(rec taskRecord) (Task, string) {
	t := Task{
		ID: rec.ID, Name: rec.Name, ClusterKey: rec.ClusterKey,
		Source: SourceConfig{Host: rec.Source.Host, Port: rec.Source.Port, User: rec.Source.User, Password: rec.Source.Password,
			Flavor: rec.Source.Flavor, ServerID: rec.Source.ServerID, SemiSync: rec.Source.SemiSync},
		Storage: rec.Storage, DesiredRun: rec.DesiredRun, SpecRevision: rec.SpecRevision,
		KeepLocalSegments: rec.Adopted, UpdatedAt: rec.UpdatedAt, State: StateStopped,
	}
	if t.DesiredRun != TaskDesiredRun {
		t.DesiredRun = TaskDesiredStop
	}
	var opStart *StartConfig
	if rec.Start != nil {
		st := *rec.Start
		opStart = &st
		t.Start = st
	}
	if rec.Failed != nil && rec.Failed.SpecRevision == rec.SpecRevision && rec.SpecRevision > 0 {
		t.State, t.LastError, t.FailedSpecRevision, t.DesiredRun = StateFailed, rec.Failed.LastError, rec.Failed.SpecRevision, TaskDesiredStop
	}
	_, hasSegments, err := lookupDiskBackupTask(s.dataDir, t.ID)
	if err != nil {
		if t.DesiredRun == TaskDesiredRun {
			return t, ResumePositionUnknown + ": the task directory under data_dir cannot be read: " + err.Error()
		}
		return t, ""
	}
	if hasSegments {
		// Adopt semantics (#388): keep every segment, raise the epoch above
		// them, and continue from the last complete event on disk.
		t.KeepLocalSegments = true
		t.StartFromDisk, t.OperatorStart = true, opStart
		if start, err := diskResumeStart(s.dataDir, t.ID); err == nil {
			t.Start = start
			return t, ""
		}
		if opStart != nil && opStart.Mode != StartModeLatest && opStart.Mode != "" && preambleOnlyDir(s.dataDir, t.ID) {
			// Nothing was ever stored: the operator start re-reads from its point.
			t.Start, t.StartFromDisk, t.OperatorStart = *opStart, false, nil
			return t, ""
		}
		if t.DesiredRun == TaskDesiredRun {
			dir, _ := taskBinlogDir(s.dataDir, t.ID)
			return t, ResumePositionUnknown + ": " + dir + " holds segments but no complete event to continue from; starting from the configured start could skip or repeat transactions. Check the directory, then Start the task to accept its configured start"
		}
		return t, ""
	}
	if t.DesiredRun == TaskDesiredRun && (opStart == nil || opStart.Mode == StartModeLatest || opStart.Mode == "") {
		return t, ResumePositionUnknown + ": the task was running but data_dir holds no segment for it, and its start is LATEST: resolving LATEST again would skip every transaction written while the server was down. Start the task to accept the current tip"
	}
	if t.State != StateFailed && t.SpecRevision == 0 && t.DesiredRun != TaskDesiredRun {
		t.State = StateCreated
	}
	return t, ""
}

// preambleOnlyDir: every segment in the task directory is only magic or a
// format-description header (nothing was stored).
func preambleOnlyDir(dataDir, id string) bool {
	dir, ok := taskBinlogDir(dataDir, id)
	if !ok {
		return false
	}
	names, err := readSegmentNames(dir)
	if err != nil || len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !binlog.PreambleOnly(filepath.Join(dir, n)) {
			return false
		}
	}
	return true
}

// ResumeRecordedTasks starts every task Restore found with desired RUN, and
// fails the ones that cannot continue safely. Standalone only; call it once
// after Restore and SetRunner. It returns the number started.
func (s *Scheduler) ResumeRecordedTasks() int {
	s.mu.Lock()
	if !s.recordsActiveLocked() {
		s.mu.Unlock()
		return 0
	}
	resume := append([]string(nil), s.records.resume...)
	failed := s.records.failed
	s.records.resume, s.records.failed = nil, nil
	for id, msg := range failed {
		s.failRestoredLocked(id, msg)
	}
	s.mu.Unlock()
	started := 0
	for _, id := range resume {
		if err := s.StartTask(id); err != nil {
			log.Printf("task records: resume of task %s after restart failed: %v", id, err)
			s.mu.Lock()
			if t, ok := s.tasks[id]; ok && t.State != StateFailed && t.State != StateRunning && t.State != StateStarting {
				s.failRestoredLocked(id, "the task could not be resumed after a restart: "+err.Error())
			}
			s.mu.Unlock()
			continue
		}
		log.Printf("task records: resumed task %s after restart", id)
		started++
	}
	return started
}

// failRestoredLocked makes a restored task FAILED for its current ask
// through the worker failure recipe, so TASK_FAILED opens and the record
// keeps it failed (best effort save). Caller holds s.mu.
func (s *Scheduler) failRestoredLocked(id, msg string) {
	t, ok := s.tasks[id]
	if !ok {
		return
	}
	if t.SpecRevision == 0 {
		t.SpecRevision = 1
		s.tasks[id] = t
	}
	if err := s.markFailedLocked(id, msg); err != nil {
		log.Printf("task records: task %s FAILED on restore, but not saved: %v", id, err)
	}
	log.Printf("task records: task %s FAILED on restore: %s", id, strings.TrimSpace(msg))
}

// recordSignalLocked raises TASK_RECORDS_FAILED/subject and remembers it.
// Caller holds s.mu.
func (s *Scheduler) recordSignalLocked(subject, detail string) {
	if s.records.open == nil {
		s.records.open = map[string]string{}
	}
	s.records.open[subject] = detail
	delete(s.records.cleared, subject)
	s.SignalFailure(Finding{TaskID: ClusterFindingTask, Kind: FindingTaskRecordsFailed, Subject: subject, Severity: FindingCritical, Detail: detail})
}

// recordClearLocked clears TASK_RECORDS_FAILED/subject. Caller holds s.mu.
func (s *Scheduler) recordClearLocked(subject string) {
	delete(s.records.open, subject)
	if s.records.cleared == nil {
		s.records.cleared = map[string]bool{}
	}
	s.records.cleared[subject] = true
	s.SignalSuccess(ClusterFindingTask, FindingTaskRecordsFailed, subject)
}

// recordReadableLocked: the file was read with a configured key (or there
// is none and a key is configured), so every startup subject holds again.
func (s *Scheduler) recordReadableLocked() {
	for _, subject := range []string{RecordsKeyMissing, RecordsKeyInvalid, RecordsWrongKey, RecordsCorrupt} {
		s.recordClearLocked(subject)
	}
}

// replayRecordSignals re-raises the TASK_RECORDS_FAILED subjects for an
// evaluator built after Restore: the successes, which never reached any
// book, and the failures (adoptEarlySignals has already moved their legacy
// copies over; a second failure is harmless).
func (s *Scheduler) replayRecordSignals() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		return
	}
	for subject := range s.records.cleared {
		s.SignalSuccess(ClusterFindingTask, FindingTaskRecordsFailed, subject)
	}
	for subject, detail := range s.records.open {
		s.SignalFailure(Finding{TaskID: ClusterFindingTask, Kind: FindingTaskRecordsFailed, Subject: subject, Severity: FindingCritical, Detail: detail})
	}
}
