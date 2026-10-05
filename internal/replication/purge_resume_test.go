// Package replication provides module-level functionality for replication.
// input: a sealed uploaded segment, a failing then succeeding object delete, and a rotate on the replication runner
// output: proof that OBJECT_PURGE_FAILED keeps the local file and checksum, and the next successful purge continues past that rotate
// pos: regression coverage for resume after a retention object-delete failure
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// TestRotateAfterObjectPurgeFailureDoesNotReseal is the #145 regression.
// A rotate seals the current file, then opening the next file fails to delete
// an expired object. The local file and checksum stay. The following run,
// once delete succeeds, must keep replicating past that rotate. Resuming on
// the file just sealed opens <name>.open.e1 beside it and the next rotate
// returns "sealed file already exists".
func TestRotateAfterObjectPurgeFailureDoesNotReseal(t *testing.T) {
	dir := t.TempDir()
	taskID := "task-1"
	taskDir := filepath.Join(dir, taskID)
	expiredName := "mysql-bin.000001"
	expiredPath := filepath.Join(taskDir, expiredName)
	const expiredKey = "old-object"
	const checksum = tasks.ChecksumMatch

	catalog := &purgeCatalog{rows: map[string]tasks.BinlogFile{}}
	deleter := &purgeDeleter{fail: map[string]error{expiredKey: errors.New("bucket denied")}}
	checkpoints := &memCheckpointStore{}
	fetcher := &fakeSourceMetaFetcher{serverUUID: "srv-uuid-1"}

	planted := false
	first := &callbackStreamer{
		next: func() (*goreplication.BinlogEvent, error) {
			if planted {
				return nil, context.Canceled
			}
			planted = true
			if err := os.MkdirAll(taskDir, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(expiredPath, []byte("expired-uploaded"), 0o644); err != nil {
				return nil, err
			}
			old := time.Now().Add(-3 * 24 * time.Hour)
			if err := os.Chtimes(expiredPath, old, old); err != nil {
				return nil, err
			}
			catalog.rows[expiredName] = tasks.BinlogFile{
				TaskID: taskID, FileName: expiredName, FilePath: expiredPath,
				State: "SEALED", UploadState: "UPLOADED", ObjectKey: expiredKey,
				Checksum: checksum,
			}
			return rotateTo("mysql-bin.000011", 4, 200), nil
		},
	}
	syncer := &recordingSyncer{}
	step := 0
	follow := &callbackStreamer{
		next: func() (*goreplication.BinlogEvent, error) {
			step++
			pos := syncer.last
			switch step {
			case 1:
				if pos.Name == "mysql-bin.000010" {
					// Dump is still on the file that was sealed. The source
					// sends the rotate again, and sealing that name fails.
					return rotateTo("mysql-bin.000011", 4, pos.Pos+10), nil
				}
				if pos.Name != "mysql-bin.000011" || pos.Pos != 4 {
					return nil, errors.New("resumed at " + pos.Name + ":" + strconv.FormatUint(uint64(pos.Pos), 10))
				}
				return queryAt(197), nil
			case 2:
				return rotateTo("mysql-bin.000012", 4, 300), nil
			default:
				return nil, context.Canceled
			}
		},
	}
	syncer.streams = []binlogStreamer{first, follow}

	runner := NewMySQLRunner(dir,
		WithCheckpointStore(checkpoints),
		WithFileMetaStore(catalog),
		WithObjectDeleter(deleter),
	)
	runner.fetcher = fetcher
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }

	task := tasks.Task{
		ID:         taskID,
		Epoch:      1,
		ClusterKey: "cluster-a",
		Source: tasks.SourceConfig{
			Host: "127.0.0.1", Port: 3306, User: "repl", Flavor: "mysql",
		},
		Start: tasks.StartConfig{
			Mode: tasks.StartModeFilePos, File: "mysql-bin.000010", Pos: 4,
		},
		Storage: tasks.Storage{RetentionDays: 1},
	}

	err := runner.Run(context.Background(), task)
	if err == nil || !containsAll(err.Error(), objectPurgeFailed, expiredName) {
		t.Fatalf("first run err=%v", err)
	}
	if containsAll(err.Error(), "sealed file already exists") {
		t.Fatalf("first run resealed: %v", err)
	}
	if _, statErr := os.Stat(expiredPath); statErr != nil {
		t.Fatalf("local file removed on failed object delete: %v", statErr)
	}
	row := catalog.rows[expiredName]
	if row.UploadState != "UPLOADED" || row.ObjectKey != expiredKey || row.Checksum != checksum {
		t.Fatalf("row changed: %+v", row)
	}
	if len(deleter.keys) != 0 {
		t.Fatalf("recorded a finished delete: %v", deleter.keys)
	}
	sealed := filepath.Join(taskDir, "mysql-bin.000010")
	if _, statErr := os.Stat(sealed); statErr != nil {
		t.Fatalf("rotate did not seal mysql-bin.000010: %v", statErr)
	}
	if _, statErr := os.Stat(sealed + ".open.e1"); !os.IsNotExist(statErr) {
		t.Fatalf("open segment left beside the sealed file after the failed open: %v", statErr)
	}

	deleter.fail = nil
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("retry after a successful purge: %v", err)
	}
	if _, statErr := os.Stat(expiredPath); !os.IsNotExist(statErr) {
		t.Fatalf("successful purge left the local file: %v", statErr)
	}
	if _, ok := catalog.rows[expiredName]; ok {
		t.Fatal("successful purge left the catalog row")
	}
	if len(deleter.keys) != 1 || deleter.keys[0] != expiredKey {
		t.Fatalf("deleted keys=%v", deleter.keys)
	}
	if _, statErr := os.Stat(filepath.Join(taskDir, "mysql-bin.000011")); statErr != nil {
		t.Fatalf("replication did not seal the next file: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(taskDir, "mysql-bin.000012.open.e1")); statErr != nil {
		t.Fatalf("replication did not open the file after the rotate: %v", statErr)
	}
	if _, statErr := os.Stat(sealed + ".open.e1"); !os.IsNotExist(statErr) {
		t.Fatalf("retry reopened the sealed file: %v", statErr)
	}
	got, ok := checkpoints.snapshot()
	if !ok || got.File != "mysql-bin.000012" || got.Pos != 4 {
		t.Fatalf("checkpoint after continuing past the rotate: %+v ok=%v", got, ok)
	}
}

func (m *memCheckpointStore) snapshot() (binlog.Checkpoint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cp, m.ok
}

type recordingSyncer struct {
	streams []binlogStreamer
	last    gomysql.Position
}

func (s *recordingSyncer) StartSync(pos gomysql.Position) (binlogStreamer, error) {
	s.last = pos
	if len(s.streams) == 0 {
		return nil, errors.New("unexpected StartSync")
	}
	st := s.streams[0]
	s.streams = s.streams[1:]
	return st, nil
}

func (s *recordingSyncer) StartSyncGTID(gomysql.GTIDSet) (binlogStreamer, error) {
	return nil, errors.New("gtid start is not used")
}

func (s *recordingSyncer) Close() {}

type callbackStreamer struct {
	next func() (*goreplication.BinlogEvent, error)
}

func (s *callbackStreamer) GetEvent(context.Context) (*goreplication.BinlogEvent, error) {
	return s.next()
}

func rotateTo(next string, nextPos, logPos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.ROTATE_EVENT,
			LogPos:    logPos,
			Timestamp: uint32(time.Now().Unix()),
		},
		Event: &goreplication.RotateEvent{
			Position:    uint64(nextPos),
			NextLogName: []byte(next),
		},
		RawData: []byte("rotate"),
	}
}

func queryAt(logPos uint32) *goreplication.BinlogEvent {
	return &goreplication.BinlogEvent{
		Header: &goreplication.EventHeader{
			EventType: goreplication.QUERY_EVENT,
			LogPos:    logPos,
			Timestamp: uint32(time.Now().Unix()),
		},
		RawData: []byte("query"),
	}
}
