// Package tasks provides module-level functionality for tasks.
// input: the replay inventory (sealed segments and the highest open epoch), each segment's bytes through OpenTaskSegment, the storage-health damage cut, and whether object storage is configured
// output: GET /window: the window internal/assurance derives (WindowOf), plus one damaged-segment, stored-hole, unreadable-segment or duplicated-source-file break when storage health stops the chain early, the same cutoff /replay uses
// pos: adapter only (ADR 0009 phase 1, P1-2): reads bytes and projects rows; the derivation lives in internal/assurance/window.go
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/binlog"
)

const (
	breakUnreadableSegment = "segment %s is not readable (corrupt binlog event size); the window stops before it and leaves out it and every later segment"
	breakOverlapSegment    = "source file %s is stored twice (%s is a second copy, STORAGE_INCONSISTENT); the window stops before both copies and leaves out them and every later segment"
	breakDamaged           = "segment %s is damaged (STORAGE_INCONSISTENT); the window stops before it and leaves out it and every later segment"
	breakStoredHole        = "gtid hole: a source switch recorded %s as stored, but no segment holds it (STORAGE_INCONSISTENT); the window stops before %s and leaves out it and every later segment"
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

// RecoveryWindow reads the retained chain and reports how far back a restore
// can go, and where it is broken. A missing task is ErrTaskNotFound.
// A segment listed in the inventory whose bytes are not on this process is a
// break, not an error. A segment that cannot be read for any other reason
// fails the call, except a corrupt event size, which is a not-readable break
// naming the segment.
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
	if health := s.storageHealth(task, ""); health.Found() {
		problem := health.Problem
		kept := health.replayCut(files, chosen)
		damage = damagedSegmentBreak(problem.Segment, chosen, kept)
		switch health.Kind {
		case StorageStoredHole:
			damage.Reason = fmt.Sprintf(breakStoredHole, problem.Missing, problem.Segment)
		case StorageUnreadable:
			damage.Reason = fmt.Sprintf(breakUnreadableSegment, problem.Segment)
		case StorageOverlap:
			named, _ := binlog.ClassifySegment(problem.Segment)
			damage.Reason = fmt.Sprintf(breakOverlapSegment, named.Source, problem.Segment)
		}
		chosen = kept
	}
	s.mu.Lock()
	objectStorage := s.fileUploader != nil
	s.mu.Unlock()

	segs := make([]assurance.WindowSegment, 0, len(chosen))
	for _, file := range chosen {
		seg := windowSegmentOf(file)
		if seg.Base == "" {
			segs = append(segs, seg)
			continue
		}
		body, _, openErr := s.OpenTaskSegment(taskID, seg.Base)
		if openErr != nil {
			if errors.Is(openErr, ErrSegmentNotOnProcess) {
				segs = append(segs, seg)
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
		read := binlog.ReadSegmentContents
		if !seg.Open {
			read = binlog.ReadSealedSegmentContents
		}
		contents, scanErr := read(bytes.NewReader(raw))
		if errors.Is(scanErr, binlog.ErrCorruptEventSize) {
			segs = append(segs, seg) // not readable: a named break, not a failed call
			continue
		}
		if scanErr != nil {
			return RecoveryWindow{}, scanErr
		}
		first, last, ok, spanErr := binlog.EventTimeSpan(bytes.NewReader(raw))
		if spanErr != nil {
			return RecoveryWindow{}, spanErr
		}
		seg.Readable = true
		seg.SpanOK = ok
		seg.First, seg.Last = first, last
		seg.Log = contents.Log
		segs = append(segs, seg)
	}
	derived, err := assurance.WindowOf(segs, task.Source.Flavor, objectStorage, StartGTIDText(task))
	if err != nil {
		return RecoveryWindow{}, err
	}
	out := recoveryWindowOf(derived)
	if damage == nil {
		return out, nil
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

// windowSegmentOf is file as the window sees it, before its bytes are read.
func windowSegmentOf(file BinlogFile) assurance.WindowSegment {
	return assurance.WindowSegment{
		Base:             segmentInventoryBasename(file),
		Source:           file.FileName,
		Path:             file.FilePath,
		Open:             segmentIsOpen(file),
		ChecksumMismatch: strings.EqualFold(strings.TrimSpace(file.Checksum), ChecksumMismatch),
		UploadState:      file.UploadState,
	}
}

func recoveryWindowOf(w assurance.Window) RecoveryWindow {
	out := RecoveryWindow{Continuous: w.Continuous, Earliest: w.Earliest, Latest: w.Latest, GTIDSet: w.GTIDSet, Breaks: make([]RecoveryBreak, 0, len(w.Breaks))}
	for _, b := range w.Breaks {
		out.Breaks = append(out.Breaks, RecoveryBreak{Files: b.Files, Reason: b.Reason})
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

// eventsContained reports that set holds every event's GTID.
func eventsContained(set binlog.GTIDSet, events []binlog.GTIDEventRef) bool {
	for _, ev := range events {
		if !set.HasEvent(ev) {
			return false
		}
	}
	return true
}
