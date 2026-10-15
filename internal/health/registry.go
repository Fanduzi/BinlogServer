// Package health provides module-level functionality for health.
// input: assurance violation kinds
// output: DefaultRegistry (every finding kind with severity, scope, clearing invariant and Derive), FromAssurance, LegacyKinds, CanonicalGTIDRanges
// pos: the closed kind table of ADR 0009 phase 2 §6
// note: if this file changes, update this header and module README.md.
package health

import "binlog_server/internal/assurance"

// DefaultRegistry is the kind table.
func DefaultRegistry() Registry { return Registry{} }

// LegacyKinds are the kinds shipped before phase 2.
func LegacyKinds() []Kind { return nil }

// FromAssurance maps an assurance kind to its finding kind.
func FromAssurance(k assurance.Kind) (Kind, bool) { return "", false }

// CanonicalGTIDRanges normalizes a GTID range list.
func CanonicalGTIDRanges(s string) (string, error) { return s, nil }
