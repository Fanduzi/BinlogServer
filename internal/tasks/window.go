// Package tasks provides module-level functionality for tasks.
// input: the replay inventory (sealed segments and the highest open epoch), each segment's event-header times and MySQL GTID log, and whether object storage is configured
// output: the retained chain's earliest and latest UTC event times, the MySQL GTID set those segments contain, and the breaks that keep the chain from being one continuous restore; a missing index is a break only inside one source-name prefix, a GTID hole inside one segment is a break, and across a source switch a hole is a transaction the new server's first header lists that the chain does not store anywhere (that header predates the transactions the GTID dump skipped); a GTID task's start set counts as stored, so a mysqldump seed inside the first file is not a hole at a failback; a sequence missing between stored sequences of one source (not in the start set) is always a break, and a stored GTID hole (stored_hole.go) stops the window like damage
// pos: read-only recoverable window a DBA can check before choosing a replay stop; a task with a damaged segment (storage_alert) stops before it, the same cutoff /replay uses, and reports one damaged-segment break
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
	breakDamaged     = "segment %s is damaged (STORAGE_INCONSISTENT); the window stops before it and leaves out it and every later segment"
	breakStoredHole  = "gtid hole: a source switch recorded %s as stored, but no segment holds it (STORAGE_INCONSISTENT); the window stops before %s and leaves out it and every later segment"
	breakInterior    = "gtid hole: %s is stored in no segment between %s and %s"
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
	// A damaged task keeps only the segments before the damage, the same
	// cutoff /replay uses, and reports the damage as a break.
	chosen := SelectReplayFiles(files)
	var damage *RecoveryBreak
	if problem, found := s.storageProblem(task); found {
		kept := excludeDamagedSegments(files, chosen, problem.Segment)
		if strings.TrimSpace(problem.Segment) == "" {
			kept = []BinlogFile{}
		}
		damage = damagedSegmentBreak(problem.Segment, chosen, kept)
		if strings.HasPrefix(problem.Detail, storedHoleDetailPrefix) {
			damage.Reason = fmt.Sprintf(breakStoredHole, problem.Missing, problem.Segment)
		}
		chosen = kept
	}
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
	out, err := assessRecoveryFrom(views, task.Source.Flavor, objectStorage, StartGTIDText(task))
	if err != nil || damage == nil {
		return out, err
	}
	out.Breaks = append(out.Breaks, *damage)
	out.Continuous = false
	return out, nil
}

// damagedSegmentBreak names the damaged segment and every later segment the
// window left out. kept is a prefix of chosen.
func damagedSegmentBreak(damaged string, chosen, kept []BinlogFile) *RecoveryBreak {
	keptSet := make(map[string]bool, len(kept))
	for _, file := range kept {
		keptSet[segmentBase(file)] = true
	}
	files := []string{damaged}
	for _, file := range chosen {
		name := segmentBase(file)
		if name == "" || name == damaged || keptSet[name] {
			continue
		}
		files = append(files, name)
	}
	return &RecoveryBreak{Files: files, Reason: fmt.Sprintf(breakDamaged, damaged)}
}

func assessRecovery(views []segmentView, flavor string, objectStorage bool) (RecoveryWindow, error) {
	return assessRecoveryFrom(views, flavor, objectStorage, "")
}

// assessRecoveryFrom is assessRecovery for a task whose GTID start set is
// startGTID. A restore applies that set first (a mysqldump or xtrabackup
// seed), so a later server's header that names part of it is not a hole.
func assessRecoveryFrom(views []segmentView, flavor string, objectStorage bool, startGTID string) (RecoveryWindow, error) {
	out := RecoveryWindow{Continuous: true, Breaks: []RecoveryBreak{}}
	mysqlFlavor := strings.EqualFold(strings.TrimSpace(flavor), "mysql")
	var earliest, latest *time.Time
	var events []binlog.GTIDEventRef

	parts := groupChain(views)
	var stored mysql.GTIDSet
	if mysqlFlavor {
		var err error
		if stored, err = chainStoredSet(views, startGTID); err != nil {
			return RecoveryWindow{}, err
		}
	}
	var before []binlog.GTIDEventRef
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
		before = append(before, mergeGTID(part.views).Events...)
		if mysqlFlavor {
			var hole bool
			var err error
			if part.prefix != next.prefix {
				// A server switch. The new server's file header was written
				// before the last transactions replicated into that file, and
				// the GTID dump skipped those because this backup has them.
				hole, err = switchHole(stored, before, mergeGTID(next.views))
			} else {
				hole, err = gtidHole(mergeGTID(part.views), mergeGTID(next.views))
			}
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
	if mysqlFlavor {
		if b, ok := interiorHoleBreak(views, startGTID, out.Breaks); ok {
			out.Breaks = append(out.Breaks, b)
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

// chainStoredSet is the history before the chain (the first previous-GTIDs
// header) plus every transaction a readable segment stores.
func chainStoredSet(views []segmentView, startGTID string) (mysql.GTIDSet, error) {
	start := ""
	var events []binlog.GTIDEventRef
	for _, view := range views {
		if !view.Readable {
			continue
		}
		if start == "" && view.Log.HasPrevious {
			start = view.Log.Previous
		}
		events = append(events, view.Log.Events...)
	}
	set, err := parseGTIDText(start)
	if err != nil {
		return nil, err
	}
	// The task's start set: a GTID dump skips those transactions even when
	// they sit in the first file after its header, and a restore applies them
	// from the seed. Without it a later server's header that names them looks
	// like a hole at every failback.
	if seed := strings.TrimSpace(startGTID); seed != "" {
		if seeded, err := parseGTIDText(seed); err == nil {
			if err := set.Update(seeded.String()); err != nil {
				return nil, err
			}
		}
	}
	if err := addGTIDEvents(set, events); err != nil {
		return nil, err
	}
	return set, nil
}

// switchHole is a transaction the new server had before its first stored
// file that this chain does not hold anywhere. Without a header, a sequence
// gap after the transactions stored before the switch is a hole.
func switchHole(stored mysql.GTIDSet, before []binlog.GTIDEventRef, next binlog.SegmentGTIDLog) (bool, error) {
	if !next.HasPrevious {
		return eventSeqHole(before, next.Events), nil
	}
	header, err := parseGTIDText(next.Previous)
	if err != nil {
		return false, err
	}
	return !stored.Contain(header), nil
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

// interiorHoleBreak is a break for sequences missing between the lowest and
// highest stored sequence of one source, when the start set does not cover
// them and no GTID-hole break already names the place. The chain's GTID set
// is never reported continuous with a hole inside it.
func interiorHoleBreak(views []segmentView, startGTID string, breaks []RecoveryBreak) (RecoveryBreak, bool) {
	for _, b := range breaks {
		if strings.HasPrefix(b.Reason, "gtid hole") || b.Reason == breakUnreadable {
			return RecoveryBreak{}, false
		}
	}
	stored := gtidRanges{}
	type viewSet struct {
		name string
		set  gtidRanges
	}
	sets := make([]viewSet, 0, len(views))
	for _, view := range views {
		if !view.Readable {
			continue
		}
		one := gtidRanges{}
		one.addEvents(view.Log.Events)
		one.normalize()
		name := segmentBase(view.File)
		if name == "" {
			name = strings.TrimSpace(view.File.FileName)
		}
		sets = append(sets, viewSet{name: name, set: one})
		stored.addAll(one)
	}
	stored.normalize()
	gaps := interiorGaps(stored)
	if seed, err := parseGTIDRanges(startGTID); err == nil {
		gaps = gaps.minus(seed)
	}
	if len(gaps) == 0 {
		return RecoveryBreak{}, false
	}
	keys := make([]string, 0, len(gaps))
	for k := range gaps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	k := keys[0]
	iv := gaps[k][0]
	left, right := "", ""
	for _, vs := range sets {
		for _, r := range vs.set[k] {
			if r[1] < iv[0] {
				left = vs.name
			}
			if r[0] > iv[1] && right == "" {
				right = vs.name
			}
		}
	}
	return RecoveryBreak{
		Files:  []string{left, right},
		Reason: fmt.Sprintf(breakInterior, gaps.String(), left, right),
	}, true
}
