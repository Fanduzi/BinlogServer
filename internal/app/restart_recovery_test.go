// Package app provides module-level functionality for app.
// input: standalone startup with persisted RUNNING tasks, metadata endpoint policy, and injected runner dependencies
// output: regression coverage proving safe active tasks resume while metadata-source conflicts stay stopped
// pos: application startup recovery test for durable standalone task state
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"testing"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/replication"
	"binlog_server/internal/tasks"
)

func TestApp_StandaloneResumesPersistedRunningTask(t *testing.T) {
	store := &fakeAppMetaStore{listTasks: []tasks.Task{{
		ID:         "19",
		Name:       "restart-recovery",
		ClusterKey: "restart-recovery",
		State:      tasks.StateRunning,
		Source: tasks.SourceConfig{
			Host:     "127.0.0.1",
			Port:     3309,
			User:     "repl",
			Password: "secret",
		},
		Start: tasks.StartConfig{Mode: tasks.StartModeLatest},
	}}}
	restoreMetaStoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(config.Config) (appMetaStore, error) { return store, nil }
	defer func() { newAppMetaStoreForRun = restoreMetaStoreFactory }()

	started := make(chan tasks.Task, 1)
	restoreRunnerFactory := newRunnerForRun
	newRunnerForRun = func(config.Config, ...replication.RunnerOption) tasks.Runner {
		return &appFakeRunner{started: started}
	}
	defer func() { newRunnerForRun = restoreRunnerFactory }()

	ctx, cancel := context.WithCancel(context.Background())
	a := New(config.Config{
		DataDir:    t.TempDir(),
		MetaDSN:    "fake-meta",
		Mode:       "standalone",
		ListenAddr: "127.0.0.1:0",
	})
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	defer func() {
		cancel()
		waitRunExit(t, errCh)
	}()

	waitReady(t, a)
	select {
	case task := <-started:
		if task.ID != "19" {
			t.Fatalf("expected recovered task 19, got %s", task.ID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("persisted RUNNING task was not resumed after standalone restart")
	}
}

func TestApp_StandaloneDoesNotResumeTaskUsingMetadataEndpoint(t *testing.T) {
	store := &fakeAppMetaStore{listTasks: []tasks.Task{{
		ID:         "unsafe-19",
		Name:       "metadata-source-conflict",
		ClusterKey: "metadata-source-conflict",
		State:      tasks.StateRunning,
		Source: tasks.SourceConfig{
			Host:     "127.0.0.1",
			Port:     3306,
			User:     "repl",
			Password: "secret",
		},
		Start: tasks.StartConfig{Mode: tasks.StartModeLatest},
	}}}
	restoreMetaStoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(config.Config) (appMetaStore, error) { return store, nil }
	defer func() { newAppMetaStoreForRun = restoreMetaStoreFactory }()

	started := make(chan tasks.Task, 1)
	restoreRunnerFactory := newRunnerForRun
	newRunnerForRun = func(config.Config, ...replication.RunnerOption) tasks.Runner {
		return &appFakeRunner{started: started}
	}
	defer func() { newRunnerForRun = restoreRunnerFactory }()

	ctx, cancel := context.WithCancel(context.Background())
	a := New(config.Config{
		DataDir:    t.TempDir(),
		MetaDSN:    "meta:secret@tcp(127.0.0.1:3306)/binlog_meta?parseTime=true",
		Mode:       "standalone",
		ListenAddr: "127.0.0.1:0",
	})
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	defer func() {
		cancel()
		waitRunExit(t, errCh)
	}()

	waitReady(t, a)
	persisted, err := store.ListTasks(ctx)
	if err != nil || len(persisted) != 1 || persisted[0].State != tasks.StateStopped {
		t.Fatalf("expected metadata source conflict task to remain STOPPED, tasks=%+v err=%v", persisted, err)
	}
	select {
	case task := <-started:
		t.Fatalf("metadata source conflict task was resumed: %s", task.ID)
	case <-time.After(200 * time.Millisecond):
	}
}
