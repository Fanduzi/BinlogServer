// Package tasks provides module-level functionality for tasks.
// input: {data_dir}/{task_id}/.source-chain and SOURCE_SWITCHOVER task events
// output: the ordered source identities that own a task's files, which identity is current, and each switch's old, new, file:pos, GTID set, and continued or stopped outcome; file rows gain source_identity when that chain is known
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

	// SwitchOutcomeContinued is a switch that kept this task copying.
	SwitchOutcomeContinued = "continued"
	// SwitchOutcomeStopped is a switch that stopped the task.
	SwitchOutcomeStopped = "stopped"

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

// SourceServer is one identity that owns a run of this task's files.
// The first server owns names with no identity prefix. A later server's files
// are named {identity}.{binlog file}. Current is the last server in the chain.
type SourceServer struct {
	Identity string `json:"identity"`
	Current  bool   `json:"current"`
}

// SourceSwitch is one SOURCE_SWITCHOVER event.
// Continued is false when the task stopped instead of copying the new server.
// Reason is a stable code on that stop. File is the source binlog name, not the disk prefix.
type SourceSwitch struct {
	Time      time.Time `json:"time"`
	Old       string    `json:"old"`
	New       string    `json:"new"`
	File      string    `json:"file,omitempty"`
	Pos       uint32    `json:"pos,omitempty"`
	GTIDSet   string    `json:"gtid_set,omitempty"`
	Continued bool      `json:"continued"`
	Reason    string    `json:"reason,omitempty"`
}

// SourceChain is the DBA view of which server wrote which files.
// Outcome is continued or stopped for the latest switch. It is empty when
// this task has not switched. Servers is the on-disk chain when that file
// exists, otherwise the identities implied by continued switches.
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
	return AssembleSourceChain(idents, events), nil
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
		switches = append(switches, parseSourceSwitch(event))
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
	for i, id := range servers {
		current := i == len(servers)-1
		out.Servers = append(out.Servers, SourceServer{Identity: id, Current: current})
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

// AnnotateFileSources sets source_identity from the chain.
// A name prefixed with a later identity belongs to that server.
// Every other file belongs to the first server. An empty chain leaves the field empty.
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
		out[i].SourceIdentity = FileSourceIdentity(fileSourceName(out[i]), idents)
	}
	return out
}

// FileSourceIdentity reports which chain identity wrote this source file name.
// name may be a disk basename, including an .open.e or .sealed.e suffix.
func FileSourceIdentity(name string, idents []string) string {
	idents = cleanIdents(idents)
	if len(idents) == 0 {
		return ""
	}
	base := strings.TrimSpace(name)
	if named, ok := binlog.ClassifySegment(base); ok {
		base = named.Source
	}
	best := ""
	for _, id := range idents[1:] {
		if strings.HasPrefix(base, id+".") && len(id) > len(best) {
			best = id
		}
	}
	if best != "" {
		return best
	}
	return idents[0]
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

func appendChainIdentity(chain []string, id string) []string {
	id = strings.TrimSpace(id)
	if id == "" {
		return chain
	}
	for _, existing := range chain {
		if existing == id {
			return chain
		}
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
