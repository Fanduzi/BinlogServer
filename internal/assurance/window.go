// Package assurance provides module-level functionality for assurance.
// input: the chosen replay segments in chain order, each with its catalog state, event-header times and MySQL GTID log, plus the task flavor, whether object storage is configured, and the GTID start set
// output: the recoverable window: earliest and latest UTC event time, the stored MySQL GTID set, and the breaks (missing index inside one source prefix, GTID hole between files or across a source switch, hole inside a segment, interior hole between stored sequences, checksum mismatch, not durable off-host, unreadable)
// pos: the one derivation behind GET /window (ADR 0009 phase 1, P1-2); tasks only reads the bytes and adds the damage cut
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

const (
	breakMissingFile = "missing source file %s between %s and %s"
	breakChecksum    = "checksum mismatch"
	breakNotDurable  = "segment is not durable off-host (%s)"
	breakGTIDHole    = "gtid hole between %s and %s"
	breakIntraGTID   = "gtid hole in %s at %s:%d"
	breakUnreadable  = "segment is not readable"
	breakInterior    = "gtid hole: %s is stored in no segment between %s and %s"
)

// Break is one place the chain cannot be restored straight through.
type Break struct {
	Files  []string
	Reason string
}

// Window is the retained chain a restore can use right now. Continuous is
// true when Breaks is empty; an empty chain is continuous with no instants.
type Window struct {
	Continuous bool
	Earliest   *time.Time
	Latest     *time.Time
	GTIDSet    string
	Breaks     []Break
}

// WindowSegment is one chosen segment as the window sees it.
type WindowSegment struct {
	// Base is the segment's inventory basename; Source is the source file
	// name the row records. Path is the stored path; its base decides the
	// (prefix, index) group.
	Base, Source, Path string
	Open               bool
	ChecksumMismatch   bool
	UploadState        string
	// Readable is false when the bytes are not on this process.
	Readable bool
	SpanOK   bool
	First    time.Time
	Last     time.Time
	Log      binlog.SegmentGTIDLog
}

func (s WindowSegment) name() string {
	if s.Base != "" {
		return s.Base
	}
	return strings.TrimSpace(s.Source)
}

type windowPart struct {
	seq    uint64
	prefix string
	segs   []WindowSegment
}

func (p windowPart) sourceName() string {
	for _, s := range p.segs {
		if name := strings.TrimSpace(s.Source); name != "" {
			return name
		}
		if named, ok := binlog.ClassifySegment(s.Base); ok && named.Source != "" {
			return named.Source
		}
		if s.Base != "" {
			return s.Base
		}
	}
	return ""
}

// WindowOf derives the window of segs, which are in chain order. A GTID
// start set counts as stored: a restore applies it first.
func WindowOf(segs []WindowSegment, flavor string, objectStorage bool, startGTID string) (Window, error) {
	out := Window{Continuous: true, Breaks: []Break{}}
	mysql := strings.EqualFold(strings.TrimSpace(flavor), "mysql")
	var events []binlog.GTIDEventRef
	parts := groupWindow(segs)
	var stored binlog.GTIDSet
	if mysql {
		var err error
		if stored, err = windowStoredSet(segs, startGTID); err != nil {
			return Window{}, err
		}
	}
	var before []binlog.GTIDEventRef
	for i, part := range parts {
		for _, s := range part.segs {
			out.Breaks = append(out.Breaks, segmentBreaks(s, objectStorage)...)
			if s.Readable && s.SpanOK {
				out.Earliest = earlier(out.Earliest, s.First)
				out.Latest = later(out.Latest, s.Last)
			}
			if !s.Readable {
				continue
			}
			events = append(events, s.Log.Events...)
			if mysql {
				if uuid, seq, hole := intraSegmentHole(s.Log.Events); hole {
					out.Breaks = append(out.Breaks, Break{Files: []string{s.name()}, Reason: fmt.Sprintf(breakIntraGTID, s.name(), uuid, seq)})
				}
			}
		}
		if i == len(parts)-1 {
			break
		}
		next := parts[i+1]
		if part.prefix == next.prefix && next.seq > part.seq+1 {
			out.Breaks = append(out.Breaks, missingIndexBreak(part, next))
		}
		before = append(before, mergeLogs(part.segs).Events...)
		if !mysql {
			continue
		}
		nextLog := mergeLogs(next.segs)
		if nextLog.HasPrevious {
			if _, err := binlog.ParseGTIDSet(nextLog.Previous); err != nil {
				return Window{}, err
			}
		}
		var hole bool
		if part.prefix != next.prefix {
			// A server switch: the new server's header predates the last
			// transactions replicated into the file, which the dump skipped.
			hole = switchHole(stored, before, nextLog)
		} else {
			hole = gtidHole(mergeLogs(part.segs), nextLog)
		}
		if hole {
			out.Breaks = append(out.Breaks, Break{
				Files:  []string{part.sourceName(), next.sourceName()},
				Reason: fmt.Sprintf(breakGTIDHole, part.sourceName(), next.sourceName()),
			})
		}
	}
	if mysql {
		if b, ok := interiorHoleBreak(segs, startGTID, out.Breaks); ok {
			out.Breaks = append(out.Breaks, b)
		}
	}
	out.Continuous = len(out.Breaks) == 0
	if mysql && len(events) > 0 {
		set := binlog.GTIDSet{}
		set.AddEvents(events)
		if !set.IsEmpty() {
			out.GTIDSet = set.String()
		}
	}
	return out, nil
}

func splitSourceIndex(name string) (prefix string, width int, ok bool) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot == len(name)-1 {
		return "", 0, false
	}
	num := name[dot+1:]
	for _, r := range num {
		if r < '0' || r > '9' {
			return "", 0, false
		}
	}
	return name[:dot], len(num), true
}

func groupWindow(segs []WindowSegment) []windowPart {
	parts := make([]windowPart, 0)
	for _, s := range segs {
		named, ok := binlog.ClassifySegment(filepath.Base(s.Path))
		if !ok {
			continue
		}
		prefix, _, split := splitSourceIndex(named.Source)
		if !split {
			prefix = named.Source
		}
		if len(parts) == 0 || parts[len(parts)-1].seq != named.Seq || parts[len(parts)-1].prefix != prefix {
			parts = append(parts, windowPart{seq: named.Seq, prefix: prefix})
		}
		last := &parts[len(parts)-1]
		last.segs = append(last.segs, s)
	}
	return parts
}

func segmentBreaks(s WindowSegment, objectStorage bool) []Break {
	name := s.name()
	var out []Break
	sealed := !s.Open
	mismatch := sealed && s.ChecksumMismatch
	if mismatch {
		out = append(out, Break{Files: []string{name}, Reason: breakChecksum})
	}
	if objectStorage && sealed && !mismatch {
		state := strings.ToUpper(strings.TrimSpace(s.UploadState))
		if state == "UPLOAD_FAILED" || state == "LOCAL_ONLY" {
			out = append(out, Break{Files: []string{name}, Reason: fmt.Sprintf(breakNotDurable, state)})
		}
	}
	if !s.Readable {
		out = append(out, Break{Files: []string{name}, Reason: breakUnreadable})
	}
	return out
}

func missingIndexBreak(left, right windowPart) Break {
	leftName, rightName := left.sourceName(), right.sourceName()
	var missing []string
	prefix, width, ok := splitSourceIndex(leftName)
	rp, rw, rok := splitSourceIndex(rightName)
	if ok && rok && prefix == rp && width == rw {
		missing = make([]string, 0, right.seq-left.seq-1)
		for seq := left.seq + 1; seq < right.seq; seq++ {
			missing = append(missing, fmt.Sprintf("%s.%0*d", prefix, width, seq))
		}
	}
	files := append([]string{}, missing...)
	if leftName != "" {
		files = append(files, leftName)
	}
	if rightName != "" {
		files = append(files, rightName)
	}
	listed := strings.Join(missing, ", ")
	if listed == "" {
		listed = fmt.Sprintf("index %d", left.seq+1)
	}
	return Break{Files: files, Reason: fmt.Sprintf(breakMissingFile, listed, leftName, rightName)}
}

func earlier(cur *time.Time, candidate time.Time) *time.Time {
	if candidate.IsZero() {
		return cur
	}
	when := candidate.UTC()
	if cur == nil || when.Before(*cur) {
		return &when
	}
	return cur
}

func later(cur *time.Time, candidate time.Time) *time.Time {
	if candidate.IsZero() {
		return cur
	}
	when := candidate.UTC()
	if cur == nil || when.After(*cur) {
		return &when
	}
	return cur
}

func mergeLogs(segs []WindowSegment) binlog.SegmentGTIDLog {
	var out binlog.SegmentGTIDLog
	for _, s := range segs {
		if !s.Readable {
			continue
		}
		if !out.HasPrevious && s.Log.HasPrevious {
			out.HasPrevious = true
			out.Previous = s.Log.Previous
		}
		out.Events = append(out.Events, s.Log.Events...)
	}
	return out
}

// windowStoredSet is the history before the chain (the first header), the
// start set, and every transaction a readable segment stores.
func windowStoredSet(segs []WindowSegment, startGTID string) (binlog.GTIDSet, error) {
	start := ""
	for _, s := range segs {
		if s.Readable && s.Log.HasPrevious {
			start = s.Log.Previous
			break
		}
	}
	set, err := binlog.ParseGTIDSet(start)
	if err != nil {
		return nil, err
	}
	if seeded, err := binlog.ParseGTIDSet(startGTID); err == nil {
		set.AddAll(seeded)
	}
	for _, s := range segs {
		if s.Readable {
			set.AddEvents(s.Log.Events)
		}
	}
	return set, nil
}

// interiorHoleBreak reports sequences missing between the lowest and highest
// stored sequence of one source that the start set does not cover, unless a
// GTID-hole or unreadable break already names the place.
func interiorHoleBreak(segs []WindowSegment, startGTID string, breaks []Break) (Break, bool) {
	for _, b := range breaks {
		if strings.HasPrefix(b.Reason, "gtid hole") || b.Reason == breakUnreadable {
			return Break{}, false
		}
	}
	stored := binlog.GTIDSet{}
	type named struct {
		name string
		set  binlog.GTIDSet
	}
	sets := make([]named, 0, len(segs))
	for _, s := range segs {
		if !s.Readable {
			continue
		}
		one := binlog.GTIDSet{}
		one.AddEvents(s.Log.Events)
		sets = append(sets, named{name: s.name(), set: one})
		stored.AddAll(one)
	}
	seed, err := binlog.ParseGTIDSet(startGTID)
	if err != nil {
		seed = nil
	}
	gaps := binlog.StoredHoles(stored, seed)
	if len(gaps) == 0 {
		return Break{}, false
	}
	keys := make([]string, 0, len(gaps))
	for k := range gaps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	k := keys[0]
	iv := gaps[k][0]
	left, right := "", ""
	for _, ns := range sets {
		for _, r := range ns.set[k] {
			if r.Hi < iv.Lo {
				left = ns.name
			}
			if r.Lo > iv.Hi && right == "" {
				right = ns.name
			}
		}
	}
	return Break{Files: []string{left, right}, Reason: fmt.Sprintf(breakInterior, gaps.String(), left, right)}, true
}
