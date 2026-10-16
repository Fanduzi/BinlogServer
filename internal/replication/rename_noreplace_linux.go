//go:build linux

// Package replication provides module-level functionality for replication.
// input: a sealed-segment rename (open path, sealed path)
// output: renameNoReplaceOS, the platform step of renameNoReplace (Linux renameat2 RENAME_NOREPLACE; elsewhere unsupported so the caller falls back to stat then rename) (ADR 0009 P2-6b, #417)
// pos: seal path of the data-plane write fence
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplaceOS(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrExist}
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return errNoReplaceUnsupported
	default:
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
}
