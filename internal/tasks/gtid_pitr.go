// Package tasks provides module-level functionality for tasks.
// input: the full replay inventory, each segment's bytes, one MySQL stop_gtid, and an optional UTC start_datetime
// output: the segments through the GTID event and one mysqlbinlog command that stops at that event's start offset; plain-text errors for a bad GTID, a GTID outside the backup, an unsupported flavor, and a start time after that event
// pos: GTID stop on the same replay selection as the datetime window; the limit window stays when neither stop is set
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

const (
	invalidStopGTIDText     = "invalid stop_gtid"
	stopGTIDNotInRangeText  = "stop_gtid is not in this task's backed-up range"
	stopGTIDUnsupportedText = "stop_gtid is not supported for this flavor"
	stopBoundsExclusiveText = "stop_datetime and stop_gtid cannot both be set"
	startAfterStopGTIDText  = "start_datetime is after stop_gtid"
)

var (
	// ErrInvalidStopGTID is an empty or unparseable stop_gtid.
	ErrInvalidStopGTID = errors.New(invalidStopGTIDText)
	// ErrStopGTIDNotInRange is a MySQL GTID whose server UUID or sequence is not an event in this task's segments.
	ErrStopGTIDNotInRange = errors.New(stopGTIDNotInRangeText)
	// ErrStopGTIDUnsupported is stop_gtid on a flavor other than mysql.
	ErrStopGTIDUnsupported = errors.New(stopGTIDUnsupportedText)
	// ErrStopBoundsExclusive is stop_datetime together with stop_gtid.
	ErrStopBoundsExclusive = errors.New(stopBoundsExclusiveText)
	// ErrStartAfterStopGTID is a start_datetime strictly after the GTID event's timestamp.
	ErrStartAfterStopGTID = errors.New(startAfterStopGTIDText)
)

var (
	mysqlStopGTIDPattern = regexp.MustCompile(`(?i)^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}):([1-9][0-9]*)$`)
	mariaStopGTIDPattern = regexp.MustCompile(`^[0-9]+-[0-9]+-[0-9]+$`)
)

// MySQLStopGTID is one transaction, server_uuid:seq.
type MySQLStopGTID struct {
	UUID string
	Seq  int64
}

// ParseMySQLStopGTID accepts server_uuid:seq. The UUID match is case-insensitive.
// A range, a GTID set, and the MariaDB domain-server-seq form are rejected.
func ParseMySQLStopGTID(raw string) (MySQLStopGTID, error) {
	raw = strings.TrimSpace(raw)
	match := mysqlStopGTIDPattern.FindStringSubmatch(raw)
	if match == nil {
		return MySQLStopGTID{}, ErrInvalidStopGTID
	}
	seq, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil || seq <= 0 {
		return MySQLStopGTID{}, ErrInvalidStopGTID
	}
	return MySQLStopGTID{UUID: strings.ToLower(match[1]), Seq: seq}, nil
}

// MariaDBStopGTID reports the domain-server-seq form. That form is not a MySQL GTID.
func MariaDBStopGTID(raw string) bool {
	return mariaStopGTIDPattern.MatchString(strings.TrimSpace(raw))
}

// GTIDReplay lists every sealed segment and the highest open epoch, finds the
// MySQL GTID event, and returns the paths up to that segment plus a command
// whose --stop-position is the event's start offset in the last file.
// Transactions before that event are included. That event and everything
// after it are not. start, when set, is the same inclusive --start-datetime
// bound as the datetime window. A flavor other than mysql is
// ErrStopGTIDUnsupported. A missing task is ErrTaskNotFound. A segment that
// cannot be opened is the error OpenTaskSegment returns.
func (s *Scheduler) GTIDReplay(taskID string, raw string, start *time.Time) (PITRSet, error) {
	selected, stopPos, task, err := s.selectGTIDFiles(taskID, raw, start)
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
		Command:    FormatGTIDCommand(client, paths, start, stopPos),
	}, nil
}

// FormatGTIDCommand is the copy-paste client invocation that stops before one
// GTID. stopPos is --stop-position and applies to the last path. An empty
// path list returns an empty string.
func FormatGTIDCommand(client string, paths []string, start *time.Time, stopPos uint64) string {
	if len(paths) == 0 {
		return ""
	}
	tokens := make([]string, 0, 2+len(paths))
	if start != nil {
		tokens = append(tokens, pitrFlag("--start-datetime", *start))
	}
	tokens = append(tokens, "--stop-position="+strconv.FormatUint(stopPos, 10))
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

func (s *Scheduler) selectGTIDFiles(taskID, raw string, start *time.Time) ([]BinlogFile, uint64, Task, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, 0, Task{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(task.Source.Flavor), "mysql") {
		return nil, 0, Task{}, ErrStopGTIDUnsupported
	}
	gtid, err := ParseMySQLStopGTID(raw)
	if err != nil {
		return nil, 0, Task{}, err
	}
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return nil, 0, Task{}, err
	}
	chosen := SelectReplayFiles(files)
	spans := make([]binlog.SegmentGTIDScan, len(chosen))
	hit := -1
	var stopPos uint64
	var hitWhen time.Time
	for i, file := range chosen {
		name := segmentInventoryBasename(file)
		if name == "" {
			continue
		}
		body, _, err := s.OpenTaskSegment(taskID, name)
		if err != nil {
			return nil, 0, Task{}, err
		}
		scan, scanErr := binlog.ScanSegmentForGTID(body, gtid.UUID, gtid.Seq)
		closeErr := body.Close()
		if scanErr != nil {
			return nil, 0, Task{}, scanErr
		}
		if closeErr != nil {
			return nil, 0, Task{}, closeErr
		}
		spans[i] = scan
		if scan.Hit != nil {
			hit = i
			stopPos = scan.Hit.Offset
			hitWhen = scan.Hit.When
			break
		}
	}
	if hit < 0 {
		return nil, 0, Task{}, ErrStopGTIDNotInRange
	}
	if start != nil && !hitWhen.IsZero() && start.After(hitWhen) {
		return nil, 0, Task{}, ErrStartAfterStopGTID
	}
	return filterGTIDFiles(chosen[:hit+1], spans[:hit+1], start), stopPos, task, nil
}

func filterGTIDFiles(files []BinlogFile, spans []binlog.SegmentGTIDScan, start *time.Time) []BinlogFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]BinlogFile, 0, len(files))
	last := len(files) - 1
	for i, file := range files {
		if i == last || gtidFileOverlapsStart(spans, i, start) {
			out = append(out, file)
		}
	}
	return out
}

func gtidFileOverlapsStart(spans []binlog.SegmentGTIDScan, i int, start *time.Time) bool {
	if start == nil {
		return true
	}
	if i >= len(spans) || !spans[i].OK {
		return false
	}
	last := spans[i].Last
	if last.IsZero() {
		last = spans[i].First
	}
	return !last.Before(*start)
}
