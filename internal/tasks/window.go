// Package tasks provides module-level functionality for tasks.
// input: the replay inventory (sealed segments and the highest open epoch), each segment's event-header times and MySQL GTID log, and whether object storage is configured
// output: the retained chain's earliest and latest UTC event times, the MySQL GTID set those segments contain, and the breaks that keep the chain from being one continuous restore; a missing index is a break only inside one source-name prefix, a GTID hole inside one segment is a break, and a GTID hole is still reported across a source switch
// pos: read-only recoverable window a DBA can check before choosing a replay stop; replay selection is unchanged
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"

	"github.com/go-mysql-org/go-mysql/mysql"
)

const (
	breakMissingFile = "missing source file %s between %s and %s"
	breakChecksum    = "checksum mismatch"
	breakNotDurable  = "segment is not durable off-host (%s)"
	breakGTIDHole    = "gtid hole between %s and %s"
	breakIntraGTID   = "gtid hole in %s at %s:%d"
	breakUnreadable  = "segment is not readable"
)

// RecoveryBreak is one place the retained chain cannot be restored straight through.
// Files names the segments involved. Reason is a plain sentence.
type RecoveryBreak struct {
	Files  []string `json:"files"`
	Reason string   `json:"reason"`
}

// RecoveryWindow is the retained chain a DBA can restore from right now.
// Earliest and Latest are UTC event-header times. They are omitted when the
// chain has no timed event. GTIDSet is the MySQL transactions stored in the
// chain, not the history a previous-GTIDs header only mentions. It is omitted
// unless the task flavor is mysql and the chain contains a GTID event.
// Continuous is true when Breaks is empty. An empty chain is continuous and
// has no instants.
type RecoveryWindow struct {
	Continuous bool            `json:"continuous"`
	Earliest   *time.Time      `json:"earliest"`
	Latest     *time.Time      `json:"latest"`
	GTIDSet    string          `json:"gtid_set,omitempty"`
	Breaks     []RecoveryBreak `json:"breaks"`
}

type segmentView struct {
	File     BinlogFile
	Span     EventSpan
	SpanOK   bool
	Log      binlog.SegmentGTIDLog
	Readable bool
}

// RecoveryWindow reads the retained chain and reports how far back a restore
// can go, and where it is broken. A missing task is ErrTaskNotFound.
// A segment listed in the inventory whose bytes are not on this process is a
// break, not an error. A segment that cannot be read for any other reason
// fails the call.
func (s *Scheduler) RecoveryWindow(taskID string) (RecoveryWindow, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return RecoveryWindow{}, err
	}
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return RecoveryWindow{}, err
	}
	chosen := SelectReplayFiles(files)
	s.mu.Lock()
	objectStorage := s.fileUploader != nil
	s.mu.Unlock()

	views := make([]segmentView, 0, len(chosen))
	for _, file := range chosen {
		view := segmentView{File: file}
		name := segmentInventoryBasename(file)
		if name == "" {
			views = append(views, view)
			continue
		}
		body, _, openErr := s.OpenTaskSegment(taskID, name)
		if openErr != nil {
			if errors.Is(openErr, ErrSegmentNotOnProcess) {
				views = append(views, view)
				continue
			}
			return RecoveryWindow{}, openErr
		}
		raw, readErr := io.ReadAll(body)
		closeErr := body.Close()
		if readErr != nil {
			return RecoveryWindow{}, readErr
		}
		if closeErr != nil {
			return RecoveryWindow{}, closeErr
		}
		first, last, ok, spanErr := binlog.EventTimeSpan(bytes.NewReader(raw))
		if spanErr != nil {
			return RecoveryWindow{}, spanErr
		}
		log, scanErr := binlog.ScanSegmentGTIDs(bytes.NewReader(raw))
		if scanErr != nil {
			return RecoveryWindow{}, scanErr
		}
		view.Readable = true
		view.SpanOK = ok
		view.Span = EventSpan{First: first, Last: last}
		view.Log = log
		views = append(views, view)
	}
	return assessRecovery(views, task.Source.Flavor, objectStorage)
}

func assessRecovery(views []segmentView, flavor string, objectStorage bool) (RecoveryWindow, error) {
	out := RecoveryWindow{Continuous: true, Breaks: []RecoveryBreak{}}
	mysqlFlavor := strings.EqualFold(strings.TrimSpace(flavor), "mysql")
	var earliest, latest *time.Time
	var events []binlog.GTIDEventRef

	parts := groupChain(views)
	for i, part := range parts {
		for _, view := range part.views {
			out.Breaks = append(out.Breaks, segmentBreaks(view, objectStorage)...)
			if view.Readable && view.SpanOK {
				earliest = earlier(earliest, view.Span.First)
				latest = later(latest, view.Span.Last)
			}
			if view.Readable {
				events = append(events, view.Log.Events...)
				if mysqlFlavor {
					if uuid, seq, hole := intraSegmentHole(view.Log.Events); hole {
						name := segmentBase(view.File)
						if name == "" {
							name = strings.TrimSpace(view.File.FileName)
						}
						out.Breaks = append(out.Breaks, RecoveryBreak{
							Files:  []string{name},
							Reason: fmt.Sprintf(breakIntraGTID, name, uuid, seq),
						})
					}
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
		if mysqlFlavor {
			hole, err := gtidHole(mergeGTID(part.views), mergeGTID(next.views))
			if err != nil {
				return RecoveryWindow{}, err
			}
			if hole {
				out.Breaks = append(out.Breaks, RecoveryBreak{
					Files:  []string{part.sourceName(), next.sourceName()},
					Reason: fmt.Sprintf(breakGTIDHole, part.sourceName(), next.sourceName()),
				})
			}
		}
	}
	out.Earliest = earliest
	out.Latest = latest
	out.Continuous = len(out.Breaks) == 0
	if mysqlFlavor && len(events) > 0 {
		set, err := eventGTIDSet(events)
		if err != nil {
			return RecoveryWindow{}, err
		}
		if set != nil && !set.IsEmpty() {
			out.GTIDSet = set.String()
		}
	}
	return out, nil
}

type chainPart struct {
	seq    uint64
	prefix string
	views  []segmentView
}

func (p chainPart) sourceName() string {
	for _, view := range p.views {
		name := strings.TrimSpace(view.File.FileName)
		if name != "" {
			return name
		}
		base := segmentBase(view.File)
		named, ok := binlog.ClassifySegment(base)
		if ok && named.Source != "" {
			return named.Source
		}
		if base != "" {
			return base
		}
	}
	return ""
}

func groupChain(views []segmentView) []chainPart {
	parts := make([]chainPart, 0)
	for _, view := range views {
		key := binlogSegmentKey(view.File)
		if !key.ok {
			continue
		}
		if len(parts) == 0 || parts[len(parts)-1].seq != key.seq || parts[len(parts)-1].prefix != key.prefix {
			parts = append(parts, chainPart{seq: key.seq, prefix: key.prefix})
		}
		last := &parts[len(parts)-1]
		last.views = append(last.views, view)
	}
	return parts
}

func segmentBreaks(view segmentView, objectStorage bool) []RecoveryBreak {
	base := segmentBase(view.File)
	if base == "" {
		base = strings.TrimSpace(view.File.FileName)
	}
	var out []RecoveryBreak
	sealed := !segmentIsOpen(view.File)
	mismatch := sealed && strings.EqualFold(strings.TrimSpace(view.File.Checksum), ChecksumMismatch)
	if mismatch {
		out = append(out, RecoveryBreak{Files: []string{base}, Reason: breakChecksum})
	}
	if objectStorage && sealed && !mismatch {
		state := strings.ToUpper(strings.TrimSpace(view.File.UploadState))
		if state == "UPLOAD_FAILED" || state == "LOCAL_ONLY" {
			out = append(out, RecoveryBreak{
				Files:  []string{base},
				Reason: fmt.Sprintf(breakNotDurable, state),
			})
		}
	}
	if !view.Readable {
		out = append(out, RecoveryBreak{Files: []string{base}, Reason: breakUnreadable})
	}
	return out
}

func missingIndexBreak(left, right chainPart) RecoveryBreak {
	leftName := left.sourceName()
	rightName := right.sourceName()
	missing := missingSourceNames(leftName, rightName, left.seq, right.seq)
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
	return RecoveryBreak{
		Files:  files,
		Reason: fmt.Sprintf(breakMissingFile, listed, leftName, rightName),
	}
}

func missingSourceNames(leftName, rightName string, leftSeq, rightSeq uint64) []string {
	prefix, width, ok := splitSourceIndex(leftName)
	rightPrefix, rightWidth, rightOK := splitSourceIndex(rightName)
	if !ok || !rightOK || prefix != rightPrefix || width != rightWidth {
		return nil
	}
	out := make([]string, 0, rightSeq-leftSeq-1)
	for seq := leftSeq + 1; seq < rightSeq; seq++ {
		out = append(out, fmt.Sprintf("%s.%0*d", prefix, width, seq))
	}
	return out
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

func segmentBase(file BinlogFile) string {
	return segmentInventoryBasename(file)
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

func mergeGTID(views []segmentView) binlog.SegmentGTIDLog {
	var out binlog.SegmentGTIDLog
	for _, view := range views {
		if !view.Readable {
			continue
		}
		if !out.HasPrevious && view.Log.HasPrevious {
			out.HasPrevious = true
			out.Previous = view.Log.Previous
		}
		out.Events = append(out.Events, view.Log.Events...)
	}
	return out
}

// gtidHole is a transaction that committed after the last event stored in
// prev and before next. A capture that starts in the middle of a source file
// keeps that file's original previous-GTIDs header, so sequences between the
// header and the first stored event are before the chain. They are not a break.
// The next previous-GTIDs set must contain every event prev stored, and must
// not already contain the sequence after the last of those events.
func gtidHole(prev, next binlog.SegmentGTIDLog) (bool, error) {
	if !prev.HasPrevious && len(prev.Events) == 0 && !next.HasPrevious && len(next.Events) == 0 {
		return false, nil
	}
	if next.HasPrevious {
		nextSet, err := parseGTIDText(next.Previous)
		if err != nil {
			return false, err
		}
		contained, err := eventsContained(nextSet, prev.Events)
		if err != nil {
			return false, err
		}
		if !contained {
			return true, nil
		}
		return previousContainsBeyond(nextSet, prev.Events)
	}
	return eventSeqHole(prev.Events, next.Events), nil
}

func eventGTIDSet(events []binlog.GTIDEventRef) (mysql.GTIDSet, error) {
	set, err := parseGTIDText("")
	if err != nil {
		return nil, err
	}
	if err := addGTIDEvents(set, events); err != nil {
		return nil, err
	}
	return set, nil
}

func parseGTIDText(text string) (mysql.GTIDSet, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return &mysql.MysqlGTIDSet{}, nil
	}
	return mysql.ParseGTIDSet(mysql.MySQLFlavor, text)
}

func addGTIDEvents(set mysql.GTIDSet, events []binlog.GTIDEventRef) error {
	for _, ev := range events {
		if ev.UUID == "" || ev.Seq <= 0 {
			continue
		}
		if err := set.Update(ev.UUID + ":" + strconv.FormatInt(ev.Seq, 10)); err != nil {
			return err
		}
	}
	return nil
}

func eventsContained(set mysql.GTIDSet, events []binlog.GTIDEventRef) (bool, error) {
	for _, ev := range events {
		one, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, ev.UUID+":"+strconv.FormatInt(ev.Seq, 10))
		if err != nil {
			return false, err
		}
		if !set.Contain(one) {
			return false, nil
		}
	}
	return true, nil
}

func previousContainsBeyond(prev mysql.GTIDSet, events []binlog.GTIDEventRef) (bool, error) {
	maxSeq := map[string]int64{}
	for _, ev := range events {
		if ev.Seq > maxSeq[ev.UUID] {
			maxSeq[ev.UUID] = ev.Seq
		}
	}
	for uuid, max := range maxSeq {
		one, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, uuid+":"+strconv.FormatInt(max+1, 10))
		if err != nil {
			return false, err
		}
		if prev.Contain(one) {
			return true, nil
		}
	}
	return false, nil
}

func intraSegmentHole(events []binlog.GTIDEventRef) (string, int64, bool) {
	byUUID := map[string][]int64{}
	for _, ev := range events {
		if ev.UUID == "" || ev.Seq <= 0 {
			continue
		}
		byUUID[ev.UUID] = append(byUUID[ev.UUID], ev.Seq)
	}
	var foundUUID string
	var foundSeq int64
	found := false
	for uuid, seqs := range byUUID {
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for i := 1; i < len(seqs); i++ {
			if seqs[i] <= seqs[i-1]+1 {
				continue
			}
			missing := seqs[i-1] + 1
			if !found || missing < foundSeq {
				found = true
				foundUUID = uuid
				foundSeq = missing
			}
			break
		}
	}
	return foundUUID, foundSeq, found
}

func eventSeqHole(prev, next []binlog.GTIDEventRef) bool {
	if len(prev) == 0 || len(next) == 0 {
		return false
	}
	maxPrev := map[string]int64{}
	minNext := map[string]int64{}
	for _, ev := range prev {
		if ev.Seq > maxPrev[ev.UUID] {
			maxPrev[ev.UUID] = ev.Seq
		}
	}
	for _, ev := range next {
		cur, ok := minNext[ev.UUID]
		if !ok || ev.Seq < cur {
			minNext[ev.UUID] = ev.Seq
		}
	}
	for uuid, max := range maxPrev {
		min, ok := minNext[uuid]
		if ok && min > max+1 {
			return true
		}
	}
	return false
}
