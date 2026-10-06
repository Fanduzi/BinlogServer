// Package replication provides module-level functionality for replication.
// input: an object body larger than one copy buffer, and a download that ends short or errors
// output: proof that takeover writes the object through a bounded read, keeps the catalog checksum flag unchanged, and leaves no open segment or temp file when the download does not finish
// pos: regression for streaming an uploaded segment onto the worker during takeover
// note: if this file changes, update this header and module README.md.
package replication

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

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

func takeoverTemps(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".takeover-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}
