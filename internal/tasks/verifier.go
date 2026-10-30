// Package tasks provides module-level functionality for tasks.
// input: the in-memory task list, a complete catalog listing, the task directory under data_dir, an object store that can HEAD and GET, and the open findings (severity and detail)
// output: one assurance.Verify pass per allowed task, findings written through the existing sink (and the standalone file store when one is set), Report.VerifiedAt and Report.Read stored for the next pass (including a pass the deadline cuts short), progress for a gone segment or task removed from .verify.json, and the verify gauges. A pass that checks nothing does not write .verify.json. Write, Sync, Close, and Rename failures remove the temp file; a failed write is zap warn and dirty stays set until a write succeeds. Startup removes leftover .verify-*.tmp in the data dir only, unlinking a symlink without touching its target; a remove error is zap warn and the other temps are still removed. Temp names include the process id. Each pass logs at info: tasks, skipped, bytes, findings newly opened, findings currently open, findings closed, cut short, object incomplete, and duration. An object-storage error is zap warn and names the task, plus the object key and bucket when they are known (an empty object key uses the segment name; no per-object row omits the object field); three consecutive ones set health detail without changing status, and a finished object pass clears it. A pass with no tasks clears that detail but does not change the streak. A pass that skips every existing task changes neither. VerifierObjectBucket reports the bucket startVerifier stored. The streak is memory only. A corrupt .verify.json, a directory at that name, a catalog-list error, a retention-marker error, a per-task Verify error, and an encode error are zap warn and do not stop the process. pass_deadline_sec 0 uses every_sec. A cancelled pass keeps the partial progress it already applied and does not mark the task verified. An empty scheduler drops tasks that are no longer listed
// pos: ADR 0009 phase 3 worker (docs/design/0009-phase3-verifier.md §10). Cluster Allows is VerifyAllows: only the task lease holder verifies, per ADR 0011 per_node. This file does not import internal/ownership
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/binlog"
	"binlog_server/internal/health"

	"go.uber.org/zap"
)

// VerifyStateFileName is the per-process verify progress file under data_dir.
// It starts with a dot so the on-disk backup listing never treats it as a task.
// Losing it only causes an extra read (design §15).
const VerifyStateFileName = ".verify.json"

// ObjectHeadOpener is the object client the verifier can HEAD and GET.
// A missing object is os.ErrNotExist. The upload client is not required to
// implement it; a client that cannot HEAD is left unused and object checks stay off.
type ObjectHeadOpener interface {
	HeadObject(ctx context.Context, key string) (int64, error)
	OpenObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// ObjectProbeKey is the startup HEAD. A missing key means the bucket answered.
// An auth error, a network error, and a missing bucket are not that answer.
const ObjectProbeKey = ".binlog-server-verify-probe"

// ProbeObjectHead HEADs ObjectProbeKey. A successful HEAD or os.ErrNotExist
// means storage answered. Any other error is returned unchanged.
func ProbeObjectHead(ctx context.Context, c ObjectHeadOpener) error {
	if c == nil {
		return errors.New("object client cannot HEAD and GET")
	}
	_, err := c.HeadObject(ctx, ObjectProbeKey)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type headOpenerStore struct{ c ObjectHeadOpener }

// NewObjectStore adapts a HEAD/GET client to the verifier's object store.
// A nil client is a nil store: the pass does not judge objects.
func NewObjectStore(c ObjectHeadOpener) assurance.ObjectStore {
	if c == nil {
		return nil
	}
	return headOpenerStore{c}
}

func (s headOpenerStore) Head(ctx context.Context, key string) (int64, error) {
	return s.c.HeadObject(ctx, key)
}

func (s headOpenerStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, _, err := s.c.OpenObject(ctx, key)
	return rc, err
}

// VerifierConfig is how the process runs Verify. BudgetBytesPerSec 0 uses
// the 4 MiB/s default. A negative rate is unlimited (tests). Every 0 means
// the caller drives Pass itself and the pass has no deadline.
type VerifierConfig struct {
	DataDir            string
	Every              time.Duration
	BudgetBytesPerSec  int64
	RotationPeriods    int
	RecentSharePercent int
	PassDeadline       time.Duration
	PassMaxBytes       int64
	ObjectRecent       time.Duration
	// Allows reports whether this process may verify taskID. Nil allows
	// every task. An error is treated as not allowed: the task is skipped,
	// and nothing is opened, closed or advanced.
	Allows  func(ctx context.Context, taskID string) (bool, error)
	Objects assurance.ObjectStore
	// ObjectBucket is the upload bucket named in an incomplete object pass.
	// Empty when the process has no bucket configured.
	ObjectBucket string
	// DrillDSN is the restore drill's only address; empty leaves the drill
	// off and it is never dialed. DrillEvery is the attempt period
	// (config default 7d); <= 0 also leaves the drill off.
	// DrillCheckSQL is the optional post-replay check statement.
	DrillDSN      string
	DrillEvery    time.Duration
	DrillCheckSQL string
}

// TaskVerifyMetrics is one task's verifier series after the last finished pass.
type TaskVerifyMetrics struct {
	LastVerifiedUnix     int64
	HaveLastVerified     bool
	LocalBytes           int64
	ObjectBytes          int64
	IncompleteLocal      bool
	IncompleteObject     bool
	IncompleteChain      bool
	UnverifiableNoDigest int
	NoDigestLocal        int
	UnverifiableSize     int
	Deferred             int
	// LastDrillSuccessUnix is the last restore-drill pass. HaveDrillSuccess
	// is false while no drill has passed, so the gauge stays unemitted.
	LastDrillSuccessUnix int64
	HaveDrillSuccess     bool
}

// VerifyMetrics is the process-wide snapshot /metrics reads.
type VerifyMetrics struct {
	Tasks map[string]TaskVerifyMetrics
}

// Verifier runs assurance.Verify for each task this process may check.
type Verifier struct {
	sched  *Scheduler
	cfg    VerifierConfig
	budget *assurance.ByteBudget

	mu      sync.Mutex
	start   int
	dirty   bool
	state   verifyStateFile
	metrics map[string]TaskVerifyMetrics
	// objectStreak counts consecutive passes whose object scope failed
	// because storage returned an error. It is not a budget skip. It is not
	// written to .verify.json: a restart starts again at zero.
	objectStreak int
}

// objectIncompleteDegradeAfter is how many consecutive object-storage
// failures it takes before /api/health detail records them. Status stays ok.
// A later pass that finishes its object checks clears that detail. Not a
// config key: the default period is 24h, so this is three periods. The count
// is not saved, so a restart waits another three periods.
const objectIncompleteDegradeAfter = 3

// verifyFileOps is the write path of .verify.json. Tests replace a function
// to fail Write, Sync, or Close; each failure still removes the temp file.
type verifyFileOps struct {
	write  func(*os.File, []byte) (int, error)
	sync   func(*os.File) error
	close  func(*os.File) error
	remove func(string) error
}

// defaultVerifyFileOps is the production write path for .verify.json.
// Tests replace fields on verifyFiles after this runs.
func defaultVerifyFileOps() verifyFileOps {
	return verifyFileOps{
		write:  func(f *os.File, p []byte) (int, error) { return f.Write(p) },
		sync:   func(f *os.File) error { return f.Sync() },
		close:  func(f *os.File) error { return f.Close() },
		remove: os.Remove,
	}
}

var verifyFiles = defaultVerifyFileOps()

// verifyEncode is json.MarshalIndent. A test returns an error the real
// encoder does not produce for verifyStateFile.
var verifyEncode = json.MarshalIndent

type verifyStateFile struct {
	Tasks map[string]verifyTaskState `json:"tasks"`
}

type verifyTaskState struct {
	LastVerified map[string]time.Time          `json:"last_verified,omitempty"`
	LastRead     map[string]assurance.ReadMark `json:"last_read,omitempty"`
	Drill        *drillTaskState               `json:"drill,omitempty"`
}

// NewVerifier loads saved progress and builds the process-wide byte budget.
func NewVerifier(s *Scheduler, cfg VerifierConfig) *Verifier {
	rate := cfg.BudgetBytesPerSec
	var budget *assurance.ByteBudget
	if rate >= 0 {
		if rate == 0 {
			rate = assurance.DefaultBudgetBytesPerSec
		}
		budget = assurance.NewByteBudget(rate)
	}
	v := &Verifier{
		sched: s, cfg: cfg, budget: budget,
		state:   verifyStateFile{Tasks: map[string]verifyTaskState{}},
		metrics: map[string]TaskVerifyMetrics{},
	}
	v.load()
	return v
}

// SetVerifier attaches v so /metrics can read VerifyMetrics. Nil detaches it.
func (s *Scheduler) SetVerifier(v *Verifier) {
	if s == nil {
		return
	}
	s.verifier.Store(v)
}

// VerifierObjectBucket is the upload bucket stored on the attached verifier.
// Empty means no verifier is attached or the bucket was not set.
func (s *Scheduler) VerifierObjectBucket() string {
	if s == nil {
		return ""
	}
	v := s.verifier.Load()
	if v == nil {
		return ""
	}
	return v.cfg.ObjectBucket
}

// VerifyMetrics is the last finished pass per task. An empty snapshot means
// no verifier is attached or no pass has finished.
func (s *Scheduler) VerifyMetrics() VerifyMetrics {
	if s == nil {
		return VerifyMetrics{}
	}
	v := s.verifier.Load()
	if v == nil {
		return VerifyMetrics{}
	}
	return v.Metrics()
}

// Metrics copies the snapshot.
func (v *Verifier) Metrics() VerifyMetrics {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := VerifyMetrics{Tasks: make(map[string]TaskVerifyMetrics, len(v.metrics))}
	for id, m := range v.metrics {
		out.Tasks[id] = m
	}
	return out
}

// Run passes immediately and then every VerifierConfig.Every until ctx ends.
// Every <= 0 runs one pass and returns.
func (v *Verifier) Run(ctx context.Context) {
	v.Pass(ctx, time.Now())
	if v.cfg.Every <= 0 {
		return
	}
	t := time.NewTicker(v.cfg.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			v.Pass(ctx, now)
		}
	}
}

// Pass verifies every allowed task once, starting at a rotating task so one
// large task does not consume the whole period every time.
func (v *Verifier) Pass(ctx context.Context, now time.Time) {
	if v == nil || v.sched == nil {
		return
	}
	started := time.Now()
	if now.IsZero() {
		now = started
	}
	deadline := v.cfg.PassDeadline
	if deadline == 0 {
		deadline = v.cfg.Every
	}
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	tasks := v.sched.ListTasks()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	live := map[string]bool{}
	for _, task := range tasks {
		live[task.ID] = true
	}
	if len(tasks) == 0 {
		v.pruneAbsentTasks(live)
		v.saveIfDirty()
		// No task remains to report. The streak stays, so a task that
		// comes back continues the count, but the detail named a task
		// that is gone.
		if v.sched != nil {
			v.sched.SetVerifyObjectDetail("")
		}
		v.noteObjectPasses(0, false, "")
		v.logPass(started, 0, 0, 0, 0, v.openFindingCount(), 0, false, false)
		return
	}
	v.mu.Lock()
	start := v.start % len(tasks)
	v.start++
	v.mu.Unlock()
	ordered := append(append([]Task(nil), tasks[start:]...), tasks[:start]...)
	var tasksChecked, opened, closed, skipped int
	var bytesRead int64
	cut := false
	objectIncomplete := false
	var causes []string
	for _, task := range ordered {
		// A deadline that cuts the list still keeps the tasks already
		// checked. saveIfDirty below writes that partial progress.
		// Tasks the deadline never reached are cut_short, not skipped.
		if ctx.Err() != nil {
			cut = true
			break
		}
		checked, n, openedN, closedN, short, objErr, cause := v.passTask(ctx, task.ID, now)
		v.drillTask(ctx, task, now)
		if !checked {
			skipped++
			continue
		}
		tasksChecked++
		bytesRead += n
		opened += openedN
		closed += closedN
		if short {
			cut = true
		}
		if objErr {
			objectIncomplete = true
			if cause != "" {
				causes = append(causes, cause)
			}
		}
	}
	if tasksChecked == 0 {
		// Tasks exist but none was checked (Allows, or the catalog list
		// failed). Not an object pass: leave the streak and the detail.
		v.pruneAbsentTasks(live)
		v.saveIfDirty()
		v.logPass(started, 0, skipped, 0, 0, v.openFindingCount(), 0, cut, false)
		return
	}
	v.noteObjectPasses(tasksChecked, objectIncomplete, strings.Join(causes, "; "))
	v.pruneAbsentTasks(live)
	v.saveIfDirty()
	v.logPass(started, tasksChecked, skipped, bytesRead, opened, v.openFindingCount(), closed, cut, objectIncomplete)
}

func (v *Verifier) logPass(started time.Time, tasks, skipped int, bytes int64, opened, open, closed int, cut, objectIncomplete bool) {
	zap.L().Info(fmt.Sprintf("verifier: pass tasks=%d skipped=%d bytes=%d findings_opened=%d findings_open=%d findings_closed=%d cut_short=%t object_incomplete=%t duration=%s",
		tasks, skipped, bytes, opened, open, closed, cut, objectIncomplete, time.Since(started)))
}

// noteObjectPasses warns when storage stopped an object check, and after
// objectIncompleteDegradeAfter consecutive passes records health detail.
// A pass that checked at least one task and finished its object scope clears both.
// A pass that checked nothing does not increment the streak and does not clear it.
// The caller clears the detail itself when the scheduler has no task left.
func (v *Verifier) noteObjectPasses(checked int, incomplete bool, cause string) {
	if incomplete {
		v.objectStreak++
		zap.L().Warn("verifier: object pass incomplete", zap.String("cause", cause), zap.Int("consecutive", v.objectStreak))
		if v.objectStreak >= objectIncompleteDegradeAfter && v.sched != nil {
			v.sched.SetVerifyObjectDetail(fmt.Sprintf("object storage: %d consecutive passes could not finish object checks: %s", v.objectStreak, cause))
		}
		return
	}
	if checked == 0 {
		return
	}
	v.objectStreak = 0
	if v.sched != nil {
		v.sched.SetVerifyObjectDetail("")
	}
}

func (v *Verifier) passTask(ctx context.Context, taskID string, now time.Time) (checked bool, bytes int64, opened, closed int, cut, objectErr bool, cause string) {
	if v.cfg.Allows != nil {
		ok, err := v.cfg.Allows(ctx, taskID)
		if err != nil || !ok {
			return
		}
	}
	files, err := v.sched.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		zap.L().Warn(fmt.Sprintf("verifier: list catalog task=%s: %v", taskID, err))
		return
	}
	v.pruneSegments(taskID, files)
	markers, err := v.sched.retentionMarkers(taskID)
	if err != nil {
		zap.L().Warn(fmt.Sprintf("verifier: retention markers task=%s: %v", taskID, err))
		return
	}
	dir, _ := taskBinlogDir(v.cfg.DataDir, taskID)
	rows := verifyRowsFromCatalog(files, markers)
	opt := v.options(taskID, now)
	rep, err := assurance.Verify(ctx, dir, rows, v.cfg.Objects, nil, opt)
	checked = true
	bytes = rep.LocalBytes + rep.ObjectBytes
	closed = len(rep.Closed)
	cut = rep.Deferred > 0 || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	cause = objectPassCause(taskID, v.cfg.ObjectBucket, rows, rep)
	objectErr = cause != ""
	if err != nil && !errors.Is(err, context.Canceled) {
		zap.L().Warn(fmt.Sprintf("verifier: task=%s: %v", taskID, err))
		return
	}
	opened = v.newlyOpened(taskID, rep)
	v.apply(taskID, rep)
	v.remember(taskID, rep)
	if !errors.Is(err, context.Canceled) {
		v.note(taskID, now, rep)
	}
	return
}

// newlyOpened counts findings this report adds. A finding already in the
// book is not opened again, even when Verify names it on every later pass.
func (v *Verifier) newlyOpened(taskID string, rep assurance.Report) int {
	before := map[string]struct{}{}
	for _, f := range v.sched.openBookFindings(taskID) {
		before[string(f.Kind)+"\x00"+f.Subject] = struct{}{}
	}
	n := 0
	for _, f := range rep.Findings {
		key := f.Kind + "\x00" + verifierStoreSubject(f.Kind, f.Subject)
		if _, ok := before[key]; ok {
			continue
		}
		n++
	}
	return n
}

// objectPassCause names the task, and the object key and bucket when they are
// known, when storage stopped an object check. Either marker is enough:
// ObjectErr, or a coverage row whose object method is MethodError. A
// MethodError row with an empty object key uses the segment name. No
// per-object row omits the object field. An empty bucket is omitted too.
// An empty string means the object scope finished.
func objectPassCause(taskID, bucket string, rows []assurance.VerifyRow, rep assurance.Report) string {
	reason := ""
	if rep.ObjectErr != nil {
		reason = rep.ObjectErr.Error()
	}
	key := ""
	for _, row := range rows {
		c, ok := rep.Coverage[row.Name]
		if !ok || c.Object.Method != assurance.MethodError {
			continue
		}
		key = row.ObjectKey
		if key == "" {
			key = row.Name
		}
		if reason == "" {
			reason = c.Object.Reason
			if reason == "" {
				reason = "object check failed"
			}
		}
		break
	}
	if reason == "" {
		return ""
	}
	msg := "task " + taskID
	if key != "" {
		msg += " object " + key
	}
	if bucket != "" {
		msg += " bucket " + bucket
	}
	return msg + ": " + reason
}

func (v *Verifier) options(taskID string, now time.Time) assurance.Options {
	v.mu.Lock()
	st := v.state.Tasks[taskID]
	v.mu.Unlock()
	opt := assurance.Options{
		Now: now, RotationPeriods: v.cfg.RotationPeriods, RecentSharePercent: v.cfg.RecentSharePercent,
		Period: v.cfg.Every, Recent: v.cfg.ObjectRecent, PassBytes: v.cfg.PassMaxBytes,
		Budget: v.budget, CatalogComplete: true,
		LastVerified: map[string]time.Time{}, LastRead: map[string]assurance.ReadMark{},
	}
	for name, at := range st.LastVerified {
		opt.LastVerified[name] = at
	}
	for name, mark := range st.LastRead {
		opt.LastRead[name] = mark
	}
	for _, f := range v.sched.openBookFindings(taskID) {
		kind := string(f.Kind)
		opt.OpenFindings = append(opt.OpenFindings, assurance.Finding{
			Kind: kind, Subject: verifierOpenSubject(kind, f.Subject),
			Severity: string(f.Severity), Detail: f.Detail,
		})
	}
	return opt
}

func (v *Verifier) apply(taskID string, rep assurance.Report) {
	for _, f := range rep.Findings {
		v.sched.Report(Finding{
			TaskID: taskID, Kind: FindingKind(f.Kind), Subject: verifierStoreSubject(f.Kind, f.Subject),
			Severity: FindingSeverity(f.Severity), Detail: f.Detail,
		})
	}
	for _, c := range rep.Closed {
		v.sched.ResolveWithReason(taskID, FindingKind(c.Finding.Kind), verifierStoreSubject(c.Finding.Kind, c.Finding.Subject), c.Reason)
	}
}

func (v *Verifier) remember(taskID string, rep assurance.Report) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := v.state.Tasks[taskID]
	if st.LastVerified == nil {
		st.LastVerified = map[string]time.Time{}
	}
	if st.LastRead == nil {
		st.LastRead = map[string]assurance.ReadMark{}
	}
	for name, at := range rep.VerifiedAt {
		st.LastVerified[name] = at
	}
	for name, mark := range rep.Read {
		st.LastRead[name] = mark
	}
	v.state.Tasks[taskID] = st
	v.dirty = true
}

func (v *Verifier) note(taskID string, now time.Time, rep assurance.Report) {
	v.mu.Lock()
	defer v.mu.Unlock()
	m := v.metrics[taskID]
	m.LocalBytes += rep.LocalBytes
	m.ObjectBytes += rep.ObjectBytes
	m.IncompleteLocal = coverageIncomplete(rep, true)
	m.IncompleteObject = coverageIncomplete(rep, false)
	m.IncompleteChain = false
	m.UnverifiableNoDigest = rep.UnverifiableNoDigest
	m.NoDigestLocal = rep.NoDigestLocal
	m.UnverifiableSize = rep.UnverifiableSize
	m.Deferred = rep.Deferred
	if verifyPassClean(rep) {
		m.LastVerifiedUnix = now.Unix()
		m.HaveLastVerified = true
	}
	v.metrics[taskID] = m
}

func verifyPassClean(rep assurance.Report) bool {
	if rep.Deferred != 0 {
		return false
	}
	for _, c := range rep.Coverage {
		if c.Local.Method == assurance.MethodError || c.Object.Method == assurance.MethodError {
			return false
		}
	}
	return true
}

func coverageIncomplete(rep assurance.Report, local bool) bool {
	for _, c := range rep.Coverage {
		method := c.Object.Method
		if local {
			method = c.Local.Method
		}
		switch method {
		case assurance.MethodError, assurance.MethodSkipped, "":
			return true
		}
	}
	return false
}

func verifyRowsFromCatalog(files []BinlogFile, markers map[string]time.Time) []assurance.VerifyRow {
	var rows []assurance.VerifyRow
	for _, f := range files {
		if CatalogRowOpen(f) {
			continue
		}
		name := segmentInventoryBasename(f)
		if name == "" {
			continue
		}
		_, marked := markers[RetentionMarkerKey(f.FileName, f.Epoch)]
		rows = append(rows, assurance.VerifyRow{
			Name: name, Sealed: true, Size: f.SizeBytes,
			StartPos: uint32(f.StartPos), EndPos: uint32(f.EndPos), SHA256: f.SHA256,
			UploadState: f.UploadState, ObjectKey: f.ObjectKey, UploadedAt: f.UploadedAt, SealedAt: f.SealedAt,
			RetentionRemoved: f.RetentionRemovedAt != nil || marked,
		})
	}
	return rows
}

func verifierStoreSubject(kind, subject string) string {
	switch kind {
	case assurance.KindObjectMissing, assurance.KindObjectSizeMismatch:
		if subject == "" {
			return ""
		}
		if _, ok := health.IsObjectSubject(subject); ok {
			return subject
		}
		return health.ObjectSubject(subject)
	default:
		return subject
	}
}

func verifierOpenSubject(kind, subject string) string {
	switch kind {
	case assurance.KindObjectMissing, assurance.KindObjectSizeMismatch:
		if seg, ok := health.IsObjectSubject(subject); ok {
			return seg
		}
	}
	return subject
}

func (s *Scheduler) countOpenBook() int {
	if s == nil || s.findings == nil {
		return 0
	}
	b := s.findings
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, f := range b.open {
		if f.Open() {
			n++
		}
	}
	return n
}

func (v *Verifier) openFindingCount() int {
	if v == nil || v.sched == nil {
		return 0
	}
	return v.sched.countOpenBook()
}

func (s *Scheduler) openBookFindings(taskID string) []Finding {
	if s == nil || s.findings == nil {
		return nil
	}
	b := s.findings
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Finding
	for _, f := range b.open {
		if f.TaskID == taskID && f.Open() {
			out = append(out, f)
		}
	}
	return out
}

func (v *Verifier) statePath() string {
	dir := strings.TrimSpace(v.cfg.DataDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, VerifyStateFileName)
}

func (v *Verifier) load() {
	path := v.statePath()
	if path == "" {
		return
	}
	if err := sweepVerifyTemps(filepath.Dir(path)); err != nil {
		// warned per file; a leftover temp must not hide saved progress
	}
	raw, err := binlog.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		zap.L().Warn(fmt.Sprintf("verifier: read %s: %v", path, err))
		return
	}
	var st verifyStateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		zap.L().Warn(fmt.Sprintf("verifier: %s is not progress (ignored; the next pass may re-read): %v", path, err))
		return
	}
	if st.Tasks == nil {
		st.Tasks = map[string]verifyTaskState{}
	}
	v.state = st
}

// pruneSegments drops progress for names the catalog no longer lists.
// A failed listing does not call this: a missing observation is not
// evidence the segment is gone.
func (v *Verifier) pruneSegments(taskID string, files []BinlogFile) {
	keep := map[string]struct{}{}
	for _, f := range files {
		if name := segmentInventoryBasename(f); name != "" {
			keep[name] = struct{}{}
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	st, ok := v.state.Tasks[taskID]
	if !ok {
		return
	}
	if dropProgress(st.LastVerified, keep) || dropReads(st.LastRead, keep) {
		v.dirty = true
		v.state.Tasks[taskID] = st
	}
}

// pruneAbsentTasks drops progress for tasks that are no longer in the scheduler.
func (v *Verifier) pruneAbsentTasks(live map[string]bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for id := range v.state.Tasks {
		if !live[id] {
			delete(v.state.Tasks, id)
			v.dirty = true
		}
	}
}

func dropProgress(m map[string]time.Time, keep map[string]struct{}) bool {
	dropped := false
	for name := range m {
		if _, ok := keep[name]; !ok {
			delete(m, name)
			dropped = true
		}
	}
	return dropped
}

func dropReads(m map[string]assurance.ReadMark, keep map[string]struct{}) bool {
	dropped := false
	for name := range m {
		if _, ok := keep[name]; !ok {
			delete(m, name)
			dropped = true
		}
	}
	return dropped
}

func (v *Verifier) saveIfDirty() {
	v.mu.Lock()
	dirty := v.dirty
	v.mu.Unlock()
	if dirty {
		v.save()
	}
}

func (v *Verifier) save() {
	path := v.statePath()
	if path == "" {
		return
	}
	v.mu.Lock()
	raw, err := verifyEncode(v.state, "", "  ")
	v.mu.Unlock()
	if err != nil {
		zap.L().Warn(fmt.Sprintf("verifier: encode progress: %v", err))
		return
	}
	if err := writeAtomic(path, raw); err != nil {
		zap.L().Warn("verifier: write progress", zap.String("path", path), zap.Error(err))
		return
	}
	v.mu.Lock()
	v.dirty = false
	v.mu.Unlock()
}

func writeAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), fmt.Sprintf(".verify-%d-*.tmp", os.Getpid()))
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := verifyFiles.write(tmp, raw); err != nil {
		return errors.Join(err, verifyFiles.close(tmp), verifyFiles.remove(tmpName))
	}
	if err := verifyFiles.sync(tmp); err != nil {
		return errors.Join(err, verifyFiles.close(tmp), verifyFiles.remove(tmpName))
	}
	if err := verifyFiles.close(tmp); err != nil {
		return errors.Join(err, verifyFiles.remove(tmpName))
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.Join(err, os.Remove(tmpName))
	}
	return nil
}

// sweepVerifyTemps removes crash leftovers in the data dir only.
// The name is exactly .verify-*.tmp; task directories are not walked.
// os.Remove unlinks a symlink and does not delete its target.
// A remove error is returned after the rest of the names are tried.
// load keeps reading progress when that error is set.
func sweepVerifyTemps(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	matches, err := binlog.Glob(filepath.Join(dir, ".verify-*.tmp"))
	if err != nil {
		return nil
	}
	var failed error
	for _, name := range matches {
		if err := verifyFiles.remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			zap.L().Warn("verifier: remove stale temp", zap.String("path", name), zap.Error(err))
			failed = err
			continue
		}
	}
	return failed
}

// VerifyAllows is the cluster gate the periodic verifier asks (ADR 0009
// phase 3 §10): only the lease holder runs a pass, because the local
// bytes live on the holder's disk (ADR 0011 per_node). A task this node
// does not hold is skipped — a skip opens and closes nothing.
func (s *Scheduler) VerifyAllows(_ context.Context, taskID string) (bool, error) {
	if s.clusterWorkerID == "" {
		return false, nil
	}
	s.mu.Lock()
	t, ok := s.tasks[taskID]
	s.mu.Unlock()
	return ok && t.OwnerWorkerID == s.clusterWorkerID && t.Epoch > 0, nil
}
