// Package replication provides module-level functionality for replication.
// input: fake source metadata, fake streamer/syncer, and injected writer/checkpoint doubles
// output: runner-level tests for start selection, checkpoint semantics, error propagation, and stop cleanup
// pos: replication runtime test boundary around mysql runner orchestration
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"io"
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
	return f.serverUUID, nil
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
}

func (f *fakeRunnerProgressReporter) ReportReplicationProgress(taskID string, sourceEventAt time.Time, file string, pos uint32) {
	f.reports = append(f.reports, progressReport{
		taskID: taskID,
		at:     sourceEventAt,
		file:   file,
		pos:    pos,
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
	results         []streamResult
	calls           int
	blockUntilCtx   bool
	getEventReturned chan struct{}
}

func (f *fakeStreamer) GetEvent(ctx context.Context) (*goreplication.BinlogEvent, error) {
	f.calls++
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
	streamer      binlogStreamer
	startPos      gomysql.Position
	startPosCalls int
	startGTID     gomysql.GTIDSet
	startGTIDCalls int
	startErr      error
	closeCalls    int
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

func newRunnerEvent(logPos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.QUERY_EVENT,
			LogPos:    logPos,
			Timestamp: uint32(time.Now().Unix()),
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
		fetcher:         &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"},
		checkpointStore: store,
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
		blockUntilCtx:   true,
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
