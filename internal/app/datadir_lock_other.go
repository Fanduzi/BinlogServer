//go:build !unix

// Package app provides module-level functionality for app.
// input: the standalone data_dir
// output: lockDataDir as a no-op on platforms without flock (the single-writer guard is unix only)
// pos: ADR 0010 §Single writer, non-unix build
// note: if this file changes, update this header and module README.md.
package app

// DataDirLockName is the lock file a standalone process holds in data_dir.
const DataDirLockName = ".tasks.lock"

func lockDataDir(string) (func(), error) { return func() {}, nil }
