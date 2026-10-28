// Package tasks provides module-level functionality for tasks.
// input: a health store that returns ErrSchemaAbsent, a task store whose seal-fact probe reports a schema version, evaluator modes on/shadow/off, and a legacy finding store for the cluster close
// output: proof that an unusable health store degrades /api/health from memory with HEALTH_STORE_UNAVAILABLE, logs the open once, and persists the close when the store returns (design 0009 §27)
// pos: evaluator tests for #436
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const healthStoreKind = FindingKind("HEALTH_STORE_UNAVAILABLE")

// absentHealth is a health store that is not a file store (the MySQL shape):
// while down, every read and write is ErrSchemaAbsent.
type absentHealth struct {
	mu   sync.Mutex
	down atomic.Bool
	open map[string][]health.Finding
	trs  []healthstore.Transition
}

func newAbsentHealth() *absentHealth {
	return &absentHealth{open: map[string][]health.Finding{}}
}

func (s *absentHealth) absent() error {
	if s.down.Load() {
		return fmt.Errorf("schema: %w", healthstore.ErrSchemaAbsent)
	}
	return nil
}

func (s *absentHealth) ListOpenFindingsForTask(_ context.Context, taskID string) ([]health.Finding, error) {
	if err := s.absent(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]health.Finding(nil), s.open[taskID]...), nil
}

func (s *absentHealth) ListHealth(context.Context) ([]health.TaskHealth, error) {
	if err := s.absent(); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *absentHealth) ApplyTransitions(_ context.Context, taskID string, _ health.Fence, ts []health.Transition, _ health.TaskHealth) (healthstore.Applied, error) {
	if err := s.absent(); err != nil {
		return healthstore.Applied{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	open := map[health.Key]health.Finding{}
	for _, f := range s.open[taskID] {
		open[f.Key()] = f
	}
	for _, tr := range ts {
		s.trs = append(s.trs, healthstore.Transition{TaskID: taskID, Kind: string(tr.Kind), Subject: tr.Subject, Op: string(tr.Op), FactAt: tr.FactAt})
		k := health.Key{TaskID: taskID, Kind: tr.Kind, Subject: tr.Subject}
		switch tr.Op {
		case health.OpClose:
			delete(open, k)
		default:
			open[k] = health.Finding{TaskID: taskID, Kind: tr.Kind, Subject: tr.Subject, Severity: tr.Instance.Severity, Detail: tr.Instance.Detail, FirstSeen: tr.FactAt, LastSeen: tr.At}
		}
	}
	next := make([]health.Finding, 0, len(open))
	for _, f := range open {
		next = append(next, f)
	}
	s.open[taskID] = next
	return healthstore.Applied{}, nil
}

func (s *absentHealth) transitions(kind string) []healthstore.Transition {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []healthstore.Transition
	for _, tr := range s.trs {
		if tr.Kind == kind {
			out = append(out, tr)
		}
	}
	return out
}

func schemaStore(ver int, columnsAbsent bool) *p254Store {
	return &p254Store{ver: ver, absent: columnsAbsent}
}

func healthEval(t *testing.T, s *Scheduler, store healthstore.Store, now func() time.Time) *Evaluator {
	t.Helper()
	return NewEvaluator(s, EvaluatorConfig{Store: store, NodeID: evalNode, Instance: "i1", Sweep: 15 * time.Millisecond, Debounce: time.Millisecond, Now: now})
}

func joinedReasons(h HealthSummary) string { return strings.Join(h.Reasons, "\n") }

func hasFinding(fs []Finding, kind FindingKind, subject string) bool {
	for _, f := range fs {
		if f.TaskID == ClusterFindingTask && f.Kind == kind && f.Subject == subject && f.Severity == FindingCritical && f.Open() {
			return true
		}
	}
	return false
}

func warnCount(obs *observer.ObservedLogs, sub string) int {
	n := 0
	for _, e := range obs.All() {
		if e.Level >= zapcore.WarnLevel && strings.Contains(e.Message, sub) {
			n++
		}
	}
	return n
}

func installZap(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, obs := observer.New(zapcore.WarnLevel)
	prev := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(prev) })
	return obs
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestHealthStoreUnavailableDegradesFromMemory(t *testing.T) {
	data := t.TempDir()
	st := schemaStore(8, false)
	s := NewScheduler(WithDataDir(data), WithStore(st))
	if _, err := s.CreateTaskFromSpec("mysql", "k", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"}, &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	hs := newAbsentHealth()
	hs.down.Store(true)
	e := healthEval(t, s, hs, time.Now)
	if err := e.SweepNow(context.Background()); err != nil && !strings.Contains(err.Error(), "health tables are absent") {
		t.Fatal(err)
	}
	got := s.HealthSummary(context.Background())
	if got.Status != HealthDegraded || !strings.Contains(joinedReasons(got), "HEALTH_STORE_UNAVAILABLE: schema 8 < 9, run ./migrate up") {
		t.Fatalf("summary %+v, want degraded naming schema 8 < 9", got)
	}
	if !strings.Contains(joinedReasons(got), "HEALTH_STORE_UNAVAILABLE/"+evalNode+" (task _cluster)") {
		t.Fatalf("summary %+v, want the in-memory finding line", got)
	}
	open, err := s.OpenFindings(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(open, healthStoreKind, evalNode) {
		t.Fatalf("OpenFindings %+v, want CRITICAL HEALTH_STORE_UNAVAILABLE/%s", open, evalNode)
	}
	listed, err := s.ListFindings(context.Background(), ClusterFindingTask, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(listed, healthStoreKind, evalNode) {
		t.Fatalf("ListFindings(_cluster) %+v, want the same finding", listed)
	}
	for _, f := range open {
		if f.Kind == FindingKind(health.SealFactsAbsent) {
			t.Fatalf("schema 8 still has seal-fact columns; opened %+v", f)
		}
	}
}

func TestHealthStoreUnavailableBeforeFirstSweep(t *testing.T) {
	st := schemaStore(8, false)
	s := NewScheduler(WithDataDir(t.TempDir()), WithStore(st))
	hs := newAbsentHealth()
	hs.down.Store(true)
	e := healthEval(t, s, hs, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	deadline := time.Now().Add(time.Second)
	var got HealthSummary
	for time.Now().Before(deadline) {
		got = s.HealthSummary(context.Background())
		if got.Status == HealthDegraded && strings.Contains(joinedReasons(got), "HEALTH_STORE_UNAVAILABLE: schema 8 < 9, run ./migrate up") {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("process started on schema 8 with the evaluator on stayed %+v before any task sweep", got)
}

func TestHealthStoreUnavailableSchema7SkipsSealFacts(t *testing.T) {
	data := t.TempDir()
	st := schemaStore(7, true) // columns absent and this process has seen them (goto 7)
	s := NewScheduler(WithDataDir(data), WithStore(st))
	task, err := s.CreateTaskFromSpec("mysql", "k", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"}, &StartConfig{Mode: StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data+"/"+task.ID, 0o755); err != nil {
		t.Fatal(err)
	}
	hs := newAbsentHealth()
	hs.down.Store(true)
	e := healthEval(t, s, hs, time.Now)
	_ = e.SweepNow(context.Background())
	got := s.HealthSummary(context.Background())
	if got.Status != HealthDegraded || !strings.Contains(joinedReasons(got), "HEALTH_STORE_UNAVAILABLE: schema 7 < 9, run ./migrate up") {
		t.Fatalf("summary %+v, want schema 7 < 9", got)
	}
	open, err := s.OpenFindings(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range open {
		if f.Kind == FindingKind(health.SealFactsAbsent) {
			t.Fatalf("000009 absent must not also open an in-memory SEAL_FACTS_ABSENT: %+v", f)
		}
	}
	if !hasFinding(open, healthStoreKind, evalNode) {
		t.Fatalf("open %+v, want only HEALTH_STORE_UNAVAILABLE", open)
	}
}

func TestHealthStoreUnavailableTablesDroppedNamesTheMigration(t *testing.T) {
	st := schemaStore(10, false)
	s := NewScheduler(WithDataDir(t.TempDir()), WithStore(st))
	hs := newAbsentHealth()
	hs.down.Store(true)
	e := healthEval(t, s, hs, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	deadline := time.Now().Add(time.Second)
	var got HealthSummary
	for time.Now().Before(deadline) {
		got = s.HealthSummary(context.Background())
		if strings.Contains(joinedReasons(got), "HEALTH_STORE_UNAVAILABLE: health tables are absent; run ./migrate up to 000009") {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("schema 10 with the tables gone: %+v", got)
}

func TestHealthStoreUnavailableLoggedOnce(t *testing.T) {
	obs := installZap(t)
	buf := captureLog(t)
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data), WithStore(schemaStore(8, false)))
	if _, err := s.CreateTaskFromSpec("mysql", "k", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"}, &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	hs := newAbsentHealth()
	hs.down.Store(true)
	e := healthEval(t, s, hs, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	time.Sleep(120 * time.Millisecond)
	cancel()
	nInfo := strings.Count(buf.String(), "health tables are absent")
	nWarn := warnCount(obs, "HEALTH_STORE_UNAVAILABLE opened")
	if nInfo != 0 || nWarn != 1 {
		t.Fatalf("absent-store info lines=%d, opened warns=%d; want 0 and 1\nlog:\n%s", nInfo, nWarn, buf.String())
	}
}

func TestHealthStoreUnavailablePersistsOpenAndClose(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	data := t.TempDir()
	s := NewScheduler(WithDataDir(data), WithStore(schemaStore(8, false)))
	if _, err := s.CreateTaskFromSpec("mysql", "k", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "u", Password: "p", Flavor: "mysql"}, &StartConfig{Mode: StartModeLatest}, nil); err != nil {
		t.Fatal(err)
	}
	hs := newAbsentHealth()
	hs.down.Store(true)
	e := healthEval(t, s, hs, clock)
	start := clock()
	_ = e.SweepNow(context.Background())
	advance(30 * time.Second)
	recovered := clock()
	hs.down.Store(false)
	_ = e.SweepNow(context.Background())
	for i := 0; i < 3; i++ {
		advance(time.Second)
		_ = e.SweepNow(context.Background())
	}
	if got := s.HealthSummary(context.Background()); got.Status != HealthOK {
		t.Fatalf("after the store returned: %+v", got)
	}
	var ops []string
	var openAt, closeAt time.Time
	for _, tr := range hs.transitions("HEALTH_STORE_UNAVAILABLE") {
		if tr.Subject != evalNode {
			continue
		}
		ops = append(ops, tr.Op)
		switch tr.Op {
		case string(health.OpOpen):
			openAt = tr.FactAt
		case string(health.OpClose):
			closeAt = tr.FactAt
		}
	}
	if strings.Join(ops, ",") != "OPEN,CLOSE" {
		t.Fatalf("transitions %v, want OPEN,CLOSE", ops)
	}
	if !openAt.Equal(start) || !closeAt.Equal(recovered) {
		t.Fatalf("fact_at open=%s close=%s, want %s and %s", openAt, closeAt, start, recovered)
	}
}

func TestHealthStoreUnavailableClusterPersistsResolvedRow(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	findings := newMemFindingStore()
	events := newFakeEventStore()
	s := NewScheduler(WithDataDir(t.TempDir()), WithStore(schemaStore(8, false)), WithFindingStore(findings), WithEventStore(events))
	hs := newAbsentHealth()
	hs.down.Store(true)
	e := NewEvaluator(s, EvaluatorConfig{Cluster: true, Store: hs, NodeID: evalNode, Instance: "i1", Sweep: time.Minute, Debounce: time.Millisecond, Now: clock})
	start := clock()
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	deadline := time.Now().Add(time.Second)
	var got HealthSummary
	for time.Now().Before(deadline) {
		got = s.HealthSummary(context.Background())
		if got.Status == HealthDegraded && strings.Contains(joinedReasons(got), "schema 8 < 9") {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancel()
	if got.Status != HealthDegraded {
		t.Fatalf("cluster gap: %+v", got)
	}
	s.FlushFindings(context.Background())
	if rows, _ := findings.ListFindings(context.Background(), ClusterFindingTask, true, 20); len(rows) != 0 {
		t.Fatalf("legacy row written during the gap: %+v", rows)
	}
	advance(20 * time.Second)
	recovered := clock()
	hs.down.Store(false)
	// A successful health write is what ends the gap. Sweep a task-less
	// cluster by running again so the startup probe sees the store answer.
	ctx, cancel = context.WithCancel(context.Background())
	go e.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	_ = e.SweepNow(context.Background())
	s.FlushFindings(context.Background())
	rows, err := findings.ListFindings(context.Background(), ClusterFindingTask, true, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != healthStoreKind || rows[0].Subject != evalNode || rows[0].ResolvedAt == nil {
		t.Fatalf("cluster close row %+v, want one resolved HEALTH_STORE_UNAVAILABLE/%s", rows, evalNode)
	}
	if !rows[0].FirstSeen.Equal(start) || !rows[0].ResolvedAt.Equal(recovered) {
		t.Fatalf("row window %s..%s, want %s..%s", rows[0].FirstSeen, rows[0].ResolvedAt, start, recovered)
	}
	evs, err := events.ListEvents(context.Background(), ClusterFindingTask, 20)
	if err != nil {
		t.Fatal(err)
	}
	var resolved, reported int
	for _, ev := range evs {
		if ev.Type == "FINDING_RESOLVED" && ev.Message == "HEALTH_STORE_UNAVAILABLE "+evalNode {
			resolved++
		}
		if ev.Type == "FINDING_REPORTED" {
			reported++
		}
	}
	if resolved != 1 || reported != 0 {
		t.Fatalf("events %+v, want one FINDING_RESOLVED and no REPORTED", evs)
	}
	if got := s.HealthSummary(context.Background()); got.Status != HealthOK {
		t.Fatalf("after the cluster close: %+v", got)
	}
}
