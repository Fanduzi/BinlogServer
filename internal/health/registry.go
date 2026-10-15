// Package health provides module-level functionality for health.
// input: assurance violation kinds
// output: DefaultRegistry (every finding kind with severity, scope, clearing invariant and Derive), FromAssurance, LegacyKinds, CanonicalGTIDRanges
// pos: the closed kind table of ADR 0009 phase 2 §6; in P2-1 no kind is Managed
// note: if this file changes, update this header and module README.md.
package health

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"binlog_server/internal/assurance"
)

var fromAssurance = map[assurance.Kind]Kind{
	assurance.I1Regression:       SegmentDamaged,
	assurance.I1Unreadable:       SegmentUnreadable,
	assurance.I2IndexGap:         ChainIndexGap,
	assurance.I2SwitchUnrecorded: ChainIndexGap,
	assurance.I2OrderInvalid:     ChainIndexGap,
	assurance.I3GTIDHole:         GTIDHole,
	assurance.I3StoredHole:       GTIDHole,
	assurance.I3LegacyFailback:   LegacyFailback,
	assurance.I4Overlap:          SegmentOverlap,
	assurance.I5CheckpointAhead:  CheckpointAhead,
	assurance.I6MissingLocal:     SegmentMissing,
	assurance.I6MissingObject:    SegmentMissing,
	assurance.I6MissingRow:       SegmentMissing,
	assurance.I6SizeMismatch:     SegmentMissing,
	assurance.I6OrphanRow:        OrphanRow,
	assurance.I7UploadLate:       UploadLate,
	assurance.I7Unverified:       UploadUnverified,
}

// FromAssurance maps an assurance kind to its finding kind (many to one).
func FromAssurance(k assurance.Kind) (Kind, bool) {
	out, ok := fromAssurance[k]
	return out, ok
}

// LegacyKinds are the sixteen kinds shipped before phase 2.
func LegacyKinds() []Kind {
	return []Kind{TaskPersistFailed, MetadataWriteBlocked, EventPersistFailed, EventReadFailed, DumpCleanupPersistFailed,
		DiskListFailed, UploadUnverified, UploadRecordFailed, RetentionRecordFailed, RetryLoopFailed, DumpStalled,
		OpenTailCutFailed, MetricSourceFailed, TransitionStalled, SegmentUnreadable, SegmentOverlap}
}

// chainDerive opens one instance per violation of the mapped assurance
// kinds, subject = the first violating segment.
func chainDerive(kind Kind) func(f *Facts) []Instance {
	return func(f *Facts) []Instance {
		if f.Chain == nil {
			return nil
		}
		var out []Instance
		for _, v := range f.Chain.Violations {
			if k, ok := fromAssurance[v.Kind]; !ok || k != kind {
				continue
			}
			subject := ""
			if len(v.Segments) > 0 {
				subject = v.Segments[0]
			}
			out = append(out, Instance{Key: Key{Subject: subject}, Detail: v.Detail, Evidence: v.Evidence, Segments: append([]string(nil), v.Segments...)})
		}
		return out
	}
}

// signalDerive opens one instance per signal of kind whose latest
// observation is a failure.
func signalDerive(kind Kind) func(f *Facts) []Instance {
	return func(f *Facts) []Instance {
		var out []Instance
		for k, st := range f.Signals {
			if k.Kind == kind && st.FailureSeq > st.SuccessSeq {
				out = append(out, Instance{Key: Key{Subject: k.Subject}, Detail: st.Detail})
			}
		}
		return out
	}
}

// signalCleared: a success observed since start that is later than any
// failure this process saw. With no success since start the open row stays
// (the internal unknown of §5.5).
func signalCleared(kind Kind) func(f *Facts, key Key) bool {
	return func(f *Facts, key Key) bool {
		st, ok := f.Signals[SignalKey{Kind: kind, Subject: key.Subject}]
		return ok && st.SuccessSeq > 0 && st.SuccessSeq > st.FailureSeq
	}
}

func signalSpec(kind Kind, sev Severity, inv, text string) KindSpec {
	return KindSpec{Kind: kind, Severity: sev, Scopes: []Scope{ScopeSignals}, Version: 1, Derive: signalDerive(kind),
		Clear: ClearRule{Invariant: inv, Holds: signalCleared(kind), Text: text}}
}

func chainSpec(kind Kind, sev Severity, scope Scope, inv, text string) KindSpec {
	return KindSpec{Kind: kind, Severity: sev, Scopes: []Scope{scope}, Version: 1, SegmentScoped: true, Derive: chainDerive(kind),
		Clear: ClearRule{Invariant: inv, Text: text}}
}

// stateSpec: kinds derived from task state; their Derive lands with the
// PR that takes them over (P2-4/P2-6), so they never open before then.
func stateSpec(kind Kind, sev Severity, scope Scope, inv, text string) KindSpec {
	return KindSpec{Kind: kind, Severity: sev, Scopes: []Scope{scope}, Version: 1, Clear: ClearRule{Invariant: inv, Text: text}}
}

// DefaultRegistry is the kind table of design §6. No kind is Managed yet:
// each later PR flips the kinds it takes over.
func DefaultRegistry() Registry {
	specs := []KindSpec{
		chainSpec(SegmentUnreadable, Critical, ScopeChainLocal, "I1", "the segment reads cleanly again, or is no longer on the chain"),
		chainSpec(SegmentOverlap, Critical, ScopeChainLocal, "I4", "replay order holds the source file once, with no GTID or position overlap"),
		chainSpec(SegmentDamaged, Critical, ScopeChainLocal, "I1", "end_log_pos no longer goes backwards"),
		chainSpec(ChainIndexGap, Critical, ScopeChainLocal, "I2", "indexes are contiguous and every source switch is recorded"),
		chainSpec(LegacyFailback, Critical, ScopeChainLocal, "I3", "the planner no longer reports a legacy failback"),
		chainSpec(CheckpointAhead, Critical, ScopeChainLocal, "I5", "the checkpoint is at or before the chain's cut point"),
		chainSpec(SegmentMissing, Critical, ScopeChainLocal, "I6", "catalog row, local file and object agree, or a retention marker explains the absence"),
		chainSpec(OrphanRow, Warning, ScopeChainObjects, "I6", "the row has its bytes again, or carries a retention marker"),
		chainSpec(UploadLate, Warning, ScopeChainLocal, "I7", "the segment is UPLOADED with checksum match"),
		chainSpec(UploadUnverified, Critical, ScopeChainLocal, "I7", "the segment's checksum is match"),
		stateSpec(SegmentEmptyLeftover, Info, ScopeChainLocal, "I4", "the leftover open file is removed, or sealed with events"),
		signalSpec(UploadRecordFailed, Critical, "S8", "a later upload record for the segment is written, or its row is already UPLOADED"),
		signalSpec(MetadataWriteBlocked, Critical, "S2", "a later checkpoint or task row write succeeds"),
		signalSpec(TaskPersistFailed, Critical, "S2/S3", "a later task row write succeeds"),
		signalSpec(EventPersistFailed, Warning, "S1", "a later event of the same type is written"),
		signalSpec(EventReadFailed, Warning, "S13", "the retry budget reads back, or a read probe succeeds"),
		signalSpec(DumpCleanupPersistFailed, Critical, "S3", "the KILL marker is written or cleared and reads back"),
		signalSpec(DiskListFailed, Critical, "S15", "the next directory listing succeeds"),
		signalSpec(RetentionRecordFailed, Warning, "S5/S6", "the next retention pass records and runs to the end"),
		signalSpec(RetryLoopFailed, Warning, "S7", "the next upload retry pass runs to the end"),
		signalSpec(OpenTailCutFailed, Critical, "S4", "the next cut succeeds, or the segment is sealed or gone"),
		signalSpec(MetricSourceFailed, Warning, "S10/S11", "the next scrape computes the source"),
		stateSpec(DumpStalled, Warning, ScopeSignals, "S12", "events or heartbeats arrive again; or the task was deliberately stopped; or the lease moved to another epoch"),
		stateSpec(TransitionStalled, Warning, ScopeTaskState, "lease", "the task leaves STARTING/STOPPING, or a run drives it"),
		stateSpec(TaskFailed, Critical, ScopeTaskState, "task", "the task leaves FAILED, or desired becomes STOP"),
		stateSpec(SourceUnreachable, Critical, ScopeTaskState, "S12", "a dump is established and receives events or heartbeats, or the task is deliberately stopped"),
		stateSpec(SealFactsAbsent, Warning, ScopeStore, "store", "the seal fact columns are found again on the next probe"),
		stateSpec(HealthStale, Warning, ScopeTaskState, "F", "the task is evaluated"),
		stateSpec(HealthStoreDegraded, Warning, ScopeStore, "F", "health writes succeed with migration 000009 present and versions not mixed"),
	}
	gh := chainSpec(GTIDHole, Critical, ScopeChainLocal, "I3", "B ∪ Stored is contiguous at this point, or the exact hole range is waived")
	gh.Waivable = true
	specs = append(specs, gh)
	r := Registry{}
	for _, s := range specs {
		r[s.Kind] = s
	}
	return r
}

// CanonicalGTIDRanges normalizes a waiver's GTID range list: lowercase
// uuid, sorted by uuid, ranges of one uuid ascending and merged when
// adjacent or overlapping, single points as a-a, joined by commas.
func CanonicalGTIDRanges(s string) (string, error) {
	type rng struct{ a, b uint64 }
	by := map[string][]rng{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.LastIndex(part, ":")
		if i <= 0 {
			return "", fmt.Errorf("gtid range %q: want uuid:a-b", part)
		}
		uuid := strings.ToLower(strings.TrimSpace(part[:i]))
		span := strings.TrimSpace(part[i+1:])
		lo, hi, found := strings.Cut(span, "-")
		if !found {
			hi = lo
		}
		a, err1 := strconv.ParseUint(lo, 10, 64)
		b, err2 := strconv.ParseUint(hi, 10, 64)
		if err1 != nil || err2 != nil || a == 0 || b < a {
			return "", fmt.Errorf("gtid range %q: want uuid:a-b with 0 < a <= b", part)
		}
		by[uuid] = append(by[uuid], rng{a, b})
	}
	uuids := make([]string, 0, len(by))
	for u := range by {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	var out []string
	for _, u := range uuids {
		rs := by[u]
		sort.Slice(rs, func(i, j int) bool { return rs[i].a < rs[j].a })
		cur := rs[0]
		flush := func() { out = append(out, fmt.Sprintf("%s:%d-%d", u, cur.a, cur.b)) }
		for _, r := range rs[1:] {
			if r.a <= cur.b+1 {
				if r.b > cur.b {
					cur.b = r.b
				}
				continue
			}
			flush()
			cur = r
		}
		flush()
	}
	return strings.Join(out, ","), nil
}
