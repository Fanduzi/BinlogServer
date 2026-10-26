// Package tasks provides module-level functionality for tasks.
// input: task directories with duplicated, stale-open, empty-leftover and bit-rotted segments, health stores that fail, fence or block, and evaluator modes off/shadow/on
// output: the P2-3 QA plan as tests: a moved-away copy closes within one sweep with a CLOSE transition, .open.e4 ≡ .sealed.e5 opens SEGMENT_OVERLAP, a 4-byte .open.e3 only opens INFO SEGMENT_EMPTY_LEFTOVER and never cuts replay, an in-place byte flip with mtime kept opens SEGMENT_UNREADABLE within one rehash cycle, a read-only meta never closes legacy findings, shadow writes nothing, dirty marks merge with per-task serial evaluation on a test clock (four workers, the merged pass is applied at exactly one debounce, a second worker cannot enter the same task, and a mark after idle leaves the evaluator not idle) and that run is joined before the temp directory is removed, 5000 open rows are all closed, ErrLeaseLagging keeps the pass and ErrFenced stops it, a cold pass over 150 and 1500 segments does a constant number of filesystem operations per segment and ten times the segments do ten times the operations, with a 100ms ceiling on the 150-segment pass and a 2s ceiling on the 1500-segment pass, the rehasher keeps to its byte budget over 100 tasks; two sweeps whose health write is ErrLockWait open META_LOCK_STALLED, and a successful write between two lock waits does not
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

	"binlog_server/internal/binlog"
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
	mu        sync.Mutex
	applyErr  []error
	applied   [][]health.Transition
	seed      []health.Finding
	block     chan struct{}
	blockFor  func(taskID string) <-chan struct{}
	inFlight  map[string]int
	maxPer    int
	appliedID []string
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
	s.appliedID = append(s.appliedID, taskID)
	var err error
	if len(s.applyErr) > 0 {
		err, s.applyErr = s.applyErr[0], s.applyErr[1:]
	}
	var block <-chan struct{}
	block = s.block
	if s.blockFor != nil {
		block = s.blockFor(taskID)
	}
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
	releaseMain := make(chan struct{})
	releaseSoak := make(chan struct{})
	var mainOnce, soakOnce sync.Once
	closeMain := func() { mainOnce.Do(func() { close(releaseMain) }) }
	closeSoak := func() { soakOnce.Do(func() { close(releaseSoak) }) }
	var soakMu sync.Mutex
	soaks := map[string]bool{}
	store := &scriptedStore{
		FileStore: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)),
		blockFor: func(id string) <-chan struct{} {
			if id == task.ID {
				return releaseMain
			}
			soakMu.Lock()
			ok := soaks[id]
			soakMu.Unlock()
			if ok {
				return releaseSoak
			}
			return nil
		},
	}
	clock := newManualClock(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	const debounce = time.Second
	e := NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorOn, Store: store, NodeID: evalNode, Workers: 4,
		Sweep: time.Hour, LivenessTick: time.Hour, Debounce: debounce,
		Now: clock.Now, After: clock.After,
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.Run(ctx)
	}()
	// TempDir cleanup runs after this function returns. Cancel alone does not
	// wait, so a worker can create a file while the directory is being removed.
	defer func() {
		closeMain()
		closeSoak()
		cancel()
		clock.Advance(time.Hour)
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("evaluator did not stop")
		}
	}()
	waitEvalApplies(t, e, store, 1, "first evaluation never reached the store")
	for i := 0; i < 200; i++ { // marks during the pass only set the bit
		e.MarkDirty(task.ID, ReasonUpload)
	}
	// Four workers, and the other three are free. Dropping the per-task
	// running bit lets a second evaluation of this task overlap the first.
	serialDeadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(serialDeadline) {
		if storeMaxPer(store) > 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := storeMaxPer(store); got != 1 {
		t.Fatalf("%d evaluations of one task ran at once", got)
	}
	// Hold the other three workers inside their own passes. A finish that
	// then sleeps cannot hand the merged pass to someone else.
	for i := 0; i < 3; i++ {
		soak, soakDir := evalTask(t, s, data)
		writeAlertSegment(t, filepath.Join(soakDir, "mysql-bin.000001"), 37, 38)
		soakMu.Lock()
		soaks[soak.ID] = true
		soakMu.Unlock()
		before := storeAppliesTask(store, soak.ID)
		e.MarkDirty(soak.ID, ReasonUpload)
		// A free worker may be parked on the test clock. Advancing its one
		// second cap wakes it. The main task is still running, so this does
		// not release the merged pass.
		deadline := time.Now().Add(3 * time.Second)
		for storeAppliesTask(store, soak.ID) == before {
			if time.Now().After(deadline) {
				t.Fatalf("soak worker did not enter (applied %d state %s)", storeApplies(store), evalState(e))
			}
			clock.Advance(time.Second)
			time.Sleep(time.Millisecond)
		}
	}
	if got := storeMaxPer(store); got != 1 {
		t.Fatalf("%d evaluations of one task ran at once", got)
	}
	// The merged pass is applied at exactly one debounce on the test clock.
	// The wall budget from releasing the first pass is 500ms, so a finish
	// that sleeps two seconds does not arrive.
	base := clock.Now()
	started := time.Now()
	closeMain()
	var n int
	advanced := false
	for {
		n = storeAppliesTask(store, task.ID)
		if n >= 2 {
			break
		}
		if time.Since(started) > 500*time.Millisecond {
			t.Fatalf("merged pass late: applied %d idle %v clock %s", n, evalIdle(e), clock.Now().Sub(base))
		}
		if !advanced && n == 1 && evalLastRun(e, task.ID).Equal(base) && !evalRunning(e, task.ID) {
			time.Sleep(20 * time.Millisecond)
			if got := storeAppliesTask(store, task.ID); got != 1 {
				t.Fatalf("merged pass ran before the debounce elapsed on the test clock (applied %d)", got)
			}
			clock.Advance(debounce)
			advanced = true
		}
		time.Sleep(time.Millisecond)
	}
	if !advanced {
		t.Fatal("merged pass ran before the debounce elapsed on the test clock")
	}
	if got := clock.Now().Sub(base); got != debounce {
		t.Fatalf("test clock moved %s for the merged pass, want one debounce (%s)", got, debounce)
	}
	if n != 2 {
		t.Fatalf("%d passes for the startup pass plus 200 marks, want exactly 2", n)
	}
	closeSoak()
	// A pass can be marked again while it is in the store (the scheduler's
	// upload bookkeeping). One debounce on the test clock lets that pass
	// run. It must then be idle. A mark that keeps coming back does not.
	idleDeadline := time.Now().Add(2 * time.Second)
	drained := false
	for {
		if evalIdle(e) && clock.WaiterCount() > 0 {
			break
		}
		if !drained && !evalRunningAny(e) {
			clock.Advance(debounce)
			drained = true
		}
		if time.Now().After(idleDeadline) {
			t.Fatalf("evaluator did not go idle after the soak passes (idle %v waiters %d applied %d state %s)", evalIdle(e), clock.WaiterCount(), storeApplies(store), evalState(e))
		}
		time.Sleep(time.Millisecond)
	}
	// A window, not one instant: a pass 30ms later on this clock or on the
	// wall must be visible. Steps land on 30ms, 100ms and 2s. The wall
	// window also requires the evaluator to be idle, so a mark that has not
	// been applied is a failure even when the apply count has not moved.
	quiet := storeApplies(store)
	startClock := clock.Now()
	startWall := time.Now()
	for _, step := range []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 70 * time.Millisecond, 1900 * time.Millisecond} {
		clock.Advance(step)
		deadline := time.Now().Add(200 * time.Millisecond)
		for {
			if got := storeApplies(store); got != quiet {
				t.Fatalf("extra pass at clock +%s wall +%s: applied %d, want %d", clock.Now().Sub(startClock), time.Since(startWall), got, quiet)
			}
			if evalIdle(e) && clock.WaiterCount() > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("after idle, clock +%s left the evaluator busy (applied %d idle %v)", clock.Now().Sub(startClock), storeApplies(store), evalIdle(e))
			}
			time.Sleep(time.Millisecond)
		}
	}
	wall := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(wall) {
		if !evalIdle(e) {
			t.Fatalf("not idle %s after the quiet window: applied %d", time.Since(startWall), storeApplies(store))
		}
		if got := storeApplies(store); got != quiet {
			t.Fatalf("extra pass %s after idle: applied %d, want %d", time.Since(startWall), got, quiet)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// evalIdle reports that no pass is running and no mark is waiting.
func evalIdle(e *Evaluator) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.dirty) == 0 && len(e.running) == 0
}

func waitEvalApplies(t *testing.T, e *Evaluator, store *scriptedStore, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if storeApplies(store) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (applied %d)", what, storeApplies(store))
		}
		time.Sleep(time.Millisecond)
	}
}

func storeApplies(store *scriptedStore) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.applied)
}

func storeAppliesTask(store *scriptedStore, taskID string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	n := 0
	for _, id := range store.appliedID {
		if id == taskID {
			n++
		}
	}
	return n
}

func storeMaxPer(store *scriptedStore) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.maxPer
}

func evalRunning(e *Evaluator, id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id]
}

func evalRunningAny(e *Evaluator) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.running) > 0
}

func evalState(e *Evaluator) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var dirty, running []string
	for id, reasons := range e.dirty {
		for r := range reasons {
			dirty = append(dirty, id+":"+string(r))
		}
	}
	for id := range e.running {
		running = append(running, id)
	}
	return "dirty=" + strings.Join(dirty, ",") + " running=" + strings.Join(running, ",")
}

func evalLastRun(e *Evaluator, id string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRun[id]
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

// TestEvaluator_1500SegmentsNearLinear checks that the cold pass which walks
// the segments does a constant amount of filesystem work per segment.
// Stat, Open, ReadDir and StampOf share one counter, so a nested walk that
// skips the call in Segments still counts. Ten times the segments do about
// ten times the operations. The only wall-clock guard is the 1500-segment
// pass under 2s. The chain cache is not warmed first, so the measured pass
// is the walk. A 150-segment pass also stays under 100ms. That is an
// absolute ceiling, not a ratio: under eight CPU hogs the pass stays
// under 80ms. An extra 0.7ms inside the per-segment walk is charged on
// every body check, so the 150-segment pass lands well above 100ms.
func TestEvaluator_1500SegmentsNearLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark-style QA condition")
	}
	const (
		smallN       = 150
		largeN       = 1500
		maxPerSeg    = 20
		walkOverhead = 4 // directory stats and listings, once per pass
		smallCeiling = 100 * time.Millisecond
		largeCeiling = 2 * time.Second
	)
	ctx := context.Background()
	dSmall, opsSmall := evalPassTime(t, ctx, smallN)
	dLarge, opsLarge := evalPassTime(t, ctx, largeN)
	t.Logf("%d segments %s ops %d; %d segments %s ops %d", smallN, dSmall, opsSmall, largeN, dLarge, opsLarge)
	checkOps := func(n int, ops int64) {
		t.Helper()
		if ops < int64(n) {
			t.Fatalf("%d-segment pass did %d filesystem ops, want at least one per segment", n, ops)
		}
		if ops > int64(n)*maxPerSeg+walkOverhead {
			t.Fatalf("%d-segment pass did %d filesystem ops, want <= %d per segment plus %d", n, ops, maxPerSeg, walkOverhead)
		}
	}
	checkOps(smallN, opsSmall)
	checkOps(largeN, opsLarge)
	perSmall := opsSmall - walkOverhead
	perLarge := opsLarge - walkOverhead
	if perSmall <= 0 || perLarge != perSmall*10 {
		t.Fatalf("%d segments did %d ops, %d did %d; want 10x the per-segment work", largeN, opsLarge, smallN, opsSmall)
	}
	if dSmall > smallCeiling {
		t.Fatalf("%d-segment pass took %s, ceiling %s", smallN, dSmall, smallCeiling)
	}
	if dLarge > largeCeiling {
		t.Fatalf("%d-segment pass took %s, ceiling %s", largeN, dLarge, largeCeiling)
	}
}

func evalPassTime(t *testing.T, ctx context.Context, n int) (time.Duration, int64) {
	t.Helper()
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data))
	task, dir := evalTask(t, s, data)
	for i := 0; i < n; i++ {
		writeAlertSegment(t, filepath.Join(dir, fmt.Sprintf("mysql-bin.%06d", i+1)), int64(37+i))
	}
	e := newEval(t, s, healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)), EvaluatorShadow)
	binlog.ResetWalkOps()
	start := time.Now()
	if _, err := e.EvaluateNow(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	return time.Since(start), binlog.WalkOps()
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
