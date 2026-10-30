//go:build darwin

// Package binlog provides module-level functionality for binlog.
// input: an os.FileInfo whose Sys() is a darwin syscall.Stat_t
// output: the file's change time in nanoseconds (0 when it cannot be read)
// pos: the darwin ctime backend of StampOf, beside stamp_linux.go
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"os"
	"syscall"
)

func changeTime(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
	}
	return 0
}
