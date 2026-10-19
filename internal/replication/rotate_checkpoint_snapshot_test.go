// Package replication provides module-level functionality for replication.
// input: an open segment whose sealed upload blocks, and a checkpoint store that records each upsert
// output: proof that a rotate does not leave the next-file checkpoint visible while the next open file is still absent (design 0009 §27, #441)
// pos: rotate ordering test for CHECKPOINT_AHEAD false alarms
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"
)

// TestRotateUploadDoesNotExposeNextCheckpointBeforeTheOpenFile is the rotate
// window behind #441. The next-file checkpoint is written before the file
// exists (a crash must resume on the next file). That gap must not stay
// visible for the whole upload: once the upload is in progress, the open
// file is already in the directory. A reader that looks inside the checkpoint
// write still sees the file missing, so the test is the ordering, not a
// disabled I5.
func TestRotateUploadDoesNotExposeNextCheckpointBeforeTheOpenFile(t *testing.T) {
	dir := t.TempDir()
	const oldName, nextName = "mysql-bin.000006", "mysql-bin.000007"
	oldPath := filepath.Join(dir, oldName)
	body := append([]byte{}, binlogMagic...)
	body = append(body, make([]byte, 64)...)
	if err := os.WriteFile(oldPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	nextPath := filepath.Join(dir, openFileName(nextName, 0))

	var missingAtCheckpoint atomic.Bool
	cp := &fakeRunnerCheckpointStore{onUpsert: func(c binlog.Checkpoint) error {
		if c.File != nextName {
			return nil
		}
		if _, statErr := os.Stat(nextPath); statErr != nil {
			missingAtCheckpoint.Store(true)
		}
		return nil
	}}

	started := make(chan struct{})
	releaseUpload := make(chan struct{})
	var signaled atomic.Bool
	gate := tasks.NewScheduler()
	var sawGap atomic.Bool
	stopWatch := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			rel := gate.HoldChain("1")
			file := ""
			if n := len(cp.upserts); n > 0 {
				file = cp.upserts[n-1].File
			}
			_, statErr := os.Stat(nextPath)
			rel()
			if file == nextName && statErr != nil {
				sawGap.Store(true)
			}
		}
	}()
	r := &MySQLRunner{
		checkpointStore: cp,
		chainGate:       gate,
		sealedHandler: func(context.Context, tasks.BinlogFile) error {
			if signaled.CompareAndSwap(false, true) {
				close(started)
			}
			<-releaseUpload
			return nil
		},
	}
	seg := &openSegment{
		r:        r,
		ctx:      context.Background(),
		task:     tasks.Task{ID: "1", ClusterKey: "ck"},
		session:  &sourceSession{dir: dir},
		executed: newExecutedGTID("mysql", ""),
		opener: func(task tasks.Task, fileName string, _ uint32) (io.Closer, *binlog.Writer, string, error) {
			path := filepath.Join(dir, openFileName(fileName, task.Epoch))
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				return nil, nil, "", err
			}
			if _, err := f.Write(binlogMagic); err != nil {
				f.Close()
				return nil, nil, "", err
			}
			return f, nil, path, nil
		},
		file: old,
		name: oldName,
		pos:  100,
		path: oldPath,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- seg.rotate(0, nil, nextName, 4, true, "11111111-1111-1111-1111-111111111111")
	}()
	select {
	case <-started:
	case err := <-errCh:
		t.Fatalf("rotate ended before the upload: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not start")
	}
	if !missingAtCheckpoint.Load() {
		t.Fatal("next file already existed when its checkpoint was written; the test no longer sees the pre-open window")
	}
	if _, err := os.Stat(nextPath); err != nil {
		t.Fatalf("checkpoint is %s but %s is missing while the upload runs: %v", nextName, nextPath, err)
	}
	close(stopWatch)
	if sawGap.Load() {
		t.Fatal("a reader holding HoldChain saw the next-file checkpoint before the open file existed")
	}
	close(releaseUpload)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}
