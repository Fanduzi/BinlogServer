// Package replication provides module-level functionality for replication.
// input: scheduler stop/start, in-memory lease, optional checkpoint store, and a scripted binlog stream
// output: proof that stop then start appends source events from the previous durable end for standalone and catalog tasks, that mysqlbinlog or the binlog parser can read that boundary, and that adopt and empty-disk takeover stay on their existing positions
// pos: operator-path regression for contiguous resume after stop
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestStopThenStartCapturesEventsAcrossBoundary(t *testing.T) {
	shapes := []struct {
		name    string
		catalog bool
	}{
		{name: "standalone", catalog: false},
		{name: "catalog", catalog: true},
	}
	layouts := []struct {
		name       string
		masterPos  uint32
		includeFDE bool
	}{
		{name: "new_file", masterPos: 4, includeFDE: true},
		{name: "mid_file", masterPos: 400, includeFDE: false},
	}
	for _, shape := range shapes {
		for _, layout := range layouts {
			t.Run(shape.name+"/"+layout.name, func(t *testing.T) {
				stopThenStart(t, shape.catalog, layout.masterPos, layout.includeFDE)
			})
		}
	}
}

func stopThenStart(t *testing.T, catalog bool, masterPos uint32, includeFDE bool) {
	t.Helper()
	dir := t.TempDir()
	beforeSQL := "INSERT INTO resume_boundary VALUES (1)"
	afterSQL := "INSERT INTO resume_boundary VALUES (2)"
	eventAt := time.Unix(1_700_000_000, 0).UTC()

	var first []namedEvent
	if includeFDE {
		first = append(first, namedEvent{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)})
	}
	first = append(first, namedEvent{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", beforeSQL)})
	firstEvents, endPos := chainBinlogEvents(masterPos, eventAt, first)
	secondEvents, _ := chainBinlogEvents(endPos, eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", afterSQL)},
	})
	phase2 := append([]*goreplication.BinlogEvent{preambleEvent(0, eventAt.Add(-time.Hour))}, secondEvents...)

	fetcher := &movableMaster{status: MasterStatus{File: "mysql-bin.000001", Pos: masterPos}, uuid: "11111111-1111-1111-1111-111111111111"}
	syncer := &stopResumeSyncer{phases: [][]*goreplication.BinlogEvent{firstEvents, phase2}}
	reporter := &fakeRunnerProgressReporter{}
	runner := NewMySQLRunner(dir)
	runner.fetcher = fetcher
	runner.progressReporter = reporter
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	if catalog {
		runner.checkpointStore = &memCheckpointStore{}
	}

	released := make(chan struct{})
	var releaseOnce sync.Once
	leases := &releaseNotifyingLease{inner: tasks.NewMemoryLease(), released: released, once: &releaseOnce}
	scheduler := tasks.NewScheduler(
		tasks.WithDataDir(dir),
		tasks.WithRunner(runner),
		tasks.WithClusterLeaseManager(leases),
		tasks.WithClusterWorkerID("standalone"),
		tasks.WithClusterLease(time.Minute, time.Minute, time.Minute),
	)
	task, err := scheduler.CreateTaskFromSpec("resume", "resume-key", &tasks.SourceConfig{
		Host:     "127.0.0.1",
		Port:     3306,
		User:     "repl",
		Password: "secret",
		Flavor:   "mysql",
	}, &tasks.StartConfig{Mode: tasks.StartModeLatest}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForMarker(t, dir, task.ID, beforeSQL)
	if err := scheduler.StopTask(task.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitTaskState(t, scheduler, task.ID, tasks.StateStopped)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not release the lease")
	}
	reportsBeforeResume := len(reporter.reports)

	firstPath := singleSegment(t, dir, task.ID)
	firstInfo, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if masterPos != 4 && uint32(firstInfo.Size()) == endPos {
		t.Fatalf("mid-file segment size %d unexpectedly equals log pos %d", firstInfo.Size(), endPos)
	}

	fetcher.set(MasterStatus{File: "mysql-bin.000001", Pos: 9000})
	if err := scheduler.StartTask(task.ID); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitForMarker(t, dir, task.ID, afterSQL)
	if err := scheduler.StopTask(task.ID); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	waitTaskState(t, scheduler, task.ID, tasks.StateStopped)

	starts := syncer.positions()
	if len(starts) < 2 {
		t.Fatalf("StartSync calls: %+v", starts)
	}
	if starts[0].Name != "mysql-bin.000001" || starts[0].Pos != masterPos {
		t.Fatalf("first start %+v, want mysql-bin.000001:%d", starts[0], masterPos)
	}
	if starts[1].Pos != endPos || starts[1].Name != "mysql-bin.000001" {
		t.Fatalf("resume start %+v, want end pos %d (not master 9000, not 4, not size %d)", starts[1], endPos, firstInfo.Size())
	}
	if reportsBeforeResume >= len(reporter.reports) {
		t.Fatal("resume produced no progress report")
	}
	for _, report := range reporter.reports[reportsBeforeResume:] {
		if report.atTip {
			t.Fatalf("catch-up after stop reported at-tip: %+v", report)
		}
	}

	path := singleSegment(t, dir, task.ID)
	events := replayBinlogFile(t, path)
	assertContiguousQueries(t, events, masterPos, beforeSQL, afterSQL, includeFDE)
	if includeFDE {
		replayWithMySQLBinlog(t, path, beforeSQL, afterSQL)
	}
}

func TestResumeAtTipStillReportsAtTip(t *testing.T) {
	dir := t.TempDir()
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	events, endPos := chainBinlogEvents(4, eventAt, []namedEvent{
		{typ: goreplication.FORMAT_DESCRIPTION_EVENT, body: formatDescriptionBody(eventAt)},
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", "INSERT INTO resume_boundary VALUES (1)")},
	})
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(taskDir, "mysql-bin.000001.open.e1")
	writeBinlogSegment(t, seedPath, events)
	seeded, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	oldEventAt := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	streamer := &fakeStreamer{results: []streamResult{
		{event: preambleEvent(0, oldEventAt)},
		{event: preambleEvent(126, oldEventAt)},
		{err: context.Canceled},
	}}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000001", Pos: endPos}, ok: true}
	runner := NewMySQLRunner(dir, WithCheckpointStore(store))
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000001", Pos: endPos},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.progressReporter = reporter
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }

	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 2
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if syncer.startPos.Name != "mysql-bin.000001" || syncer.startPos.Pos != endPos {
		t.Fatalf("caught-up resume started at %+v, want pos %d", syncer.startPos, endPos)
	}
	if len(reporter.reports) == 0 || !reporter.reports[0].atTip || reporter.reports[0].pos != endPos {
		t.Fatalf("caught-up resume reports %+v, want at-tip pos %d", reporter.reports, endPos)
	}
	for _, got := range reporter.reports {
		if !got.atTip || !got.at.After(oldEventAt) {
			t.Fatalf("preamble changed at-tip report %+v", got)
		}
	}
	continued := filepath.Join(taskDir, "mysql-bin.000001.open.e2")
	got, err := os.ReadFile(continued)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seeded) {
		t.Fatalf("caught-up resume changed segment bytes: got %d seeded %d", len(got), len(seeded))
	}
	if _, err := os.Stat(seedPath); !os.IsNotExist(err) {
		t.Fatalf("old epoch still present: %v", err)
	}
}

func TestTakeoverWithoutLocalSegmentStillRebuildsFromPos4(t *testing.T) {
	dir := t.TempDir()
	streamer := &fakeStreamer{results: []streamResult{{err: context.Canceled}}}
	syncer := &fakeSyncer{streamer: streamer}
	store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000123", Pos: 789}, ok: true}
	runner := NewMySQLRunner(dir, WithCheckpointStore(store))
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000123", Pos: 9000},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 2
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if syncer.startPos.Name != "mysql-bin.000123" || syncer.startPos.Pos != 4 {
		t.Fatalf("empty-disk takeover started at %+v, want mysql-bin.000123:4", syncer.startPos)
	}
}

func TestAdoptKeepsSavedFilePosWhenSourceMoved(t *testing.T) {
	dir := t.TempDir()
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	events, logPos := chainBinlogEvents(500, eventAt, []namedEvent{
		{typ: goreplication.QUERY_EVENT, body: queryEventBody("t", "INSERT INTO resume_boundary VALUES (1)")},
	})
	taskDir := filepath.Join(dir, "task-1")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(taskDir, "mysql-bin.000004.open.e2")
	writeBinlogSegment(t, openPath, events)
	seeded, err := os.ReadFile(openPath)
	if err != nil {
		t.Fatal(err)
	}
	if uint32(len(seeded)) == logPos {
		t.Fatalf("fixture size %d equals log pos; adopt would not be distinguishable", len(seeded))
	}

	streamer := &fakeStreamer{results: []streamResult{{err: context.Canceled}}}
	syncer := &fakeSyncer{streamer: streamer}
	runner := NewMySQLRunner(dir)
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000004", Pos: 9000},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000004",
		Pos:  uint32(len(seeded)),
	})
	task.Epoch = 3
	task.KeepLocalSegments = true
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if syncer.startPos.Pos != uint32(len(seeded)) || syncer.startPos.Name != "mysql-bin.000004" {
		t.Fatalf("adopt start %+v, want file size %d not log pos %d or master 9000", syncer.startPos, len(seeded), logPos)
	}
	got, err := os.ReadFile(openPath)
	if err != nil || !bytes.Equal(got, seeded) {
		t.Fatalf("adopt rewrote the previous segment: %v", err)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "mysql-bin.000004.open.e3")); err != nil {
		t.Fatalf("adopt did not open the next epoch: %v", err)
	}
}

type namedEvent struct {
	typ  goreplication.EventType
	body []byte
}

func chainBinlogEvents(start uint32, ts time.Time, parts []namedEvent) ([]*goreplication.BinlogEvent, uint32) {
	pos := start
	out := make([]*goreplication.BinlogEvent, 0, len(parts))
	for _, part := range parts {
		ev := frameBinlogEvent(part.typ, part.body, pos, ts)
		pos = ev.Header.LogPos
		out = append(out, ev)
	}
	return out, pos
}

func frameBinlogEvent(eventType goreplication.EventType, body []byte, startPos uint32, ts time.Time) *goreplication.BinlogEvent {
	size := uint32(goreplication.EventHeaderSize + len(body))
	logPos := startPos + size
	raw := make([]byte, size)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(ts.Unix()))
	raw[4] = byte(eventType)
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint32(raw[13:17], logPos)
	copy(raw[goreplication.EventHeaderSize:], body)
	return &goreplication.BinlogEvent{
		RawData: raw,
		Header: &goreplication.EventHeader{
			Timestamp: uint32(ts.Unix()),
			EventType: eventType,
			ServerID:  1,
			EventSize: size,
			LogPos:    logPos,
		},
	}
}

func formatDescriptionBody(ts time.Time) []byte {
	body := make([]byte, 2+50+4+1+40)
	binary.LittleEndian.PutUint16(body[0:2], 4)
	copy(body[2:52], []byte("5.5.0-log"))
	binary.LittleEndian.PutUint32(body[52:56], uint32(ts.Unix()))
	body[56] = byte(goreplication.EventHeaderSize)
	body[58] = 13 // QUERY_EVENT post-header length, index is event type - 1
	return body
}

func queryEventBody(schema, query string) []byte {
	body := make([]byte, 13+len(schema)+1+len(query))
	body[8] = byte(len(schema))
	copy(body[13:], schema)
	body[13+len(schema)] = 0
	copy(body[14+len(schema):], query)
	return body
}

func writeBinlogSegment(t *testing.T, path string, events []*goreplication.BinlogEvent) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(binlogMagic)
	for _, ev := range events {
		buf.Write(ev.RawData)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

type replayedEvent struct {
	pos   uint32
	size  uint32
	typ   goreplication.EventType
	query string
}

func replayBinlogFile(t *testing.T, path string) []replayedEvent {
	t.Helper()
	parser := goreplication.NewBinlogParser()
	var events []replayedEvent
	err := parser.ParseFile(path, 0, func(ev *goreplication.BinlogEvent) error {
		item := replayedEvent{pos: ev.Header.LogPos, size: ev.Header.EventSize, typ: ev.Header.EventType}
		if q, ok := ev.Event.(*goreplication.QueryEvent); ok {
			item.query = string(q.Query)
		}
		events = append(events, item)
		return nil
	})
	if err != nil {
		t.Fatalf("replay %s: %v", path, err)
	}
	return events
}

func assertContiguousQueries(t *testing.T, events []replayedEvent, firstStart uint32, beforeSQL, afterSQL string, wantFDE bool) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("replay produced no events")
	}
	var prev uint32
	fde := 0
	before := 0
	after := 0
	var beforeEnd uint32
	var afterStart uint32
	for i, ev := range events {
		if ev.size == 0 || ev.pos < ev.size {
			t.Fatalf("event %+v has no start position", ev)
		}
		start := ev.pos - ev.size
		if i == 0 {
			if start != firstStart {
				t.Fatalf("first event starts at %d, want %d", start, firstStart)
			}
		} else if start != prev {
			t.Fatalf("event %d starts at %d, previous event ended at %d", i, start, prev)
		}
		prev = ev.pos
		if ev.typ == goreplication.FORMAT_DESCRIPTION_EVENT {
			fde++
		}
		if ev.query == beforeSQL {
			before++
			beforeEnd = ev.pos
		}
		if ev.query == afterSQL {
			after++
			afterStart = start
		}
	}
	if before != 1 || after != 1 {
		t.Fatalf("before=%d after=%d events=%+v", before, after, events)
	}
	if afterStart != beforeEnd {
		t.Fatalf("stop boundary: next event starts at %d, previous ended at %d", afterStart, beforeEnd)
	}
	if wantFDE && fde != 1 {
		t.Fatalf("format description count %d, want 1", fde)
	}
	if !wantFDE && fde != 0 {
		t.Fatalf("mid-file replay wrote a format description")
	}
}

func replayWithMySQLBinlog(t *testing.T, path, beforeSQL, afterSQL string) {
	t.Helper()
	bin, err := exec.LookPath("mysqlbinlog")
	if err != nil {
		t.Log("mysqlbinlog not installed; parser replay covered the boundary")
		return
	}
	cmd := exec.Command(bin, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mysqlbinlog %s: %v\n%s", path, err, out)
	}
	if !bytes.Contains(out, []byte(beforeSQL)) || !bytes.Contains(out, []byte(afterSQL)) {
		t.Fatalf("mysqlbinlog output missing boundary statements:\n%s", out)
	}
	if bytes.Count(out, []byte(beforeSQL)) != 1 || bytes.Count(out, []byte(afterSQL)) != 1 {
		t.Fatalf("mysqlbinlog duplicated a boundary statement:\n%s", out)
	}
}

func singleSegment(t *testing.T, dir, taskID string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, taskID, "mysql-bin.000001*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("segments: %v", matches)
	}
	return matches[0]
}

func waitForMarker(t *testing.T, dir, taskID, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, taskID, "*"))
		for _, match := range matches {
			body, err := os.ReadFile(match)
			if err == nil && bytes.Contains(body, []byte(marker)) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("marker %q was not durable", marker)
}

func waitTaskState(t *testing.T, scheduler *tasks.Scheduler, id string, state tasks.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got tasks.Task
	var err error
	for time.Now().Before(deadline) {
		got, err = scheduler.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task state %s, want %s", got.State, state)
}

type movableMaster struct {
	mu     sync.Mutex
	status MasterStatus
	uuid   string
}

func (m *movableMaster) set(status MasterStatus) {
	m.mu.Lock()
	m.status = status
	m.mu.Unlock()
}

func (m *movableMaster) FetchMasterStatus(context.Context, tasks.SourceConfig) (MasterStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, nil
}

func (m *movableMaster) FetchServerUUID(context.Context, tasks.SourceConfig) (string, error) {
	return m.uuid, nil
}

type stopResumeSyncer struct {
	mu     sync.Mutex
	starts []gomysql.Position
	phases [][]*goreplication.BinlogEvent
}

func (s *stopResumeSyncer) StartSync(pos gomysql.Position) (binlogStreamer, error) {
	s.mu.Lock()
	s.starts = append(s.starts, pos)
	idx := len(s.starts) - 1
	var events []*goreplication.BinlogEvent
	if idx < len(s.phases) {
		events = append([]*goreplication.BinlogEvent(nil), s.phases[idx]...)
	}
	s.mu.Unlock()
	return &thenBlockStreamer{events: events}, nil
}

func (s *stopResumeSyncer) StartSyncGTID(gomysql.GTIDSet) (binlogStreamer, error) {
	return nil, errors.New("gtid start is not used")
}

func (s *stopResumeSyncer) Close() {}

func (s *stopResumeSyncer) positions() []gomysql.Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gomysql.Position(nil), s.starts...)
}

type thenBlockStreamer struct {
	mu     sync.Mutex
	events []*goreplication.BinlogEvent
}

func (s *thenBlockStreamer) GetEvent(ctx context.Context) (*goreplication.BinlogEvent, error) {
	s.mu.Lock()
	if len(s.events) > 0 {
		ev := s.events[0]
		s.events = s.events[1:]
		s.mu.Unlock()
		return ev, nil
	}
	s.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type memCheckpointStore struct {
	mu sync.Mutex
	cp binlog.Checkpoint
	ok bool
}

func (m *memCheckpointStore) LoadCheckpoint(context.Context, string) (binlog.Checkpoint, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cp, m.ok, nil
}

func (m *memCheckpointStore) UpsertCheckpoint(_ context.Context, _ string, checkpoint binlog.Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cp = checkpoint
	m.ok = true
	return nil
}

type releaseNotifyingLease struct {
	inner    *tasks.MemoryLease
	released chan struct{}
	once     *sync.Once
}

func (l *releaseNotifyingLease) Acquire(ctx context.Context, taskID, workerID string, ttl time.Duration) (int64, bool, error) {
	return l.inner.Acquire(ctx, taskID, workerID, ttl)
}

func (l *releaseNotifyingLease) Renew(ctx context.Context, taskID, workerID string, epoch int64, now time.Time, ttl time.Duration) (bool, error) {
	return l.inner.Renew(ctx, taskID, workerID, epoch, now, ttl)
}

func (l *releaseNotifyingLease) Release(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	ok, err := l.inner.Release(ctx, taskID, workerID, epoch)
	if ok {
		l.once.Do(func() { close(l.released) })
	}
	return ok, err
}

func (l *releaseNotifyingLease) Verify(ctx context.Context, taskID, workerID string, epoch int64) (bool, error) {
	return l.inner.Verify(ctx, taskID, workerID, epoch)
}
