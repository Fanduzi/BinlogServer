// Package tasks provides module-level functionality for tasks.
// input: the full files inventory, one open segment per source index, and each segment's event-header time span
// output: the ordered paths that cover a UTC point-in-time window, locations aligned with those paths, plus one mysqlbinlog or mariadb-binlog command; start equal to stop yields no paths and no command
// pos: point-in-time seek on the existing replay selection; the limit window stays on GET /replay without stop_datetime
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

const pitrClockLayout = "2006-01-02 15:04:05"

// PITRSet is the point-in-time replay argument list.
// Paths follow SelectReplayFiles: one file_path per source index, ascending.
// Command is empty when Paths is empty. The clock in Command is UTC.
// Run it with the TZ=UTC prefix already in the string so mysqlbinlog compares
// those flags to the event-header timestamps.
type PITRSet struct {
	Flavor     string   `json:"flavor"`
	Client     string   `json:"client"`
	ClientHint string   `json:"client_hint"`
	Paths      []string `json:"paths"`
	// Locations matches Paths. bucket means that path is not on this process.
	Locations []string `json:"locations,omitempty"`
	Command   string   `json:"command"`
}

// EventSpan is the first and last non-zero event-header time of one segment.
// A zero span means the segment has no timed event.
type EventSpan struct {
	First time.Time
	Last  time.Time
}

// ParsePITRDatetime accepts YYYY-MM-DD HH:MM:SS and YYYY-MM-DDTHH:MM:SS as UTC,
// and RFC3339 with a zone. The result is UTC.
func ParsePITRDatetime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, errInvalidPITRDatetime
	}
	for _, layout := range []string{
		pitrClockLayout,
		"2006-01-02T15:04:05",
	} {
		parsed, err := time.ParseInLocation(layout, raw, time.UTC)
		if err == nil {
			return parsed.UTC(), nil
		}
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, errInvalidPITRDatetime
}

// FilterPITRFiles keeps segments that contain an event in [start, stop).
// stop is exclusive, matching mysqlbinlog --stop-datetime. start is inclusive
// when set, matching --start-datetime. start equal to stop selects nothing:
// that window contains no instant. files and spans are paired in order.
// A segment with no timed event is left out. The input order is kept.
func FilterPITRFiles(files []BinlogFile, spans []EventSpan, start *time.Time, stop time.Time) []BinlogFile {
	out := make([]BinlogFile, 0)
	for i, file := range files {
		var span EventSpan
		if i < len(spans) {
			span = spans[i]
		}
		if coversPITR(span, start, stop) {
			out = append(out, file)
		}
	}
	return out
}

// FormatPITRCommand is the copy-paste client invocation for paths.
// An empty path list returns an empty string. When client is set, the string
// starts with TZ=UTC and that client. Flags are --start-datetime (when start
// is set) then --stop-datetime, then paths, in order. Clock text is UTC.
func FormatPITRCommand(client string, paths []string, start *time.Time, stop time.Time) string {
	if len(paths) == 0 {
		return ""
	}
	tokens := make([]string, 0, 2+len(paths))
	if start != nil {
		tokens = append(tokens, pitrFlag("--start-datetime", *start))
	}
	tokens = append(tokens, pitrFlag("--stop-datetime", stop))
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

// PITRReplay lists one path per source index across the full inventory, keeps
// the paths whose event times cover [start, stop), and builds the client command.
// A missing task is ErrTaskNotFound. A selected segment that cannot be opened
// is the same error OpenTaskSegment returns. An empty cover is an empty Paths
// and Command with the flavor client still set.
func (s *Scheduler) PITRReplay(taskID string, start *time.Time, stop time.Time) (PITRSet, error) {
	selected, task, err := s.selectPITRFiles(taskID, start, stop)
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
		Command:    FormatPITRCommand(client, paths, start, stop),
	}, nil
}

func (s *Scheduler) selectPITRFiles(taskID string, start *time.Time, stop time.Time) ([]BinlogFile, Task, error) {
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return nil, Task{}, err
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, Task{}, err
	}
	chosen := SelectReplayFiles(files)
	spans := make([]EventSpan, len(chosen))
	for i, file := range chosen {
		name := segmentInventoryBasename(file)
		if name == "" {
			continue
		}
		body, _, err := s.OpenTaskSegment(taskID, name)
		if err != nil {
			return nil, Task{}, err
		}
		first, last, ok, spanErr := binlog.EventTimeSpan(body)
		closeErr := body.Close()
		if spanErr != nil {
			return nil, Task{}, spanErr
		}
		if closeErr != nil {
			return nil, Task{}, closeErr
		}
		if ok {
			spans[i] = EventSpan{First: first, Last: last}
		}
	}
	return FilterPITRFiles(chosen, spans, start, stop), task, nil
}

func coversPITR(span EventSpan, start *time.Time, stop time.Time) bool {
	if span.First.IsZero() && span.Last.IsZero() {
		return false
	}
	first, last := span.First, span.Last
	if first.IsZero() {
		first = last
	}
	if last.IsZero() {
		last = first
	}
	// [start, stop) is empty when start is not strictly before stop.
	// A span that crosses that single instant is not a hit.
	if start != nil && !start.Before(stop) {
		return false
	}
	if !first.Before(stop) {
		return false
	}
	if start != nil && last.Before(*start) {
		return false
	}
	return true
}

func pitrClock(t time.Time) string {
	return t.UTC().Format(pitrClockLayout)
}

func pitrFlag(name string, t time.Time) string {
	return name + "=" + shellToken(pitrClock(t))
}

func shellToken(text string) string {
	if text == "" {
		return "''"
	}
	for _, r := range text {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("_./:@+-", r) {
			return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
		}
	}
	return text
}

var errInvalidPITRDatetime = errors.New("invalid datetime")
