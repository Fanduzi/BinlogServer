// Package tasks provides module-level functionality for tasks.
// input: {data_dir}/{task_id}/.source-chain and SOURCE_SWITCHOVER task events
// output: the ordered source stints that own a task's files (a failback A, B, A is three stints; a return to a server uses the {identity}~{n} prefix), which stint is current, and each switch's old, new, file:pos, GTID set, and continued, stopped, or resumed outcome, with a repeated check of the same move counted once and a stop resolved for good once a TASK_RUNNING event follows its latest check; file rows gain source_identity and source_server when that chain is known
// pos: read-only view of a VIP source switch for the task API, the files list, and the switchover metric
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

const (
	sourceChainFileName = ".source-chain"
	// sourceSwitchListLimit is how many recent events the fallback scan reads
	// when the event store cannot list SOURCE_SWITCHOVER rows by type.
	sourceSwitchListLimit = 10000
	resumeEventScanLimit  = 1000

	// SwitchOutcomeContinued is a switch that kept this task copying.
	SwitchOutcomeContinued = "continued"
	// SwitchOutcomeStopped is a switch that stopped the task.
	SwitchOutcomeStopped = "stopped"
	// SwitchOutcomeResumed is a stop the task has left: it runs again.
	SwitchOutcomeResumed = "resumed"

	// SwitchReasonNoGTID means the backup has no executed GTID set.
	SwitchReasonNoGTID = "no_gtid"
	// SwitchReasonMissing means the new server does not have transactions already stored.
	SwitchReasonMissing = "missing_transactions"
	// SwitchReasonPurged means the new server has purged transactions this backup still needs.
	SwitchReasonPurged = "purged"
	// SwitchReasonMariaDB means MariaDB has no GTID path for a source switch.
	SwitchReasonMariaDB = "mariadb"
	// SwitchReasonUnreadable means the new server's GTID state could not be read.
	SwitchReasonUnreadable = "gtid_unreadable"
)

// SourceServer is one stint: a run of this task's files written by one server.
// The first stint owns names with no prefix. A later stint's files are named
// {prefix}.{binlog file}, where Prefix is the identity on that server's first
// stint and {identity}~{n} when the chain returns to it for the n-th time
// (a failback A, B, A). Current is the last stint: the server that wrote the
// newest files, not necessarily one the task is copying now.
type SourceServer struct {
	Identity string `json:"identity"`
	Prefix   string `json:"prefix,omitempty"`
	Current  bool   `json:"current"`
}

// SourceStintPrefixes returns the file-name prefix of each chain line.
// The first line has none. Every later line is its identity, with ~n added
// when that identity already appeared n-1 times earlier in the chain.
func SourceStintPrefixes(chain []string) []string {
	out := make([]string, len(chain))
	seen := make(map[string]int, len(chain))
	for i, id := range chain {
		id = strings.TrimSpace(id)
		n := seen[id]
		seen[id] = n + 1
		switch {
		case i == 0:
			out[i] = ""
		case n == 0:
			out[i] = id
		default:
			out[i] = id + "~" + strconv.Itoa(n+1)
		}
	}
	return out
}

// SourceStintOf returns the chain index whose prefix names this source file
// (longest prefix wins). A name with no stint prefix belongs to line 0.
// It returns -1 for an empty chain.
func SourceStintOf(name string, chain []string) int {
	if len(chain) == 0 {
		return -1
	}
	prefixes := SourceStintPrefixes(chain)
	best, bestLen := 0, 0
	for i, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(name, prefix+".") && len(prefix) > bestLen {
			best, bestLen = i, len(prefix)
		}
	}
	return best
}

// SourceSwitch is one SOURCE_SWITCHOVER event.
// Continued is false when the task stopped instead of copying the new server.
// Reason is a stable code on that stop. File is the source binlog name, not the disk prefix.
// Resolved is set on a stop the task no longer sits on: it runs again, or a
// later switch continued.
type SourceSwitch struct {
	Time      time.Time `json:"time"`
	Old       string    `json:"old"`
	New       string    `json:"new"`
	File      string    `json:"file,omitempty"`
	Pos       uint32    `json:"pos,omitempty"`
	GTIDSet   string    `json:"gtid_set,omitempty"`
	Continued bool      `json:"continued"`
	Reason    string    `json:"reason,omitempty"`
	Resolved  bool      `json:"resolved,omitempty"`

	// lastSeen is the newest event of a switch checked more than once.
	lastSeen time.Time
}

// SourceChain is the DBA view of which server wrote which files.
// Outcome is continued or stopped for the latest switch, and resumed when the
// latest switch stopped the task but the task no longer sits on that stop. It
// is empty when this task has not switched. Servers is the on-disk chain when
// that file exists, otherwise the identities implied by continued switches.
// A Start that checks the same move again does not add a second switch.
type SourceChain struct {
	Current  string         `json:"current,omitempty"`
	Outcome  string         `json:"outcome,omitempty"`
	Servers  []SourceServer `json:"servers"`
	Switches []SourceSwitch `json:"switches"`
}

// Empty reports that there is no identity and no switch to show.
func (c SourceChain) Empty() bool {
	return len(c.Servers) == 0 && len(c.Switches) == 0
}

type sourceSwitchEventReader interface {
	ListSourceSwitchEvents(ctx context.Context, taskID string) ([]TaskEvent, error)
}

// SourceChain reads the on-disk identity list and the task's SOURCE_SWITCHOVER events.
// A missing chain file is an empty identity list. Events come from a store that
// can list that type, or from the newest sourceSwitchListLimit events.
func (s *Scheduler) SourceChain(taskID string) (SourceChain, error) {
	if s == nil || strings.TrimSpace(taskID) == "" {
		return SourceChain{}, ErrTaskNotFound
	}
	s.mu.Lock()
	dataDir := s.dataDir
	store := s.eventStore
	s.mu.Unlock()

	events, err := s.readSourceSwitchEvents(taskID, store)
	if err != nil {
		return SourceChain{}, err
	}
	var idents []string
	if dir, ok := taskBinlogDir(dataDir, taskID); ok {
		idents = readSourceChainFile(dir)
	}
	chain := AssembleSourceChain(idents, events)
	if task, err := s.GetTask(taskID); err == nil {
		chain = ResolveSourceStop(chain, task)
	}
	return s.resolveStopsRunPast(taskID, chain), nil
}

// resolveStopsRunPast marks each stop resolved when a TASK_RUNNING event
// follows its newest check. The task got past that stop and ran, so it stays
// resolved after a normal Stop or a process restart. A stop checked again
// after that run is newer than the run and stays held.
func (s *Scheduler) resolveStopsRunPast(taskID string, chain SourceChain) SourceChain {
	held := false
	for _, sw := range chain.Switches {
		if !sw.Continued && !sw.Resolved {
			held = true
		}
	}
	if !held {
		return chain
	}
	// Newest events first in the meta store; a run after a stop is recent.
	events, err := s.ListEvents(taskID, resumeEventScanLimit)
	if err != nil {
		return chain
	}
	var lastRun time.Time
	for _, event := range events {
		if event.Type == "TASK_RUNNING" && event.Time.After(lastRun) {
			lastRun = event.Time
		}
	}
	if lastRun.IsZero() {
		return chain
	}
	switches := append([]SourceSwitch(nil), chain.Switches...)
	for i := range switches {
		if switches[i].Continued || switches[i].Resolved {
			continue
		}
		seen := switches[i].lastSeen
		if seen.IsZero() {
			seen = switches[i].Time
		}
		if lastRun.After(seen) {
			switches[i].Resolved = true
		}
	}
	chain.Switches = switches
	if n := len(switches); n > 0 && chain.Outcome == SwitchOutcomeStopped && switches[n-1].Resolved {
		chain.Outcome = SwitchOutcomeResumed
	}
	return chain
}

func markLatestStopResolved(chain SourceChain) SourceChain {
	if chain.Outcome != SwitchOutcomeStopped || len(chain.Switches) == 0 {
		return chain
	}
	switches := append([]SourceSwitch(nil), chain.Switches...)
	switches[len(switches)-1].Resolved = true
	chain.Switches = switches
	chain.Outcome = SwitchOutcomeResumed
	return chain
}

// ResolveSourceStop marks the latest stop resolved when the task no longer
// sits on it: last_error is not SOURCE_SWITCHOVER and the task is RUNNING,
// STARTING or in retry backoff. Outcome becomes resumed. The stop stays in
// Switches. Scheduler.SourceChain also resolves a stop the task ran past
// (a TASK_RUNNING event after it), so a later normal Stop or restart does
// not bring the stop back.
func ResolveSourceStop(chain SourceChain, task Task) SourceChain {
	if chain.Outcome != SwitchOutcomeStopped || len(chain.Switches) == 0 {
		return chain
	}
	if strings.HasPrefix(strings.TrimSpace(task.LastError), CodeSourceSwitchover) {
		return chain
	}
	switch task.State {
	case StateRunning, StateStarting, StateRetryBackoff:
	default:
		return chain
	}
	return markLatestStopResolved(chain)
}

// OutcomeCounts returns how many switches continued, and how many stops the
// task still sits on (at most the latest one). A repeated check is one switch.
func (c SourceChain) OutcomeCounts() (continued, stopped int) {
	for _, sw := range c.Switches {
		if sw.Continued {
			continued++
		} else if !sw.Resolved {
			stopped++
		}
	}
	return continued, stopped
}

func (s *Scheduler) readSourceSwitchEvents(taskID string, store EventStore) ([]TaskEvent, error) {
	if reader, ok := store.(sourceSwitchEventReader); ok {
		ctx, cancel := s.withReadTimeout(context.Background())
		defer cancel()
		return reader.ListSourceSwitchEvents(ctx, taskID)
	}
	events, err := s.ListEvents(taskID, sourceSwitchListLimit)
	if err != nil {
		if err == ErrTaskNotFound {
			return nil, nil
		}
		return nil, err
	}
	return filterSourceSwitchEvents(events), nil
}

func filterSourceSwitchEvents(events []TaskEvent) []TaskEvent {
	out := make([]TaskEvent, 0)
	for _, event := range events {
		if event.Type == "SOURCE_SWITCHOVER" {
			out = append(out, event)
		}
	}
	return out
}

// AssembleSourceChain builds the API view from a chain file and switch events.
// idents is one identity per line, first line first. An empty idents list is
// rebuilt from continued switches: the first old identity, then each new one.
// A stopped switch does not add its new identity.
func AssembleSourceChain(idents []string, events []TaskEvent) SourceChain {
	switches := make([]SourceSwitch, 0)
	for _, event := range events {
		if event.Type != "SOURCE_SWITCHOVER" {
			continue
		}
		sw := parseSourceSwitch(event)
		// Each Start of a stopped task checks the same move again and
		// records it again. That is one switch, kept at its first time.
		if n := len(switches); n > 0 && sameSwitch(switches[n-1], sw) {
			if event.Time.After(switches[n-1].lastSeen) {
				switches[n-1].lastSeen = event.Time
			}
			continue
		}
		sw.lastSeen = event.Time
		switches = append(switches, sw)
	}
	// A stop followed by a later continued switch no longer holds the task.
	for i := range switches {
		if !switches[i].Continued {
			for _, later := range switches[i+1:] {
				if later.Continued {
					switches[i].Resolved = true
					break
				}
			}
		}
	}
	servers := cleanIdents(idents)
	if len(servers) == 0 {
		servers = serversFromSwitches(switches)
	}
	if len(servers) == 0 && len(switches) == 0 {
		return SourceChain{}
	}
	out := SourceChain{
		Servers:  make([]SourceServer, 0, len(servers)),
		Switches: switches,
	}
	prefixes := SourceStintPrefixes(servers)
	for i, id := range servers {
		current := i == len(servers)-1
		out.Servers = append(out.Servers, SourceServer{Identity: id, Prefix: prefixes[i], Current: current})
		if current {
			out.Current = id
		}
	}
	if len(switches) > 0 {
		if switches[len(switches)-1].Continued {
			out.Outcome = SwitchOutcomeContinued
		} else {
			out.Outcome = SwitchOutcomeStopped
		}
	}
	return out
}

// sameSwitch reports one move checked twice: same servers, place, set, and result.
func sameSwitch(a, b SourceSwitch) bool {
	return a.Old == b.Old && a.New == b.New && a.File == b.File && a.Pos == b.Pos &&
		a.GTIDSet == b.GTIDSet && a.Continued == b.Continued && a.Reason == b.Reason
}

// AnnotateFileSources sets source_identity and source_server from the chain.
// A name with a stint prefix belongs to that stint. Every other file belongs
// to the first server. source_server is the 1-based stint number, so a
// failback file names the third stint, not the first server.
// An empty chain leaves both fields empty.
func AnnotateFileSources(files []BinlogFile, chain SourceChain) []BinlogFile {
	if len(files) == 0 || len(chain.Servers) == 0 {
		return files
	}
	idents := make([]string, len(chain.Servers))
	for i, server := range chain.Servers {
		idents[i] = server.Identity
	}
	out := make([]BinlogFile, len(files))
	copy(out, files)
	for i := range out {
		stint := SourceStintOf(stintSourceName(fileSourceName(out[i])), idents)
		if stint < 0 {
			continue
		}
		out[i].SourceIdentity = idents[stint]
		out[i].SourceServer = stint + 1
	}
	return out
}

func stintSourceName(name string) string {
	base := strings.TrimSpace(name)
	if named, ok := binlog.ClassifySegment(base); ok {
		return named.Source
	}
	return base
}

// FileSourceIdentity reports which chain identity wrote this source file name.
// name may be a disk basename, including an .open.e or .sealed.e suffix.
func FileSourceIdentity(name string, idents []string) string {
	idents = cleanIdents(idents)
	if len(idents) == 0 {
		return ""
	}
	return idents[SourceStintOf(stintSourceName(name), idents)]
}

func fileSourceName(file BinlogFile) string {
	name := strings.TrimSpace(file.FilePath)
	if name != "" {
		name = filepath.Base(name)
	} else {
		name = strings.TrimSpace(file.FileName)
	}
	if named, ok := binlog.ClassifySegment(name); ok {
		return named.Source
	}
	return name
}

func readSourceChainFile(dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(dir, sourceChainFileName))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func cleanIdents(idents []string) []string {
	out := make([]string, 0, len(idents))
	for _, id := range idents {
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func serversFromSwitches(switches []SourceSwitch) []string {
	if len(switches) == 0 {
		return nil
	}
	var chain []string
	chain = appendChainIdentity(chain, switches[0].Old)
	for _, sw := range switches {
		if sw.Continued {
			chain = appendChainIdentity(chain, sw.New)
		}
	}
	return chain
}

// appendChainIdentity adds a stint. Only a repeat of the last server is
// dropped: a failback A, B, A is three stints.
func appendChainIdentity(chain []string, id string) []string {
	id = strings.TrimSpace(id)
	if id == "" {
		return chain
	}
	if len(chain) > 0 && chain[len(chain)-1] == id {
		return chain
	}
	return append(chain, id)
}

func parseSourceSwitch(event TaskEvent) SourceSwitch {
	oldID, newID, gtid, file, pos := parseSwitchDetail(event.Detail)
	if oldID == "" || newID == "" {
		msgOld, msgNew := parseSwitchMessageIdentities(event.Message)
		if oldID == "" {
			oldID = msgOld
		}
		if newID == "" {
			newID = msgNew
		}
	}
	continued := switchMessageContinued(event.Message)
	sw := SourceSwitch{
		Time:      event.Time,
		Old:       oldID,
		New:       newID,
		File:      file,
		Pos:       pos,
		GTIDSet:   gtid,
		Continued: continued,
	}
	if !continued {
		sw.Reason = switchMessageReason(event.Message)
	}
	return sw
}

func switchMessageContinued(message string) bool {
	return strings.Contains(message, "continuing from the executed GTID set") ||
		strings.Contains(message, "continuing with the configured start")
}

func switchMessageReason(message string) string {
	switch {
	case strings.Contains(message, "has no GTID set"):
		return SwitchReasonNoGTID
	case strings.Contains(message, "missing transactions"):
		return SwitchReasonMissing
	case strings.Contains(message, "has purged transactions"):
		return SwitchReasonPurged
	case strings.Contains(message, "MariaDB has no usable GTID path"):
		return SwitchReasonMariaDB
	case strings.Contains(message, "GTID state could not be read"):
		return SwitchReasonUnreadable
	default:
		return ""
	}
}

func parseSwitchDetail(detail string) (oldID, newID, gtid, file string, pos uint32) {
	const (
		keyOld  = "old="
		keyNew  = " new="
		keyGTID = " gtid_set="
		keyFile = " file="
		keyPos  = " pos="
	)
	if !strings.HasPrefix(detail, keyOld) {
		return "", "", "", "", 0
	}
	rest := detail[len(keyOld):]
	i := strings.Index(rest, keyNew)
	if i < 0 {
		return "", "", "", "", 0
	}
	oldID = rest[:i]
	rest = rest[i+len(keyNew):]
	i = strings.Index(rest, keyGTID)
	if i < 0 {
		return "", "", "", "", 0
	}
	newID = rest[:i]
	rest = rest[i+len(keyGTID):]
	i = strings.Index(rest, keyFile)
	if i < 0 {
		return "", "", "", "", 0
	}
	gtid = rest[:i]
	rest = rest[i+len(keyFile):]
	i = strings.Index(rest, keyPos)
	if i < 0 {
		return "", "", "", "", 0
	}
	file = rest[:i]
	n, err := strconv.ParseUint(strings.TrimSpace(rest[i+len(keyPos):]), 10, 32)
	if err != nil {
		return "", "", "", "", 0
	}
	return oldID, newID, gtid, file, uint32(n)
}

func parseSwitchMessageIdentities(message string) (oldID, newID string) {
	const prefix = "source switched from "
	if !strings.HasPrefix(message, prefix) {
		return "", ""
	}
	rest := message[len(prefix):]
	const mid = " to "
	i := strings.Index(rest, mid)
	if i < 0 {
		return "", ""
	}
	oldID = rest[:i]
	rest = rest[i+len(mid):]
	for _, sep := range []string{" at ", " before ", ". "} {
		if j := strings.Index(rest, sep); j >= 0 {
			return oldID, rest[:j]
		}
	}
	return oldID, strings.TrimSpace(rest)
}
