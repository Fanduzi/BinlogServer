// Package replication provides module-level functionality for replication.
// input: source replication config, task state, checkpoint/file store dependencies
// output: replication run control, local binlog artifacts, and upload/recovery signals
// pos: data-plane runtime that consumes MySQL binlog stream and emits durable outputs
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"testing"

	"binlog_server/internal/tasks"
)

type fakeStatusFetcher struct {
	status MasterStatus
	err    error
	calls  int
}

// FetchMasterStatus 实现对应功能逻辑。
func (f *fakeStatusFetcher) FetchMasterStatus(_ context.Context, _ tasks.SourceConfig) (MasterStatus, error) {
	f.calls++
	if f.err != nil {
		return MasterStatus{}, f.err
	}
	return f.status, nil
}

// TestResolveStart_LatestUsesMasterStatus 验证相关行为。
func TestResolveStart_LatestUsesMasterStatus(t *testing.T) {
	fetcher := &fakeStatusFetcher{
		status: MasterStatus{
			File: "mysql-bin.000123",
			Pos:  456,
		},
	}
	task := tasks.Task{
		ID:    "1",
		Name:  "cluster-a",
		Start: tasks.StartConfig{Mode: tasks.StartModeLatest},
		Source: tasks.SourceConfig{
			Host: "127.0.0.1",
			Port: 3306,
			User: "repl",
		},
	}

	start, err := ResolveStart(context.Background(), task, fetcher)
	if err != nil {
		t.Fatalf("ResolveStart returned error: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected fetcher called once, got %d", fetcher.calls)
	}
	if start.Mode != tasks.StartModeFilePos {
		t.Fatalf("expected mode FILE_POS, got %s", start.Mode)
	}
	if start.File != "mysql-bin.000123" || start.Pos != 456 {
		t.Fatalf("unexpected start position: %+v", start)
	}
}

// TestResolveStart_FilePosDirect 验证相关行为。
func TestResolveStart_FilePosDirect(t *testing.T) {
	fetcher := &fakeStatusFetcher{}
	task := tasks.Task{
		Start: tasks.StartConfig{
			Mode: tasks.StartModeFilePos,
			File: "mysql-bin.000010",
			Pos:  4,
		},
	}

	start, err := ResolveStart(context.Background(), task, fetcher)
	if err != nil {
		t.Fatalf("ResolveStart returned error: %v", err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("expected fetcher not called, got %d", fetcher.calls)
	}
	if start.Mode != tasks.StartModeFilePos {
		t.Fatalf("expected mode FILE_POS, got %s", start.Mode)
	}
}

// TestResolveStart_GtidDirect 验证相关行为。
func TestResolveStart_GtidDirect(t *testing.T) {
	fetcher := &fakeStatusFetcher{}
	task := tasks.Task{
		Start: tasks.StartConfig{
			Mode:    tasks.StartModeGTID,
			GTIDSet: "24BC785E-9A61-11E1-8A5D-080027635EF5:1-10",
		},
	}

	start, err := ResolveStart(context.Background(), task, fetcher)
	if err != nil {
		t.Fatalf("ResolveStart returned error: %v", err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("expected fetcher not called, got %d", fetcher.calls)
	}
	if start.Mode != tasks.StartModeGTID {
		t.Fatalf("expected mode GTID, got %s", start.Mode)
	}
}

// TestResolveStart_LatestFetchError 验证相关行为。
func TestResolveStart_LatestFetchError(t *testing.T) {
	wantErr := errors.New("connection failed")
	fetcher := &fakeStatusFetcher{err: wantErr}
	task := tasks.Task{
		Start:  tasks.StartConfig{Mode: tasks.StartModeLatest},
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
	}

	_, err := ResolveStart(context.Background(), task, fetcher)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
}
