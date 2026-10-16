//go:build !linux

// Package replication provides module-level functionality for replication.
// input: a sealed-segment rename (open path, sealed path)
// output: renameNoReplaceOS, the platform step of renameNoReplace (Linux renameat2 RENAME_NOREPLACE; elsewhere unsupported so the caller falls back to stat then rename) (ADR 0009 P2-6b, #417)
// pos: seal path of the data-plane write fence
// note: if this file changes, update this header and module README.md.
package replication

func renameNoReplaceOS(string, string) error { return errNoReplaceUnsupported }
