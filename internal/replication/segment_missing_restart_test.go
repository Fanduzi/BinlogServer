// Package replication provides module-level functionality for replication.
// input: a task directory with sealed LOCAL_ONLY segments in the catalog and no uploader, one of them deleted while still inside local retention; a real Scheduler.StartTask and a second MySQLRunner taking the task over on the same data_dir with a higher epoch; a real health Evaluator on a FileStore
// output: proof that a runner start (operator Start or takeover) keeps the catalog row of a missing segment that is inside local retention, writes no RETENTION_REMOVED for it, and that SEGMENT_MISSING stays open (no CLOSE) on the passes after that start; and that the same start still drops a gone row past local retention (#209)
// pos: regression for the PR #507 QA P2 (ADR 0011 PR-3, #462): dropGoneLocalOnlyRows must not erase the evidence SEGMENT_MISSING is judged from
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

type restartEnv struct {
	t         *testing.T
	dir       string
	taskDir   string
	catalog   *purgeCatalog
	scheduler *tasks.Scheduler
	eval      *tasks.Evaluator
	store     *healthstore.FileStore
	cp        *memCheckpointStore
	fetcher   *movableMaster
	syncer    *stopResumeSyncer
	taskID    string
	eventAt   time.Time
}

const (
	restartFirstSQL  = "INSERT INTO restart_missing VALUES (1)"
	restartSecondSQL = "INSERT INTO restart_missing VALUES (2)"
)

func newRestartEnv(t *testing.T) *restartEnv {
	t.Helper()
	env := &restartEnv{t: t, dir: t.TempDir(), eventAt: time.Unix(1_700_000_000, 0).UTC()}
	env.catalog = &purgeCatalog{rows: map[string]tasks.BinlogFile{}}
	env.cp = &memCheckpointStore{}
	first, endPos := chainBinlogEvents(4, env.eventAt, []namedEvent{
		{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(env.eventAt)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", restartFirstSQL)},
	})
	second, _ := chainBinlogEvents(endPos, env.eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", restartSecondSQL)},
	})
	phase2 := append([]*goreplication.BinlogEvent{preambleEvent(0, env.eventAt.Add(-time.Hour))}, second...)
	env.fetcher = &movableMaster{status: MasterStatus{File: "mysql-bin.000004", Pos: 4}, uuid: "11111111-1111-1111-1111-111111111111"}
	env.syncer = &stopResumeSyncer{phases: [][]*goreplication.BinlogEvent{first, phase2}}
	runner := NewMySQLRunner(env.dir, WithFileMetaStore(env.catalog), WithCheckpointStore(env.cp))
	runner.fetcher = env.fetcher
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return env.syncer }

	released := make(chan struct{}, 8)
	leases := &releaseNotifyingLease{inner: tasks.NewMemoryLease(), released: released, once: &sync.Once{}}
	env.scheduler = tasks.NewScheduler(
		tasks.WithDataDir(env.dir),
		tasks.WithRunner(runner),
		tasks.WithFileStore(env.catalog),
		tasks.WithEventStore(env.catalog),
		tasks.WithClusterLeaseManager(leases),
		tasks.WithClusterWorkerID("node-a"),
		tasks.WithClusterLease(time.Minute, time.Minute, time.Minute),
	)
	task, err := env.scheduler.CreateTaskFromSpec("restart", "restart-key", &tasks.SourceConfig{
		Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql",
	}, &tasks.StartConfig{Mode: tasks.StartModeLatest}, &tasks.Storage{RetentionDays: 7})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	env.taskID = task.ID
	env.taskDir = filepath.Join(env.dir, task.ID)
	if err := os.MkdirAll(env.taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Three sealed LOCAL_ONLY segments, sealed an hour ago: well inside the
	// 7-day local retention. 000001 is past it by its row (sealed 10 days
	// ago) and its file is already gone: the #209 crash leftover.
	young := time.Now().Add(-time.Hour)
	env.sealed("mysql-bin.000002", young)
	env.sealed("mysql-bin.000003", young)
	gone := filepath.Join(env.taskDir, "mysql-bin.000001")
	env.catalog.rows["mysql-bin.000001"] = tasks.BinlogFile{
		TaskID: task.ID, FileName: "mysql-bin.000001", FilePath: gone, State: "SEALED", UploadState: "LOCAL_ONLY",
		CreatedAt: time.Now().Add(-11 * 24 * time.Hour), SealedAt: time.Now().Add(-10 * 24 * time.Hour),
	}
	env.store = healthstore.NewFileStore(filepath.Join(t.TempDir(), healthstore.HealthFileName))
	env.eval = tasks.NewEvaluator(env.scheduler, tasks.EvaluatorConfig{
		Mode: tasks.EvaluatorOn, Store: env.store, NodeID: "node-a", Instance: "node-a-i",
		Sweep: time.Minute, Debounce: time.Millisecond,
	})
	t.Cleanup(func() { _ = env.scheduler.StopTask(task.ID) })
	return env
}

func (env *restartEnv) sealed(name string, at time.Time) {
	env.t.Helper()
	events, endPos := chainBinlogEvents(4, env.eventAt, []namedEvent{
		{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(env.eventAt)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", "SELECT '"+name+"'")},
	})
	path := filepath.Join(env.taskDir, name)
	writeBinlogSegment(env.t, path, events)
	if err := os.Chtimes(path, at, at); err != nil {
		env.t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		env.t.Fatal(err)
	}
	env.catalog.rows[name] = tasks.BinlogFile{
		TaskID: env.taskID, FileName: name, FilePath: path, State: "SEALED", UploadState: "LOCAL_ONLY",
		SizeBytes: info.Size(), StartPos: 4, EndPos: tasks.FilePos(endPos),
		CreatedAt: at.Add(-time.Minute), SealedAt: at,
	}
}

func (env *restartEnv) evaluate(passes int) {
	env.t.Helper()
	for i := 0; i < passes; i++ {
		if _, err := env.eval.EvaluateNow(context.Background(), env.taskID); err != nil {
			env.t.Fatalf("evaluate: %v", err)
		}
	}
}

func (env *restartEnv) missingOpen(name string) bool {
	env.t.Helper()
	open, err := env.store.ListOpenFindingsForTask(context.Background(), env.taskID)
	if err != nil {
		env.t.Fatal(err)
	}
	for _, f := range open {
		if f.Kind == health.SegmentMissing && f.Subject == name {
			return true
		}
	}
	return false
}

func (env *restartEnv) closes(name string) []string {
	env.t.Helper()
	ts, err := env.store.ListTransitions(context.Background(), env.taskID)
	if err != nil {
		env.t.Fatal(err)
	}
	var out []string
	for _, tr := range ts {
		if tr.Kind == string(health.SegmentMissing) && tr.Subject == name && tr.Op == string(health.OpClose) {
			out = append(out, tr.Reason)
		}
	}
	return out
}

// openMissing runs one real start so the directory has an open segment,
// then deletes the young sealed 000002 and lets the evaluator open
// SEGMENT_MISSING for it.
func (env *restartEnv) openMissing() {
	env.t.Helper()
	if err := env.scheduler.StartTask(env.taskID); err != nil {
		env.t.Fatalf("start: %v", err)
	}
	waitForMarker(env.t, env.dir, env.taskID, restartFirstSQL)
	if err := env.scheduler.StopTask(env.taskID); err != nil {
		env.t.Fatalf("stop: %v", err)
	}
	waitTaskState(env.t, env.scheduler, env.taskID, tasks.StateStopped)
	if _, ok := env.catalog.rows["mysql-bin.000001"]; ok {
		env.t.Fatal("#209: the gone row past local retention survived the start")
	}
	if countRetentionRemoved(env.catalog.events, "mysql-bin.000001") != 1 {
		env.t.Fatalf("#209: RETENTION_REMOVED for the expired row: %+v", env.catalog.events)
	}
	env.evaluate(2)
	if env.missingOpen("mysql-bin.000002") {
		env.t.Fatal("SEGMENT_MISSING open before the delete")
	}
	if err := os.Remove(filepath.Join(env.taskDir, "mysql-bin.000002")); err != nil {
		env.t.Fatal(err)
	}
	env.evaluate(2)
	if !env.missingOpen("mysql-bin.000002") {
		open, _ := env.store.ListOpenFindingsForTask(context.Background(), env.taskID)
		env.t.Fatalf("setup: SEGMENT_MISSING not open after the delete: %+v", open)
	}
}

func (env *restartEnv) assertStillMissing(start string) {
	env.t.Helper()
	if _, ok := env.catalog.rows["mysql-bin.000002"]; !ok {
		env.t.Fatalf("%s dropped the catalog row of a missing segment inside local retention", start)
	}
	if n := countRetentionRemoved(env.catalog.events, "mysql-bin.000002"); n != 0 {
		env.t.Fatalf("%s wrote %d RETENTION_REMOVED for a segment retention never removed", start, n)
	}
	if _, ok := env.catalog.rows["mysql-bin.000003"]; !ok {
		env.t.Fatalf("%s dropped a present sealed row", start)
	}
	env.evaluate(2)
	if !env.missingOpen("mysql-bin.000002") {
		env.t.Fatalf("SEGMENT_MISSING closed after %s while the segment is still gone (closes %v)", start, env.closes("mysql-bin.000002"))
	}
	if c := env.closes("mysql-bin.000002"); len(c) != 0 {
		env.t.Fatalf("SEGMENT_MISSING closed after %s: %v", start, c)
	}
}

// TestRunnerStartKeepsYoungMissingSegment is the PR #507 QA P2: Start after
// the segment went missing used to drop its row in the file-open retention
// pass, and the next pass closed SEGMENT_MISSING with EVAL.
func TestRunnerStartKeepsYoungMissingSegment(t *testing.T) {
	t.Run("operator start", func(t *testing.T) {
		env := newRestartEnv(t)
		env.openMissing()
		env.fetcher.set(MasterStatus{File: "mysql-bin.000004", Pos: 9000})
		if err := env.scheduler.StartTask(env.taskID); err != nil {
			t.Fatalf("restart: %v", err)
		}
		waitForMarker(t, env.dir, env.taskID, restartSecondSQL)
		if err := env.scheduler.StopTask(env.taskID); err != nil {
			t.Fatalf("second stop: %v", err)
		}
		waitTaskState(t, env.scheduler, env.taskID, tasks.StateStopped)
		if starts := env.syncer.positions(); len(starts) < 2 {
			t.Fatalf("the second start never reached the dump: %+v", starts)
		}
		env.assertStillMissing("an operator Start")

		// The file comes back: the row was kept, so the next pass closes EVAL.
		env.sealed("mysql-bin.000002", time.Now().Add(-time.Hour))
		env.evaluate(1)
		if env.missingOpen("mysql-bin.000002") {
			t.Fatal("SEGMENT_MISSING still open after the segment returned")
		}
		if c := env.closes("mysql-bin.000002"); len(c) != 1 || c[0] != health.ReasonEval {
			t.Fatalf("close reasons %v, want one EVAL", c)
		}
	})

	t.Run("takeover on the shared data_dir", func(t *testing.T) {
		env := newRestartEnv(t)
		env.openMissing()
		task, err := env.scheduler.GetTask(env.taskID)
		if err != nil {
			t.Fatal(err)
		}
		// Another process on the same data_dir takes the task at a higher
		// epoch, as after SIGTERM or SIGKILL of the old holder.
		_, endPos := chainBinlogEvents(4, env.eventAt, []namedEvent{
			{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(env.eventAt)},
			{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", restartFirstSQL)},
		})
		next, _ := chainBinlogEvents(endPos, env.eventAt, []namedEvent{
			{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", restartSecondSQL)},
		})
		syncer := &stopResumeSyncer{phases: [][]*goreplication.BinlogEvent{
			append([]*goreplication.BinlogEvent{preambleEvent(0, env.eventAt.Add(-time.Hour))}, next...),
		}}
		runner := NewMySQLRunner(env.dir, WithFileMetaStore(env.catalog), WithCheckpointStore(env.cp))
		runner.fetcher = &fakeSourceMetaFetcher{
			status:     MasterStatus{File: "mysql-bin.000004", Pos: 9000},
			serverUUID: "11111111-1111-1111-1111-111111111111",
		}
		runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
		task.Epoch++
		task.OwnerWorkerID = "node-b"
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx, task) }()
		waitForMarker(t, env.dir, env.taskID, restartSecondSQL)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("takeover Run: %v", err)
		}
		if starts := syncer.positions(); len(starts) != 1 {
			t.Fatalf("takeover starts %+v", starts)
		}
		env.assertStillMissing("a takeover")
	})
}
