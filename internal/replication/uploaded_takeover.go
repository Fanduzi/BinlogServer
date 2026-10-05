// Package replication provides module-level functionality for replication.
// input: runner options from process startup, and one sealed UPLOADED object body
// output: the dump file and position a takeover reaches when that object is the only copy of the checkpoint
// pos: production-wiring proof that startup options can read an uploaded segment
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"encoding/binary"
	"path/filepath"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// UploadedTakeoverFixture is the sealed object the process must read when the
// local file is gone. App startup serves this body from its upload client.
func UploadedTakeoverFixture() (objectKey string, body []byte, file string, endPos uint32) {
	file = "mysql-bin.000004"
	objectKey = "dogfood/dogfood-148-obj/uuid/mysql-bin.000004"
	payload := []byte("uploaded-tail")
	size := uint32(goreplication.EventHeaderSize + len(payload))
	endPos = 4 + size
	raw := make([]byte, size)
	binary.LittleEndian.PutUint32(raw[0:4], 1_700_000_000)
	raw[4] = byte(goreplication.QUERY_EVENT)
	binary.LittleEndian.PutUint32(raw[5:9], 1)
	binary.LittleEndian.PutUint32(raw[9:13], size)
	binary.LittleEndian.PutUint32(raw[13:17], endPos)
	copy(raw[goreplication.EventHeaderSize:], payload)
	body = append(append([]byte{}, binlogMagic...), raw...)
	return objectKey, body, file, endPos
}

// ContinueUploadedTakeover runs one epoch>1 start with opts applied first.
// opts are the options process startup passed to the runner. The local sealed
// file is absent. A readable object continues from its last complete event.
func ContinueUploadedTakeover(ctx context.Context, dataDir string, opts ...RunnerOption) (string, uint32, error) {
	objectKey, _, file, endPos := UploadedTakeoverFixture()
	missing := filepath.Join(dataDir, "dead-worker", "2", file)
	const taskID = "2"
	catalog := &uploadedTakeoverCatalog{rows: []tasks.BinlogFile{{
		TaskID: taskID, FileName: file, FilePath: missing,
		State: "SEALED", EndPos: endPos, UploadState: "UPLOADED", ObjectKey: objectKey,
	}}}
	store := &uploadedTakeoverCheckpoint{cp: binlog.Checkpoint{File: file, Pos: endPos}, ok: true}
	all := append(append([]RunnerOption{}, opts...), WithCheckpointStore(store), WithFileMetaStore(catalog))
	runner := NewMySQLRunner(dataDir, all...)
	syncer := &uploadedTakeoverSyncer{}
	runner.fetcher = uploadedTakeoverFetcher{}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := tasks.Task{
		ID: taskID, ClusterKey: "dogfood", Epoch: 2, OwnerWorkerID: "worker-b",
		Start:  tasks.StartConfig{Mode: tasks.StartModeLatest},
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Flavor: "mysql"},
	}
	err := runner.Run(ctx, task)
	return syncer.pos.Name, syncer.pos.Pos, err
}

type uploadedTakeoverFetcher struct{}

func (uploadedTakeoverFetcher) FetchMasterStatus(context.Context, tasks.SourceConfig) (MasterStatus, error) {
	return MasterStatus{File: "mysql-bin.000005", Pos: 495}, nil
}

func (uploadedTakeoverFetcher) FetchServerUUID(context.Context, tasks.SourceConfig) (string, error) {
	return "11111111-1111-1111-1111-111111111111", nil
}

type uploadedTakeoverSyncer struct {
	pos gomysql.Position
}

func (s *uploadedTakeoverSyncer) StartSync(pos gomysql.Position) (binlogStreamer, error) {
	s.pos = pos
	return uploadedTakeoverStream{}, nil
}

func (s *uploadedTakeoverSyncer) StartSyncGTID(gomysql.GTIDSet) (binlogStreamer, error) {
	return nil, context.Canceled
}

func (s *uploadedTakeoverSyncer) Close() {}

type uploadedTakeoverStream struct{}

func (uploadedTakeoverStream) GetEvent(context.Context) (*goreplication.BinlogEvent, error) {
	return nil, context.Canceled
}

type uploadedTakeoverCheckpoint struct {
	cp binlog.Checkpoint
	ok bool
}

func (s *uploadedTakeoverCheckpoint) LoadCheckpoint(context.Context, string) (binlog.Checkpoint, bool, error) {
	return s.cp, s.ok, nil
}

func (s *uploadedTakeoverCheckpoint) UpsertCheckpoint(context.Context, string, binlog.Checkpoint) error {
	return nil
}

type uploadedTakeoverCatalog struct {
	rows []tasks.BinlogFile
}

func (c *uploadedTakeoverCatalog) UpsertBinlogFile(_ context.Context, meta tasks.BinlogFile) error {
	for i := range c.rows {
		if c.rows[i].FileName == meta.FileName && c.rows[i].Epoch == meta.Epoch {
			c.rows[i] = meta
			return nil
		}
	}
	c.rows = append(c.rows, meta)
	return nil
}

func (c *uploadedTakeoverCatalog) ListBinlogFiles(_ context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	out := make([]tasks.BinlogFile, 0, len(c.rows))
	for _, row := range c.rows {
		if row.TaskID == taskID {
			out = append(out, row)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (c *uploadedTakeoverCatalog) DeleteBinlogFile(_ context.Context, taskID, fileName string, epoch int64) error {
	kept := c.rows[:0]
	for _, row := range c.rows {
		if row.TaskID == taskID && row.FileName == fileName && row.Epoch == epoch {
			continue
		}
		kept = append(kept, row)
	}
	c.rows = kept
	return nil
}
