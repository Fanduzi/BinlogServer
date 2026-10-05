// Package replication provides module-level functionality for replication.
// input: a sealed binlog whose upload state was not written, a hung uploader, and a task with no uploader
// output: proof that a crash between seal and the upload-state write is uploaded by the existing retry path, a hung post-seal upload times out as UPLOAD_FAILED and replication continues, and LOCAL_ONLY without an uploader stays unuploaded
// pos: regression for sealed-file upload recovery and the post-seal upload deadline
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestSealedUpload_CrashBeforeStateWriteIsRetried(t *testing.T) {
	dir := t.TempDir()
	const (
		taskID   = "task-1"
		sealed   = "mysql-bin.000003"
		nextFile = "mysql-bin.000004"
	)
	body := rotateEventBody(nextFile, 4)
	ev := frameBinlogEvent(goreplication.ROTATE_EVENT, body, 4, time.Unix(1_700_000_000, 0).UTC())
	ev.Event = &goreplication.RotateEvent{Position: 4, NextLogName: []byte(nextFile)}

	catalog := &sealFailCatalog{inner: &takeoverCatalog{}, fail: true}
	checkpoints := &memCheckpointStore{}
	uploader := &recordingUploader{}
	fetcher := &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
		serverUUID: "11111111-1111-1111-1111-111111111111",
	}
	runner := NewMySQLRunner(dir,
		WithCheckpointStore(checkpoints),
		WithFileMetaStore(catalog),
		WithUploader(uploader, "prefix"),
		WithUploadTimeout(time.Second),
	)
	runner.fetcher = fetcher
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer {
		return &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{event: ev}}}}
	}
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeFilePos, File: sealed, Pos: 4})
	task.Epoch = 1
	task.Storage.RetentionDays = 1

	err := runner.Run(context.Background(), task)
	if err == nil || !strings.Contains(err.Error(), "killed before upload state write") {
		t.Fatalf("first run err=%v", err)
	}
	if uploader.callCount() != 0 {
		t.Fatalf("upload ran before the state write, calls=%d", uploader.callCount())
	}
	sealedPath := filepath.Join(dir, taskID, sealed)
	if _, statErr := os.Stat(sealedPath); statErr != nil {
		t.Fatalf("sealed file: %v", statErr)
	}
	openGone := filepath.Join(dir, taskID, sealed+".open.e1")
	if _, statErr := os.Stat(openGone); !os.IsNotExist(statErr) {
		t.Fatalf("open segment still present: %v", statErr)
	}
	rows, err := catalog.ListBinlogFiles(context.Background(), taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rowByName(rows, sealed)
	if !ok || row.State != "OPEN" || row.UploadState != "LOCAL_ONLY" {
		t.Fatalf("catalog after the killed write: %+v", row)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(sealedPath, old, old); err != nil {
		t.Fatal(err)
	}

	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task.Epoch = 2
	if err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if syncer.startPos.Name != nextFile || syncer.startPos.Pos != 4 {
		t.Fatalf("resume start %+v, want %s:4", syncer.startPos, nextFile)
	}
	if _, statErr := os.Stat(sealedPath); statErr != nil {
		t.Fatalf("retention removed the sealed file: %v", statErr)
	}
	if uploader.deletes != 0 {
		t.Fatalf("retention deleted the object, deletes=%d", uploader.deletes)
	}
	rows, err = catalog.ListBinlogFiles(context.Background(), taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	row, ok = rowByName(rows, sealed)
	if !ok || row.State != "SEALED" || row.UploadState != "UPLOAD_FAILED" || row.ObjectKey == "" || row.FilePath != sealedPath {
		t.Fatalf("files list during recovery: %+v", row)
	}
	if row.SealedAt.IsZero() {
		t.Fatal("sealed_at is empty, retry would skip the file")
	}

	updated, err := tasks.ApplySealedUpload(context.Background(), uploader, catalog, row)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UploadState != "UPLOADED" || updated.Checksum != tasks.ChecksumMatch {
		t.Fatalf("retry verify: state=%s checksum=%s err=%s", updated.UploadState, updated.Checksum, updated.UploadError)
	}
	if _, statErr := os.Stat(sealedPath); statErr != nil {
		t.Fatalf("local file after verify: %v", statErr)
	}
}

func TestSealedUpload_HungUploaderTimesOutAndReplicationContinues(t *testing.T) {
	dir := t.TempDir()
	const nextFile = "mysql-bin.000002"
	body := rotateEventBody(nextFile, 4)
	ev := frameBinlogEvent(goreplication.ROTATE_EVENT, body, 4, time.Unix(1_700_000_000, 0).UTC())
	ev.Event = &goreplication.RotateEvent{Position: 4, NextLogName: []byte(nextFile)}

	catalog := &takeoverCatalog{}
	checkpoints := &memCheckpointStore{}
	uploader := &hangUploader{catalog: catalog, taskID: "task-1", fileName: "mysql-bin.000001"}
	runner := NewMySQLRunner(dir,
		WithCheckpointStore(checkpoints),
		WithFileMetaStore(catalog),
		WithUploader(uploader, "prefix"),
		WithUploadTimeout(150*time.Millisecond),
	)
	runner.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
		serverUUID: "srv-uuid-1",
	}
	runner.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer {
		return &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{
			{event: ev},
			{err: context.Canceled},
		}}}
	}
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeFilePos, File: "mysql-bin.000001", Pos: 4})
	task.Epoch = 1

	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- runner.Run(context.Background(), task) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hung uploader blocked the dump")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("dump stayed blocked for %s", time.Since(started))
	}
	if uploader.early != "" {
		t.Fatal(uploader.early)
	}
	cp, ok := checkpoints.snapshot()
	if !ok || cp.File != nextFile || cp.Pos != 4 {
		t.Fatalf("checkpoint after timeout: %+v ok=%v", cp, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, task.ID, nextFile+".open.e1")); err != nil {
		t.Fatalf("next file was not opened: %v", err)
	}
	rows, err := catalog.ListBinlogFiles(context.Background(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	row, found := rowByName(rows, "mysql-bin.000001")
	if !found || row.UploadState != "UPLOAD_FAILED" || row.State != "SEALED" {
		t.Fatalf("files list after timeout: %+v", row)
	}
}

func TestSealedUpload_NoUploaderStaysLocalOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(path, []byte("binlog-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	metaStore := &fakeMetaStore{}
	runner := &MySQLRunner{fileMetaStore: metaStore}
	if err := runner.finalizeSealedFile(context.Background(), tasks.Task{ID: "1", Epoch: 0}, "", path, 4, 20, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(metaStore.metas) != 1 || metaStore.metas[0].UploadState != "LOCAL_ONLY" || metaStore.metas[0].ObjectKey != "" {
		t.Fatalf("no uploader meta: %+v", metaStore.metas)
	}

	workerB := t.TempDir()
	sealed := filepath.Join(workerB, "task-1", "mysql-bin.000003")
	if err := os.MkdirAll(filepath.Dir(sealed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sealed, []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := &takeoverCatalog{rows: []tasks.BinlogFile{{
		TaskID: "task-1", FileName: "mysql-bin.000003", FilePath: sealed,
		State: "SEALED", EndPos: 1000, UploadState: "LOCAL_ONLY", SealedAt: time.Now(),
	}}}
	store := &memCheckpointStore{cp: binlog.Checkpoint{File: "mysql-bin.000003", Pos: 1000}, ok: true}
	syncer := &fakeSyncer{streamer: &fakeStreamer{results: []streamResult{{err: context.Canceled}}}}
	resume := NewMySQLRunner(workerB, WithCheckpointStore(store), WithFileMetaStore(catalog))
	resume.fetcher = &fakeSourceMetaFetcher{
		status:     MasterStatus{File: "mysql-bin.000099", Pos: 9000},
		serverUUID: "srv-uuid-1",
	}
	resume.newSyncer = func(goreplication.BinlogSyncerConfig) binlogSyncer { return syncer }
	task := newRunnerTask(tasks.StartConfig{Mode: tasks.StartModeLatest})
	task.Epoch = 2
	err := resume.Run(context.Background(), task)
	if err == nil || !tasks.IsPermanent(err) || !strings.Contains(err.Error(), "SEGMENT_NOT_ON_WORKER") {
		t.Fatalf("err=%v", err)
	}
	rows, listErr := catalog.ListBinlogFiles(context.Background(), task.ID, 0)
	if listErr != nil {
		t.Fatal(listErr)
	}
	row, ok := rowByName(rows, "mysql-bin.000003")
	if !ok || row.UploadState != "LOCAL_ONLY" || row.ObjectKey != "" {
		t.Fatalf("local-only row changed: %+v", row)
	}
	if syncer.startPosCalls != 0 {
		t.Fatalf("started at %+v", syncer.startPos)
	}
}

type sealFailCatalog struct {
	inner *takeoverCatalog
	fail  bool
}

func (s *sealFailCatalog) UpsertBinlogFile(ctx context.Context, meta tasks.BinlogFile) error {
	if s.fail && strings.EqualFold(meta.State, "SEALED") {
		s.fail = false
		return errors.New("killed before upload state write")
	}
	return s.inner.UpsertBinlogFile(ctx, meta)
}

func (s *sealFailCatalog) ListBinlogFiles(ctx context.Context, taskID string, limit int) ([]tasks.BinlogFile, error) {
	return s.inner.ListBinlogFiles(ctx, taskID, limit)
}

func (s *sealFailCatalog) DeleteBinlogFile(ctx context.Context, taskID, fileName string, epoch int64) error {
	return s.inner.DeleteBinlogFile(ctx, taskID, fileName, epoch)
}

type recordingUploader struct {
	mu      sync.Mutex
	calls   int
	deletes int
	objects map[string][]byte
}

func (u *recordingUploader) UploadFile(_ context.Context, _, localPath, objectKey string) error {
	body, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	if u.objects == nil {
		u.objects = map[string][]byte{}
	}
	u.objects[objectKey] = append([]byte(nil), body...)
	return nil
}

func (u *recordingUploader) SealedObjectMatches(_ context.Context, localPath, objectKey string) (bool, error) {
	local, err := os.ReadFile(localPath)
	if err != nil {
		return false, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return string(local) == string(u.objects[objectKey]), nil
}

func (u *recordingUploader) DeleteObject(context.Context, string) error {
	u.mu.Lock()
	u.deletes++
	u.mu.Unlock()
	return nil
}

func (u *recordingUploader) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

type hangUploader struct {
	catalog  *takeoverCatalog
	taskID   string
	fileName string
	early    string
}

func (h *hangUploader) UploadFile(ctx context.Context, _, _, _ string) error {
	rows, err := h.catalog.ListBinlogFiles(context.Background(), h.taskID, 0)
	if err != nil {
		h.early = err.Error()
	} else if row, ok := rowByName(rows, h.fileName); !ok || row.UploadState != "UPLOAD_FAILED" || row.ObjectKey == "" {
		h.early = "upload started before UPLOAD_FAILED was recorded"
	}
	<-ctx.Done()
	return ctx.Err()
}

func rotateEventBody(next string, pos uint64) []byte {
	body := make([]byte, 8+len(next))
	binary.LittleEndian.PutUint64(body[:8], pos)
	copy(body[8:], next)
	return body
}

func rowByName(rows []tasks.BinlogFile, name string) (tasks.BinlogFile, bool) {
	for _, row := range rows {
		if row.FileName == name {
			return row, true
		}
	}
	return tasks.BinlogFile{}, false
}
