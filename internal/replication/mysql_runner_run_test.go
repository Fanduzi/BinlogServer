// Package replication provides module-level functionality for replication.
// input: fake source metadata, fake streamer/syncer, and injected writer/checkpoint doubles
// output: runner-level tests for start selection, LATEST and caught-up FILE_POS at-tip vs catch-up/idle-behind progress, a resolved LATEST file/pos kept across retry with an empty gtid_set, a mid-file format description kept ahead of the first copied event without moving the cursor or the delay sample, checkpoint semantics, error propagation, stop cleanup, a leftover dump thread killed with the current password before the next StartSync, and a fake source whose uuid and GTID probe can change between dump connections An unconfirmed KILL does not call StartSync.
// pos: replication runtime test boundary around mysql runner orchestration
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

type fakeSourceMetaFetcher struct {
	status          MasterStatus
	statusErr       error
	serverUUID      string
	serverUUIDErr   error
	fetchStatusCall int
	fetchUUIDCall   int
	// uuidSeq, when set, is consumed one identity per FetchServerUUID call.
	// The last identity stays so a later call still has an answer.
	uuidSeq []string
	// switchProbe is the GTID subset answer. Nil means the probe is unavailable.
	switchProbe func(ours string) (haveOurs bool, missing string, missingPurged bool, err error)
}

func (f *fakeSourceMetaFetcher) FetchMasterStatus(_ context.Context, _ tasks.SourceConfig) (MasterStatus, error) {
	f.fetchStatusCall++
	if f.statusErr != nil {
		return MasterStatus{}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeSourceMetaFetcher) FetchServerUUID(_ context.Context, _ tasks.SourceConfig) (string, error) {
	f.fetchUUIDCall++
	if f.serverUUIDErr != nil {
		return "", f.serverUUIDErr
	}
	if len(f.uuidSeq) > 0 {
		next := f.uuidSeq[0]
		if len(f.uuidSeq) > 1 {
			f.uuidSeq = f.uuidSeq[1:]
		}
		return next, nil
	}
	return f.serverUUID, nil
}

func (f *fakeSourceMetaFetcher) ProbeSwitchGTID(_ context.Context, _ tasks.SourceConfig, ours string) (bool, string, bool, error) {
	if f.switchProbe == nil {
		return false, "", false, errors.New("probe not set")
	}
	return f.switchProbe(ours)
}

type fakeRunnerCheckpointStore struct {
	loadCheckpoint binlog.Checkpoint
	loadOK         bool
	loadErr        error
	upserts        []binlog.Checkpoint
	upsertErr      error
	onUpsert       func(binlog.Checkpoint) error
}

func (f *fakeRunnerCheckpointStore) LoadCheckpoint(_ context.Context, _ string) (binlog.Checkpoint, bool, error) {
	return f.loadCheckpoint, f.loadOK, f.loadErr
}

func (f *fakeRunnerCheckpointStore) UpsertCheckpoint(_ context.Context, _ string, checkpoint binlog.Checkpoint) error {
	if f.onUpsert != nil {
		if err := f.onUpsert(checkpoint); err != nil {
			return err
		}
	}
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, checkpoint)
	return nil
}

type fakeRunnerProgressReporter struct {
	reports []progressReport
}

type progressReport struct {
	taskID string
	at     time.Time
	file   string
	pos    uint32
	atTip  bool
}

func (f *fakeRunnerProgressReporter) ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32, atTip bool) {
	f.reports = append(f.reports, progressReport{
		taskID: taskID,
		at:     sourceEventAt,
		file:   file,
		pos:    pos,
		atTip:  atTip,
	})
}

type fakeSyncFile struct {
	writeErr  error
	syncErr   error
	writes    [][]byte
	syncCalls int
}

func (f *fakeSyncFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	f.writes = append(f.writes, buf)
	return len(p), nil
}

func (f *fakeSyncFile) Sync() error {
	f.syncCalls++
	return f.syncErr
}

type fakeCloser struct {
	closeCalls int
}

func (f *fakeCloser) Close() error {
	f.closeCalls++
	return nil
}

type streamResult struct {
	event *goreplication.BinlogEvent
	err   error
}

type fakeStreamer struct {
	results          []streamResult
	calls            int
	blockUntilCtx    bool
	getEventReturned chan struct{}
	onCall           func(call int)
}

func (f *fakeStreamer) GetEvent(ctx context.Context) (*goreplication.BinlogEvent, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall(f.calls)
	}
	defer func() {
		if f.getEventReturned != nil {
			select {
			case <-f.getEventReturned:
			default:
				close(f.getEventReturned)
			}
		}
	}()
	if f.blockUntilCtx {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if len(f.results) == 0 {
		return nil, io.EOF
	}
	next := f.results[0]
	f.results = f.results[1:]
	return next.event, next.err
}

type fakeSyncer struct {
	streamer       binlogStreamer
	startPos       gomysql.Position
	startPosCalls  int
	startGTID      gomysql.GTIDSet
	startGTIDCalls int
	startErr       error
	closeCalls     int
	connID         uint32
}

func (f *fakeSyncer) StartSync(pos gomysql.Position) (binlogStreamer, error) {
	f.startPosCalls++
	f.startPos = pos
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.streamer, nil
}

func (f *fakeSyncer) StartSyncGTID(set gomysql.GTIDSet) (binlogStreamer, error) {
	f.startGTIDCalls++
	f.startGTID = set
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.streamer, nil
}

func (f *fakeSyncer) Close() {
	f.closeCalls++
}

func (f *fakeSyncer) LastConnectionID() uint32 {
	return f.connID
}

func newRunnerEvent(logPos uint32) *goreplication.BinlogEvent {
	return newRunnerEventAt(logPos, time.Now())
}

func newRunnerEventAt(logPos uint32, ts time.Time) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.QUERY_EVENT,
			LogPos:    logPos,
			Timestamp: uint32(ts.Unix()),
		},
		RawData: []byte{0x01, 0x02, 0x03},
	}
}

func newRunnerTask(start tasks.StartConfig) tasks.Task {
	return tasks.Task{
		ID:         "task-1",
		ClusterKey: "cluster-a",
		Start:      start,
		Source: tasks.SourceConfig{
			Host:   "127.0.0.1",
			Port:   3306,
			User:   "repl",
			Flavor: "mysql",
		},
	}
}

// TestMySQLRunnerRun_LatestResolvesAndStartsFromMasterStatus 验证 LATEST 会解析为 master status 并从对应位点起跑。
func TestMySQLRunnerRun_LatestResolvesAndStartsFromMasterStatus(t *testing.T) {
	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000123", Pos: 456},
		serverUUID: "srv-uuid-1",
	}
	streamer := &fakeStreamer{
		results: []streamResult{{err: context.Canceled}},
	}
	syncer := &fakeSyncer{streamer: streamer}
	closer := &fakeCloser{}

	runner := &MySQLRunner{
		fetcher: fetcher,
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return closer, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if syncer.startPosCalls != 1 {
		t.Fatalf("expected StartSync called once, got %d", syncer.startPosCalls)
	}
	if syncer.startPos.Name != "mysql-bin.000123" || syncer.startPos.Pos != 456 {
		t.Fatalf("unexpected start position: %+v", syncer.startPos)
	}
	if closer.closeCalls != 1 {
		t.Fatalf("expected writer closer called once, got %d", closer.closeCalls)
	}
}

// TestMySQLRunnerRun_LatestRetryKeepsResolvedAnchor 验证 LATEST 一旦解析出 file/pos，
// 源库宕机后的重试从该位点续，不再按新的 SHOW MASTER STATUS 开拉。gtid_set 保持为空。
func TestMySQLRunnerRun_LatestRetryKeepsResolvedAnchor(t *testing.T) {
	const (
		anchorFile = "mysql-bin.000010"
		anchorPos  = uint32(197)
		laterFile  = "mysql-bin.000011"
		laterPos   = uint32(1193)
	)

	runTwice := func(t *testing.T, store *memCheckpointStore) *fakeSyncer {
		t.Helper()
		fetcher := &fakeSourceMetaFetcher{
			status:     MasterStatus{File: anchorFile, Pos: anchorPos},
			serverUUID: "srv-uuid-1",
		}
		streamer := &fakeStreamer{results: []streamResult{{err: io.ErrUnexpectedEOF}}}
		syncer := &fakeSyncer{streamer: streamer}
		reporter := &fakeRunnerProgressReporter{}
		runner := newTestRunner(t, fetcher, syncer, reporter)
		runner.dataDir = t.TempDir()
		if store != nil {
			runner.checkpointStore = store
		}
		task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})

		err := runner.Run(context.Background(), task)
		if err == nil || !tasks.IsSourceUnreachable(err) {
			t.Fatalf("first run err=%v, want SOURCE_UNREACHABLE before any event", err)
		}
		if len(reporter.reports) == 0 || !reporter.reports[0].atTip || reporter.reports[0].file != anchorFile || reporter.reports[0].pos != anchorPos {
			t.Fatalf("first connect reports %+v, want at-tip %s:%d", reporter.reports, anchorFile, anchorPos)
		}
		if syncer.startGTIDCalls != 0 {
			t.Fatalf("LATEST anchor opened a GTID dump: %+v", syncer.startGTID)
		}

		fetcher.status = MasterStatus{File: laterFile, Pos: laterPos}
		streamer.results = []streamResult{{err: context.Canceled}}
		err = runner.Run(context.Background(), task)
		if err != nil {
			t.Fatalf("retry Run: %v", err)
		}
		if syncer.startPos.Name != anchorFile || syncer.startPos.Pos != anchorPos {
			t.Fatalf("retry StartSync %+v, want %s:%d (not %s:%d)", syncer.startPos, anchorFile, anchorPos, laterFile, laterPos)
		}
		if syncer.startGTIDCalls != 0 {
			t.Fatalf("retry opened a GTID dump: %+v", syncer.startGTID)
		}
		return syncer
	}

	t.Run("checkpoint", func(t *testing.T) {
		store := &memCheckpointStore{}
		runTwice(t, store)
		store.mu.Lock()
		defer store.mu.Unlock()
		if !store.ok || store.cp.File != anchorFile || store.cp.Pos != anchorPos || store.cp.GTIDSet != "" {
			t.Fatalf("anchor checkpoint %+v ok=%v, want %s:%d with empty gtid_set", store.cp, store.ok, anchorFile, anchorPos)
		}
	})

	t.Run("no store", func(t *testing.T) {
		runTwice(t, nil)
	})

	t.Run("existing gtid checkpoint stays", func(t *testing.T) {
		const gtid = "24bc785e-9a61-11e1-8a5d-080027635ef5:1-20"
		store := &memCheckpointStore{
			cp: binlog.Checkpoint{File: anchorFile, Pos: anchorPos, GTIDSet: gtid},
			ok: true,
		}
		fetcher := &fakeSourceMetaFetcher{
			status:     MasterStatus{File: laterFile, Pos: laterPos},
			serverUUID: "srv-uuid-1",
		}
		streamer := &fakeStreamer{results: []streamResult{{err: context.Canceled}}}
		syncer := &fakeSyncer{streamer: streamer}
		runner := newTestRunner(t, fetcher, syncer, nil)
		runner.dataDir = t.TempDir()
		runner.checkpointStore = store
		err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest}))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if syncer.startPos.Name != anchorFile || syncer.startPos.Pos != anchorPos {
			t.Fatalf("stored checkpoint StartSync %+v, want %s:%d", syncer.startPos, anchorFile, anchorPos)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.cp.File != anchorFile || store.cp.Pos != anchorPos || store.cp.GTIDSet != gtid {
			t.Fatalf("checkpoint %+v, want gtid_set kept", store.cp)
		}
	})
}

func newTestRunner(t *testing.T, fetcher sourceMetaFetcher, syncer binlogSyncer, reporter *fakeRunnerProgressReporter) *MySQLRunner {
	t.Helper()
	return &MySQLRunner{
		fetcher:          fetcher,
		progressReporter: reporter,
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}
}

// TestMySQLRunnerRun_LatestStartReportsAtTipBeforeNextEvent 验证 LATEST 在 StartSync 成功后立刻按 at-tip 上报，
// 不等 2s idle，也不把 dump 握手事件的旧 header 时间当成落后。
func TestMySQLRunnerRun_LatestStartReportsAtTipBeforeNextEvent(t *testing.T) {
	oldEventAt := time.Date(2026, 8, 27, 4, 47, 20, 0, time.UTC)
	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000003", Pos: 56456},
		serverUUID: "srv-uuid-1",
	}
	streamer := &fakeStreamer{
		results: []streamResult{
			{event: newRunnerEventAt(0, oldEventAt)},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	runner := newTestRunner(t, fetcher, syncer, reporter)

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(reporter.reports) == 0 {
		t.Fatal("expected LATEST start to report at-tip progress before the next dump event / idle timeout")
	}
	first := reporter.reports[0]
	if !first.atTip || first.file != "mysql-bin.000003" || first.pos != 56456 {
		t.Fatalf("first progress report %+v, want at-tip mysql-bin.000003:56456 immediately after StartSync", first)
	}
	last := reporter.reports[len(reporter.reports)-1]
	if !last.atTip {
		t.Fatalf("handshake event overwrote at-tip: last report %+v", last)
	}
	if last.file != "mysql-bin.000003" || last.pos != 56456 {
		t.Fatalf("last progress report %+v, want tip file/pos preserved", last)
	}
}

// preambleEvent is the format description MySQL sends before events at the
// requested position. logPos 0 is the documented rewrite; a positive logPos
// still below the dump cursor is the original end_log_pos (126 on MySQL 8).
func preambleEvent(logPos uint32, ts time.Time) *goreplication.BinlogEvent {
	raw := make([]byte, 122)
	raw[0] = 0xfe
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.FORMAT_DESCRIPTION_EVENT,
			LogPos:    logPos,
			Timestamp: uint32(ts.Unix()),
		},
		RawData: raw,
	}
}

// TestMySQLRunnerRun_FilePosAtTipIgnoresDumpPreamble 验证已经在源 tip 的 FILE_POS
// （adopt 续传的最高分段大小）在 StartSync 之后立刻是 at-tip。dump 开头的
// format description 不能写成新字节，也不能用它的 header 时间报 DELAYED。
func TestMySQLRunnerRun_FilePosAtTipIgnoresDumpPreamble(t *testing.T) {
	oldEventAt := time.Now().UTC().Add(-163 * time.Second).Truncate(time.Second)
	for _, logPos := range []uint32{0, 126} {
		t.Run(fmt.Sprintf("logpos_%d", logPos), func(t *testing.T) {
			file := &fakeSyncFile{}
			fetcher := &fakeSourceMetaFetcher{
				status:     MasterStatus{File: "mysql-bin.000006", Pos: 197},
				serverUUID: "srv-uuid-1",
			}
			streamer := &fakeStreamer{
				results: []streamResult{
					{event: preambleEvent(logPos, oldEventAt)},
					{err: context.Canceled},
				},
			}
			syncer := &fakeSyncer{streamer: streamer}
			reporter := &fakeRunnerProgressReporter{}
			runner := &MySQLRunner{
				fetcher:          fetcher,
				progressReporter: reporter,
				newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
					return syncer
				},
				writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
					return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
				},
			}

			err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
				Mode: tasks.StartModeFilePos,
				File: "mysql-bin.000006",
				Pos:  197,
			}))
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if len(reporter.reports) == 0 {
				t.Fatal("caught-up FILE_POS must report at-tip before idle, or the first poll shows DELAYED from the preamble")
			}
			for _, got := range reporter.reports {
				if !got.atTip {
					t.Fatalf("report %+v, want at-tip; preamble header must not be DELAY_EXCEEDS_THRESHOLD", got)
				}
				if !got.at.After(oldEventAt) {
					t.Fatalf("report time %s leaked the format-description header %s", got.at, oldEventAt)
				}
				if got.pos != 197 || got.file != "mysql-bin.000006" {
					t.Fatalf("report %+v, want cursor to stay at mysql-bin.000006:197", got)
				}
			}
			for _, chunk := range file.writes {
				if len(chunk) == 122 {
					t.Fatalf("wrote format-description preamble (%d bytes) into the open segment", len(chunk))
				}
			}
		})
	}
}

// TestMySQLRunnerRun_PreambleDoesNotHideCatchUpLag 验证仍落后 tip 时，preamble 不能
// 改写位点，真实事件的 header 时间仍然是延迟。format description 要写在第一个
// 业务事件前面，mysqlbinlog 才读得开，但不能单独成为续传位点。
func TestMySQLRunnerRun_PreambleDoesNotHideCatchUpLag(t *testing.T) {
	oldEventAt := time.Date(2026, 8, 27, 4, 47, 20, 0, time.UTC)
	file := &fakeSyncFile{}
	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000006", Pos: 500},
		serverUUID: "srv-uuid-1",
	}
	streamer := &fakeStreamer{
		results: []streamResult{
			{event: preambleEvent(126, oldEventAt)},
			{event: newRunnerEventAt(400, oldEventAt)},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	runner := &MySQLRunner{
		fetcher:          fetcher,
		progressReporter: reporter,
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000006",
		Pos:  197,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(reporter.reports) != 1 {
		t.Fatalf("expected only the catch-up event, got %+v", reporter.reports)
	}
	got := reporter.reports[0]
	if got.atTip || got.pos != 400 || !got.at.Equal(oldEventAt) {
		t.Fatalf("catch-up report %+v, want pos=400, old header, not at-tip", got)
	}
	if len(file.writes) != 1 {
		t.Fatalf("writes=%d, want the format description and the event in one write", len(file.writes))
	}
	preamble := preambleEvent(126, oldEventAt).RawData
	event := newRunnerEventAt(400, oldEventAt).RawData
	if !bytes.HasPrefix(file.writes[0], preamble) || !bytes.HasSuffix(file.writes[0], event) {
		t.Fatalf("write %d bytes, want format description then the event", len(file.writes[0]))
	}
}

// TestMySQLRunnerRun_MidFileFormatDescriptionDoesNotRewindResume 验证从源文件
// 中部开始时，format description 落在第一个事件前面，续传位点仍是该事件的
// end_log_pos，不是 description 原来的 126。下一次 start 不再写第二条。
func TestMySQLRunnerRun_MidFileFormatDescriptionDoesNotRewindResume(t *testing.T) {
	dir := t.TempDir()
	eventAt := time.Unix(1_700_000_000, 0).UTC()
	headerAt := eventAt.Add(-2 * time.Hour)
	const sql = "INSERT INTO mid_file_backup VALUES (1)"
	fde := frameBinlogEvent(goreplication.FORMAT_DESCRIPTION_EVENT, formatDescriptionBody(headerAt), 4, headerAt)
	binary.LittleEndian.PutUint32(fde.RawData[13:17], 126)
	fde.Header.LogPos = 126
	query := frameBinlogEvent(goreplication.QUERY_EVENT, queryEventBody("t", sql), 400, eventAt)

	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000006", Pos: 9000},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{
		{event: fde},
		{event: query},
		{err: context.Canceled},
	}}}
	reporter := &fakeRunnerProgressReporter{}
	runner := NewMySQLRunner(dir)
	runner.fetcher = fetcher
	runner.progressReporter = reporter
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000006", Pos: 400})
	task.Epoch = 1
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(reporter.reports) != 1 || reporter.reports[0].pos != query.Header.LogPos || !reporter.reports[0].at.Equal(eventAt) {
		t.Fatalf("reports %+v, want one sample at %d with the query time", reporter.reports, query.Header.LogPos)
	}
	path := filepath.Join(dir, task.ID, "mysql-bin.000006.open.e1")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, binlogMagic) || !bytes.HasPrefix(body[len(binlogMagic):], fde.RawData) {
		t.Fatalf("segment does not start with magic plus the format description (%d bytes)", len(body))
	}
	if bytes.Count(body, fde.RawData) != 1 {
		t.Fatalf("format description copies=%d, want 1", bytes.Count(body, fde.RawData))
	}
	file, pos, ok := binlog.DurableResume(dir, task.ID)
	if !ok || file != "mysql-bin.000006" || pos != query.Header.LogPos {
		t.Fatalf("resume %s:%d ok=%v, want mysql-bin.000006:%d (not 126)", file, pos, ok, query.Header.LogPos)
	}

	next := frameBinlogEvent(goreplication.QUERY_EVENT, queryEventBody("t", "INSERT INTO mid_file_backup VALUES (2)"), pos, eventAt.Add(time.Second))
	again := frameBinlogEvent(goreplication.FORMAT_DESCRIPTION_EVENT, formatDescriptionBody(headerAt), 4, headerAt)
	binary.LittleEndian.PutUint32(again.RawData[13:17], 0)
	again.Header.LogPos = 0
	syncer.streamer = &fakeStreamer{results: []streamResult{
		{event: again},
		{event: next},
		{err: context.Canceled},
	}}
	syncer.startPosCalls = 0
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if syncer.startPos.Name != "mysql-bin.000006" || syncer.startPos.Pos != query.Header.LogPos {
		t.Fatalf("resume StartSync %+v, want %d", syncer.startPos, query.Header.LogPos)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(body, fde.RawData) != 1 || bytes.Contains(body, again.RawData) {
		t.Fatalf("resume rewrote the format description")
	}
	file, pos, ok = binlog.DurableResume(dir, task.ID)
	if !ok || pos != next.Header.LogPos {
		t.Fatalf("resume after second start %s:%d ok=%v, want %d", file, pos, ok, next.Header.LogPos)
	}
}

// TestMySQLRunnerRun_FilePosCatchUpKeepsEventHeaderLag 验证 FILE_POS 追旧事件时不能标成 at-tip。
func TestMySQLRunnerRun_FilePosCatchUpKeepsEventHeaderLag(t *testing.T) {
	oldEventAt := time.Date(2026, 8, 27, 4, 47, 20, 0, time.UTC)
	streamer := &fakeStreamer{
		results: []streamResult{
			{event: newRunnerEventAt(120, oldEventAt)},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	runner := newTestRunner(t, &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"}, syncer, reporter)

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(reporter.reports) != 1 {
		t.Fatalf("expected 1 catch-up progress report, got %+v", reporter.reports)
	}
	got := reporter.reports[0]
	if got.atTip {
		t.Fatalf("FILE_POS catch-up reported at-tip: %+v", got)
	}
	if got.pos != 120 || !got.at.Equal(oldEventAt) {
		t.Fatalf("catch-up report %+v, want pos=120 and old event header time", got)
	}
}

// TestMySQLRunnerRun_IdlePollReportsAtTip 验证 dump 已在 master file/pos 时，2s idle 标 at-tip。
func TestMySQLRunnerRun_IdlePollReportsAtTip(t *testing.T) {
	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000010", Pos: 4},
		serverUUID: "srv-uuid-1",
	}
	streamer := &fakeStreamer{
		results: []streamResult{
			{err: context.DeadlineExceeded},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	runner := newTestRunner(t, fetcher, syncer, reporter)

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if fetcher.fetchStatusCall != 2 {
		t.Fatalf("at-tip FILE_POS checks master at start and again on idle, got %d", fetcher.fetchStatusCall)
	}
	if len(reporter.reports) != 2 || !reporter.reports[0].atTip || !reporter.reports[1].atTip {
		t.Fatalf("start and idle at master file/pos should both report at-tip, got %+v", reporter.reports)
	}
	if reporter.reports[0].file != "mysql-bin.000010" || reporter.reports[0].pos != 4 {
		t.Fatalf("idle at-tip report %+v, want start file/pos", reporter.reports[0])
	}
}

// TestMySQLRunnerRun_IdlePollBehindMasterDoesNotReportAtTip 验证追位点时 2s idle
// 不能把 atTip 粘成 true；落后 master 时 delay 继续按 event time。
func TestMySQLRunnerRun_IdlePollBehindMasterDoesNotReportAtTip(t *testing.T) {
	oldEventAt := time.Date(2026, 8, 27, 4, 47, 20, 0, time.UTC)
	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000020", Pos: 100},
		serverUUID: "srv-uuid-1",
	}
	streamer := &fakeStreamer{
		results: []streamResult{
			{err: context.DeadlineExceeded},
			{event: newRunnerEventAt(120, oldEventAt)},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	runner := newTestRunner(t, fetcher, syncer, reporter)

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if fetcher.fetchStatusCall != 2 {
		t.Fatalf("FILE_POS start and idle should each confirm tip, got %d", fetcher.fetchStatusCall)
	}
	if len(reporter.reports) != 2 {
		t.Fatalf("expected idle + catch-up progress reports, got %+v", reporter.reports)
	}
	idle := reporter.reports[0]
	if idle.atTip {
		t.Fatalf("idle while behind master reported at-tip: %+v", idle)
	}
	if !idle.at.IsZero() {
		t.Fatalf("idle-behind report must not overwrite event time, got %+v", idle)
	}
	if idle.file != "mysql-bin.000010" || idle.pos != 4 {
		t.Fatalf("idle-behind report %+v, want start file/pos", idle)
	}
	got := reporter.reports[1]
	if got.atTip {
		t.Fatalf("atTip stuck true after idle while behind: %+v", got)
	}
	if got.pos != 120 || !got.at.Equal(oldEventAt) {
		t.Fatalf("catch-up report %+v, want pos=120 and old event header time", got)
	}
}

// TestMySQLRunnerRun_IdlePollMasterStatusErrorDoesNotReportAtTip 验证 idle 无法确认 master 位点时保守保持 atTip=false。
func TestMySQLRunnerRun_IdlePollMasterStatusErrorDoesNotReportAtTip(t *testing.T) {
	fetcher := &fakeSourceMetaFetcher{
		statusErr:  errors.New("show master status failed"),
		serverUUID: "srv-uuid-1",
	}
	streamer := &fakeStreamer{
		results: []streamResult{
			{err: context.DeadlineExceeded},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	runner := newTestRunner(t, fetcher, syncer, reporter)

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if fetcher.fetchStatusCall != 2 {
		t.Fatalf("start and idle should each call FetchMasterStatus, got %d", fetcher.fetchStatusCall)
	}
	if len(reporter.reports) != 1 || reporter.reports[0].atTip {
		t.Fatalf("idle with master-status error should not report at-tip, got %+v", reporter.reports)
	}
}

// TestMySQLRunnerRun_FilePosUsesDirectStart 验证 FILE_POS 直接走 StartSync。
func TestMySQLRunnerRun_FilePosUsesDirectStart(t *testing.T) {
	streamer := &fakeStreamer{
		results: []streamResult{{err: context.Canceled}},
	}
	syncer := &fakeSyncer{streamer: streamer}
	runner := &MySQLRunner{
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if syncer.startPosCalls != 1 || syncer.startPos.Name != "mysql-bin.000010" || syncer.startPos.Pos != 4 {
		t.Fatalf("unexpected start position: %+v calls=%d", syncer.startPos, syncer.startPosCalls)
	}
}

// TestMySQLRunnerRun_GTIDUsesGTIDStart 验证 GTID 起点走 StartSyncGTID。
func TestMySQLRunnerRun_GTIDUsesGTIDStart(t *testing.T) {
	streamer := &fakeStreamer{
		results: []streamResult{{err: context.Canceled}},
	}
	syncer := &fakeSyncer{streamer: streamer}
	gtidSet := "24BC785E-9A61-11E1-8A5D-080027635EF5:1-10"
	runner := &MySQLRunner{
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode:    tasks.StartModeGTID,
		GTIDSet: gtidSet,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if syncer.startGTIDCalls != 1 {
		t.Fatalf("expected StartSyncGTID called once, got %d", syncer.startGTIDCalls)
	}
	if syncer.startGTID == nil || !strings.EqualFold(syncer.startGTID.String(), gtidSet) {
		t.Fatalf("unexpected GTID start: %v", syncer.startGTID)
	}
}

// TestMySQLRunnerRun_AdvancesCheckpointOnlyAfterFlush 验证只有 flush 成功后才推进 checkpoint。
func TestMySQLRunnerRun_AdvancesCheckpointOnlyAfterFlush(t *testing.T) {
	file := &fakeSyncFile{}
	closer := &fakeCloser{}
	store := &fakeRunnerCheckpointStore{
		onUpsert: func(checkpoint binlog.Checkpoint) error {
			if file.syncCalls == 0 {
				t.Fatalf("checkpoint upsert happened before sync for checkpoint %+v", checkpoint)
			}
			return nil
		},
	}
	streamer := &fakeStreamer{
		results: []streamResult{
			{event: newRunnerEvent(120)},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	reporter := &fakeRunnerProgressReporter{}
	runner := &MySQLRunner{
		fetcher:          &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore:  store,
		progressReporter: reporter,
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			return closer, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("expected 1 checkpoint upsert, got %d", len(store.upserts))
	}
	if store.upserts[0].File != "mysql-bin.000010" || store.upserts[0].Pos != 120 {
		t.Fatalf("unexpected checkpoint upsert: %+v", store.upserts[0])
	}
	if len(reporter.reports) != 1 || reporter.reports[0].pos != 120 {
		t.Fatalf("unexpected progress reports: %+v", reporter.reports)
	}
	if closer.closeCalls != 1 {
		t.Fatalf("expected writer closer called once, got %d", closer.closeCalls)
	}
}

// TestMySQLRunnerRun_WriteFailureDoesNotAdvanceCheckpoint 验证写入失败不会错误推进 checkpoint。
func TestMySQLRunnerRun_WriteFailureDoesNotAdvanceCheckpoint(t *testing.T) {
	wantErr := errors.New("append failed")
	store := &fakeRunnerCheckpointStore{}
	streamer := &fakeStreamer{
		results: []streamResult{{event: newRunnerEvent(120)}},
	}
	syncer := &fakeSyncer{streamer: streamer}
	runner := &MySQLRunner{
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{writeErr: wantErr}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
	if len(store.upserts) != 0 {
		t.Fatalf("expected no checkpoint upsert, got %d", len(store.upserts))
	}
}

// TestMySQLRunnerRun_FlushFailureDoesNotAdvanceCheckpoint 验证 flush 失败不会错误推进 checkpoint。
func TestMySQLRunnerRun_FlushFailureDoesNotAdvanceCheckpoint(t *testing.T) {
	wantErr := errors.New("sync failed")
	store := &fakeRunnerCheckpointStore{}
	streamer := &fakeStreamer{
		results: []streamResult{{event: newRunnerEvent(120)}},
	}
	syncer := &fakeSyncer{streamer: streamer}
	runner := &MySQLRunner{
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{syncErr: wantErr}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
	if len(store.upserts) != 0 {
		t.Fatalf("expected no checkpoint upsert, got %d", len(store.upserts))
	}
}

// TestMySQLRunnerRun_PropagatesUpstreamReadError 验证上游读取错误会原样向上返回。
func TestMySQLRunnerRun_PropagatesUpstreamReadError(t *testing.T) {
	wantErr := errors.New("stream read failed")
	streamer := &fakeStreamer{
		results: []streamResult{{err: wantErr}},
	}
	syncer := &fakeSyncer{streamer: streamer}
	runner := &MySQLRunner{
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
}

// TestMySQLRunnerRun_ContextCancelStopsAndReleasesResources 验证 context cancel 后主循环退出并释放资源。
func TestMySQLRunnerRun_ContextCancelStopsAndReleasesResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamer := &fakeStreamer{
		blockUntilCtx:    true,
		getEventReturned: make(chan struct{}),
	}
	syncer := &fakeSyncer{streamer: streamer}
	closer := &fakeCloser{}
	runner := &MySQLRunner{
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return closer, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, newRunnerTask(tasks.StartConfig{
			Mode: tasks.StartModeFilePos,
			File: "mysql-bin.000010",
			Pos:  4,
		}))
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on context cancel, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not exit after context cancel")
	}

	select {
	case <-streamer.getEventReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("streamer GetEvent did not return after context cancel")
	}
	if syncer.closeCalls != 1 {
		t.Fatalf("expected syncer.Close called once, got %d", syncer.closeCalls)
	}
	if closer.closeCalls != 1 {
		t.Fatalf("expected writer closer called once, got %d", closer.closeCalls)
	}
}

// TestMySQLRunnerRun_EmptyEventDoesNotAdvanceCheckpoint 验证空事件不会误推进 checkpoint。
func TestMySQLRunnerRun_EmptyEventDoesNotAdvanceCheckpoint(t *testing.T) {
	store := &fakeRunnerCheckpointStore{}
	reporter := &fakeRunnerProgressReporter{}
	streamer := &fakeStreamer{
		results: []streamResult{
			{event: nil},
			{err: context.Canceled},
		},
	}
	syncer := &fakeSyncer{streamer: streamer}
	runner := &MySQLRunner{
		fetcher:          &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore:  store,
		progressReporter: reporter,
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
	}

	err := runner.Run(context.Background(), newRunnerTask(tasks.StartConfig{
		Mode: tasks.StartModeFilePos,
		File: "mysql-bin.000010",
		Pos:  4,
	}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(store.upserts) != 0 {
		t.Fatalf("expected no checkpoint upsert, got %d", len(store.upserts))
	}
	if len(reporter.reports) != 0 {
		t.Fatalf("expected no progress report, got %d", len(reporter.reports))
	}
}

// TestMySQLRunnerRun_KillsLeftoverDumpBeforeNextStart 验证上一次 Close 用旧密码
// KILL 失败后，下一次 StartSync 之前会用新密码杀掉那个连接号。
func TestMySQLRunnerRun_KillsLeftoverDumpBeforeNextStart(t *testing.T) {
	var order []string
	syncer := &fakeSyncer{
		streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}, {err: context.Canceled}}},
		connID:   77,
	}
	runner := &MySQLRunner{
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			order = append(order, "start")
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
		killDump: func(source tasks.SourceConfig, id uint32) error {
			order = append(order, fmt.Sprintf("kill:%s:%d", source.Password, id))
			if source.Password != "new" {
				return errors.New("access denied")
			}
			return nil
		},
	}
	start := tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000010", Pos: 4}
	first := newRunnerTask(start)
	first.Source.Password = "old"
	if err := runner.Run(context.Background(), first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	second := newRunnerTask(start)
	second.Source.Password = "new"
	if err := runner.Run(context.Background(), second); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got := strings.Join(order, ",")
	want := "start,kill:old:77,kill:new:77,start,kill:new:77"
	if got != want {
		t.Fatalf("order %s, want %s", got, want)
	}
}

func TestMySQLRunnerRun_UnconfirmedKillDoesNotOpenDump(t *testing.T) {
	starts := 0
	syncer := &fakeSyncer{
		streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}},
		connID:   77,
	}
	runner := &MySQLRunner{
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			starts++
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
		killDump: func(tasks.SourceConfig, uint32) error {
			return errors.New("dial tcp: i/o timeout")
		},
	}
	start := tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000010", Pos: 4}
	first := newRunnerTask(start)
	if err := runner.Run(context.Background(), first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	second := newRunnerTask(start)
	err := runner.Run(context.Background(), second)
	if err == nil || !strings.Contains(err.Error(), "still open") {
		t.Fatalf("second run err %v", err)
	}
	if starts != 1 {
		t.Fatalf("StartSync calls %d, want 1", starts)
	}
}

// TestMySQLRunnerRun_CloseKillsDumpWithBoundPassword 验证 Close 用调度器绑上的当前密码，
// 而不是打开 dump 时的旧密码。
func TestMySQLRunnerRun_CloseKillsDumpWithBoundPassword(t *testing.T) {
	var killed string
	syncer := &fakeSyncer{
		streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}},
		connID:   88,
	}
	runner := &MySQLRunner{
		fetcher: &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		newSyncer: func(_ goreplication.BinlogSyncerConfig) binlogSyncer {
			return syncer
		},
		writerOpener: func(_ tasks.Task, fileName string, initialPos uint32) (io.Closer, *binlog.Writer, string, error) {
			file := &fakeSyncFile{}
			return &fakeCloser{}, binlog.NewWriter(file, binlog.Checkpoint{File: fileName, Pos: initialPos}), t.TempDir() + "/" + fileName, nil
		},
		killDump: func(source tasks.SourceConfig, id uint32) error {
			killed = fmt.Sprintf("%s:%d", source.Password, id)
			return nil
		},
	}
	runner.BindDumpSource(func(string) (tasks.SourceConfig, bool) {
		return tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "current"}, true
	})
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000010", Pos: 4})
	task.Source.Password = "opened-with"
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("run: %v", err)
	}
	if killed != "current:88" {
		t.Fatalf("killed %s, want current:88", killed)
	}
}

func TestBuildSyncerConfigRequestsHeartbeat(t *testing.T) {
	cfg := buildSyncerConfig(newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest}))
	if cfg.HeartbeatPeriod != dumpHeartbeatPeriod || dumpHeartbeatPeriod != 15*time.Second {
		t.Fatalf("heartbeat %s", cfg.HeartbeatPeriod)
	}
}

func TestReleaseDumpConnReportsUnreachableKill(t *testing.T) {
	var got error
	var id uint32
	runner := &MySQLRunner{
		killDump: func(tasks.SourceConfig, uint32) error {
			return errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")
		},
	}
	runner.BindDumpCleanup(func(_ string, _ tasks.SourceConfig, connectionID uint32, killErr error) {
		id = connectionID
		got = killErr
	})
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	runner.releaseDumpConn(task, 42)
	if id != 42 || got == nil {
		t.Fatalf("id=%d err=%v", id, got)
	}
	if runner.notedDumpConn(task.ID) != 42 {
		t.Fatal("unreachable kill must keep the connection id for the next start")
	}
}
