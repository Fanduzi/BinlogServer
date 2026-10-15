// Package health provides module-level functionality for health.
// input: a Registry, the stored open findings of one task, and that task's Facts
// output: Desire (findings that should be open), Reconcile (transitions), HealthOf (verdict)
// pos: the one lifecycle function; pure and idempotent
// note: if this file changes, update this header and module README.md.
package health

import (
	"sort"
	"strings"
	"time"
)

func (f *Facts) complete(s Scope) bool { return f != nil && f.Complete[s] }

// allComplete: every scope the kind needs is complete (close and update).
func (f *Facts) allComplete(scopes []Scope) bool {
	for _, s := range scopes {
		if !f.complete(s) {
			return false
		}
	}
	return len(scopes) > 0
}

// waived reports the instance's evidence is exactly waived.
func waived(f *Facts, in Instance, spec KindSpec) bool {
	if !spec.Waivable || in.Evidence == "" {
		return false
	}
	ev, err := CanonicalGTIDRanges(in.Evidence)
	if err != nil {
		return false
	}
	for _, w := range f.Waivers {
		if w.TaskID != in.TaskID || w.Kind != in.Kind || w.Subject != in.Subject {
			continue
		}
		if we, err := CanonicalGTIDRanges(w.Evidence); err == nil && we == ev {
			return true
		}
	}
	return false
}

// derive is Derive of every managed kind whose scope is complete, before
// waivers; the bool map says which instances a waiver removes.
func (r Registry) derive(f *Facts) (Desired, map[Key]bool) {
	out, waivedKeys := Desired{}, map[Key]bool{}
	for _, k := range r.kinds() {
		spec := r[k]
		if !spec.Managed || spec.Derive == nil || len(spec.Scopes) == 0 || !f.complete(spec.Scopes[0]) {
			continue
		}
		for _, in := range spec.Derive(f) {
			in.TaskID, in.Kind = f.TaskID, k
			if in.Severity == "" {
				in.Severity = spec.Severity
			}
			if waived(f, in, spec) {
				waivedKeys[in.Key] = true
				continue
			}
			if prev, ok := out[in.Key]; ok && prev.Severity.rank() >= in.Severity.rank() {
				continue
			}
			out[in.Key] = in
		}
	}
	return out, waivedKeys
}

func (r Registry) kinds() []Kind {
	ks := make([]Kind, 0, len(r))
	for k := range r {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

// Desire is what should be open: Derive of every managed kind whose scope
// is complete, less exactly waived evidence.
func (r Registry) Desire(f *Facts) Desired {
	d, _ := r.derive(f)
	return d
}

// Reconcile turns the stored open set into the desired set. Only managed
// kinds transition; a finding whose scope is incomplete, whose ClearRule
// does not hold, or that is inside its hold-down stays open. Pure: the same
// (open, f) gives the same transitions, sorted by key, and reconciling the
// result again gives none. open must be the task's complete open set.
func (r Registry) Reconcile(open []Finding, f *Facts) ([]Transition, Desired) {
	desired, waivedKeys := r.derive(f)
	var out []Transition
	seen := map[Key]bool{}
	for _, o := range open {
		spec, ok := r[o.Kind]
		if !ok || !spec.Managed || o.TaskID != f.TaskID {
			continue
		}
		key := o.Key()
		if seen[key] {
			continue // a duplicate stored row for the same key: one transition per key
		}
		seen[key] = true
		if want, ok := desired[key]; ok {
			op := Op("")
			switch {
			case want.Severity.rank() > o.Severity.rank():
				op = OpEscalate
			case want.Severity.rank() < o.Severity.rank():
				op = OpDowngrade
			case want.Severity != o.Severity || want.Detail != o.Detail || want.Evidence != o.Evidence || strings.Join(want.Segments, "\x00") != strings.Join(o.Segments, "\x00"):
				op = OpUpdate
			}
			if op != "" {
				out = append(out, Transition{Key: key, Op: op, Reason: ReasonEval, At: f.Now, FactAt: factAt(want, f), KindVersion: spec.Version, Instance: want})
			}
			continue
		}
		if !f.allComplete(spec.Scopes) {
			continue
		}
		if waivedKeys[key] {
			out = append(out, closeOf(o, spec, ReasonWaived, f))
			continue
		}
		if spec.Clear.Holds != nil && !spec.Clear.Holds(f, key) {
			continue
		}
		if spec.Clear.HoldDown > 0 {
			since, ok := f.AbsentSince[key]
			if !ok || f.Now.Sub(since) < spec.Clear.HoldDown {
				continue
			}
		}
		out = append(out, closeOf(o, spec, ReasonEval, f))
	}
	for key, want := range desired {
		if !seen[key] {
			out = append(out, Transition{Key: key, Op: OpOpen, Reason: ReasonEval, At: f.Now, FactAt: factAt(want, f), KindVersion: r[key.Kind].Version, Instance: want})
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out, desired
}

func closeOf(o Finding, spec KindSpec, reason string, f *Facts) Transition {
	return Transition{Key: o.Key(), Op: OpClose, Reason: reason, At: f.Now, FactAt: f.Now, KindVersion: spec.Version,
		Instance: Instance{Key: o.Key(), Severity: o.Severity, Detail: o.Detail, Evidence: o.Evidence, Segments: o.Segments}}
}

// factAt is the instance's fact time, or the evaluation time.
func factAt(in Instance, f *Facts) time.Time {
	if in.FactAt.IsZero() {
		return f.Now
	}
	return in.FactAt
}

func lessKey(a, b Key) bool {
	if a.TaskID != b.TaskID {
		return a.TaskID < b.TaskID
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Subject < b.Subject
}

// HealthOf is the task's health given its open findings (after applying
// this pass's transitions): any CRITICAL is UNHEALTHY, any WARNING is
// DEGRADED (INFO counts for neither), otherwise an incomplete scope that a
// managed kind is judged from is UNKNOWN; a scope no evaluator produces
// yet (Produced) never is. The signal scope's internal
// unknown (open row, no success since start) is never a reason: the open
// row already decides.
func (r Registry) HealthOf(open []Finding, f *Facts) TaskHealth {
	h := TaskHealth{TaskID: f.TaskID, Status: StatusHealthy, EvaluatedAt: f.Now}
	worst := 0
	for _, o := range open {
		if rk := o.Severity.rank(); rk >= Warning.rank() {
			if rk > worst {
				worst, h.Reasons = rk, nil
			}
			if rk == worst {
				h.Reasons = append(h.Reasons, o.Key())
			}
		}
	}
	scopes := map[Scope]bool{}
	for _, spec := range r {
		if !spec.Managed {
			continue
		}
		for _, s := range spec.Scopes {
			if s != ScopeSignals && Produced[s] && !f.complete(s) {
				scopes[s] = true
			}
		}
	}
	for s := range scopes {
		h.Unknown = append(h.Unknown, s)
	}
	sort.Slice(h.Unknown, func(i, j int) bool { return h.Unknown[i] < h.Unknown[j] })
	sort.Slice(h.Reasons, func(i, j int) bool { return lessKey(h.Reasons[i], h.Reasons[j]) })
	switch {
	case worst == Critical.rank():
		h.Status = StatusUnhealthy
	case worst == Warning.rank():
		h.Status = StatusDegraded
	case len(h.Unknown) > 0:
		h.Status = StatusUnknown
	}
	return h
}
