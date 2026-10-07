// Package tasks provides module-level functionality for tasks.
// input: one restored backup's MySQL executed GTID set, one existing stop (stop_datetime or stop_gtid), and each selected segment's GTID events
// output: the segments that still contain a transaction the backup does not have, a mysqlbinlog command that skips the executed set, an explanatory note when nothing remains to apply, and plain-text errors for a bad set, a non-mysql flavor, a missing stop, or a gap before the retained range including sequences a GTID dump skipped between previous-GTIDs and the first copied event
// pos: roll forward from a restored full backup to an existing stop, on the same replay selection as stop_datetime and stop_gtid
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"

	"github.com/go-mysql-org/go-mysql/mysql"
)

const (
	invalidStartGTIDSetText     = "invalid start_gtid_set"
	startGTIDSetUnsupportedText = "start_gtid_set is not supported for this flavor"
	startBoundsExclusiveText    = "start_gtid_set and start_datetime cannot both be set"
	startGTIDSetNeedsStopText   = "stop_datetime or stop_gtid is required"
	startGTIDSetGapText         = "start_gtid_set has a gap before this task's backed-up range"
	executedGTIDCoveredNote     = "every transaction up to the stop is already in start_gtid_set"
)

var (
	// ErrInvalidStartGTIDSet is an empty or unparseable MySQL GTID set.
	ErrInvalidStartGTIDSet = errors.New(invalidStartGTIDSetText)
	// ErrStartGTIDSetUnsupported is start_gtid_set on a flavor other than mysql.
	ErrStartGTIDSetUnsupported = errors.New(startGTIDSetUnsupportedText)
	// ErrStartBoundsExclusive is start_gtid_set together with start_datetime.
	ErrStartBoundsExclusive = errors.New(startBoundsExclusiveText)
	// ErrStartGTIDSetNeedsStop is start_gtid_set without stop_datetime or stop_gtid.
	ErrStartGTIDSetNeedsStop = errors.New(startGTIDSetNeedsStopText)
	// ErrStartGTIDSetGap is a set that does not cover the transactions before the first segment that would be applied.
	ErrStartGTIDSetGap = errors.New(startGTIDSetGapText)
)

// ParseStartGTIDSet accepts a MySQL GTID set, including a pasted
// gtid_executed value with spaces or newlines. A single server_uuid:seq
// is a one-transaction set. An empty set and the MariaDB domain-server-seq
// form are rejected.
func ParseStartGTIDSet(raw string) (mysql.GTIDSet, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\n", "")
	raw = strings.ReplaceAll(raw, "\r", "")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrInvalidStartGTIDSet
	}
	set, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, raw)
	if err != nil || set == nil || set.IsEmpty() || !executedIntervalsOpenAtOne(set) {
		return nil, ErrInvalidStartGTIDSet
	}
	return set, nil
}

func executedIntervalsOpenAtOne(set mysql.GTIDSet) bool {
	ms, ok := set.(*mysql.MysqlGTIDSet)
	if !ok {
		return false
	}
	for _, tags := range *ms {
		for _, intervals := range tags {
			for _, in := range intervals {
				if in.Start <= 0 {
					return false
				}
			}
		}
	}
	return true
}

// ExecutedGTIDReplay lists the segments that still contain a transaction
// outside executedRaw, up to stopGTIDRaw or stop. Segments whose in-window
// transactions are all in the set are omitted. The command passes that set to
// mysqlbinlog --exclude-gtids and keeps the existing stop flag. When every
// in-window transaction is already in the set, Paths and Command are empty
// and Note says so. A set that would skip transactions missing from this
// task's retained segments is ErrStartGTIDSetGap.
func (s *Scheduler) ExecutedGTIDReplay(taskID, executedRaw, stopGTIDRaw string, stop *time.Time) (PITRSet, error) {
	selected, task, note, exclude, stopPos, useStopPos, stopTime, err := s.selectExecutedGTID(taskID, executedRaw, stopGTIDRaw, stop)
	if err != nil {
		return PITRSet{}, err
	}
	client, hint := ReplayClient(task.Source.Flavor)
	paths := make([]string, 0, len(selected))
	for _, file := range selected {
		paths = append(paths, file.FilePath)
	}
	return PITRSet{
		Flavor:     task.Source.Flavor,
		Client:     client,
		ClientHint: hint,
		Paths:      paths,
		Locations:  ReplayLocations(selected),
		Command:    FormatExecutedGTIDCommand(client, paths, exclude, stopPos, useStopPos, stopTime),
		Note:       note,
	}, nil
}

// FormatExecutedGTIDCommand is the copy-paste client invocation that skips
// transactions in exclude. useStopPos adds --stop-position for the last path.
// stopTime adds --stop-datetime. An empty path list returns an empty string.
func FormatExecutedGTIDCommand(client string, paths []string, exclude string, stopPos uint64, useStopPos bool, stopTime *time.Time) string {
	if len(paths) == 0 {
		return ""
	}
	tokens := make([]string, 0, 3+len(paths))
	tokens = append(tokens, "--exclude-gtids="+shellToken(exclude))
	if stopTime != nil {
		tokens = append(tokens, pitrFlag("--stop-datetime", *stopTime))
	}
	if useStopPos {
		tokens = append(tokens, "--stop-position="+strconv.FormatUint(stopPos, 10))
	}
	for _, path := range paths {
		tokens = append(tokens, shellToken(path))
	}
	var b strings.Builder
	if client != "" {
		b.WriteString("TZ=UTC ")
		b.WriteString(client)
		b.WriteString(" \\\n")
	}
	for i, token := range tokens {
		if client != "" || i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(token)
		if i != len(tokens)-1 {
			b.WriteString(" \\\n")
		}
	}
	return b.String()
}

type executedFile struct {
	file BinlogFile
	log  binlog.SegmentGTIDLog
}

func (s *Scheduler) selectExecutedGTID(taskID, executedRaw, stopGTIDRaw string, stop *time.Time) ([]BinlogFile, Task, string, string, uint64, bool, *time.Time, error) {
	if strings.TrimSpace(stopGTIDRaw) != "" && stop != nil {
		return nil, Task{}, "", "", 0, false, nil, ErrStopBoundsExclusive
	}
	if strings.TrimSpace(stopGTIDRaw) == "" && stop == nil {
		return nil, Task{}, "", "", 0, false, nil, ErrStartGTIDSetNeedsStop
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, Task{}, "", "", 0, false, nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(task.Source.Flavor), "mysql") {
		return nil, Task{}, "", "", 0, false, nil, ErrStartGTIDSetUnsupported
	}
	executed, err := ParseStartGTIDSet(executedRaw)
	if err != nil {
		return nil, Task{}, "", "", 0, false, nil, err
	}
	var want MySQLStopGTID
	if strings.TrimSpace(stopGTIDRaw) != "" {
		want, err = ParseMySQLStopGTID(stopGTIDRaw)
		if err != nil {
			return nil, Task{}, "", "", 0, false, nil, err
		}
	}
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return nil, Task{}, "", "", 0, false, nil, err
	}
	chosen := SelectReplayFiles(files)
	logs := make([]executedFile, 0, len(chosen))
	hit := -1
	var stopPos uint64
	for _, file := range chosen {
		name := segmentInventoryBasename(file)
		seg := executedFile{file: file}
		if name != "" {
			body, _, openErr := s.OpenTaskSegment(taskID, name)
			if openErr != nil {
				return nil, Task{}, "", "", 0, false, nil, openErr
			}
			log, scanErr := binlog.ScanSegmentGTIDs(body)
			closeErr := body.Close()
			if scanErr != nil {
				return nil, Task{}, "", "", 0, false, nil, scanErr
			}
			if closeErr != nil {
				return nil, Task{}, "", "", 0, false, nil, closeErr
			}
			seg.log = log
		}
		logs = append(logs, seg)
		if stop == nil && hit < 0 {
			for _, ev := range seg.log.Events {
				if ev.UUID == want.UUID && ev.Seq == want.Seq {
					hit = len(logs) - 1
					stopPos = ev.Offset
					break
				}
			}
			if hit >= 0 {
				break
			}
		}
	}
	if stop == nil && hit < 0 {
		return nil, Task{}, "", "", 0, false, nil, ErrStopGTIDNotInRange
	}

	type neededHit struct {
		file int
	}
	var needed []neededHit
	inWindow := 0
	for i, seg := range logs {
		for _, ev := range seg.log.Events {
			if !executedEventInWindow(i, ev, hit, stopPos, stop) {
				continue
			}
			inWindow++
			one, parseErr := mysql.ParseGTIDSet(mysql.MySQLFlavor, ev.UUID+":"+strconv.FormatInt(ev.Seq, 10))
			if parseErr != nil {
				return nil, Task{}, "", "", 0, false, nil, parseErr
			}
			if !executed.Contain(one) {
				needed = append(needed, neededHit{file: i})
			}
		}
	}
	if len(needed) == 0 {
		note := ""
		if inWindow > 0 {
			note = executedGTIDCoveredNote
		}
		return []BinlogFile{}, task, note, "", 0, false, nil, nil
	}
	first := needed[0].file
	if err := executedGap(executed, logs, first); err != nil {
		return nil, Task{}, "", "", 0, false, nil, err
	}
	include := map[int]bool{}
	for _, hit := range needed {
		include[hit.file] = true
	}
	selected := make([]BinlogFile, 0)
	last := -1
	for i, seg := range logs {
		if include[i] {
			selected = append(selected, seg.file)
			last = i
		}
	}
	useStopPos := stop == nil && last == hit && stopPos > 0
	var stopTime *time.Time
	if stop != nil {
		stopTime = stop
	}
	return selected, task, "", executed.String(), stopPos, useStopPos, stopTime, nil
}

func executedEventInWindow(fileIndex int, ev binlog.GTIDEventRef, hit int, stopPos uint64, stop *time.Time) bool {
	if stop != nil {
		return ev.When.Before(*stop)
	}
	if fileIndex > hit {
		return false
	}
	if fileIndex < hit {
		return true
	}
	return ev.Offset < stopPos
}

func executedGap(executed mysql.GTIDSet, logs []executedFile, first int) error {
	log := logs[first].log
	if log.HasPrevious {
		if err := executedPreviousCovered(executed, log.Previous); err != nil {
			return err
		}
	}
	// A GTID dump keeps the source file's previous-GTIDs and then the first
	// transaction that was not already executed at task start. The numbers
	// between that header and the first copied event are not in the segment.
	// They have to be in the backup set, or in an earlier retained segment.
	covered := executed.Clone()
	if log.HasPrevious {
		text := strings.TrimSpace(log.Previous)
		if text != "" {
			if err := covered.Update(text); err != nil {
				return err
			}
		}
	}
	for i := 0; i < first; i++ {
		for _, ev := range logs[i].log.Events {
			if err := covered.Update(ev.UUID + ":" + strconv.FormatInt(ev.Seq, 10)); err != nil {
				return err
			}
		}
	}
	return executedPrefixCovered(covered, log.Events)
}

func executedPrefixCovered(covered mysql.GTIDSet, events []binlog.GTIDEventRef) error {
	minSeq := map[string]int64{}
	for _, ev := range events {
		cur, ok := minSeq[ev.UUID]
		if !ok || ev.Seq < cur {
			minSeq[ev.UUID] = ev.Seq
		}
	}
	for sid, seq := range minSeq {
		if seq <= 1 {
			continue
		}
		prefix, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, sid+":1-"+strconv.FormatInt(seq-1, 10))
		if err != nil {
			return err
		}
		if !covered.Contain(prefix) {
			return ErrStartGTIDSetGap
		}
	}
	return nil
}

func executedPreviousCovered(executed mysql.GTIDSet, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	prev, err := mysql.ParseGTIDSet(mysql.MySQLFlavor, text)
	if err != nil {
		return err
	}
	if prev.IsEmpty() || executed.Contain(prev) {
		return nil
	}
	return ErrStartGTIDSetGap
}
