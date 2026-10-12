// Package binlog provides module-level functionality for binlog.
// input: a task directory, checkpoint and start sets
// output: the three questions TestLedgerGolden asks, answered through the segment ledger
// pos: the only file that differs from the main-branch adapter used to record golden.json
// note: if this file changes, update this header and module README.md.
package binlog

func goldenDamage(dir, checkpoint, start, flavor string) (StorageProblem, bool) {
	return LedgerAt(dir).Damage(checkpoint, start, flavor)
}

func goldenTail(dir string) (OpenTail, bool) { return LedgerAt(dir).Tail() }

func goldenUnion(flavor, seed string, tail OpenTail) string { return unionDurable(flavor, seed, tail) }
