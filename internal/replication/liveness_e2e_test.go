// Package replication provides module-level functionality for replication.
// input: a real MySQL source (BINLOG_TEST_MYSQL_ROOT_DSNS, first DSN) behind faultProxy, a real Scheduler + MySQLRunner + health Evaluator on a FileStore, thresholds and retry backoff scaled 1/15 (heartbeat 1s, read timeout and DUMP_STALLED 3s, SOURCE_UNREACHABLE 6s, probe every 0.7s with a 0.4s timeout, backoff 67ms..2s)
// output: the #383 QA plan for one source liveness clock: a black-holed source (under load and idle) and a killed source raise a finding within DUMP_STALLED's bound and keep at least one finding open until it is back; SOURCE_UNREACHABLE opens within its bound and its detail tells the real outage; a planned restart shorter than the SOURCE_UNREACHABLE threshold (the 50s and 63s cases) never opens it, whatever the backoff does; recovery closes everything
// pos: ADR 0009 P2-4 e2e (docs/design/0009-phase2-health-evaluator.md §18); env-gated, CI skips it
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/tasks"

	_ "github.com/go-sql-driver/mysql"
)

// Scaled thresholds: production is 15x (heartbeat 15s, read timeout and
// DUMP_STALLED 45s, SOURCE_UNREACHABLE 90s, probe every 10s for 5s).
const (
	lvScale        = 15
	lvHeartbeat    = time.Second
	lvReadTimeout  = 3 * time.Second
	lvStalled      = 3 * time.Second
	lvUnreachable  = 6 * time.Second
	lvProbeEvery   = 700 * time.Millisecond
	lvProbeTimeout = 400 * time.Millisecond
	lvTick         = 250 * time.Millisecond
	lvSlack        = 1500 * time.Millisecond
)

type lvEnv struct {
	t      *testing.T
	proxy  *faultProxy
	s      *tasks.Scheduler
	e      *tasks.Evaluator
	store  *healthstore.FileStore
	task   tasks.Task
	root   *sql.DB
	cancel context.CancelFunc
}

func lvSource(t *testing.T) (dsn, addr string) {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("BINLOG_TEST_MYSQL_ROOT_DSNS"))
	if raw == "" {
		t.Skip("BINLOG_TEST_MYSQL_ROOT_DSNS is not set")
	}
	dsn = strings.Split(raw, ",")[0]
	m := regexp.MustCompile(`tcp\(([^)]+)\)`).FindStringSubmatch(dsn)
	if m == nil {
		t.Skipf("no tcp address in %q", dsn)
	}
	return dsn, m[1]
}

func newLvEnv(t *testing.T) *lvEnv {
	t.Helper()
	dsn, addr := lvSource(t)
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	db := fmt.Sprintf("lv%d", time.Now().UnixNano()%1e9)
	for _, q := range []string{"CREATE DATABASE " + db, "CREATE TABLE " + db + ".t (id INT PRIMARY KEY AUTO_INCREMENT, v VARCHAR(64))"} {
		if _, err := root.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + db) })
	user, pass := "root", "root"
	if m := regexp.MustCompile(`^([^:@]+):([^@]*)@`).FindStringSubmatch(dsn); m != nil {
		user, pass = m[1], m[2]
	}
	proxy := newFaultProxy(t, addr)
	data := t.TempDir()
	runner := NewMySQLRunner(data, WithDumpTimeouts(lvHeartbeat, lvReadTimeout))
	s := tasks.NewScheduler(tasks.WithDataDir(data), tasks.WithSourceLiveness(lvProbeEvery, lvProbeTimeout), tasks.WithRetryBackoff(time.Second/lvScale, 30*time.Second/lvScale))
	s.SetRunner(runner)
	store := healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName))
	e := tasks.NewEvaluator(s, tasks.EvaluatorConfig{Mode: tasks.EvaluatorOn, Store: store, NodeID: "lv", Instance: "i1",
		Sweep: time.Second, Debounce: 50 * time.Millisecond, LivenessTick: lvTick,
		DumpStalledAfter: lvStalled, UnreachableAfter: lvUnreachable})
	ctx, cancel := context.WithCancel(context.Background())
	s.StartFindingSink(ctx, 200*time.Millisecond)
	s.StartSourceLiveness(ctx)
	go e.Run(ctx)
	env := &lvEnv{t: t, proxy: proxy, s: s, e: e, store: store, root: root, cancel: cancel}
	t.Cleanup(func() {
		_ = s.StopTask(env.task.ID)
		cancel()
	})
	task, err := s.CreateTaskFromSpec("lv", "lv-"+db, &tasks.SourceConfig{Host: "127.0.0.1", Port: uint16(proxy.port()), User: user, Password: pass, Flavor: "mysql", ServerID: uint32(40000 + time.Now().UnixNano()%10000)},
		&tasks.StartConfig{Mode: tasks.StartModeLatest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.task = task
	if err := s.StartTask(task.ID); err != nil {
		t.Fatal(err)
	}
	env.waitState(tasks.StateRunning, 20*time.Second)
	env.waitOpen(func(m map[string]health.Finding) bool { return len(m) == 0 }, 10*time.Second, "no finding while healthy")
	return env
}

func (env *lvEnv) load(ctx context.Context, db string) {
	go func() {
		for i := 0; ctx.Err() == nil; i++ {
			_, _ = env.root.ExecContext(ctx, "INSERT INTO "+db+".t (v) VALUES (REPEAT(MD5(?), 30))", i)
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func (env *lvEnv) state() tasks.State {
	task, err := env.s.GetTask(env.task.ID)
	if err != nil {
		return ""
	}
	return task.State
}

func (env *lvEnv) open() map[string]health.Finding {
	out := map[string]health.Finding{}
	rows, err := env.store.ListOpenFindingsForTask(context.Background(), env.task.ID)
	if err != nil {
		env.t.Fatal(err)
	}
	for _, f := range rows {
		out[string(f.Kind)] = f
	}
	return out
}

func (env *lvEnv) waitState(want tasks.State, within time.Duration) {
	env.t.Helper()
	deadline := time.Now().Add(within)
	for env.state() != want {
		if time.Now().After(deadline) {
			env.t.Fatalf("task is %s after %s, want %s", env.state(), within, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (env *lvEnv) waitOpen(ok func(map[string]health.Finding) bool, within time.Duration, what string) time.Duration {
	env.t.Helper()
	start := time.Now()
	for {
		m := env.open()
		if ok(m) {
			return time.Since(start)
		}
		if time.Since(start) > within {
			env.t.Fatalf("%s: not within %s; open %v, task %s", what, within, keys(m), env.state())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func keys(m map[string]health.Finding) []string {
	var out []string
	for k, f := range m {
		out = append(out, k+"/"+string(f.Severity))
	}
	return out
}

// outage samples the open findings every 100ms for d after the fault and
// returns the time to the first finding, to SOURCE_UNREACHABLE, and every
// sample after the first finding that had none open.
type outageReport struct {
	firstFinding, unreachable time.Duration
	gaps                      []string
	sawUnreachable            bool
	unreachableDetail         string
	runnerSawStall            bool
}

func (env *lvEnv) watch(d time.Duration) outageReport {
	var rep outageReport
	start := time.Now()
	for time.Since(start) < d {
		m := env.open()
		el := time.Since(start)
		if len(m) > 0 && rep.firstFinding == 0 {
			rep.firstFinding = el
		}
		if rep.firstFinding > 0 && len(m) == 0 {
			rep.gaps = append(rep.gaps, fmt.Sprintf("%s(%s)", el.Truncate(100*time.Millisecond), env.state()))
		}
		if task, err := env.s.GetTask(env.task.ID); err == nil && strings.Contains(task.LastError, "dump silent") {
			rep.runnerSawStall = true
		}
		if f, ok := m[string(health.SourceUnreachable)]; ok {
			if !rep.sawUnreachable {
				rep.unreachable = el
			}
			rep.sawUnreachable = true
			rep.unreachableDetail = f.Detail
		}
		time.Sleep(100 * time.Millisecond)
	}
	return rep
}

func (env *lvEnv) assertDeadSource(rep outageReport, outage time.Duration) {
	env.t.Helper()
	env.t.Logf("first finding %s, SOURCE_UNREACHABLE %s (%q), runner saw the stall %v", rep.firstFinding, rep.unreachable, rep.unreachableDetail, rep.runnerSawStall)
	if rep.firstFinding == 0 || rep.firstFinding > lvStalled+lvTick+lvSlack {
		env.t.Errorf("first finding after %s, want within %s (DUMP_STALLED, %ds in production)", rep.firstFinding, lvStalled+lvTick+lvSlack, 45)
	}
	if len(rep.gaps) > 0 {
		env.t.Errorf("a dead source had no finding open at %v", rep.gaps)
	}
	bound := lvUnreachable + lvProbeEvery + lvProbeTimeout + lvTick + lvSlack
	if !rep.sawUnreachable || rep.unreachable > bound {
		env.t.Errorf("SOURCE_UNREACHABLE at %s (seen %v), want within %s (~%s in production)", rep.unreachable, rep.sawUnreachable, bound, bound*lvScale)
	}
	if m := env.open(); m[string(health.DumpStalled)].Kind != "" && m[string(health.SourceUnreachable)].Kind != "" {
		env.t.Errorf("DUMP_STALLED not handed off once SOURCE_UNREACHABLE opened: %v", keys(m))
	}
	// The detail tells the silence since the last byte, not since the first retry.
	if d := silenceIn(rep.unreachableDetail); d < outage-2*time.Second {
		env.t.Errorf("SOURCE_UNREACHABLE detail %q says %s for an outage of at least %s", rep.unreachableDetail, d, outage)
	}
}

var durRe = regexp.MustCompile(`for ([0-9hms.]+)`)

func silenceIn(detail string) time.Duration {
	m := durRe.FindStringSubmatch(detail)
	if m == nil {
		return 0
	}
	d, _ := time.ParseDuration(m[1])
	return d
}

func (env *lvEnv) recover(what string) {
	env.t.Helper()
	env.proxy.restore()
	env.waitState(tasks.StateRunning, 45*time.Second)
	env.waitOpen(func(m map[string]health.Finding) bool { return len(m) == 0 }, 10*time.Second, what+": findings close once the source is back")
}

func TestLiveness_BlackholeUnderLoad(t *testing.T) {
	env := newLvEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.load(ctx, strings.TrimPrefix(env.task.ClusterKey, "lv-"))
	time.Sleep(2 * time.Second)
	env.proxy.blackhole()
	rep := env.watch(lvUnreachable + 4*time.Second)
	env.assertDeadSource(rep, lvUnreachable+4*time.Second)
	if !rep.runnerSawStall {
		t.Error("the runner did not end the run as a stall (\"dump silent\"): the read timeout became a plain error")
	}
	cancel()
	env.recover("blackhole under load")
}

func TestLiveness_BlackholeIdle(t *testing.T) {
	env := newLvEnv(t)
	time.Sleep(2 * lvHeartbeat)
	env.proxy.blackhole()
	rep := env.watch(lvUnreachable + 4*time.Second)
	env.assertDeadSource(rep, lvUnreachable+4*time.Second)
	if !rep.runnerSawStall {
		t.Error("the runner did not end the run as a stall (\"dump silent\"): the read timeout became a plain error")
	}
	env.recover("blackhole idle")
}

func TestLiveness_KilledSource(t *testing.T) {
	env := newLvEnv(t)
	time.Sleep(2 * lvHeartbeat)
	env.proxy.kill()
	rep := env.watch(lvUnreachable + 4*time.Second)
	env.assertDeadSource(rep, lvUnreachable+4*time.Second)
	env.recover("killed source")
}

// A planned restart shorter than the SOURCE_UNREACHABLE threshold never
// opens it, whatever the retry backoff does: 50s, 63s and 80s at production
// scale (the 90s threshold counts from the last byte read, not from RUNNING).
func TestLiveness_PlannedRestartNeverOpensUnreachable(t *testing.T) {
	for _, down := range []time.Duration{50 * time.Second / lvScale, 63 * time.Second / lvScale, 80 * time.Second / lvScale} {
		t.Run(down.String(), func(t *testing.T) {
			env := newLvEnv(t)
			time.Sleep(2 * lvHeartbeat)
			env.proxy.kill()
			start := time.Now()
			restored := false
			var seen []string
			for time.Since(start) < down+15*time.Second {
				if !restored && time.Since(start) >= down {
					env.proxy.restore()
					restored = true
				}
				if f, ok := env.open()[string(health.SourceUnreachable)]; ok {
					seen = append(seen, f.Detail)
				}
				time.Sleep(100 * time.Millisecond)
			}
			if len(seen) > 0 {
				t.Fatalf("a %s restart (%s in production) opened SOURCE_UNREACHABLE: %q", down, down*lvScale, seen[0])
			}
			env.waitState(tasks.StateRunning, 45*time.Second)
			env.waitOpen(func(m map[string]health.Finding) bool { return len(m) == 0 }, 10*time.Second, "planned restart: everything closes")
		})
	}
}
