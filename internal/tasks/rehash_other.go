//go:build !linux

// Package tasks provides module-level functionality for tasks.
// input: the rehasher's goroutine and each file it read
// output: nothing: no I/O hints off Linux; the byte budget alone limits the rehasher
// pos: non-Linux stubs for the P2-3 Rehasher I/O hints
// note: if this file changes, update this header and module README.md.
package tasks

import "os"

func setIdleIOPriority() {}

func dropPageCache(*os.File) {}
