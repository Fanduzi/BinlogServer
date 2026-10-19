// Package health provides module-level functionality for health.
// input: a Registry, the stored open findings of one task, and that task's Facts
// output: Desire (findings that should be open), Reconcile (transitions; a signal's CLOSE carries SignalState.SuccessFactAt when the success named one, §25.8 R3; a chain-directory finding closes with OWNER_CHANGED when Facts.ChainOwnerChanged, §30), HealthOf (verdict; Facts.NodeGone forces chain.local unknown and ignores chain-directory severity, §30), Candidates and Pending (the two-pass Confirm of design §20.3: a Confirm kind opens only when the previous pass derived it too)
// pos: the one lifecycle function; pure and idempotent
// note: if this file changes, update this header and module README.md.
package health

import (
	"sort"
	"strings"
	"time"
)

func (f *Facts) complete(s Scope) bool { return f != nil && f.Complete[s] }

// instanceComplete: every scope this instance is judged from is complete
// (design §21.4). An object-origin instance needs chain.objects and its
// segment's prefix listed completely; a local-origin SEGMENT_MISSING
// instance needs only chain.local. Other kinds need all their scopes.
func (f *Facts) instanceComplete(spec KindSpec, key Key) bool {
	if seg, ok := IsObjectSubject(key.Subject); ok {
		return f.complete(ScopeChainObjects) && f.ObjectSegments[seg]
	}
	if spec.Kind != SegmentMissing {
		return f.allComplete(spec.Scopes)
	}
	var scopes []Scope
	for _, s := range spec.Scopes {
		if s != ScopeChainObjects {
			scopes = append(scopes, s)
		}
	}
	return f.allComplete(scopes)
}

// configScoped: the instance exists only because the task has object
// storage, so removing that configuration closes it (Reason CONFIG).
func configScoped(key Key) bool {
	if _, ok := IsObjectSubject(key.Subject); ok {
		return true
	}
	return key.Kind == UploadLate || key.Kind == OrphanRow
}

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
func (r Registry) derive(f *Facts, open map[Key]bool) (Desired, map[Key]bool) {
	out := Desired{}
	raw, waivedKeys := r.deriveRaw(f)
	for key, in := range raw {
		if (r[key.Kind].Confirm || in.Confirm) && !open[key] && !f.Open[key] && !f.Confirmed[key] {
			continue
		}
		out[key] = in
	}
	return out, waivedKeys
}

// Candidates are the Confirm-kind keys derived this pass: the next pass's
// Facts.Confirmed (design §20.3).
func (r Registry) Candidates(f *Facts) map[Key]bool {
	raw, _ := r.deriveRaw(f)
	out := map[Key]bool{}
	for key, in := range raw {
		if r[key.Kind].Confirm || in.Confirm {
			out[key] = true
		}
	}
	return out
}

// Pending are derived Confirm-kind keys neither open nor confirmed: they
// open if the next pass derives them again. Sorted.
func (r Registry) Pending(f *Facts) []Key {
	var out []Key
	for key := range r.Candidates(f) {
		if !f.Open[key] && !f.Confirmed[key] {
			out = append(out, key)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Subject < b.Subject
	})
	return out
}

func (r Registry) deriveRaw(f *Facts) (Desired, map[Key]bool) {
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
	d, _ := r.derive(f, nil)
	return d
}

// Reconcile turns the stored open set into the desired set. Only managed
// kinds transition; a finding whose scope is incomplete, whose ClearRule
// does not hold, or that is inside its hold-down stays open. Pure: the same
// (open, f) gives the same transitions, sorted by key, and reconciling the
// result again gives none. open must be the task's complete open set.
func (r Registry) Reconcile(open []Finding, f *Facts) ([]Transition, Desired) {
	openKeys := map[Key]bool{}
	for _, o := range open {
		if o.TaskID == f.TaskID {
			openKeys[o.Key()] = true
		}
	}
	desired, waivedKeys := r.derive(f, openKeys)
	var out []Transition
	seen := map[Key]bool{}
	// ownerChanged is not in seen: a still-desired instance of the same key
	// opens in this pass (design §30).
	ownerChanged := map[Key]bool{}
	for _, o := range open {
		spec, ok := r[o.Kind]
		if !ok || !spec.Managed || o.TaskID != f.TaskID {
			continue
		}
		key := o.Key()
		if seen[key] || ownerChanged[key] {
			continue // a duplicate stored row for the same key: one transition per key
		}
		if f.ChainOwnerChanged && chainDirectoryOnly(spec) {
			ownerChanged[key] = true
			out = append(out, closeOf(o, spec, ReasonOwnerChanged, f))
			continue
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
			if (op == OpUpdate || op == OpDowngrade) && !f.instanceComplete(spec, key) {
				op = "" // a pass missing one of the kind's scopes may raise severity, never rewrite or lower it
			}
			if op != "" {
				out = append(out, Transition{Key: key, Op: op, Reason: ReasonEval, At: f.Now, FactAt: factAt(want, f), KindVersion: spec.Version, Instance: want})
			}
			continue
		}
		if f.NoObjectStorage && configScoped(key) {
			tr := closeOf(o, spec, ReasonConfig, f)
			tr.Instance.Detail = strings.TrimSpace(tr.Instance.Detail + " — object storage no longer configured (" + f.ObjectConfigAudit + "; at " + f.Now.UTC().Format(time.RFC3339) + ")")
			out = append(out, tr)
			continue
		}
		if !f.instanceComplete(spec, key) {
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
		reason := ReasonEval
		if spec.Clear.Reason != nil {
			if r := spec.Clear.Reason(f, key); r != "" {
				reason = r
			}
		}
		out = append(out, closeOf(o, spec, reason, f))
	}
	for key, want := range desired {
		if !seen[key] {
			out = append(out, Transition{Key: key, Op: OpOpen, Reason: ReasonEval, At: f.Now, FactAt: factAt(want, f), KindVersion: r[key.Kind].Version, Instance: want})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if lessKey(out[i].Key, out[j].Key) {
			return true
		}
		if lessKey(out[j].Key, out[i].Key) {
			return false
		}
		return opRank(out[i].Op) < opRank(out[j].Op)
	})
	return out, desired
}

// chainDirectoryOnly: the finding describes the task directory (chain.local
// and maybe chain.objects), not task.state or signals. UPLOAD_UNVERIFIED
// is not in this set (design §30).
func chainDirectoryOnly(spec KindSpec) bool {
	if len(spec.Scopes) == 0 {
		return false
	}
	for _, s := range spec.Scopes {
		if s != ScopeChainLocal && s != ScopeChainObjects {
			return false
		}
	}
	return true
}

func opRank(op Op) int {
	switch op {
	case OpClose:
		return 0
	case OpOpen:
		return 2
	default:
		return 1
	}
}

func closeOf(o Finding, spec KindSpec, reason string, f *Facts) Transition {
	fact := f.Now
	// A signal that answered again before this pass closes at that answer
	// when the caller named its time (META_UNREACHABLE, design §25.8 R3).
	if st, ok := f.Signals[SignalKey{Kind: o.Kind, Subject: o.Subject}]; ok && succeeded(st, ok) &&
		!st.SuccessFactAt.IsZero() && !st.SuccessFactAt.After(f.Now) {
		fact = st.SuccessFactAt
	}
	return Transition{Key: o.Key(), Op: OpClose, Reason: reason, At: f.Now, FactAt: fact, KindVersion: spec.Version,
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
		if f.NodeGone {
			if spec, ok := r[o.Kind]; ok && chainDirectoryOnly(spec) {
				continue // the directory is on a node that is gone (§30)
			}
		}
		if rk := o.Severity.rank(); rk >= Warning.rank() {
			if rk > worst {
				worst, h.Reasons = rk, nil
			}
			if rk == worst {
				h.Reasons = append(h.Reasons, o.Key())
			}
		}
	}
	produced := Produced
	if f.Produced != nil {
		produced = f.Produced
	}
	scopes := map[Scope]bool{}
	for _, spec := range r {
		if !spec.Managed {
			continue
		}
		for _, s := range spec.Scopes {
			if s != ScopeSignals && produced[s] && !f.complete(s) {
				scopes[s] = true
			}
		}
	}
	if f.NodeGone {
		scopes[ScopeChainLocal] = true
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
	if f.NodeGone && h.Status == StatusUnknown {
		h.UnknownDetail = "node gone"
	}
	return h
}
