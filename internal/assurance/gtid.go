// Package assurance provides module-level functionality for assurance.
// input: MySQL GTID logs of segments (binlog.SegmentGTIDLog)
// output: the GTID hole judgements I3 uses: inside one segment, between neighbouring source files, and across a source switch
// pos: moved here from tasks/window.go (ADR 0009 phase 1); window.go keeps its copy until P1-2 removes it
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"sort"
	"strings"

	"binlog_server/internal/binlog"
)

// gtidHole is a transaction committed after the last event stored in prev
// and before next. A capture that starts mid-file keeps that file's header,
// so sequences between the header and the first stored event are not a hole.
func gtidHole(prev, next binlog.SegmentGTIDLog) bool {
	if !prev.HasPrevious && len(prev.Events) == 0 && !next.HasPrevious && len(next.Events) == 0 {
		return false
	}
	if next.HasPrevious {
		nextSet, err := binlog.ParseGTIDSet(next.Previous)
		if err != nil {
			return false
		}
		if !eventsContained(nextSet, prev.Events) {
			return true
		}
		return previousContainsBeyond(nextSet, prev.Events)
	}
	return eventSeqHole(prev.Events, next.Events)
}

// switchHole is a transaction the new server had before its first stored
// file that the chain holds nowhere.
func switchHole(stored binlog.GTIDSet, before []binlog.GTIDEventRef, next binlog.SegmentGTIDLog) bool {
	if !next.HasPrevious {
		return eventSeqHole(before, next.Events)
	}
	header, err := binlog.ParseGTIDSet(next.Previous)
	if err != nil {
		return false
	}
	return !stored.Contains(header)
}

func eventsContained(set binlog.GTIDSet, events []binlog.GTIDEventRef) bool {
	for _, ev := range events {
		if !set.HasEvent(ev) {
			return false
		}
	}
	return true
}

func previousContainsBeyond(prev binlog.GTIDSet, events []binlog.GTIDEventRef) bool {
	maxSeq := map[string]int64{}
	for _, ev := range events {
		if ev.Seq > maxSeq[ev.UUID] {
			maxSeq[ev.UUID] = ev.Seq
		}
	}
	for uuid, max := range maxSeq {
		if prev.Has(strings.ToLower(uuid), max+1) {
			return true
		}
	}
	return false
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
			if !found || missing < foundSeq || (missing == foundSeq && uuid < foundUUID) {
				found, foundUUID, foundSeq = true, uuid, missing
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
		if cur, ok := minNext[ev.UUID]; !ok || ev.Seq < cur {
			minNext[ev.UUID] = ev.Seq
		}
	}
	for uuid, max := range maxPrev {
		if min, ok := minNext[uuid]; ok && min > max+1 {
			return true
		}
	}
	return false
}
