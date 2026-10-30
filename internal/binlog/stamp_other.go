//go:build !linux && !darwin

// Package binlog provides module-level functionality for binlog.
// input: an os.FileInfo on an OS without a readable change time
// output: 0; the stamp then depends on size and mtime alone
// pos: the ctime fallback of StampOf for every platform stamp_linux.go and stamp_darwin.go do not cover
// note: if this file changes, update this header and module README.md.
package binlog

import "os"

func changeTime(os.FileInfo) int64 { return 0 }
