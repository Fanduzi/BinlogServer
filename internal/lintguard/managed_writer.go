// Package lintguard provides module-level functionality for lintguard.
// input: Go source directories
// output: managed-write sites (stub)
// pos: ADR 0009 phase 2 P2-4
// note: if this file changes, update this header and module README.md.
package lintguard

// ManagedWrite is a Report/Resolve of a kind the evaluator manages.
const ManagedWrite Kind = "managed-write"

// FindingConstants (stub).
func FindingConstants(dir string) (map[string]string, error) { return map[string]string{}, nil }

// ScanManagedWrites (stub).
func ScanManagedWrites(root, dir string, managed map[string]bool) ([]Site, error) { return nil, nil }
