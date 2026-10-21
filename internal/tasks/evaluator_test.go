// Package tasks provides module-level functionality for tasks.
// input: task directories with duplicated, stale-open, empty-leftover and bit-rotted segments, health stores that fail, fence or block, and evaluator modes off/shadow/on
// output: the P2-3 QA plan as tests: a moved-away copy closes within one sweep with a CLOSE transition, .open.e4 ≡ .sealed.e5 opens SEGMENT_OVERLAP, a 4-byte .open.e3 only opens INFO SEGMENT_EMPTY_LEFTOVER and never cuts replay, an in-place byte flip with mtime kept opens SEGMENT_UNREADABLE within one rehash cycle, a read-only meta never closes legacy findings, shadow writes nothing, dirty marks merge with per-task serial evaluation and that run is joined before the temp directory is removed, 5000 open rows are all closed, ErrLeaseLagging keeps the pass and ErrFenced stops it, 1500 segments evaluate in under 50ms, the rehasher keeps to its byte budget over 100 tasks; two sweeps whose health write is ErrLockWait open META_LOCK_STALLED, and a successful write between two lock waits does not
// pos: ADR 0009 phase 2 P2-3 (docs/design/0009-phase2-health-evaluator.md §5, §5.7, §6)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

const evalNode = "node-test"

func evalTask(t *testing.T, s *Scheduler, data string) (Task, string) {
	t.Helper()
	task, err := s.CreateTaskFromSpec("mysql", "mysql-key-"+fmt.Sprint(time.Now().UnixNano()), &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-36"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return task, dir
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newEval(t *testing.T, s *Scheduler, store healthstore.Store, mode EvaluatorMode) *Evaluator {
	t.Helper()
	return NewEvaluator(s, EvaluatorConfig{Mode: mode, Store: store, NodeID: evalNode, Instance: "i1", Sweep: time.Minute, Debounce: time.Millisecond})
}

func openOf(t *testing.T, store healthstore.Store, taskID string) map[string]health.Finding {
	t.Helper()
	open, err := store.ListOpenFindingsForTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]health.Finding{}
	for _, f := range open {
		out[string(f.Kind)+"/"+f.Subject] = f
	}
	return out
}

func TestEvaluator_MovedCopyClosesWithinOneSweep(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002"), 39, 40)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000003"), 41, 42)
	copyFile(t, filepath.Join(dir, "mysql-bin.000002"), filepath.Join(dir, "mysql-bin.000002.sealed.e3"))
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store, EvaluatorOn)
	ctx := context.Background()
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := openOf(t, store, task.ID)["SEGMENT_OVERLAP/mysql-bin.000002.sealed.e3"]; !ok {
		t.Fatalf("overlap not open after a sweep: %+v", openOf(t, store, task.ID))
	}
	api, err := s.ListFindings(ctx, task.ID, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(api) != 1 || api[0].Kind != FindingSegmentOverlap {
		t.Fatalf("/findings shows %+v, want the evaluator's one SEGMENT_OVERLAP", api)
	}
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.sealed.e3")); err != nil {
		t.Fatal(err)
	}
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if open := openOf(t, store, task.ID); len(open) != 0 {
		t.Fatalf("moved-away copy still open after one sweep: %+v", open)
	}
	trs, err := store.ListTransitions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 2 || trs[0].Op != string(health.OpOpen) || trs[1].Op != string(health.OpClose) {
		t.Fatalf("transitions %+v, want OPEN then CLOSE", trs)
	}
	if api, _ := s.ListFindings(ctx, task.ID, false, 50); len(api) != 0 {
		t.Fatalf("/findings still shows %+v", api)
	}
	hs, _ := store.ListHealth(ctx)
	if len(hs) != 1 || hs[0].Status != health.StatusHealthy {
		t.Fatalf("health %+v", hs)
	}
}

func TestEvaluator_OpenEqualsSealedOpensOverlap(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002.sealed.e5"), 39, 40)
	copyFile(t, filepath.Join(dir, "mysql-bin.000002.sealed.e5"), filepath.Join(dir, "mysql-bin.000002.open.e4"))
	h := s.storageHealth(task, "")
	if h.Kind != StorageOverlap {
		t.Fatalf("verdict %+v, want overlap for .open.e4 ≡ .sealed.e5", h)
	}
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store, EvaluatorOn)
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	open := openOf(t, store, task.ID)
	if len(open) != 1 {
		t.Fatalf("open %+v, want one SEGMENT_OVERLAP", open)
	}
	for k := range open {
		if !strings.HasPrefix(k, "SEGMENT_OVERLAP/mysql-bin.000002.") {
			t.Fatalf("open %+v", open)
		}
	}
}

func TestEvaluator_EmptyOpenLeftoverIsInfoAndNeverCuts(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002"), 39, 40)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000003"), 41, 42)
	if err := os.WriteFile(filepath.Join(dir, "mysql-bin.000002.open.e3"), []byte{0xfe, 'b', 'i', 'n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if h := s.storageHealth(task, ""); h.Found() {
		t.Fatalf("a 4-byte open leftover changed the verdict: %+v", h)
	}
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store, EvaluatorOn)
	hv, err := e.EvaluateNow(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	open := openOf(t, store, task.ID)
	f, ok := open["SEGMENT_EMPTY_LEFTOVER/mysql-bin.000002.open.e3"]
	if len(open) != 1 || !ok || f.Severity != health.Info {
		t.Fatalf("open %+v, want only INFO SEGMENT_EMPTY_LEFTOVER", open)
	}
	if hv.Status != health.StatusHealthy {
		t.Fatalf("health %+v, INFO counts for nothing", hv)
	}
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.open.e3")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EvaluateNow(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if open := openOf(t, store, task.ID); len(open) != 0 {
		t.Fatalf("leftover removed but still open: %+v", open)
	}
}

// shaFileStore is a catalog whose rows carry the 000008 sha256.
type shaFileStore struct {
	mu   sync.Mutex
	rows map[string][]BinlogFile
}

func (f *shaFileStore) UpsertBinlogFile(ctx context.Context, meta BinlogFile) error { return nil }
func (f *shaFileStore) ListBinlogFiles(ctx context.Context, taskID string, limit int) ([]BinlogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BinlogFile(nil), f.rows[taskID]...), nil
}

func (f *shaFileStore) add(t *testing.T, taskID, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string][]BinlogFile{}
	}
	f.rows[taskID] = append(f.rows[taskID], BinlogFile{TaskID: taskID, FileName: filepath.Base(path), FilePath: path, State: "SEALED", SizeBytes: int64(len(b)), SHA256: hex.EncodeToString(sum[:])})
}

func TestRehasher_InPlaceByteFlipOpensUnreadable(t *testing.T) {
	data := t.TempDir()
	cat := &shaFileStore{}
	s := NewScheduler(WithDataDir(data), WithFileStore(cat))
	task, dir := evalTask(t, s, data)
	for i, seqs := range [][]int64{{37, 38}, {39, 40}, {41, 42}} {
		p := filepath.Join(dir, fmt.Sprintf("mysql-bin.%06d", i+1))
		writeAlertSegment(t, p, seqs...)
		cat.add(t, task.ID, p)
	}
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := newEval(t, s, store, EvaluatorOn)
	r := NewRehasher(e, 1<<20)
	ctx := context.Background()
	target := filepath.Join(dir, "mysql-bin.000002")
	info, _ := os.Stat(target)
	b, _ := os.ReadFile(target)
	orig := b[len(b)-10]
	b[len(b)-10] ^= 0xff // a byte inside the last event's payload: the ledger still reads it
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // one full cycle over three sealed segments
		if _, err := r.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	f, ok := openOf(t, store, task.ID)["SEGMENT_UNREADABLE/mysql-bin.000002"]
	if !ok || !strings.Contains(f.Evidence, "sha256") {
		t.Fatalf("bit rot not found within one rehash cycle: %+v", openOf(t, store, task.ID))
	}
	if st := r.Stats(); st.Mismatches != 1 || st.Segments < 3 {
		t.Fatalf("rehash stats %+v", st)
	}
	b[len(b)-10] = orig
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	if open := openOf(t, store, task.ID); len(open) != 0 {
		t.Fatalf("repaired segment still open: %+v", open)
	}
}

func TestRehasher_SkipsSegmentsWithoutSHA(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	_, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	e := newEval(t, s, healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), EvaluatorOn)
	r := NewRehasher(e, 1<<20)
	if _, err := r.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := r.Stats(); st.Unavailable != 1 || st.Mismatches != 0 || st.Bytes != 0 {
		t.Fatalf("stats %+v, want one unavailable and nothing read", st)
	}
}

// scriptedStore is a health store with hooks; it wraps a FileStore.
type scriptedStore struct {
	*healthstore.FileStore
	mu       sync.Mutex
	applyErr []error
	applied  [][]health.Transition
	seed     []health.Finding
	block    chan struct{}
	inFlight map[string]int
	maxPer   int
}

func (s *scriptedStore) ApplyTransitions(ctx context.Context, taskID string, f health.Fence, ts []health.Transition, h health.TaskHealth) (healthstore.Applied, error) {
	s.mu.Lock()
	if s.inFlight == nil {
		s.inFlight = map[string]int{}
	}
	s.inFlight[taskID]++
	if s.inFlight[taskID] > s.maxPer {
		s.maxPer = s.inFlight[taskID]
	}
	s.applied = append(s.applied, append([]health.Transition(nil), ts...))
	var err error
	if len(s.applyErr) > 0 {
		err, s.applyErr = s.applyErr[0], s.applyErr[1:]
	}
	block := s.block
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	defer func() { s.mu.Lock(); s.inFlight[taskID]--; s.mu.Unlock() }()
	if err != nil {
		return healthstore.Applied{}, err
	}
	return s.FileStore.ApplyTransitions(ctx, taskID, f, ts, h)
}

func (s *scriptedStore) ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error) {
	out, err := s.FileStore.ListOpenFindingsForTask(ctx, taskID)
	for _, f := range s.seed {
		if f.TaskID == taskID {
			out = append(out, f)
		}
	}
	return out, err
}

func TestEvaluator_ReadOnlyMetaNeverClosesLegacyFindings(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	var seed []health.Finding
	for i, k := range []health.Kind{health.DumpStalled, health.MetadataWriteBlocked, health.TaskPersistFailed, health.EventPersistFailed,
		health.TransitionStalled, health.UploadRecordFailed, health.SegmentOverlap, health.SegmentUnreadable} {
		seed = append(seed, health.Finding{TaskID: task.ID, Kind: k, Subject: fmt.Sprintf("legacy-%d", i), Severity: health.Critical, FirstSeen: at, LastSeen: at})
	}
	readOnly := errors.New("Error 1290: --read-only")
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), seed: seed,
		applyErr: []error{readOnly, readOnly, readOnly, readOnly}}
	e := newEval(t, s, store, EvaluatorOn)
	ctx := context.Background()
	for i := 0; i < 3; i++ { // 60s of read-only meta at a 20s sweep
		_ = e.SweepNow(ctx)
	}
	store.mu.Lock()
	applied := append([][]health.Transition(nil), store.applied...)
	store.mu.Unlock()
	for _, ts := range applied {
		for _, tr := range ts {
			spec := health.DefaultRegistry()[tr.Kind]
			if !spec.Managed {
				t.Fatalf("unmanaged legacy kind transitioned: %+v", tr)
			}
		}
	}
	if open := openOf(t, store.FileStore, task.ID); len(open) != 0 {
		t.Fatalf("nothing may be written while meta is read-only: %+v", open)
	}
	if got := e.Stats().Pending; got == 0 {
		t.Fatalf("failed writes must stay pending")
	}
	h, err := e.EvaluateNow(ctx, task.ID)
	if err == nil {
		t.Fatalf("health %+v with a failing store, want an error and UNKNOWN", h)
	}
	if h.Status != health.StatusUnknown {
		t.Fatalf("status %s, want UNKNOWN(store)", h.Status)
	}
}

func TestEvaluator_ShadowWritesNoFindingRows(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002"), 39, 40)
	copyFile(t, filepath.Join(dir, "mysql-bin.000002"), filepath.Join(dir, "mysql-bin.000002.sealed.e3"))
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))}
	e := newEval(t, s, store, EvaluatorShadow)
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.applied) != 0 {
		t.Fatalf("shadow wrote %+v", store.applied)
	}
	if st := e.Stats(); st.Shadowed == 0 {
		t.Fatalf("shadow computed nothing: %+v", st)
	}
	// The legacy path still owns the row in shadow mode.
	s.storageHealth(task, "")
	api, _ := s.ListFindings(context.Background(), task.ID, false, 10)
	if len(api) != 1 || api[0].Kind != FindingSegmentOverlap {
		t.Fatalf("legacy finding %+v", api)
	}
}

func TestEvaluator_OnModeStopsLegacyReport(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002"), 39, 40)
	copyFile(t, filepath.Join(dir, "mysql-bin.000002"), filepath.Join(dir, "mysql-bin.000002.sealed.e3"))
	newEval(t, s, healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), EvaluatorOn)
	if h := s.storageHealth(task, ""); h.Kind != StorageOverlap {
		t.Fatalf("verdict %+v", h)
	}
	if st := s.FindingStats(); len(st.Open) != 0 {
		t.Fatalf("the legacy Report wrote a managed kind with the evaluator on: %+v", st)
	}
}

func TestEvaluator_DirtyMergesAndTaskIsSerial(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	block := make(chan struct{})
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), block: block}
	e := NewEvaluator(s, EvaluatorConfig{Mode: EvaluatorOn, Store: store, NodeID: evalNode, Workers: 4, Sweep: time.Hour, Debounce: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.Run(ctx)
	}()
	// TempDir cleanup runs after this function returns. Cancel alone does not
	// wait, so a worker can create a file while the directory is being removed.
	defer func() {
		cancel()
		<-stopped
	}()
	e.MarkDirty(task.ID, ReasonSeal)
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		n := len(store.applied)
		store.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first evaluation never reached the store")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < 200; i++ { // marks during the pass only set the bit
		e.MarkDirty(task.ID, ReasonUpload)
	}
	close(block)
	time.Sleep(300 * time.Millisecond)
	store.mu.Lock()
	n, maxPer := len(store.applied), store.maxPer
	store.mu.Unlock()
	if maxPer != 1 {
		t.Fatalf("%d evaluations of one task ran at once", maxPer)
	}
	if n < 2 || n > 3 {
		t.Fatalf("%d passes for 1 + 200 marks, want them merged into one more", n)
	}
}

func TestEvaluator_ClosesAllOf5000OpenAcrossTasks(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	ctx := context.Background()
	fence := health.Fence{Mode: health.FenceStandalone, Node: evalNode, Instance: "old"}
	var ids []string
	for n := 0; n < 3; n++ {
		task, dir := evalTask(t, s, data)
		writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
		ids = append(ids, task.ID)
		var ts []health.Transition
		for i := 0; i < 5000/3+1; i++ {
			k := health.Key{TaskID: task.ID, Kind: health.SegmentUnreadable, Subject: fmt.Sprintf("gone-%05d", i)}
			ts = append(ts, health.Transition{Key: k, Op: health.OpOpen, Instance: health.Instance{Key: k, Severity: health.Critical}})
		}
		if _, err := store.ApplyTransitions(ctx, task.ID, fence, ts, health.TaskHealth{TaskID: task.ID, Status: health.StatusUnhealthy}); err != nil {
			t.Fatal(err)
		}
	}
	e := newEval(t, s, store, EvaluatorOn)
	if err := e.SweepNow(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if open := openOf(t, store, id); len(open) != 0 {
			t.Fatalf("task %s: %d of the stored open rows were not closed", id, len(open))
		}
	}
}

func TestEvaluator_LaggingRetriesFencedStops(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000002"), 39, 40)
	copyFile(t, filepath.Join(dir, "mysql-bin.000002"), filepath.Join(dir, "mysql-bin.000002.sealed.e3"))
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), applyErr: []error{healthstore.ErrLeaseLagging}}
	e := newEval(t, s, store, EvaluatorOn)
	ctx := context.Background()
	if _, err := e.EvaluateNow(ctx, task.ID); !errors.Is(err, healthstore.ErrLeaseLagging) {
		t.Fatalf("err %v", err)
	}
	if e.Stats().Pending == 0 {
		t.Fatal("lagging lease must keep the pass's transitions")
	}
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if len(openOf(t, store, task.ID)) != 1 || e.Stats().Pending != 0 {
		t.Fatalf("retry after renew did not apply: %+v", openOf(t, store, task.ID))
	}
	store.mu.Lock()
	store.applyErr = []error{healthstore.ErrFenced}
	store.mu.Unlock()
	if err := os.Remove(filepath.Join(dir, "mysql-bin.000002.sealed.e3")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EvaluateNow(ctx, task.ID); !errors.Is(err, healthstore.ErrFenced) {
		t.Fatalf("err %v", err)
	}
	if e.Stats().Pending != 0 {
		t.Fatal("a fenced pass is dropped, not kept")
	}
}

func TestEvaluator_OffDoesNothing(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	writeAlertSegment(t, filepath.Join(dir, "mysql-bin.000001"), 37, 38)
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))}
	e := newEval(t, s, store, EvaluatorOff)
	e.MarkDirty(task.ID, ReasonSeal)
	if err := e.SweepNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.applied) != 0 || e.Stats().Evaluations != 0 {
		t.Fatalf("off evaluated: %+v", e.Stats())
	}
}

func TestParseEvaluatorMode(t *testing.T) {
	for in, want := range map[string]EvaluatorMode{"": EvaluatorOff, "off": EvaluatorOff, "shadow": EvaluatorShadow, "ON": EvaluatorOn} {
		if got, err := ParseEvaluatorMode(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseEvaluatorMode("maybe"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestNodeIDPersistsInDataDir(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateNodeID(dir)
	if err != nil || len(a) < 16 {
		t.Fatalf("%q %v", a, err)
	}
	b, err := LoadOrCreateNodeID(dir)
	if err != nil || a != b {
		t.Fatalf("node id changed across restarts: %q %q %v", a, b, err)
	}
}

func TestEvaluator_1500SegmentsUnder50ms(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark-style QA condition")
	}
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	for i := 0; i < 1500; i++ {
		writeAlertSegment(t, filepath.Join(dir, fmt.Sprintf("mysql-bin.%06d", i+1)), int64(37+i))
	}
	e := newEval(t, s, healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), EvaluatorShadow)
	ctx := context.Background()
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil { // warms the ledger's size+mtime cache
		t.Fatal(err)
	}
	best := time.Hour
	for i := 0; i < 5; i++ {
		start := time.Now()
		if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	t.Logf("best pass over 1500 segments: %s", best)
	if best > 50*time.Millisecond {
		t.Fatalf("one pass over 1500 segments took %s, want < 50ms", best)
	}
}

func TestRehasher_KeepsToByteBudgetOver100Tasks(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	// The budget is what must bind, not the per-segment overhead (listing
	// 100 tasks, stat, open): ~1 KiB segments keep the rehasher at ~32
	// segments/s, far below what a loaded CI runner manages, where 180 B
	// segments (~180/s) left it CPU-bound under the floor. The 4s window
	// still cycles every task and halves the start-up burst's share.
	data := t.TempDir()
	cat := &shaFileStore{}
	s := NewScheduler(WithDataDir(data), WithFileStore(cat))
	seqs := make([]int64, 23) // 4 B magic + 23 x 44 B GTID events = 1016 B
	for i := range seqs {
		seqs[i] = int64(37 + i)
	}
	for i := 0; i < 100; i++ {
		task, dir := evalTask(t, s, data)
		p := filepath.Join(dir, "mysql-bin.000001")
		writeAlertSegment(t, p, seqs...)
		cat.add(t, task.ID, p)
	}
	e := newEval(t, s, healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), EvaluatorOn)
	const rate = 32 << 10
	r := NewRehasher(e, rate)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	var done atomic.Bool
	go func() { r.Run(ctx); done.Store(true) }()
	<-ctx.Done()
	for !done.Load() {
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(start).Seconds()
	st := r.Stats()
	got := float64(st.Bytes) / elapsed
	t.Logf("rehash read %d B in %d segments over %.3fs: %.0f B/s", st.Bytes, st.Segments, elapsed, got)
	if got > rate*1.1 || got < rate*0.9 {
		t.Fatalf("rehash read %.0f B/s over 100 tasks (%d segments), want %d ±10%%", got, st.Segments, rate)
	}
}

func TestEvaluatorLockWaitOpensOnTheSecondSweep(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, _ := evalTask(t, s, data)
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)),
		applyErr: []error{healthstore.ErrLockWait, healthstore.ErrLockWait}}
	e := newEval(t, s, store, EvaluatorOn)
	ctx := context.Background()
	_ = e.SweepNow(ctx)
	if strings.Contains(strings.Join(s.HealthSummary(ctx).Reasons, " "), "META_LOCK_STALLED") {
		t.Fatal("the first lock wait opened META_LOCK_STALLED")
	}
	_ = e.SweepNow(ctx)
	sum := s.HealthSummary(ctx)
	if sum.Status != HealthDegraded {
		t.Fatalf("status %s, want degraded", sum.Status)
	}
	if !strings.Contains(strings.Join(sum.Reasons, " "), "META_LOCK_STALLED/"+task.ID) && !strings.Contains(strings.Join(sum.Reasons, " "), "META_LOCK_STALLED") {
		t.Fatalf("reasons %v, want META_LOCK_STALLED for %s", sum.Reasons, task.ID)
	}
}

func TestEvaluatorSuccessfulWriteResetsLockCount(t *testing.T) {
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	_, _ = evalTask(t, s, data)
	store := &scriptedStore{FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)),
		applyErr: []error{healthstore.ErrLockWait}}
	e := newEval(t, s, store, EvaluatorOn)
	ctx := context.Background()
	_ = e.SweepNow(ctx)
	_ = e.SweepNow(ctx)
	store.mu.Lock()
	store.applyErr = []error{healthstore.ErrLockWait}
	store.mu.Unlock()
	_ = e.SweepNow(ctx)
	if strings.Contains(strings.Join(s.HealthSummary(ctx).Reasons, " "), "META_LOCK_STALLED") {
		t.Fatal("a successful write left the lock-wait count set")
	}
}
