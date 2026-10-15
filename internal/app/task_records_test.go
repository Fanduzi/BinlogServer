// Package app provides module-level functionality for app.
// input: a standalone data_dir (no meta_dsn) holding an encrypted task record written by an earlier process, a 0600 key file, and an injected runner
// output: ADR 0010 at the app boundary: with standalone.task_records on and the key configured, a restart Starts the recorded task with its source password; with records off the same data_dir starts nothing
// pos: application startup coverage for the standalone task record (#385)
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/replication"
	"binlog_server/internal/taskrecords"
	"binlog_server/internal/tasks"
)

func writeRecordedTask(t *testing.T, data, keyFile string) {
	t.Helper()
	key, err := taskrecords.GenerateKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(map[string]any{
		"seq":      21,
		"saved_at": time.Now().UTC(),
		"tasks": []map[string]any{{
			"id": "21", "name": "recorded", "cluster_key": "recorded-21",
			"source":        map[string]any{"host": "127.0.0.1", "port": 3309, "user": "repl", "password": "app-s3cret", "flavor": "mysql", "server_id": 2121},
			"start":         map[string]any{"mode": "GTID", "gtid_set": "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5"},
			"storage":       map[string]any{"retention_days": 7},
			"desired_run":   "RUN",
			"spec_revision": 2,
		}},
	})
	if err := taskrecords.Open(data, key, nil).Save(plain); err != nil {
		t.Fatal(err)
	}
}

func runStandaloneWithRecords(t *testing.T, cfg config.Config) chan tasks.Task {
	t.Helper()
	started := make(chan tasks.Task, 1)
	restoreRunnerFactory := newRunnerForRun
	newRunnerForRun = func(config.Config, ...replication.RunnerOption) tasks.Runner {
		return &appFakeRunner{started: started}
	}
	t.Cleanup(func() { newRunnerForRun = restoreRunnerFactory })
	ctx, cancel := context.WithCancel(context.Background())
	a := New(cfg)
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		waitRunExit(t, errCh)
	})
	waitReady(t, a)
	return started
}

func TestApp_StandaloneTaskRecordsResumeAfterRestart(t *testing.T) {
	data := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "task.key")
	writeRecordedTask(t, data, keyFile)
	started := runStandaloneWithRecords(t, config.Config{
		DataDir: data, Mode: "standalone", ListenAddr: "127.0.0.1:0",
		Standalone: config.StandaloneConfig{TaskRecords: "on", TaskKeyFile: keyFile},
	})
	select {
	case task := <-started:
		if task.ID != "21" || task.Source.Password != "app-s3cret" || task.Start.Mode != tasks.StartModeGTID {
			t.Fatalf("resumed %s source=%s start=%+v", task.ID, task.Source.Host, task.Start)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recorded task was not resumed after a standalone restart")
	}
}

func TestApp_StandaloneTaskRecordsOffStartsNothing(t *testing.T) {
	data := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "task.key")
	writeRecordedTask(t, data, keyFile)
	started := runStandaloneWithRecords(t, config.Config{
		DataDir: data, Mode: "standalone", ListenAddr: "127.0.0.1:0",
		Standalone: config.StandaloneConfig{TaskRecords: "off", TaskKeyFile: keyFile},
	})
	select {
	case task := <-started:
		t.Fatalf("records off started task %s", task.ID)
	case <-time.After(500 * time.Millisecond):
	}
}
