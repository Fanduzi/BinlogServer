// Package health provides module-level functionality for health.
// input: a Registry, the stored open findings of one task, and that task's Facts
// output: Desire (findings that should be open), Reconcile (transitions), HealthOf (verdict)
// pos: the one lifecycle function; pure and idempotent
// note: if this file changes, update this header and module README.md.
package health

// Desire is what should be open: Derive of every managed kind whose scope is complete.
func (r Registry) Desire(f *Facts) Desired { return Desired{} }

// Reconcile turns the stored open set into the desired set.
func (r Registry) Reconcile(open []Finding, f *Facts) ([]Transition, Desired) { return nil, Desired{} }

// HealthOf is the task's health given its open findings.
func (r Registry) HealthOf(open []Finding, f *Facts) TaskHealth { return TaskHealth{} }
