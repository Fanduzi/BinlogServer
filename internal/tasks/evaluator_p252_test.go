package tasks

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
)

// design 0009 §21: P2-5a grace, UPLOAD_LATE, object kinds through the evaluator.

type fakeLister struct {
	mu    sync.Mutex
	objs  map[string]int64
	err   error
	calls int
}

func (l *fakeLister) UploadFile(context.Context, string, string, string) error { return nil }

func (l *fakeLister) ListObjects(_ context.Context, prefix string, limit int) (map[string]int64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, false, l.err
	}
	var keys []string
	for k := range l.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := map[string]int64{}
	for i, k := range keys {
		if limit > 0 && i >= limit {
			return out, true, nil
		}
		out[k] = l.objs[k]
	}
	return out, false, nil
}

type p252Clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *p252Clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *p252Clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type p252Fixture struct {
	*lfFixture
	lister *fakeLister
	clock  *p252Clock
	store  healthstore.Store
}

// p252Dir: two sealed segments and an open one, catalog rows with disk sizes.
func p252Dir(t *testing.T, lister *fakeLister) *p252Fixture {
	t.Helper()
	var f *lfFixture
	if lister != nil {
		f = sizedDirWith(t, WithFileUploader(lister))
	} else {
		f = sizedDirWith(t)
	}
	clock := &p252Clock{t: time.Now()}
	return &p252Fixture{lfFixture: f, lister: lister, clock: clock, store: healthstore.NewFileStore(filepath.Join(t.TempDir(), "h"))}
}

func (p *p252Fixture) setRow(name string, mut func(*BinlogFile)) {
	k := catalogKey(name, 0)
	row := p.catalog.rows[k]
	mut(&row)
	p.catalog.rows[k] = row
}

func (p *p252Fixture) uploaded(name string) string {
	key := "p/c/" + fbA + "/" + name
	p.setRow(name, func(r *BinlogFile) { r.UploadState, r.Checksum, r.ObjectKey = "UPLOADED", "match", key })
	return key
}

func (p *p252Fixture) eval(t *testing.T, cfg EvaluatorConfig) *Evaluator {
	t.Helper()
	cfg.Mode, cfg.Store, cfg.NodeID, cfg.Instance = EvaluatorOn, p.store, evalNode, "i1"
	cfg.Sweep, cfg.Debounce, cfg.Now = time.Minute, time.Millisecond, p.clock.Now
	return NewEvaluator(p.s, cfg)
}

func (p *p252Fixture) passes(t *testing.T, e *Evaluator, n int) health.TaskHealth {
	t.Helper()
	var h health.TaskHealth
	for i := 0; i < n; i++ {
		var err error
		if h, err = e.EvaluateNow(context.Background(), p.task.ID); err != nil {
			t.Fatal(err)
		}
		p.clock.Add(time.Second)
	}
	return h
}

func (p *p252Fixture) open(t *testing.T) map[string]health.Finding {
	return openOf(t, p.store, p.task.ID)
}

func TestP252UploadGraceGlobalAndPerTask(t *testing.T) {
	for _, c := range []struct {
		name   string
		global time.Duration
		task   map[string]time.Duration
		want   bool
	}{
		{"default 15m, sealed 20m ago", 0, nil, true},
		{"global 30m", 30 * time.Minute, nil, false},
		{"task 10m over global 30m", 30 * time.Minute, map[string]time.Duration{"*": 10 * time.Minute}, true},
	} {
		p := p252Dir(t, &fakeLister{objs: map[string]int64{}})
		p.setRow("mysql-bin.000001", func(r *BinlogFile) { r.SealedAt = p.clock.Now().Add(-20 * time.Minute) })
		p.setRow("mysql-bin.000002", func(r *BinlogFile) { r.SealedAt = p.clock.Now() })
		task := map[string]time.Duration{}
		for range c.task {
			task[p.task.ID] = c.task["*"]
		}
		e := p.eval(t, EvaluatorConfig{UploadGrace: c.global, UploadGraceTask: task})
		p.passes(t, e, 2)
		_, got := p.open(t)["UPLOAD_LATE/mysql-bin.000001"]
		if got != c.want {
			t.Errorf("%s: UPLOAD_LATE open=%v, want %v (%v)", c.name, got, c.want, p.open(t))
		}
		if _, bad := p.open(t)["UPLOAD_LATE/mysql-bin.000002"]; bad {
			t.Errorf("%s: a segment sealed just now is late", c.name)
		}
	}
}

func TestP252UploadLateEscalatesAndClosesOnUpload(t *testing.T) {
	p := p252Dir(t, &fakeLister{objs: map[string]int64{}})
	p.setRow("mysql-bin.000001", func(r *BinlogFile) { r.SealedAt = p.clock.Now().Add(-23*time.Hour - 59*time.Minute) })
	p.setRow("mysql-bin.000002", func(r *BinlogFile) { r.SealedAt = p.clock.Now() })
	e := p.eval(t, EvaluatorConfig{})
	p.passes(t, e, 2)
	if got := p.open(t)["UPLOAD_LATE/mysql-bin.000001"]; got.Severity != health.Warning {
		t.Fatalf("before 24h: %+v", got)
	}
	p.clock.Add(2 * time.Minute)
	p.passes(t, e, 1)
	if got := p.open(t)["UPLOAD_LATE/mysql-bin.000001"]; got.Severity != health.Critical {
		t.Fatalf("after 24h: %+v", got)
	}
	key := p.uploaded("mysql-bin.000001")
	p.lister.mu.Lock()
	p.lister.objs[key] = p.catalog.rows[catalogKey("mysql-bin.000001", 0)].SizeBytes
	p.lister.mu.Unlock()
	p.passes(t, e, 1)
	if _, ok := p.open(t)["UPLOAD_LATE/mysql-bin.000001"]; ok {
		t.Fatalf("still open after the upload: %v", p.open(t))
	}
}

func TestP252ObjectMissingOpensAndCloses(t *testing.T) {
	p := p252Dir(t, &fakeLister{objs: map[string]int64{}})
	k1, k2 := p.uploaded("mysql-bin.000001"), p.uploaded("mysql-bin.000002")
	size := p.catalog.rows[catalogKey("mysql-bin.000002", 0)].SizeBytes
	p.lister.objs[k2] = size // k1 is missing from the bucket
	e := p.eval(t, EvaluatorConfig{ObjectListInterval: time.Minute})
	p.passes(t, e, 2)
	if _, ok := p.open(t)["SEGMENT_MISSING/mysql-bin.000001#object"]; !ok {
		t.Fatalf("missing object not open: %v", p.open(t))
	}
	if _, ok := p.open(t)["SEGMENT_MISSING/mysql-bin.000002#object"]; ok {
		t.Fatal("present object reported")
	}
	p.lister.mu.Lock()
	p.lister.objs[k1] = p.catalog.rows[catalogKey("mysql-bin.000001", 0)].SizeBytes
	p.lister.mu.Unlock()
	p.clock.Add(2 * time.Minute) // past the listing interval
	p.passes(t, e, 1)
	if _, ok := p.open(t)["SEGMENT_MISSING/mysql-bin.000001#object"]; ok {
		t.Fatalf("still open after the object is back: %v", p.open(t))
	}
}

func TestP252TruncatedOrFailedListingJudgesNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		limit int
		err   error
	}{{"truncated", 1, nil}, {"error", 0, errors.New("AccessDenied")}} {
		p := p252Dir(t, &fakeLister{objs: map[string]int64{}, err: c.err})
		p.uploaded("mysql-bin.000001")
		k2 := p.uploaded("mysql-bin.000002")
		p.lister.objs[k2] = 1
		p.lister.objs["p/c/"+fbA+"/aaa-extra"] = 1 // sorts first: a limit of 1 never reaches the task's keys
		e := p.eval(t, EvaluatorConfig{ObjectListLimit: c.limit})
		h := p.passes(t, e, 2)
		for k := range p.open(t) {
			if strings.HasSuffix(k, "#object") {
				t.Errorf("%s: %s opened from an incomplete listing", c.name, k)
			}
		}
		if h.Status != health.StatusUnknown {
			t.Errorf("%s: health %s, want UNKNOWN (chain.objects incomplete)", c.name, h.Status)
		}
	}
}

func TestP252UploadedAfterSnapshotNotJudged(t *testing.T) {
	p := p252Dir(t, &fakeLister{objs: map[string]int64{}})
	k1 := p.uploaded("mysql-bin.000001")
	p.lister.objs[k1] = p.catalog.rows[catalogKey("mysql-bin.000001", 0)].SizeBytes
	e := p.eval(t, EvaluatorConfig{ObjectListInterval: time.Hour})
	p.passes(t, e, 1) // snapshot taken: 000002 not uploaded yet
	p.uploaded("mysql-bin.000002")
	p.passes(t, e, 2)
	if _, ok := p.open(t)["SEGMENT_MISSING/mysql-bin.000002#object"]; ok {
		t.Fatalf("judged against a listing taken before its upload: %v", p.open(t))
	}
}

func TestP252NoObjectStorageClosesWithConfig(t *testing.T) {
	p := p252Dir(t, &fakeLister{objs: map[string]int64{}})
	p.uploaded("mysql-bin.000001")
	e := p.eval(t, EvaluatorConfig{})
	p.passes(t, e, 2)
	if _, ok := p.open(t)["SEGMENT_MISSING/mysql-bin.000001#object"]; !ok {
		t.Fatalf("setup: %v", p.open(t))
	}
	p.s.mu.Lock()
	p.s.fileUploader = nil // the configuration no longer has object storage
	p.s.mu.Unlock()
	p.passes(t, e, 1)
	if _, ok := p.open(t)["SEGMENT_MISSING/mysql-bin.000001#object"]; ok {
		t.Fatalf("not closed: %v", p.open(t))
	}
	trs, err := p.store.(healthstore.TransitionLister).ListTransitions(context.Background(), p.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tr := range trs {
		if tr.Op == string(health.OpClose) && tr.Subject == "mysql-bin.000001#object" {
			found = tr.Reason == health.ReasonConfig && strings.Contains(tr.Detail, "object storage no longer configured")
		}
	}
	if !found {
		t.Fatalf("no CONFIG close with an audit note: %+v", trs)
	}
}
