// Package replication provides module-level functionality for replication.
// input: task start strategy, fake MasterStatusFetcher, dump file/pos fixtures
// output: resolver coverage for start modes and conservative dump-vs-master file/pos comparison
// pos: start-point resolver and idle at-tip comparison test boundary
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

// TestResolveStart_DefaultsEmptyModeToLatest 验证空 mode 会按 LATEST 解析。
func TestResolveStart_DefaultsEmptyModeToLatest(t *testing.T) {
	fetcher := &fakeStatusFetcher{
		status: MasterStatus{
			File: "mysql-bin.000123",
			Pos:  456,
		},
	}
	task := tasks.Task{
		Start:  tasks.StartConfig{},
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
	}

	start, err := ResolveStart(context.Background(), task, fetcher)
	if err != nil {
		t.Fatalf("ResolveStart returned error: %v", err)
	}
	if start.Mode != tasks.StartModeFilePos || start.File != "mysql-bin.000123" || start.Pos != 456 {
		t.Fatalf("unexpected start: %+v", start)
	}
}

// TestResolveStart_LatestRequiresFetcher 验证 LATEST 模式缺少 fetcher 时返回错误。
func TestResolveStart_LatestRequiresFetcher(t *testing.T) {
	task := tasks.Task{
		Start: tasks.StartConfig{Mode: tasks.StartModeLatest},
	}

	_, err := ResolveStart(context.Background(), task, nil)
	if err == nil || err.Error() != "latest mode requires master status fetcher" {
		t.Fatalf("expected latest mode requires master status fetcher, got %v", err)
	}
}

// TestResolveStart_LatestRejectsInvalidMasterStatus 验证无效 master status 不会被接受。
func TestResolveStart_LatestRejectsInvalidMasterStatus(t *testing.T) {
	fetcher := &fakeStatusFetcher{
		status: MasterStatus{
			File: "",
			Pos:  456,
		},
	}
	task := tasks.Task{
		Start:  tasks.StartConfig{Mode: tasks.StartModeLatest},
		Source: tasks.SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl"},
	}

	_, err := ResolveStart(context.Background(), task, fetcher)
	if err == nil || err.Error() != `invalid master status: file="" pos=456` {
		t.Fatalf("expected invalid master status error, got %v", err)
	}
}

// TestResolveStart_FilePosRequiresFileAndPos 验证 FILE_POS 缺失字段时返回错误。
func TestResolveStart_FilePosRequiresFileAndPos(t *testing.T) {
	task := tasks.Task{
		Start: tasks.StartConfig{
			Mode: tasks.StartModeFilePos,
			File: "",
			Pos:  0,
		},
	}

	_, err := ResolveStart(context.Background(), task, &fakeStatusFetcher{})
	if err == nil || err.Error() != "file_pos requires file and pos" {
		t.Fatalf("expected file_pos requires file and pos, got %v", err)
	}
}

// TestResolveStart_GtidRequiresGTIDSet 验证 GTID 缺失 GTIDSet 时返回错误。
func TestResolveStart_GtidRequiresGTIDSet(t *testing.T) {
	task := tasks.Task{
		Start: tasks.StartConfig{
			Mode:    tasks.StartModeGTID,
			GTIDSet: "",
		},
	}

	_, err := ResolveStart(context.Background(), task, &fakeStatusFetcher{})
	if err == nil || err.Error() != "gtid requires gtid_set" {
		t.Fatalf("expected gtid requires gtid_set, got %v", err)
	}
}

// TestResolveStart_UnsupportedMode 验证非法起点模式返回错误。
func TestResolveStart_UnsupportedMode(t *testing.T) {
	task := tasks.Task{
		Start: tasks.StartConfig{
			Mode: tasks.StartMode("BAD_MODE"),
		},
	}

	_, err := ResolveStart(context.Background(), task, &fakeStatusFetcher{})
	if err == nil || err.Error() != "unsupported start mode: BAD_MODE" {
		t.Fatalf("expected unsupported start mode error, got %v", err)
	}
}

// TestDumpAtOrBeyondMaster 验证 dump 与 master file/pos 的保守比较（先 file 后 pos）。
func TestDumpAtOrBeyondMaster(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		pos    uint32
		master MasterStatus
		want   bool
	}{
		{
			name:   "same file and pos",
			file:   "mysql-bin.000010",
			pos:    4,
			master: MasterStatus{File: "mysql-bin.000010", Pos: 4},
			want:   true,
		},
		{
			name:   "same file dump ahead",
			file:   "mysql-bin.000010",
			pos:    100,
			master: MasterStatus{File: "mysql-bin.000010", Pos: 4},
			want:   true,
		},
		{
			name:   "same file dump behind",
			file:   "mysql-bin.000010",
			pos:    4,
			master: MasterStatus{File: "mysql-bin.000010", Pos: 100},
			want:   false,
		},
		{
			name:   "dump later file",
			file:   "mysql-bin.000011",
			pos:    4,
			master: MasterStatus{File: "mysql-bin.000010", Pos: 999},
			want:   true,
		},
		{
			name:   "dump earlier file",
			file:   "mysql-bin.000009",
			pos:    999,
			master: MasterStatus{File: "mysql-bin.000010", Pos: 4},
			want:   false,
		},
		{
			name:   "padded suffix same seq",
			file:   "mysql-bin.000010",
			pos:    4,
			master: MasterStatus{File: "mysql-bin.10", Pos: 4},
			want:   true,
		},
		{
			name:   "different prefix",
			file:   "mysql-bin.000010",
			pos:    4,
			master: MasterStatus{File: "binlog.000010", Pos: 4},
			want:   false,
		},
		{
			name:   "unparseable dump file",
			file:   "custom-binlog",
			pos:    4,
			master: MasterStatus{File: "mysql-bin.000010", Pos: 4},
			want:   false,
		},
		{
			name:   "empty master",
			file:   "mysql-bin.000010",
			pos:    4,
			master: MasterStatus{},
			want:   false,
		},
		{
			name:   "master pos zero",
			file:   "mysql-bin.000010",
			pos:    4,
			master: MasterStatus{File: "mysql-bin.000010", Pos: 0},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dumpAtOrBeyondMaster(tt.file, tt.pos, tt.master)
			if got != tt.want {
				t.Fatalf("dumpAtOrBeyondMaster(%q, %d, %+v)=%v, want %v", tt.file, tt.pos, tt.master, got, tt.want)
			}
		})
	}
}
