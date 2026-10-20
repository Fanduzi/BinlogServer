// Package tasks provides module-level functionality for tasks.
// input: the in-memory task list, a complete catalog listing, the task directory under data_dir, an object store that can HEAD and GET, and the open findings (severity and detail)
// output: one assurance.Verify pass per allowed task, findings written through the existing sink (and the standalone file store when one is set), Report.VerifiedAt and Report.Read stored for the next pass, and the verify gauges
// pos: ADR 0009 phase 3 worker (docs/design/0009-phase3-verifier.md §10). Cluster mode does not run it; Allows stays with ADR 0011. This file does not import internal/ownership
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/health"
)

// VerifyStateFileName is the per-process verify progress file under data_dir.
// It starts with a dot so the on-disk backup listing never treats it as a task.
// Losing it only causes an extra read (design §15).
const VerifyStateFileName = ".verify.json"

// ObjectHeadOpener is the object client the verifier can HEAD and GET.
// A missing object is os.ErrNotExist. The upload client is not required to
// implement it; a client that cannot HEAD is left unused.
type ObjectHeadOpener interface {
	HeadObject(ctx context.Context, key string) (int64, error)
	OpenObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
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
	state   verifyStateFile
	metrics map[string]TaskVerifyMetrics
}

type verifyStateFile struct {
	Tasks map[string]verifyTaskState `json:"tasks"`
}

type verifyTaskState struct {
	LastVerified map[string]time.Time          `json:"last_verified,omitempty"`
	LastRead     map[string]assurance.ReadMark `json:"last_read,omitempty"`
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
	if now.IsZero() {
		now = time.Now()
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
	if len(tasks) == 0 {
		return
	}
	v.mu.Lock()
	start := v.start % len(tasks)
	v.start++
	v.mu.Unlock()
	ordered := append(append([]Task(nil), tasks[start:]...), tasks[:start]...)
	for _, task := range ordered {
		if ctx.Err() != nil {
			return
		}
		v.passTask(ctx, task.ID, now)
	}
	v.save()
}

func (v *Verifier) passTask(ctx context.Context, taskID string, now time.Time) {
	if v.cfg.Allows != nil {
		ok, err := v.cfg.Allows(ctx, taskID)
		if err != nil || !ok {
			return
		}
	}
	files, err := v.sched.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		log.Printf("verifier: list catalog task=%s: %v", taskID, err)
		return
	}
	markers, err := v.sched.retentionMarkers(taskID)
	if err != nil {
		log.Printf("verifier: retention markers task=%s: %v", taskID, err)
		return
	}
	dir, _ := taskBinlogDir(v.cfg.DataDir, taskID)
	rows := verifyRowsFromCatalog(files, markers)
	opt := v.options(taskID, now)
	rep, err := assurance.Verify(ctx, dir, rows, v.cfg.Objects, nil, opt)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("verifier: task=%s: %v", taskID, err)
		return
	}
	v.apply(taskID, rep)
	v.remember(taskID, rep)
	if !errors.Is(err, context.Canceled) {
		v.note(taskID, now, rep)
	}
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
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("verifier: read %s: %v", path, err)
		return
	}
	var st verifyStateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Printf("verifier: %s is not progress (ignored; the next pass may re-read): %v", path, err)
		return
	}
	if st.Tasks == nil {
		st.Tasks = map[string]verifyTaskState{}
	}
	v.state = st
}

func (v *Verifier) save() {
	path := v.statePath()
	if path == "" {
		return
	}
	v.mu.Lock()
	raw, err := json.MarshalIndent(v.state, "", "  ")
	v.mu.Unlock()
	if err != nil {
		log.Printf("verifier: encode progress: %v", err)
		return
	}
	if err := writeAtomic(path, raw); err != nil {
		log.Printf("verifier: write %s: %v", path, err)
	}
}

func writeAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".verify-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmpName))
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmpName))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmpName))
	}
	return os.Rename(tmpName, path)
}
