// Package tasks provides module-level functionality for tasks.
// input: a standalone scheduler whose task dir holds one GTID segment (coverageGTIDFile), a stubbed drillExec, and a verifier carrying a drill DSN
// output: the drill never dials an empty DSN or the task's own source address; a stubbed replay plus a containing executed set resolves DRILL_FAILED, stamps the metric and writes RESTORE_DRILL; a failing replay or a short executed set opens CRITICAL DRILL_FAILED; one period runs one attempt and reuses its persisted point
// pos: restore-drill unit coverage for ADR 0009 phase 3 §12
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeDrillDB is the stubbed drill instance.
type fakeDrillDB struct {
	set      string
	setErr   error
	checkErr error
	closed   bool
}

func (f *fakeDrillDB) ExecutedSet(ctx context.Context, flavor string) (string, error) {
	return f.set, f.setErr
}
func (f *fakeDrillDB) ExecCheck(ctx context.Context, q string) error { return f.checkErr }
func (f *fakeDrillDB) Close() error                                  { f.closed = true; return nil }

// stubDrill swaps the package-level drillExec for the test.
func stubDrill(t *testing.T, ops drillOps) {
	t.Helper()
	old := drillExec
	drillExec = ops
	t.Cleanup(func() { drillExec = old })
}

// newDrillFixture builds a standalone scheduler + verifier with a task whose
// source is 127.0.0.1:3306 and one GTID segment on disk. files is the task's
// catalog so tests can register more rows (a bucket-only segment).
func newDrillFixture(t *testing.T, cfg VerifierConfig) (*Scheduler, *Verifier, Task, *fakeFileStore) {
	t.Helper()
	dir := t.TempDir()
	files := newFakeFileStore()
	s := NewScheduler(WithDataDir(dir), WithFileStore(files),
		WithFindingStore(NewFileFindingStore(filepath.Join(dir, FindingFileName))))
	task, err := s.CreateTaskFromSpec("drill", "drill-key",
		&SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "pw", Flavor: "mysql"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(dir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	raw := coverageGTIDFile(t, when, when.Add(10*time.Second))
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000001"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	putSealed(files, task.ID, "mysql-bin.000001", dir, BinlogFile{
		FilePath: filepath.Join(taskDir, "mysql-bin.000001"), SizeBytes: int64(len(raw)), UploadState: "UPLOADED"})
	cfg.DataDir = dir
	v := NewVerifier(s, cfg)
	s.SetVerifier(v)
	return s, v, task, files
}

func drillCfg() VerifierConfig {
	return VerifierConfig{
		BudgetBytesPerSec: -1,
		Drill:             DrillConfig{DSN: "drill:pw@tcp(127.0.0.1:4406)/drill", Every: time.Hour},
	}
}

func TestDrill_OffWithoutDSN(t *testing.T) {
	s, v, task, _ := newDrillFixture(t, VerifierConfig{BudgetBytesPerSec: -1})
	stubDrill(t, drillOps{
		lookPath: func(string) (string, error) { t.Fatal("lookPath with the drill off"); return "", nil },
		openDSN:  func(string) (drillDB, error) { t.Fatal("dial with the drill off"); return nil, nil },
	})
	v.drillTask(context.Background(), task, time.Now())
	if _, ok := openKind(s, task.ID, string(FindingDrillFailed)); ok {
		t.Fatal("DRILL_FAILED opened with the drill off")
	}
}

func TestDrill_RefusesTaskSource(t *testing.T) {
	cfg := drillCfg()
	// The drill DSN names the task's own source 127.0.0.1:3306.
	cfg.Drill.DSN = "repl:pw@tcp(127.0.0.1:3306)/x"
	s, v, task, _ := newDrillFixture(t, cfg)
	stubDrill(t, drillOps{
		lookPath: func(string) (string, error) { t.Fatal("lookPath on a refused DSN"); return "", nil },
		openDSN:  func(string) (drillDB, error) { t.Fatal("dialed the task source"); return nil, nil },
	})
	now := time.Now()
	v.drillTask(context.Background(), task, now)
	f, ok := openKind(s, task.ID, string(FindingDrillRefused))
	if !ok || f.Severity != FindingWarning {
		t.Fatalf("DRILL_REFUSED missing/wrong severity: %+v", f)
	}
	// The refusal consumes the period: a second call stays quiet.
	v.drillTask(context.Background(), task, now.Add(time.Second))
	if st := v.drillState(task.ID); st == nil || !st.Done {
		t.Fatalf("refusal did not mark the period done: %+v", st)
	}
}

func TestDrill_Pass(t *testing.T) {
	var ran bool
	db := &fakeDrillDB{set: "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-2"}
	stubDrill(t, drillOps{
		lookPath: func(string) (string, error) { return "/usr/bin/x", nil },
		run: func(ctx context.Context, dir string, env []string, name string, args ...string) error {
			ran = true
			return nil
		},
		openDSN: func(string) (drillDB, error) { return db, nil },
	})
	s, v, task, _ := newDrillFixture(t, drillCfg())
	// Anchor inside the hour period: a same-period call below adds a minute,
	// so now must not sit within the last minute of a period.
	now := time.Unix((time.Now().Unix()/3600)*3600+10, 0)
	v.drillTask(context.Background(), task, now)
	if !ran {
		t.Fatal("replay never ran")
	}
	if _, bad := openKind(s, task.ID, string(FindingDrillFailed)); bad {
		t.Fatal("DRILL_FAILED open after a pass")
	}
	m := v.Metrics().Tasks[task.ID]
	if !m.HaveDrillSuccess || m.LastDrillSuccessUnix != now.Unix() {
		t.Fatalf("drill success metric %+v", m)
	}
	// A same-period pass must not re-run: the attempt is done.
	ran = false
	v.drillTask(context.Background(), task, now.Add(time.Minute))
	if ran {
		t.Fatal("the same drill period ran twice")
	}
	// The next period drills again at a fresh point.
	v.drillTask(context.Background(), task, now.Add(2*time.Hour))
	if !ran {
		t.Fatal("the next drill period did not run")
	}
}

func TestDrill_ReplayFailureOpensCritical(t *testing.T) {
	stubDrill(t, drillOps{
		lookPath: func(string) (string, error) { return "/usr/bin/x", nil },
		run:      func(context.Context, string, []string, string, ...string) error { return errors.New("boom") },
		openDSN:  func(string) (drillDB, error) { t.Fatal("dial after a failed replay"); return nil, nil },
	})
	s, v, task, _ := newDrillFixture(t, drillCfg())
	now := time.Now()
	v.drillTask(context.Background(), task, now)
	f, ok := openKind(s, task.ID, string(FindingDrillFailed))
	if !ok || f.Severity != FindingCritical {
		t.Fatalf("DRILL_FAILED %+v", f)
	}
	if st := v.drillState(task.ID); st == nil || !st.Done {
		t.Fatal("a failed drill did not close its period")
	}
}

func TestDrill_ExecutedSetShortFails(t *testing.T) {
	stubDrill(t, drillOps{
		lookPath: func(string) (string, error) { return "/usr/bin/x", nil },
		run:      func(context.Context, string, []string, string, ...string) error { return nil },
		openDSN: func(string) (drillDB, error) {
			return &fakeDrillDB{set: "3e11fa47-71ca-11e1-9e33-c80aa9429562:1"}, nil
		},
	})
	s, v, task, _ := newDrillFixture(t, drillCfg())
	v.drillTask(context.Background(), task, time.Now())
	f, ok := openKind(s, task.ID, string(FindingDrillFailed))
	if !ok || f.Severity != FindingCritical {
		t.Fatalf("short executed set did not open DRILL_FAILED: %+v", f)
	}
}

func TestDrill_AllowsDeniedSkips(t *testing.T) {
	cfg := drillCfg()
	cfg.Allows = func(context.Context, string) (bool, error) { return false, nil }
	s, v, task, _ := newDrillFixture(t, cfg)
	stubDrill(t, drillOps{
		lookPath: func(string) (string, error) { t.Fatal("lookPath on a denied task"); return "", nil },
	})
	v.drillTask(context.Background(), task, time.Now())
	if _, ok := openKind(s, task.ID, string(FindingDrillRefused)); ok {
		t.Fatal("refused opened on a non-holder")
	}
	if _, ok := openKind(s, task.ID, string(FindingDrillFailed)); ok {
		t.Fatal("failed opened on a non-holder")
	}
}

func TestDrill_FetchUsesObjectKey(t *testing.T) {
	// A manifest file with no local path downloads by object key into the
	// drill staging dir. A file that is covered but not local only appears
	// in a plan the object store produced, so drillFetch is tested directly.
	_, v, _, _ := newDrillFixture(t, drillCfg())
	var gotKey string
	stubDrill(t, drillOps{
		openObj: func(ctx context.Context, key string) (io.ReadCloser, error) {
			gotKey = key
			return io.NopCloser(bytes.NewReader([]byte("seg-bytes"))), nil
		},
	})
	dir := t.TempDir()
	p, err := v.drillFetch(context.Background(), dir, RestoreFile{Name: "mysql-bin.000009", ObjectKey: "obj/k9"})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "obj/k9" {
		t.Fatalf("fetched key %q", gotKey)
	}
	body, err := os.ReadFile(p)
	if err != nil || string(body) != "seg-bytes" {
		t.Fatalf("fetched %q err %v", body, err)
	}
	// No object store and no stub: the fetch fails honestly.
	stubDrill(t, drillOps{})
	if _, err := v.drillFetch(context.Background(), dir, RestoreFile{Name: "mysql-bin.000009"}); err == nil {
		t.Fatal("fetch succeeded without an object store")
	}
}
