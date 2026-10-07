// Package replication provides module-level functionality for replication.
// input: an object body larger than one copy buffer, and a download that ends short or errors
// output: proof that takeover writes the object through a bounded read, keeps the catalog checksum flag unchanged, leaves no open segment or temp file when the download does not finish, removes a crashed .takeover-* on startup and before the next materialize, keeps an in-flight temp, and leaves every other name in that task directory
// pos: regression for streaming an uploaded segment onto the worker during takeover
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

const streamObjectSize = 8 << 20

// chunkReader records the largest Read and can fail after failAt bytes.
type chunkReader struct {
	rest   []byte
	max    int
	read   int
	failAt int
	closed bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.failAt > 0 && c.read >= c.failAt {
		return 0, errors.New("download failed")
	}
	n := len(p)
	if n > len(c.rest) {
		n = len(c.rest)
	}
	if c.failAt > 0 && c.read+n > c.failAt {
		n = c.failAt - c.read
	}
	copy(p[:n], c.rest[:n])
	c.rest = c.rest[n:]
	c.read += n
	if n > c.max {
		c.max = n
	}
	if n == 0 {
		return 0, io.EOF
	}
	if c.failAt > 0 && c.read >= c.failAt {
		return n, errors.New("download failed")
	}
	return n, nil
}

func (c *chunkReader) Close() error {
	c.closed = true
	return nil
}

type streamOpener struct {
	body   []byte
	size   int64
	failAt int
	reader *chunkReader
}

func (o *streamOpener) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	o.reader = &chunkReader{rest: append([]byte(nil), o.body...), failAt: o.failAt}
	return o.reader, o.size, nil
}

func TestMaterializeUploadedStreamsWithoutBufferingObject(t *testing.T) {
	body := bytes.Repeat([]byte("binlog-segment-"), streamObjectSize/len("binlog-segment-"))
	opener := &streamOpener{body: body, size: int64(len(body))}
	dir := t.TempDir()
	runner := NewMySQLRunner(dir)
	runner.objectOpener = opener
	row := tasks.BinlogFile{
		FileName: "mysql-bin.000007", ObjectKey: "uploaded-segment",
		UploadState: "UPLOADED", Checksum: tasks.ChecksumMatch,
	}
	gotDir, err := runner.materializeUploaded(context.Background(), tasks.Task{ID: "task-1", Epoch: 3}, row)
	if err != nil {
		t.Fatal(err)
	}
	if gotDir != filepath.Join(dir, "task-1") {
		t.Fatalf("dir=%s", gotDir)
	}
	if row.Checksum != tasks.ChecksumMatch {
		t.Fatalf("checksum changed to %q", row.Checksum)
	}
	if opener.reader.max <= 0 || opener.reader.max > 32<<10 || opener.reader.max >= len(body) {
		t.Fatalf("max read=%d object=%d", opener.reader.max, len(body))
	}
	if !opener.reader.closed {
		t.Fatal("object reader was not closed")
	}
	path := filepath.Join(gotDir, openFileName(row.FileName, 3))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("wrote %d bytes, want %d", len(got), len(body))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if leftovers := takeoverTemps(t, gotDir); len(leftovers) != 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}

func TestMaterializeUploadedDropsPartialDownload(t *testing.T) {
	body := bytes.Repeat([]byte("partial-segment-"), 1<<20/len("partial-segment-"))
	dir := t.TempDir()
	task := tasks.Task{ID: "task-1", Epoch: 3}
	row := tasks.BinlogFile{
		FileName: "mysql-bin.000007", ObjectKey: "uploaded-segment",
		UploadState: "UPLOADED", Checksum: tasks.ChecksumMatch,
	}
	finalPath := filepath.Join(dir, task.ID, openFileName(row.FileName, task.Epoch))

	t.Run("short read", func(t *testing.T) {
		opener := &streamOpener{body: body, size: int64(len(body) + 1)}
		runner := NewMySQLRunner(dir)
		runner.objectOpener = opener
		if _, err := runner.materializeUploaded(context.Background(), task, row); err == nil {
			t.Fatal("short object was installed")
		}
		if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
			t.Fatalf("truncated open segment left: %v", err)
		}
		if leftovers := takeoverTemps(t, filepath.Join(dir, task.ID)); len(leftovers) != 0 {
			t.Fatalf("temp files left: %v", leftovers)
		}
	})

	t.Run("read error", func(t *testing.T) {
		opener := &streamOpener{body: body, size: int64(len(body)), failAt: 1 << 20}
		runner := NewMySQLRunner(dir)
		runner.objectOpener = opener
		if _, err := runner.materializeUploaded(context.Background(), task, row); err == nil {
			t.Fatal("failed download was installed")
		}
		if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
			t.Fatalf("truncated open segment left: %v", err)
		}
		if leftovers := takeoverTemps(t, filepath.Join(dir, task.ID)); len(leftovers) != 0 {
			t.Fatalf("temp files left: %v", leftovers)
		}
		if opener.reader.max > 32<<10 {
			t.Fatalf("max read=%d", opener.reader.max)
		}
	})
}

func TestMaterializeUploadedRemovesCrashLeftover(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "6")
	otherDir := filepath.Join(dir, "7")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := writeKeptNames(t, taskDir)
	otherKept := writeKeptNames(t, otherDir)
	stale := bytes.Repeat([]byte("x"), 4096)
	if err := os.WriteFile(filepath.Join(taskDir, ".takeover-894519183"), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, ".takeover-other"), []byte("other-task"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := []byte("uploaded-object-body")
	opener := &streamOpener{body: body, size: int64(len(body))}
	runner := NewMySQLRunner(dir)
	runner.objectOpener = opener
	row := tasks.BinlogFile{
		FileName: "mysql-bin.000436", ObjectKey: "uploaded-segment",
		UploadState: "UPLOADED", Checksum: tasks.ChecksumMatch,
	}
	logs := captureLog(t)
	gotDir, err := runner.materializeUploaded(context.Background(), tasks.Task{ID: "6", Epoch: 5}, row)
	if err != nil {
		t.Fatal(err)
	}
	if gotDir != taskDir {
		t.Fatalf("dir=%s", gotDir)
	}
	if leftovers := takeoverTemps(t, taskDir); len(leftovers) != 0 {
		t.Fatalf("crash leftover still in task dir: %v", leftovers)
	}
	assertKeptNames(t, taskDir, kept)
	assertKeptNames(t, otherDir, otherKept)
	if _, err := os.Stat(filepath.Join(otherDir, ".takeover-other")); err != nil {
		t.Fatalf("other task takeover temp changed: %v", err)
	}
	root, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(root) != "root" {
		t.Fatalf("data dir root notes changed: %q %v", root, err)
	}
	got, err := os.ReadFile(filepath.Join(taskDir, openFileName(row.FileName, 5)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("open segment = %q", got)
	}
	if !bytes.Contains(logs.Bytes(), []byte("takeover: removed stale temp .takeover-894519183 task=6 size=4096")) {
		t.Fatalf("log missing cleanup line:\n%s", logs.String())
	}
}

func TestStartupRemovesCrashTakeoverTemp(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "6")
	otherDir := filepath.Join(dir, "7")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := writeKeptNames(t, taskDir)
	otherKept := writeKeptNames(t, otherDir)
	if err := os.WriteFile(filepath.Join(taskDir, ".takeover-894519183"), bytes.Repeat([]byte("x"), 128), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, ".takeover-111"), []byte("yy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".worker-id"), []byte("worker-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := NewMySQLRunner(dir)
	logs := captureLog(t)
	if err := runner.RemoveStaleTakeoverTemps(); err != nil {
		t.Fatal(err)
	}
	if leftovers := takeoverTemps(t, taskDir); len(leftovers) != 0 {
		t.Fatalf("startup left %v", leftovers)
	}
	if leftovers := takeoverTemps(t, otherDir); len(leftovers) != 0 {
		t.Fatalf("startup left other task temp %v", leftovers)
	}
	assertKeptNames(t, taskDir, kept)
	assertKeptNames(t, otherDir, otherKept)
	id, err := os.ReadFile(filepath.Join(dir, ".worker-id"))
	if err != nil || string(id) != "worker-b\n" {
		t.Fatalf("worker id file changed: %q %v", id, err)
	}
	text := logs.String()
	if !bytes.Contains(logs.Bytes(), []byte("takeover: removed stale temp .takeover-894519183 task=6 size=128")) {
		t.Fatalf("log missing task 6 line:\n%s", text)
	}
	if !bytes.Contains(logs.Bytes(), []byte("takeover: removed stale temp .takeover-111 task=7 size=2")) {
		t.Fatalf("log missing task 7 line:\n%s", text)
	}
}

func TestInFlightTakeoverTempIsNotRemoved(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "6")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := writeKeptNames(t, taskDir)
	body := bytes.Repeat([]byte("inflight-segment"), 64)
	started := make(chan struct{})
	release := make(chan struct{})
	opener := &gateOpener{body: body, size: int64(len(body)), started: started, release: release}
	runner := NewMySQLRunner(dir)
	runner.objectOpener = opener
	row := tasks.BinlogFile{
		FileName: "mysql-bin.000436", ObjectKey: "uploaded-segment",
		UploadState: "UPLOADED", Checksum: tasks.ChecksumMatch,
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := runner.materializeUploaded(context.Background(), tasks.Task{ID: "6", Epoch: 5}, row)
		errCh <- err
	}()
	select {
	case <-started:
	case err := <-errCh:
		t.Fatalf("materialize finished before the download blocked: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}

	live := takeoverTemps(t, taskDir)
	if len(live) != 1 {
		t.Fatalf("in-flight temps=%v", live)
	}
	livePath := live[0]
	extra := filepath.Join(taskDir, ".takeover-extra")
	if err := os.WriteFile(extra, []byte("stale-beside-live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runner.RemoveStaleTakeoverTemps(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("in-flight temp removed: %v", err)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Fatalf("stale temp beside the download still present: %v", err)
	}
	assertKeptNames(t, taskDir, kept)

	close(release)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if leftovers := takeoverTemps(t, taskDir); len(leftovers) != 0 {
		t.Fatalf("temps left after the download finished: %v", leftovers)
	}
	got, err := os.ReadFile(filepath.Join(taskDir, openFileName(row.FileName, 5)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("open segment = %d bytes", len(got))
	}
	assertKeptNames(t, taskDir, kept)
}

func writeKeptNames(t *testing.T, taskDir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"mysql-bin.000436.sealed.e4": []byte("sealed"),
		"mysql-bin.000437.open.e5":   []byte("open"),
		"mysql-bin.000438":           []byte("active"),
		"task-6.binlog":              []byte("placeholder"),
		"notes.txt":                  []byte("notes"),
		"leftover.dat":               []byte("other"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(taskDir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func assertKeptNames(t *testing.T, taskDir string, files map[string][]byte) {
	t.Helper()
	for name, body := range files {
		got, err := os.ReadFile(filepath.Join(taskDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("%s changed", name)
		}
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

type gateOpener struct {
	body    []byte
	size    int64
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (o *gateOpener) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	return &gateReader{rest: append([]byte(nil), o.body...), started: o.started, release: o.release, once: &o.once}, o.size, nil
}

type gateReader struct {
	rest    []byte
	started chan struct{}
	release chan struct{}
	once    *sync.Once
	closed  bool
}

func (g *gateReader) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.started) })
	<-g.release
	n := len(p)
	if n > len(g.rest) {
		n = len(g.rest)
	}
	copy(p[:n], g.rest[:n])
	g.rest = g.rest[n:]
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (g *gateReader) Close() error {
	g.closed = true
	return nil
}

func takeoverTemps(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".takeover-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}
