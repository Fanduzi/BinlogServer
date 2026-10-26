// Package tasks provides module-level functionality for tasks.
// input: a MySQL task directory that switched source at least once, its SOURCE_SWITCHOVER events (the gtid_set each continued switch recorded as stored), the task's start gtid_set, and the GTIDs of its local segments
// output: a storage problem when a switch recorded transactions as stored that no local segment holds, between transactions the segments do hold, or right after the task's start set while the task's first file is still on disk (a stored GTID hole), naming the first segment after the hole so /replay and /window stop before it; segment GTID ranges come from the segment ledger (binlog.Ledger.Contents, ADR 0006)
// pos: safety net for a failback chain whose segment was lost (a hole is not reported while a sealed UPLOADED catalog row before the cut is only in object storage: replay reads it from there) (an earlier build's first-stint file kept only in object storage, or a file deleted by hand); read-only, Start is not refused by it; the third class of storageHealth, sharing its ledger (ADR 0007)
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"binlog_server/internal/binlog"
)

const storedHoleDetailPrefix = "stored gtid hole:"

type holeFile struct {
	name string
	set  binlog.GTIDSet
	prev string
}

type holeCacheEntry struct {
	sig     string
	problem binlog.StorageProblem
	found   bool
}

var storedHoleCache sync.Map // task dir -> holeCacheEntry

// storedHoleIn finds transactions a continued switch recorded as stored that
// no local segment holds, with stored transactions of the same source both
// before and after them (or the start set before them, while the task's
// first file is still on disk). Retention removes the oldest files first,
// so it never makes such a hole. A task that never switched is not read.
func (s *Scheduler) storedHoleIn(task Task, dir string, ledger *binlog.Ledger) (binlog.StorageProblem, bool) {
	if s == nil {
		return binlog.StorageProblem{}, false
	}
	if flavor := strings.TrimSpace(task.Source.Flavor); flavor != "" && !strings.EqualFold(flavor, "mysql") {
		return binlog.StorageProblem{}, false
	}
	s.mu.Lock()
	dataDir := s.dataDir
	store := s.eventStore
	s.mu.Unlock()
	chainFile := readSourceChainFile(dir)
	if len(chainFile) < 2 {
		return binlog.StorageProblem{}, false
	}
	files, err := listTaskBinlogFilesOnDisk(dataDir, task.ID, segmentInventoryLimit)
	if err != nil {
		return binlog.StorageProblem{}, false
	}
	chosen := SelectReplayFiles(files)
	var sig strings.Builder
	sig.WriteString(strings.Join(chainFile, ","))
	sig.WriteString("|" + StartGTIDText(task))
	for _, f := range chosen {
		sig.WriteString("|" + segmentInventoryBasename(f))
		if !segmentIsOpen(f) {
			sig.WriteString("#" + strconv.FormatInt(f.SizeBytes, 10))
		}
	}
	if v, ok := storedHoleCache.Load(dir); ok {
		if e := v.(holeCacheEntry); e.sig == sig.String() {
			return e.problem, e.found
		}
	}
	events, err := s.readSourceSwitchEvents(task.ID, store)
	if err != nil {
		return binlog.StorageProblem{}, false
	}
	problem, found := planStoredHole(ledger, chainFile, events, StartGTIDText(task), chosen)
	if found && s.uploadedOnlyBefore(task.ID, dir, problem.Segment) {
		// A catalog row whose bytes are only in object storage sits before
		// the cut: replay reads it from there and it may hold the
		// transactions. Not proven missing.
		found = false
		problem = binlog.StorageProblem{}
	}
	storedHoleCache.Store(dir, holeCacheEntry{sig: sig.String(), problem: problem, found: found})
	return problem, found
}

// uploadedOnlyBefore reports a sealed UPLOADED catalog row that is not on
// local disk and comes before segment in replay order.
func (s *Scheduler) uploadedOnlyBefore(taskID, dir, segment string) bool {
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return false
	}
	for _, f := range SelectReplayFiles(files) {
		name := segmentInventoryBasename(f)
		if name == segment {
			return false
		}
		if _, ok := sealedUploadedObjectKey([]BinlogFile{f}, name); !ok {
			continue
		}
		if _, err := binlog.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// planStoredHole is storedHoleFor on a listing: chosen is the replay order.
func planStoredHole(ledger *binlog.Ledger, chainFile []string, events []TaskEvent, startGTID string, chosen []BinlogFile) (binlog.StorageProblem, bool) {
	chainFile = cleanIdents(chainFile)
	if len(chainFile) == 0 {
		return binlog.StorageProblem{}, false
	}
	stints := deriveStints(chainFile[0], events)
	recorded := binlog.GTIDSet{}
	switchTo := map[string]string{} // key -> identity of the first switch that recorded it
	for _, st := range stints[1:] {
		set, err := binlog.ParseGTIDSet(st.in)
		if err != nil {
			continue
		}
		recorded.AddAll(set)
		for k := range set {
			if _, ok := switchTo[k]; !ok {
				switchTo[k] = st.id
			}
		}
	}
	if len(recorded) == 0 {
		return binlog.StorageProblem{}, false
	}
	held, err := binlog.ParseGTIDSet(startGTID)
	if err != nil {
		held = binlog.GTIDSet{}
	}
	parts := make([]holeFile, 0, len(chosen))
	for _, f := range chosen {
		name := segmentInventoryBasename(f)
		if name == "" {
			continue
		}
		c, err := ledger.Contents(name)
		if err != nil {
			// An unreadable file is reported by the window; it is not a hole.
			return binlog.StorageProblem{}, false
		}
		parts = append(parts, holeFile{name: name, set: c.GTIDs, prev: c.Log.Previous})
		held.AddAll(c.GTIDs)
	}
	// The start set anchors a hole from below only while the task's first
	// file is still here: an unprefixed first file whose header is inside
	// the start set. After retention the first local file starts later, and
	// the transactions before it still restore from object storage.
	start := binlog.GTIDSet{}
	if len(parts) > 0 {
		if named, ok := binlog.ClassifySegment(parts[0].name); ok && SourceStintOf(named.Source, cleanIdents(chainFile)) == 0 {
			seed, seedErr := binlog.ParseGTIDSet(startGTID)
			header, headerErr := binlog.ParseGTIDSet(parts[0].prev)
			if seedErr == nil && headerErr == nil && len(seed) > 0 && len(header.Minus(seed)) == 0 {
				start = seed
			}
		}
	}
	missing := recorded.Minus(held)
	cut := -1
	lost := binlog.GTIDSet{}
	for k, list := range missing {
		for _, iv := range list {
			below := -2 // index of the last local file holding k below iv; -1: only the start set
			above := false
			for _, r := range start[k] {
				if r.Lo < iv.Lo {
					below = -1
				}
			}
			for i, p := range parts {
				for _, r := range p.set[k] {
					if r.Lo < iv.Lo {
						below = i
					}
					if r.Hi > iv.Hi {
						above = true
					}
				}
			}
			if below < -1 || !above {
				continue
			}
			lost.Add(k, iv.Lo, iv.Hi)
			next := below + 1
			if below == -1 {
				// Only the start set is below: cut at the first file that
				// is after the hole (holds a later sequence, or a header
				// that already lists part of it).
				next = len(parts)
				one := binlog.GTIDSet{}
				one.Add(k, iv.Lo, iv.Hi)
				for i, p := range parts {
					later := false
					for _, r := range p.set[k] {
						if r.Hi > iv.Hi {
							later = true
						}
					}
					if header, err := binlog.ParseGTIDSet(p.prev); err == nil && one.Minus(header).String() != one.String() {
						later = true
					}
					if later {
						next = i
						break
					}
				}
			}
			if cut < 0 || next < cut {
				cut = next
			}
		}
	}
	if cut < 0 || cut >= len(parts) {
		return binlog.StorageProblem{}, false
	}
	problem := binlog.StorageProblem{
		Segment: parts[cut].name,
		Missing: lost.String(),
		Detail:  storedHoleDetailPrefix + " " + lost.String() + " before " + parts[cut].name,
	}
	for _, p := range parts[:cut] {
		problem.Valid = append(problem.Valid, p.name)
	}
	who := ""
	for k := range lost {
		if id := switchTo[k]; id != "" {
			who = id
			break
		}
	}
	problem.Message = fmt.Sprintf("A source switch (to %s) recorded that this task had stored %s, but no segment on this disk holds them, and segments before and after them do. A replay through %s would skip them silently, so replay stops before %s. If an earlier build's first-stint segment is kept only in object storage, or a file was removed by hand, put it back into the task directory (see troubleshooting \"Task directory from an earlier build after a failback\").", who, lost.String(), parts[cut].name, parts[cut].name)
	return problem, true
}
