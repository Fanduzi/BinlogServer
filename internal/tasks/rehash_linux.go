//go:build linux

// Package tasks provides module-level functionality for tasks.
// input: the rehasher's goroutine (locked to its OS thread) and each file it read
// output: IOPRIO_CLASS_IDLE for that thread and POSIX_FADV_DONTNEED after a read; failures are ignored (the byte budget still holds)
// pos: Linux I/O hints for the P2-3 Rehasher (design §5.7)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"

	"golang.org/x/sys/unix"
)

const (
	ioprioWhoProcess = 1
	ioprioClassIdle  = 3
	ioprioClassShift = 13
)

func setIdleIOPriority() {
	_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<ioprioClassShift)
}

func dropPageCache(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
