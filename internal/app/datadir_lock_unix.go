//go:build unix

// Package app provides module-level functionality for app.
// input: the standalone data_dir
// output: lockDataDir: an exclusive non-blocking flock on {data_dir}/.tasks.lock held for the process lifetime, with the holder pid written in the file; a held lock is an error naming the file and that pid
// pos: ADR 0010 §Single writer (#393 QA P2-2): one standalone process per data_dir
// note: if this file changes, update this header and module README.md.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DataDirLockName is the lock file a standalone process holds in data_dir.
const DataDirLockName = ".tasks.lock"

// lockDataDir takes the single-writer lock. The kernel drops it when the
// process exits, including kill -9, so a crash never leaves it held.
func lockDataDir(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data_dir lock: %w", err)
	}
	path := filepath.Join(dataDir, DataDirLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("data_dir lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			pid := strings.TrimSpace(string(holder))
			if pid == "" {
				pid = "unknown"
			}
			return nil, fmt.Errorf("another binlog-server process holds %s (pid %s): one standalone process per data_dir; stop the other process or use a different data_dir", path, pid)
		}
		return nil, fmt.Errorf("data_dir lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		_ = f.Sync()
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
