// Package health provides module-level functionality for health.
// input: assurance violation kinds
// output: DefaultRegistry (every finding kind with severity, scope, clearing invariant and Derive), FromAssurance, LegacyKinds, CanonicalGTIDRanges
// pos: the closed kind table of ADR 0009 phase 2 §6; since P2-3 SEGMENT_UNREADABLE, SEGMENT_OVERLAP and SEGMENT_EMPTY_LEFTOVER are Managed
// note: if this file changes, update this header and module README.md.
package health

import (
	"fmt"
	"math"
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
	assurance.I4EmptyLeftover:    SegmentEmptyLeftover,
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
				out = append(out, Instance{Key: Key{Subject: k.Subject}, Detail: st.Detail, FactAt: st.FactAt})
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

func chainSpec(kind Kind, sev Severity, scope Scope, inv, text string, more ...Scope) KindSpec {
	return KindSpec{Kind: kind, Severity: sev, Scopes: append([]Scope{scope}, more...), Version: 1, SegmentScoped: true, Derive: chainDerive(kind),
		Clear: ClearRule{Invariant: inv, Text: text}}
}

// stateSpec: kinds derived from task state; their Derive lands with the
// PR that takes them over (P2-4/P2-6), so they never open before then.
func stateSpec(kind Kind, sev Severity, scope Scope, inv, text string, more ...Scope) KindSpec {
	return KindSpec{Kind: kind, Severity: sev, Scopes: append([]Scope{scope}, more...), Version: 1, Clear: ClearRule{Invariant: inv, Text: text}}
}

// p23Managed are the kinds the evaluator owns since P2-3 (design §5.6):
// the legacy Report path no longer writes them when the evaluator is on.
var p23Managed = map[Kind]bool{SegmentUnreadable: true, SegmentOverlap: true, SegmentEmptyLeftover: true}

// DefaultRegistry is the kind table of design §6. P2-3 manages the three
// local segment kinds; each later PR flips the kinds it takes over.
func DefaultRegistry() Registry {
	specs := []KindSpec{
		chainSpec(SegmentUnreadable, Critical, ScopeChainLocal, "I1", "the segment reads cleanly again, or is no longer on the chain"),
		chainSpec(SegmentOverlap, Critical, ScopeChainLocal, "I4", "replay order holds the source file once, with no GTID or position overlap"),
		chainSpec(SegmentDamaged, Critical, ScopeChainLocal, "I1", "end_log_pos no longer goes backwards"),
		chainSpec(ChainIndexGap, Critical, ScopeChainLocal, "I2", "indexes are contiguous and every source switch is recorded"),
		chainSpec(LegacyFailback, Critical, ScopeChainLocal, "I3", "the planner no longer reports a legacy failback"),
		chainSpec(CheckpointAhead, Critical, ScopeChainLocal, "I5", "the checkpoint is at or before the chain's cut point"),
		chainSpec(SegmentMissing, Critical, ScopeChainLocal, "I6", "catalog row, local file and object agree, or a retention marker explains the absence", ScopeChainObjects),
		chainSpec(OrphanRow, Warning, ScopeChainLocal, "I6", "the row has its bytes again, or carries a retention marker", ScopeChainObjects),
		chainSpec(UploadLate, Warning, ScopeChainLocal, "I7", "the segment is UPLOADED with checksum match"),
		chainSpec(UploadUnverified, Critical, ScopeChainLocal, "I7", "the segment's checksum is match", ScopeSignals),
		chainSpec(SegmentEmptyLeftover, Info, ScopeChainLocal, "I4", "the leftover open file is removed, or sealed with events"),
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
		stateSpec(DumpStalled, Warning, ScopeTaskState, "S12", "events or heartbeats arrive again; or the task was deliberately stopped; or the lease moved to another epoch", ScopeSignals),
		stateSpec(TransitionStalled, Warning, ScopeTaskState, "lease", "the task leaves STARTING/STOPPING, or a run drives it"),
		stateSpec(TaskFailed, Critical, ScopeTaskState, "task", "the task leaves FAILED, or desired becomes STOP"),
		stateSpec(SourceUnreachable, Critical, ScopeTaskState, "S12", "a dump is established and receives events or heartbeats, or the task is deliberately stopped", ScopeSignals),
		stateSpec(SealFactsAbsent, Warning, ScopeStore, "store", "the seal fact columns are found again on the next probe"),
		stateSpec(HealthStale, Warning, ScopeTaskState, "F", "the task is evaluated"),
		stateSpec(HealthStoreDegraded, Warning, ScopeStore, "F", "health writes succeed with migration 000009 present and versions not mixed"),
	}
	gh := chainSpec(GTIDHole, Critical, ScopeChainLocal, "I3", "B ∪ Stored is contiguous at this point, or the exact hole range is waived")
	gh.Waivable = true
	specs = append(specs, gh)
	r := Registry{}
	for _, s := range specs {
		s.Managed = p23Managed[s.Kind]
		r[s.Kind] = s
	}
	return r
}

// CanonicalGTIDRanges normalizes a waiver's GTID range list (design §7.1).
// Input: MySQL sets "uuid:a-b[:c-d...]" (hex uuid, single points allowed,
// MySQL 8.4 tags "uuid:tag:a-b" applying to the intervals after them) and
// MariaDB GTIDs "domain-server-seq" or ranges "domain-server:a-b",
// comma separated; one flavor per string. Canonical form: one
// "uuid:a-b" (lowercase uuid) or "domain-server:a-b" (decimal) per merged
// interval, tagged ones as "uuid:tag:a-b" (lowercase tag), sorted by uuid
// then tag (untagged first), or by domain then server numerically, intervals
// ascending, adjacent or overlapping intervals merged, single points as
// a-a, joined by commas with no spaces. "" stays "".
func CanonicalGTIDRanges(s string) (string, error) {
	type rng struct{ a, b uint64 }
	type src struct {
		name      string
		uuid, tag string
		d, srv    uint64
	}
	by := map[src][]rng{}
	flavor := ""
	setFlavor := func(f, part string) error {
		if flavor != "" && flavor != f {
			return fmt.Errorf("gtid %q: MySQL and MariaDB GTIDs mixed", part)
		}
		flavor = f
		return nil
	}
	parseRange := func(span, part string) (rng, error) {
		lo, hi, found := strings.Cut(strings.TrimSpace(span), "-")
		if !found {
			hi = lo
		}
		a, err1 := strconv.ParseUint(lo, 10, 64)
		b, err2 := strconv.ParseUint(hi, 10, 64)
		if err1 != nil || err2 != nil || a == 0 || b < a {
			return rng{}, fmt.Errorf("gtid range %q: want a-b with 0 < a <= b", part)
		}
		return rng{a, b}, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		head := strings.TrimSpace(fields[0])
		if dash := strings.Split(head, "-"); len(dash) == 5 {
			// MySQL: uuid:interval[:interval...], with MySQL 8.4 tags
			// (uuid:tag:interval...) applying to the intervals after them.
			if len(fields) < 2 || !isUUID(head) {
				return "", fmt.Errorf("gtid set %q: want uuid:a-b[:c-d...] with a hex uuid", part)
			}
			if err := setFlavor("mysql", part); err != nil {
				return "", err
			}
			uuid, tag, pendingTag := strings.ToLower(head), "", false
			for _, field := range fields[1:] {
				field = strings.TrimSpace(field)
				if field != "" && !isDigit(field[0]) {
					if !isGTIDTag(field) {
						return "", fmt.Errorf("gtid set %q: tag %q must match [a-z_][a-z0-9_]{0,31}", part, field)
					}
					if pendingTag {
						return "", fmt.Errorf("gtid set %q: tag with no interval", part)
					}
					tag, pendingTag = strings.ToLower(field), true
					continue
				}
				rg, err := parseRange(field, part)
				if err != nil {
					return "", err
				}
				name := uuid
				if tag != "" {
					name = uuid + ":" + tag
				}
				key := src{name: name, uuid: uuid, tag: tag}
				by[key] = append(by[key], rg)
				pendingTag = false
			}
			if pendingTag {
				return "", fmt.Errorf("gtid set %q: tag with no interval", part)
			}
			continue
		}
		// MariaDB: domain-server-seq, or domain-server:a-b
		var d, srv uint64
		var rg rng
		var err error
		switch {
		case len(fields) == 1 && len(strings.Split(head, "-")) == 3:
			p := strings.Split(head, "-")
			d, err = strconv.ParseUint(p[0], 10, 32)
			if err == nil {
				srv, err = strconv.ParseUint(p[1], 10, 32)
			}
			if err == nil {
				rg, err = parseRange(p[2], part)
			}
		case len(fields) == 2 && len(strings.Split(head, "-")) == 2:
			p := strings.Split(head, "-")
			d, err = strconv.ParseUint(p[0], 10, 32)
			if err == nil {
				srv, err = strconv.ParseUint(p[1], 10, 32)
			}
			if err == nil {
				rg, err = parseRange(fields[1], part)
			}
		default:
			err = fmt.Errorf("gtid %q: want uuid:a-b, domain-server-seq or domain-server:a-b", part)
		}
		if err != nil {
			return "", fmt.Errorf("gtid %q: %v", part, err)
		}
		if err := setFlavor("mariadb", part); err != nil {
			return "", err
		}
		key := src{name: fmt.Sprintf("%d-%d", d, srv), d: d, srv: srv}
		by[key] = append(by[key], rg)
	}
	keys := make([]src, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if flavor == "mariadb" {
			if keys[i].d != keys[j].d {
				return keys[i].d < keys[j].d
			}
			return keys[i].srv < keys[j].srv
		}
		if keys[i].uuid != keys[j].uuid {
			return keys[i].uuid < keys[j].uuid
		}
		return keys[i].tag < keys[j].tag
	})
	var out []string
	for _, k := range keys {
		rs := by[k]
		sort.Slice(rs, func(i, j int) bool { return rs[i].a < rs[j].a })
		cur := rs[0]
		flush := func() { out = append(out, fmt.Sprintf("%s:%d-%d", k.name, cur.a, cur.b)) }
		for _, r := range rs[1:] {
			if cur.b == math.MaxUint64 || r.a <= cur.b+1 {
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

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isUUID: 8-4-4-4-12 hex digits.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// isGTIDTag: a MySQL 8.4 GTID tag, [a-z_][a-z0-9_]{0,31}, case-insensitive.
func isGTIDTag(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i] | 0x20
		if s[i] == '_' || (c >= 'a' && c <= 'z') || (i > 0 && isDigit(s[i])) {
			continue
		}
		return false
	}
	return true
}
