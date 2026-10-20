// Package ownership is the pure task-ownership transition from ADR 0011.
// input: State facts (tail, chain, refusal, phase)
// output: the open finding set, as a function of those facts
// pos: ADR 0011 §3.5 derivation; a held fact does not close and reopen
// note: if this file changes, update this header and module README.md.
package ownership

import "sort"

func derive(s State) []Finding {
	if s.Deleted {
		return nil
	}
	var out []Finding
	if s.Desired == DesiredRun && !s.TailSince.IsZero() {
		out = append(out, Finding{Name: FindingTail, Subject: s.TaskID, Severity: SeverityCritical})
	}
	if s.Chain.Verified() && s.Chain.MissingSealed {
		out = append(out, Finding{Name: FindingMissing, Subject: s.TaskID, Severity: SeverityCritical})
	}
	if s.Chain.Verified() && s.Chain.Unreadable {
		out = append(out, Finding{Name: FindingUnreadable, Subject: s.TaskID, Severity: SeverityCritical})
	}
	if s.NoProgress {
		out = append(out, Finding{Name: FindingNoProgress, Subject: s.TaskID, Severity: SeverityCritical})
	}
	if s.Phase == PhaseFailed && s.FailedReason != "" {
		out = append(out, Finding{Name: FindingFailed, Subject: s.FailedReason, Severity: SeverityCritical})
	}
	if s.MetaLockStalled {
		out = append(out, Finding{Name: FindingLockStalled, Subject: s.TaskID, Severity: SeverityCritical, OnCluster: true})
	}
	if s.MixedVersion {
		out = append(out, Finding{Name: FindingMixedVersion, Subject: "_cluster", Severity: SeverityCritical, OnCluster: true})
	}
	if len(s.Refusal) > 0 {
		nodes := make([]string, 0, len(s.Refusal))
		for id, r := range s.Refusal {
			if r.Reason == RefusalEpochLost {
				nodes = append(nodes, id)
			}
		}
		sort.Strings(nodes)
		for _, id := range nodes {
			r := s.Refusal[id]
			sev := SeverityWarning
			if r.Attempts >= 3 {
				sev = SeverityCritical
			}
			out = append(out, Finding{Name: FindingEpochLost, Subject: id, Severity: sev})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func diffFindings(before, after []Finding) (opened, closed []Finding) {
	prev := map[string]Finding{}
	next := map[string]Finding{}
	for _, f := range before {
		prev[f.Key()] = f
	}
	for _, f := range after {
		next[f.Key()] = f
	}
	for k, f := range next {
		if _, ok := prev[k]; !ok {
			opened = append(opened, f)
		}
	}
	for k, f := range prev {
		if _, ok := next[k]; !ok {
			closed = append(closed, f)
		}
	}
	sort.Slice(opened, func(i, j int) bool { return opened[i].Key() < opened[j].Key() })
	sort.Slice(closed, func(i, j int) bool { return closed[i].Key() < closed[j].Key() })
	return opened, closed
}

func hasFinding(s State, name, subject string) bool {
	for _, f := range s.Findings {
		if f.Name == name && f.Subject == subject {
			return true
		}
	}
	return false
}

func criticalOpen(s State) bool {
	for _, f := range s.Findings {
		if f.Severity == SeverityCritical {
			return true
		}
	}
	return false
}
