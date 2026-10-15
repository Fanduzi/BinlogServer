package app

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/replication"
	"binlog_server/internal/tasks"
)

// probeRunner stands in for the real runner's shutdown: after its context
// ends it keeps writing into data_dir for a moment (closing and syncing the
// open segment) before it returns.
type probeRunner struct {
	dataDir string
	started chan struct{}
}

func (r *probeRunner) Run(ctx context.Context, task tasks.Task) error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	time.Sleep(200 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(r.dataDir, "runner-exit-"+task.ID), []byte("flushed"), 0o600)
	return context.Canceled
}

func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			out = append(out, p+"|"+info.ModTime().String()+"|"+strconv.FormatInt(info.Size(), 10))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// App.Run owns its task runners: when its context ends it cancels them and
// waits for them, so nothing touches data_dir after Run returns (the
// TestApp_SecondStandaloneOnSameDataDirFailsToStart CI flake on v0.5.67:
// a resumed runner on context.Background outlived Run and raced cleanup).
func TestApp_RunCancelsAndJoinsRunnersBeforeReturning(t *testing.T) {
	data := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "task.key")
	writeRecordedTask(t, data, keyFile)
	r := &probeRunner{dataDir: data, started: make(chan struct{}, 1)}
	restore := newRunnerForRun
	newRunnerForRun = func(config.Config, ...replication.RunnerOption) tasks.Runner { return r }
	t.Cleanup(func() { newRunnerForRun = restore })

	ctx, cancel := context.WithCancel(context.Background())
	a := New(config.Config{
		DataDir: data, Mode: "standalone", ListenAddr: "127.0.0.1:0",
		Standalone: config.StandaloneConfig{TaskRecords: "on", TaskKeyFile: keyFile},
	})
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("recorded task was not resumed")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, err := os.Stat(filepath.Join(data, "runner-exit-21")); err != nil {
		t.Fatalf("Run returned before the runner finished its shutdown write: %v", err)
	}
	before := listTree(t, data)
	time.Sleep(400 * time.Millisecond)
	if after := listTree(t, data); len(after) != len(before) || !equalStrings(before, after) {
		t.Fatalf("data_dir changed after Run returned:\nbefore %v\nafter  %v", before, after)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
