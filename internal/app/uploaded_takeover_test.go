// Package app provides module-level functionality for app.
// input: process startup with an upload client, and a checkpoint covered only by a sealed UPLOADED object
// output: proof that startup wires that client as the object reader and does not fail with SEGMENT_NOT_ON_WORKER, and that worker startup deletes a crashed .takeover-* without touching other names
// pos: regression for lease takeover when the local sealed file is gone
// note: if this file changes, update this header and module README.md.
package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/replication"
	"binlog_server/internal/tasks"
	"binlog_server/internal/upload"
)

// TestApp_StartupReadsUploadedSegment is the production startup path.
// App.Run builds the runner options. The test does not call WithUploader.
func TestApp_StartupReadsUploadedSegment(t *testing.T) {
	objectKey, body, file, endPos := replication.UploadedTakeoverFixture()
	client := &startupObjectClient{key: objectKey, body: body}
	restoreUpload := newUploadClientForRun
	newUploadClientForRun = func(upload.S3Config) (uploadClient, error) {
		return client, nil
	}
	t.Cleanup(func() { newUploadClientForRun = restoreUpload })

	gotOpts := make(chan []replication.RunnerOption, 1)
	restoreRunner := newRunnerForRun
	newRunnerForRun = func(_ config.Config, opts ...replication.RunnerOption) tasks.Runner {
		gotOpts <- append([]replication.RunnerOption{}, opts...)
		return &appFakeRunner{}
	}
	t.Cleanup(func() { newRunnerForRun = restoreRunner })

	cfg := config.Config{
		DataDir:         t.TempDir(),
		ListenAddr:      "127.0.0.1:0",
		UploadEndpoint:  "127.0.0.1:9000",
		UploadBucket:    "binlogs",
		UploadAccessKey: "key",
		UploadSecretKey: "secret",
		UploadPrefix:    "dogfood",
	}
	a := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	var opts []replication.RunnerOption
	select {
	case opts = <-gotOpts:
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not build a runner")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not exit")
	}

	gotFile, gotPos, err := replication.ContinueUploadedTakeover(context.Background(), t.TempDir(), opts...)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("takeover: %v", err)
	}
	if strings.Contains(errString(err), "SEGMENT_NOT_ON_WORKER") {
		t.Fatalf("startup left the object unread: %v", err)
	}
	if client.opens != 1 || client.opened[0] != objectKey {
		t.Fatalf("object opens %v, want one GET of %s", client.opened, objectKey)
	}
	if gotFile != file || gotPos != endPos {
		t.Fatalf("dump start %s:%d, want %s:%d", gotFile, gotPos, file, endPos)
	}
}

func TestApp_StartupRemovesStaleTakeoverTemp(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "6")
	otherDir := filepath.Join(dir, "7")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(taskDir, ".takeover-894519183")
	if err := os.WriteFile(stale, []byte("leftover"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "notes.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "mysql-bin.000436.sealed.e4"), []byte("sealed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "task-6.binlog"), []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "notes.txt"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, ".takeover-111"), []byte("yy"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New(config.Config{DataDir: dir, ListenAddr: "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	select {
	case <-a.Ready():
	case err := <-errCh:
		t.Fatalf("startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("app did not become ready")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app did not exit")
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("crash leftover still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherDir, ".takeover-111")); !os.IsNotExist(err) {
		t.Fatalf("other task crash leftover still present: %v", err)
	}
	for _, path := range []string{
		filepath.Join(taskDir, "notes.txt"),
		filepath.Join(taskDir, "mysql-bin.000436.sealed.e4"),
		filepath.Join(taskDir, "task-6.binlog"),
		filepath.Join(otherDir, "notes.txt"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type startupObjectClient struct {
	key    string
	body   []byte
	opens  int
	opened []string
}

func (c *startupObjectClient) UploadFile(context.Context, string, string, string) error { return nil }

func (c *startupObjectClient) DeleteObject(context.Context, string) error { return nil }

func (c *startupObjectClient) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	c.opens++
	c.opened = append(c.opened, key)
	if key != c.key {
		return nil, 0, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(c.body)), int64(len(c.body)), nil
}
