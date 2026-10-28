// Package assurance provides module-level functionality for assurance.
// input: the retained replay segments in chain order, each already scanned, plus the window breaks
// output: whether a UTC second or one MySQL GTID is inside the retained chain, the segment prefix that covers it, and the mysqlbinlog stop offset; prechecks for a restore of that answer
// pos: the coverage and restore-plan decision on the existing chain (ADR 0009 phases 4 and 5); no second index
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"fmt"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

const (
	coverBefore        = "before earliest"
	coverAfter         = "after latest"
	coverEmpty         = "empty chain"
	coverGTIDMissing   = "gtid is not in the retained window"
	coverBreakPrefix   = "falls on break: "
	coverFreshDefault  = 48 * time.Hour
	checksumMatchValue = "match"
)

// CoverSegment is one retained segment as coverage sees it.
// Events come from one ScanCover of that segment. End is the byte after the
// last complete event. Readable is false when those bytes are not on this
// process or the sealed segment is corrupt.
type CoverSegment struct {
	Name           string
	Source         string
	SourceIdentity string
	Epoch          int64
	LocalPath      string
	ObjectKey      string
	UploadState    string
	Checksum       string
	SHA256         string
	Size           int64
	LastVerified   time.Time
	Readable       bool
	Previous       string
	HasPrevious    bool
	Events         []binlog.CoverEvent
	End            uint64
}

// CoverQuery is one point. At and GTID are exclusive. Latest means the last
// copied event in the chain.
type CoverQuery struct {
	At     *time.Time
	GTID   string
	Latest bool
}

// CoverResult is the answer for one point. Segments is the prefix from the
// first retained segment through the stop segment, never a page.
// Applied is the transactions this prefix includes. It is not part of
// the HTTP body. The name is not Coverage: that type is the verifier's
// per-segment record.
type CoverResult struct {
	Covered   bool              `json:"covered"`
	Reason    string            `json:"reason,omitempty"`
	Segments  []CoverageSegment `json:"segments"`
	StartFrom *CoveragePos      `json:"start_from,omitempty"`
	StopAt    *CoveragePos      `json:"stop_at,omitempty"`
	Window    CoverageWindow    `json:"window"`
	Applied   string            `json:"-"`
}

// CoverageSegment is one file in a covered prefix.
type CoverageSegment struct {
	Name           string     `json:"name"`
	SourceIdentity string     `json:"source_identity,omitempty"`
	Epoch          int64      `json:"epoch,omitempty"`
	FirstEventTime *time.Time `json:"first_event_time,omitempty"`
	LastEventTime  *time.Time `json:"last_event_time,omitempty"`
	GTIDSet        string     `json:"gtid_set,omitempty"`
	LocalPath      string     `json:"local_path,omitempty"`
	ObjectKey      string     `json:"object_key,omitempty"`
	UploadState    string     `json:"upload_state,omitempty"`
	Checksum       string     `json:"checksum,omitempty"`
	SHA256         string     `json:"sha256,omitempty"`
	Size           int64      `json:"size,omitempty"`
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty"`
}

// CoveragePos is a segment and a byte offset in that file.
type CoveragePos struct {
	Segment string `json:"segment"`
	Pos     uint64 `json:"pos"`
}

// CoverageWindow is the retained chain's time bounds and the breaks the
// caller already derived.
type CoverageWindow struct {
	Earliest *time.Time `json:"earliest,omitempty"`
	Latest   *time.Time `json:"latest,omitempty"`
	Breaks   []Break    `json:"breaks"`
}

// Cover answers q from segs. breaks are the chain's recovery breaks, in
// chain order of discovery. A nil At, an empty GTID, and Latest false is an
// error. At and GTID together is an error.
func Cover(segs []CoverSegment, q CoverQuery, breaks []Break) (CoverResult, error) {
	if q.At != nil && strings.TrimSpace(q.GTID) != "" {
		return CoverResult{}, fmt.Errorf("at and gtid cannot both be set")
	}
	if q.At == nil && strings.TrimSpace(q.GTID) == "" && !q.Latest {
		return CoverResult{}, fmt.Errorf("at or gtid is required")
	}
	window := coverWindow(segs, breaks)
	if strings.TrimSpace(q.GTID) != "" {
		return coverGTID(segs, strings.TrimSpace(q.GTID), breaks, window)
	}
	var at time.Time
	if q.Latest {
		if window.Latest == nil {
			return uncovered(coverEmpty, window), nil
		}
		at = *window.Latest
	} else {
		at = q.At.UTC().Truncate(time.Second)
	}
	return coverTime(segs, at, breaks, window), nil
}

func coverWindow(segs []CoverSegment, breaks []Break) CoverageWindow {
	out := CoverageWindow{Breaks: []Break{}}
	if breaks != nil {
		out.Breaks = breaks
	}
	for _, s := range segs {
		for _, ev := range s.Events {
			if !copiedEvent(ev) {
				continue
			}
			when := ev.When.UTC()
			if out.Earliest == nil || when.Before(*out.Earliest) {
				w := when
				out.Earliest = &w
			}
			if out.Latest == nil || when.After(*out.Latest) {
				w := when
				out.Latest = &w
			}
		}
	}
	return out
}

func copiedEvent(ev binlog.CoverEvent) bool {
	return ev.Counts && !ev.When.IsZero()
}

func coverTime(segs []CoverSegment, at time.Time, breaks []Break, window CoverageWindow) CoverResult {
	if window.Earliest == nil || window.Latest == nil {
		return uncovered(coverEmpty, window)
	}
	if at.Before(*window.Earliest) {
		return uncovered(coverBefore, window)
	}
	if at.After(*window.Latest) {
		return uncovered(coverAfter, window)
	}
	stopSeg := -1
	var stopPos uint64
	for i, s := range segs {
		for _, ev := range s.Events {
			if !copiedEvent(ev) || ev.When.After(at) {
				continue
			}
			stopSeg = i
			stopPos = ev.End
		}
	}
	if stopSeg < 0 {
		return uncovered(coverBefore, window)
	}
	return finish(segs, stopSeg, stopPos, breaks, window, func(ev binlog.CoverEvent) bool {
		return ev.Seq > 0 && !ev.When.After(at)
	})
}

func coverGTID(segs []CoverSegment, raw string, breaks []Break, window CoverageWindow) (CoverResult, error) {
	uuid, seq, err := parseCoverGTID(raw)
	if err != nil {
		return CoverResult{}, err
	}
	foundSeg, foundEv := -1, -1
	for i, s := range segs {
		for j, ev := range s.Events {
			if ev.UUID == uuid && ev.Seq == seq {
				foundSeg, foundEv = i, j
			}
		}
	}
	if foundSeg < 0 {
		return uncovered(coverGTIDMissing, window), nil
	}
	stopPos := segs[foundSeg].End
	for _, ev := range segs[foundSeg].Events[foundEv+1:] {
		if ev.UUID != "" && ev.Seq > 0 {
			stopPos = ev.Offset
			break
		}
	}
	return finish(segs, foundSeg, stopPos, breaks, window, func(ev binlog.CoverEvent) bool {
		return ev.Seq > 0 && ev.Offset < stopPos
	}), nil
}

func finish(segs []CoverSegment, stopSeg int, stopPos uint64, breaks []Break, window CoverageWindow, take func(binlog.CoverEvent) bool) CoverResult {
	if reason := breakAt(segs, stopSeg, breaks); reason != "" {
		return uncovered(coverBreakPrefix+reason, window)
	}
	for i := 0; i <= stopSeg; i++ {
		if !segs[i].Readable {
			return uncovered(fmt.Sprintf("segment %s is not readable", segs[i].Name), window)
		}
	}
	out := CoverResult{Covered: true, Segments: make([]CoverageSegment, 0, stopSeg+1), Window: window}
	applied := binlog.GTIDSet{}
	for i := 0; i <= stopSeg; i++ {
		out.Segments = append(out.Segments, segmentView(segs[i]))
		for _, ev := range segs[i].Events {
			if ev.Seq <= 0 || ev.UUID == "" {
				continue
			}
			if i == stopSeg && !take(ev) {
				continue
			}
			applied.Add(ev.UUID, ev.Seq, ev.Seq)
		}
	}
	if !applied.IsEmpty() {
		out.Applied = applied.String()
	}
	out.StartFrom = &CoveragePos{Segment: segs[0].Name, Pos: 4}
	out.StopAt = &CoveragePos{Segment: segs[stopSeg].Name, Pos: stopPos}
	return out
}

func segmentView(s CoverSegment) CoverageSegment {
	view := CoverageSegment{
		Name: s.Name, SourceIdentity: s.SourceIdentity, Epoch: s.Epoch,
		LocalPath: s.LocalPath, ObjectKey: s.ObjectKey, UploadState: s.UploadState,
		Checksum: s.Checksum, SHA256: s.SHA256, Size: s.Size, GTIDSet: gtidText(s.Events),
	}
	if !s.LastVerified.IsZero() {
		at := s.LastVerified.UTC()
		view.LastVerifiedAt = &at
	}
	var first, last time.Time
	var have bool
	for _, ev := range s.Events {
		if !copiedEvent(ev) {
			continue
		}
		if !have || ev.When.Before(first) {
			first = ev.When
		}
		if !have || ev.When.After(last) {
			last = ev.When
		}
		have = true
	}
	if have {
		f, l := first.UTC(), last.UTC()
		view.FirstEventTime, view.LastEventTime = &f, &l
	}
	return view
}

func gtidText(events []binlog.CoverEvent) string {
	set := binlog.GTIDSet{}
	for _, ev := range events {
		if ev.Seq > 0 && ev.UUID != "" {
			set.Add(ev.UUID, ev.Seq, ev.Seq)
		}
	}
	if set.IsEmpty() {
		return ""
	}
	return set.String()
}

func uncovered(reason string, window CoverageWindow) CoverResult {
	return CoverResult{Covered: false, Reason: reason, Segments: []CoverageSegment{}, Window: window}
}

// breakAt reports the break that makes a restore through stopSeg impossible.
// A break that names one chain segment cuts at that segment. A break that
// names several cuts at the later one, so a stop on the earlier segment
// still covers.
func breakAt(segs []CoverSegment, stopSeg int, breaks []Break) string {
	for _, b := range breaks {
		idxs := namedIndexes(segs, b.Files)
		if len(idxs) == 0 {
			continue
		}
		cut := idxs[0]
		if len(idxs) > 1 {
			cut = idxs[len(idxs)-1]
		}
		if stopSeg >= cut {
			return b.Reason
		}
	}
	return ""
}

func namedIndexes(segs []CoverSegment, files []string) []int {
	var out []int
	seen := map[int]bool{}
	for i, s := range segs {
		for _, f := range files {
			if f == "" {
				continue
			}
			if f == s.Name || f == s.Source {
				if !seen[i] {
					out = append(out, i)
					seen[i] = true
				}
			}
		}
	}
	return out
}

func parseCoverGTID(raw string) (string, int64, error) {
	raw = strings.TrimSpace(raw)
	colon := strings.LastIndex(raw, ":")
	if colon <= 0 || colon == len(raw)-1 {
		return "", 0, fmt.Errorf("invalid gtid")
	}
	uuid := strings.ToLower(raw[:colon])
	if !looksCoverUUID(uuid) {
		return "", 0, fmt.Errorf("invalid gtid")
	}
	var seq int64
	for _, r := range raw[colon+1:] {
		if r < '0' || r > '9' {
			return "", 0, fmt.Errorf("invalid gtid")
		}
		seq = seq*10 + int64(r-'0')
		if seq <= 0 {
			return "", 0, fmt.Errorf("invalid gtid")
		}
	}
	if seq <= 0 {
		return "", 0, fmt.Errorf("invalid gtid")
	}
	return uuid, seq, nil
}

func looksCoverUUID(s string) bool {
	// 8-4-4-4-12 hex.
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	widths := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != widths[i] {
			return false
		}
		for _, r := range p {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
	}
	return true
}

// Precheck is one restore-plan line. Result is green, red, yellow, or manual.
type Precheck struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	How         string `json:"how"`
	Result      string `json:"result"`
}

// WindowPrecheck is green only when the point is covered.
func WindowPrecheck(cov CoverResult) Precheck {
	if cov.Covered {
		return Precheck{ID: "window", Description: "the point is inside the retained chain", How: "GET /coverage", Result: "green"}
	}
	reason := cov.Reason
	if reason == "" {
		reason = "the point is not covered"
	}
	return Precheck{ID: "window", Description: reason, How: "GET /coverage", Result: "red"}
}

// ChecksumPrecheck is green when every segment in the prefix is checksum
// match, or present locally and verified within fresh. fresh <= 0 means 48h.
func ChecksumPrecheck(cov CoverResult, fresh time.Duration, now time.Time) Precheck {
	if !cov.Covered {
		return Precheck{ID: "checksum", Description: "the point is not covered", How: "checksum match, or a local file verified within the verify window", Result: "red"}
	}
	if fresh <= 0 {
		fresh = coverFreshDefault
	}
	yellow := ""
	for _, s := range cov.Segments {
		state := strings.ToLower(strings.TrimSpace(s.Checksum))
		switch {
		case state == "mismatch":
			return Precheck{ID: "checksum", Description: s.Name + " checksum is mismatch", How: "re-upload the segment from the local file", Result: "red"}
		case state == checksumMatchValue:
			continue
		case s.LocalPath != "" && verifiedFresh(s.LastVerifiedAt, fresh, now):
			continue
		case s.LocalPath != "":
			if yellow == "" {
				yellow = s.Name + " is local and has not been verified recently"
			}
		default:
			return Precheck{ID: "checksum", Description: s.Name + " has no local copy and checksum is not match", How: "restore the object or re-verify the segment", Result: "red"}
		}
	}
	if yellow != "" {
		return Precheck{ID: "checksum", Description: yellow, How: "run a verify pass or accept the local bytes", Result: "yellow"}
	}
	return Precheck{ID: "checksum", Description: "every segment is checksum match or locally verified", How: "catalog checksum, or last_verified_at", Result: "green"}
}

func verifiedFresh(at *time.Time, fresh time.Duration, now time.Time) bool {
	if at == nil || at.IsZero() {
		return false
	}
	if at.After(now.Add(time.Minute)) {
		return false
	}
	return now.Sub(at.UTC()) <= fresh
}

// FullBackupPrecheck compares a pasted full-backup gtid_executed with the
// chain start set and the transactions this plan applies.
// An empty full backup is manual. A start set that is not a subset is a hole.
// An intersection with the applied set is an overlap. Either is red.
func FullBackupPrecheck(start, applied, full string) Precheck {
	how := "SELECT @@gtid_executed"
	full = strings.TrimSpace(full)
	if full == "" {
		return Precheck{
			ID: "full_backup_gtid", Result: "manual", How: how,
			Description: fmt.Sprintf("paste the full backup @@gtid_executed; it must contain %s and must not intersect %s", emptySet(start), emptySet(applied)),
		}
	}
	got, err := binlog.ParseGTIDSet(full)
	if err != nil {
		return Precheck{ID: "full_backup_gtid", Description: "full_backup_gtid is not a MySQL GTID set", How: how, Result: "red"}
	}
	base, err := binlog.ParseGTIDSet(start)
	if strings.TrimSpace(start) != "" && err != nil {
		return Precheck{ID: "full_backup_gtid", Description: "chain start GTID set is not a MySQL GTID set", How: how, Result: "red"}
	}
	use, err := binlog.ParseGTIDSet(applied)
	if strings.TrimSpace(applied) != "" && err != nil {
		return Precheck{ID: "full_backup_gtid", Description: "applied GTID set is not a MySQL GTID set", How: how, Result: "red"}
	}
	hole := !got.Contains(base)
	overlap := gtidIntersects(got, use)
	switch {
	case hole && overlap:
		return Precheck{ID: "full_backup_gtid", Description: "full backup GTID has a hole against the chain start and overlaps transactions this plan applies", How: how, Result: "red"}
	case hole:
		return Precheck{ID: "full_backup_gtid", Description: "full backup GTID has a hole against the chain start", How: how, Result: "red"}
	case overlap:
		return Precheck{ID: "full_backup_gtid", Description: "full backup GTID overlaps transactions this plan applies", How: how, Result: "red"}
	default:
		return Precheck{ID: "full_backup_gtid", Description: "full backup GTID meets the chain start without overlapping the applied transactions", How: how, Result: "green"}
	}
}

func emptySet(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return s
}

func gtidIntersects(a, b binlog.GTIDSet) bool {
	for key, list := range b {
		have := a[key]
		for _, iv := range list {
			for _, h := range have {
				if h.Hi >= iv.Lo && h.Lo <= iv.Hi {
					return true
				}
			}
		}
	}
	return false
}

// ManualPrechecks are the checks the server does not run against a target
// instance. client is mysqlbinlog or mariadb-binlog.
func ManualPrechecks(client string) []Precheck {
	if client == "" {
		client = "mysqlbinlog"
	}
	return []Precheck{
		{
			ID: "target_settings", Result: "manual",
			Description: "target gtid_mode, binlog_format, log_replica_updates, super_read_only, and version",
			How:         "SELECT @@gtid_mode, @@binlog_format, @@log_replica_updates, @@super_read_only, @@version",
		},
		{
			ID: "client_version", Result: "manual",
			Description: client + " version must be at least the source version",
			How:         client + " --version",
		},
	}
}
