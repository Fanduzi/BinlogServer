// Package tasks provides module-level functionality for tasks.
// input: a task directory, its .source-chain, and its SOURCE_SWITCHOVER events (a builds before the failback fix wrote a failback A, B, A without a third chain line and with A's first-stint names)
// output: a plan that moves each segment of a later stint back to that stint's name ({identity}~{n}.{binlog}), proven by the GTID sets recorded at each switch; or a refusal naming the files when the order cannot be proven; the storage alert and replay cutoff for both; and the repair itself (a journal, the renames, the catalog rows, the checkpoint file name, the chain) run before the runner starts
// pos: upgrade path for task directories written by v0.5.57 / 0a84917a after a real failback
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/binlog"

	"github.com/go-mysql-org/go-mysql/mysql"
)

const legacyRepairJournal = ".legacy-failback-repair"

// legacyMove renames one segment onto the stint that wrote it.
type legacyMove struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Stint int    `json:"stint"`
}

// legacyFailback is what a task directory from an earlier build needs.
// Moves is empty and Problem is empty on a healthy directory.
type legacyFailback struct {
	// Chain is one identity per stint, from the continued switches.
	Chain []string     `json:"chain"`
	Moves []legacyMove `json:"moves"`
	// Shared lists the earlier stint's segments that share a source name
	// with a moved one. The old build pointed their catalog row at the
	// later file, so the repair writes their row again.
	Shared []string `json:"shared,omitempty"`
	// Problem is set when the order cannot be proven. Start refuses.
	Problem string `json:"problem,omitempty"`
	// Affected lists the files involved, in replay order.
	Affected []string `json:"affected,omitempty"`
	// Segment is the earliest affected file. Replay stops before it.
	Segment string `json:"segment,omitempty"`
	// Missing lists the earlier stint's segments whose name a later stint
	// took, that are not on disk. The old build pointed their catalog row at
	// the later file, so only object storage may still hold them. Start
	// fetches each back before the repair, or refuses.
	Missing []legacyMissing `json:"missing,omitempty"`
}

// legacyMissing is one earlier-stint segment that is not on disk.
// In and Out are the GTID sets stored when its stint began and when it left:
// the fetched file must hold none of In and only transactions in Out.
type legacyMissing struct {
	Name  string `json:"name"`
	Stint int    `json:"stint"`
	In    string `json:"in,omitempty"`
	Out   string `json:"out,omitempty"`
}

func (l legacyFailback) found() bool {
	return len(l.Moves) > 0 || l.Problem != "" || len(l.Missing) > 0
}

// derivedStint is one stint rebuilt from the switch events.
// in is the GTID set stored when the stint began; leave is the source binlog
// name the next switch left it at.
type derivedStint struct {
	id    string
	in    string
	leave string
}

// deriveStints rebuilds the stints from continued switches. A continued
// switch whose old identity is not the current stint did not move the task:
// an earlier build recorded it again on restart, or this build recorded it
// after reading a chain file that lacked the failback line. It is skipped.
func deriveStints(first string, events []TaskEvent) []derivedStint {
	first = strings.TrimSpace(first)
	if first == "" {
		return nil
	}
	ordered := append([]TaskEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].Time.Equal(ordered[j].Time) {
			return ordered[i].Time.Before(ordered[j].Time)
		}
		return ordered[i].Sequence < ordered[j].Sequence
	})
	stints := []derivedStint{{id: first}}
	for _, event := range ordered {
		if event.Type != "SOURCE_SWITCHOVER" {
			continue
		}
		sw := parseSourceSwitch(event)
		if !sw.Continued {
			continue
		}
		cur := stints[len(stints)-1].id
		if sw.Old != cur || sw.New == "" || sw.New == cur {
			continue
		}
		stints[len(stints)-1].leave = sw.File
		stints = append(stints, derivedStint{id: sw.New, in: strings.TrimSpace(sw.GTIDSet)})
	}
	return stints
}

type legacySegment struct {
	name   string
	named  binlog.SegmentName
	stint  int
	txns   []binlog.GTIDEventRef
	server string
	suffix string
}

// planLegacyFailback checks a MySQL task directory against the stints its
// switch events prove. chainFile is the directory's .source-chain. Only a
// chain that returns to an earlier server, with a chain file that is the
// start of that chain, is checked. Only the segments of a stint whose server
// comes back later, from the file it left at onward, are read.
func planLegacyFailback(dir string, chainFile []string, events []TaskEvent) legacyFailback {
	chainFile = cleanIdents(chainFile)
	if len(chainFile) == 0 {
		return legacyFailback{}
	}
	stints := deriveStints(chainFile[0], events)
	if len(stints) < 3 || len(chainFile) > len(stints) {
		return legacyFailback{}
	}
	ids := make([]string, len(stints))
	for i, st := range stints {
		ids[i] = st.id
	}
	for i, id := range chainFile {
		if ids[i] != id {
			return legacyFailback{}
		}
	}
	later := make(map[int]bool)
	for k := range ids {
		for j := k + 1; j < len(ids); j++ {
			if ids[j] == ids[k] {
				later[k] = true
				break
			}
		}
	}
	if len(later) == 0 {
		return legacyFailback{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return legacyFailback{}
	}
	prefixes := SourceStintPrefixes(ids)
	var all []legacySegment
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		named, ok := binlog.ClassifySegment(entry.Name())
		if !ok {
			continue
		}
		stint := SourceStintOf(named.Source, ids)
		server := named.Source
		if p := prefixes[stint]; p != "" {
			server = strings.TrimPrefix(named.Source, p+".")
		}
		all = append(all, legacySegment{
			name:   entry.Name(),
			named:  named,
			stint:  stint,
			server: server,
			suffix: strings.TrimPrefix(entry.Name(), named.Source),
		})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].named.Seq != all[j].named.Seq {
			return all[i].named.Seq < all[j].named.Seq
		}
		if all[i].named.Epoch != all[j].named.Epoch {
			return all[i].named.Epoch < all[j].named.Epoch
		}
		return all[i].name < all[j].name
	})
	out := legacyFailback{Chain: ids}
	var problems []string
	// Read only what can be misnamed.
	var checked []*legacySegment
	for i := range all {
		seg := &all[i]
		if !later[seg.stint] {
			continue
		}
		if leave, ok := binlog.ClassifySegment(strings.TrimSpace(stints[seg.stint].leave)); ok {
			if base, ok := binlog.ClassifySegment(seg.server); ok && base.Seq < leave.Seq {
				continue
			}
		}
		log, err := scanSegmentFile(filepath.Join(dir, seg.name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s cannot be read (%v)", seg.name, err))
			out.Affected = append(out.Affected, seg.name)
			continue
		}
		seg.txns = log.Events
		checked = append(checked, seg)
	}
	ins := make([]mysql.GTIDSet, len(stints))
	for k, st := range stints {
		set, err := parseGTIDText(st.in)
		if err != nil {
			// Nothing can be proven. Replay stops before the first file
			// that could be misnamed: the earliest file read above.
			bad := legacyFailback{Chain: ids, Problem: fmt.Sprintf("the switch to %s recorded gtid_set %q, which cannot be parsed", st.id, st.in)}
			for _, seg := range checked {
				bad.Affected = append(bad.Affected, seg.name)
			}
			bad.Affected = uniqueStrings(append(bad.Affected, out.Affected...))
			sort.SliceStable(bad.Affected, func(i, j int) bool { return segmentNameLess(bad.Affected[i], bad.Affected[j]) })
			if len(bad.Affected) > 0 {
				bad.Segment = bad.Affected[0]
			} else if len(all) > 0 {
				bad.Segment = all[0].name
				bad.Affected = []string{all[0].name}
			}
			return bad
		}
		ins[k] = set
	}
	fits := func(txns []binlog.GTIDEventRef, k int) bool {
		if k > 0 {
			if disjoint, err := eventsDisjoint(ins[k], txns); err != nil || !disjoint {
				return false
			}
		}
		if k < len(stints)-1 {
			if contained, err := eventsContained(ins[k+1], txns); err != nil || !contained {
				return false
			}
		}
		return true
	}
	dest := make(map[string]int) // name -> target stint, for moved files with transactions
	for _, seg := range checked {
		if len(seg.txns) == 0 || fits(seg.txns, seg.stint) {
			continue
		}
		var candidates []int
		for j := range ids {
			if j != seg.stint && ids[j] == ids[seg.stint] && fits(seg.txns, j) {
				candidates = append(candidates, j)
			}
		}
		out.Affected = append(out.Affected, seg.name)
		switch len(candidates) {
		case 1:
			dest[seg.name] = candidates[0]
		case 0:
			problems = append(problems, fmt.Sprintf("%s holds transactions that no stint of %s stored", seg.name, ids[seg.stint]))
		default:
			problems = append(problems, fmt.Sprintf("%s fits more than one stint of %s", seg.name, ids[seg.stint]))
		}
	}
	// A segment without transactions follows the newest segment of its name
	// with transactions at or before its epoch.
	for _, seg := range checked {
		if len(seg.txns) != 0 {
			continue
		}
		owner := ""
		ownerEpoch := int64(-2)
		for _, other := range checked {
			if other.named.Source != seg.named.Source || len(other.txns) == 0 || other.named.Epoch > seg.named.Epoch {
				continue
			}
			if other.named.Epoch > ownerEpoch || (other.named.Epoch == ownerEpoch && !other.named.Open) {
				owner, ownerEpoch = other.name, other.named.Epoch
			}
		}
		if j, moved := dest[owner]; moved {
			dest[seg.name] = j
			out.Affected = append(out.Affected, seg.name)
		}
	}
	movedSources := make(map[string]bool)
	for _, seg := range checked {
		j, moved := dest[seg.name]
		if !moved {
			continue
		}
		to := prefixes[j] + "." + seg.server + seg.suffix
		if prefixes[j] == "" {
			to = seg.server + seg.suffix
		}
		if _, err := os.Lstat(filepath.Join(dir, to)); err == nil {
			problems = append(problems, fmt.Sprintf("%s belongs to stint %d but %s already exists", seg.name, j+1, to))
			continue
		}
		out.Moves = append(out.Moves, legacyMove{From: seg.name, To: to, Stint: j})
		movedSources[seg.named.Source] = true
	}
	for _, seg := range all {
		if _, moved := dest[seg.name]; moved || !movedSources[seg.named.Source] {
			continue
		}
		out.Shared = append(out.Shared, seg.name)
		out.Affected = append(out.Affected, seg.name)
	}
	// The earlier stint's file at the name a later stint took. The old
	// build pointed its catalog row at the later file; if it is not on disk
	// either, nothing in this task lists it any more.
	for k := range ids {
		if !later[k] || k >= len(stints)-1 {
			continue
		}
		leave, ok := binlog.ClassifySegment(strings.TrimSpace(stints[k].leave))
		if !ok {
			continue
		}
		name := leave.Source
		if prefixes[k] != "" {
			name = prefixes[k] + "." + leave.Source
		}
		if !movedSources[name] {
			continue
		}
		present := false
		for _, seg := range all {
			if _, moved := dest[seg.name]; !moved && seg.named.Source == name {
				present = true
				break
			}
		}
		if !present {
			out.Missing = append(out.Missing, legacyMissing{Name: name, Stint: k, In: stints[k].in, Out: stints[k+1].in})
		}
	}
	if len(problems) > 0 {
		out.Problem = strings.Join(problems, "; ")
	}
	if !out.found() {
		return legacyFailback{}
	}
	order := make(map[string]int, len(all))
	for i, seg := range all {
		order[seg.name] = i
	}
	out.Affected = uniqueStrings(out.Affected)
	sort.SliceStable(out.Affected, func(i, j int) bool { return order[out.Affected[i]] < order[out.Affected[j]] })
	if len(out.Affected) > 0 {
		out.Segment = out.Affected[0]
	}
	// A missing file sorts before the files that took its name; the cut
	// stays on a file that is on disk.
	if len(out.Missing) > 0 {
		names := make([]string, 0, len(out.Missing))
		for _, m := range out.Missing {
			names = append(names, m.Name)
		}
		out.Affected = append(names, out.Affected...)
	}
	return out
}

// segmentNameLess orders disk names by binlog index, then epoch, then name.
func segmentNameLess(a, b string) bool {
	na, okA := binlog.ClassifySegment(a)
	nb, okB := binlog.ClassifySegment(b)
	if okA && okB {
		if na.Seq != nb.Seq {
			return na.Seq < nb.Seq
		}
		if na.Epoch != nb.Epoch {
			return na.Epoch < nb.Epoch
		}
	}
	return a < b
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func scanSegmentFile(p string) (binlog.SegmentGTIDLog, error) {
	f, err := os.Open(p)
	if err != nil {
		return binlog.SegmentGTIDLog{}, err
	}
	defer f.Close()
	return binlog.ScanSegmentGTIDs(f)
}

// eventsDisjoint reports that set holds none of the events' GTIDs.
func eventsDisjoint(set mysql.GTIDSet, events []binlog.GTIDEventRef) (bool, error) {
	if set == nil || set.String() == "" {
		return true, nil
	}
	for _, ev := range events {
		if ev.UUID == "" || ev.Seq <= 0 {
			continue
		}
		one, err := parseGTIDText(fmt.Sprintf("%s:%d", ev.UUID, ev.Seq))
		if err != nil {
			return false, err
		}
		if set.Contain(one) {
			return false, nil
		}
	}
	return true, nil
}

// legacyMessage is the operator text for a found plan.
func legacyMessage(l legacyFailback) string {
	chain := strings.Join(l.Chain, " → ")
	if l.Problem != "" {
		return fmt.Sprintf("This task directory was written by an earlier build across the failback %s, and the files cannot be put back in order automatically: %s. Replay stops before %s. Do not restore past that point from this task; see troubleshooting \"Task directory from an earlier build after a failback\".", chain, l.Problem, l.Segment)
	}
	moves := make([]string, 0, len(l.Moves))
	for _, mv := range l.Moves {
		moves = append(moves, mv.From+" → "+mv.To)
	}
	msg := fmt.Sprintf("This task directory was written by an earlier build across the failback %s: a later stint reused the first stint's file names (%s). Start renames them and restores the catalog rows; no file is deleted. Until then replay stops before %s.", chain, strings.Join(moves, ", "), l.Segment)
	if len(l.Shared) > 0 {
		msg += " The catalog had lost " + strings.Join(l.Shared, ", ") + "; Start lists it again."
	}
	for _, m := range l.Missing {
		msg += fmt.Sprintf(" The earlier stint's %s is not on disk and its catalog row was taken by the later file: Start fetches it back from object storage (the %s directory) and checks it holds that stint's transactions before it repairs anything, and refuses if it cannot.", m.Name, l.Chain[m.Stint])
	}
	return msg
}

func legacyProblem(l legacyFailback) binlog.StorageProblem {
	detail := "legacy failback: " + strings.Join(l.Affected, ", ")
	if len(l.Missing) > 0 {
		names := make([]string, 0, len(l.Missing))
		for _, m := range l.Missing {
			names = append(names, m.Name)
		}
		detail += " (not on disk: " + strings.Join(names, ", ") + ")"
	}
	if l.Problem != "" {
		detail += " (cannot repair: " + l.Problem + ")"
	} else {
		detail += " (Start repairs)"
	}
	return binlog.StorageProblem{Message: legacyMessage(l), Segment: l.Segment, Detail: detail}
}

type legacyCacheEntry struct {
	sig    string
	result legacyFailback
}

var legacyCache sync.Map // task dir -> legacyCacheEntry

// legacyFailbackFor returns the plan for task. The answer is kept until a
// segment is added, renamed, or removed, or the chain file changes, so a
// repeated read does not reread the switch events or the segments.
func (s *Scheduler) legacyFailbackFor(task Task) legacyFailback {
	if s == nil || !strings.EqualFold(strings.TrimSpace(task.Source.Flavor), "mysql") && strings.TrimSpace(task.Source.Flavor) != "" {
		return legacyFailback{}
	}
	s.mu.Lock()
	dataDir := s.dataDir
	store := s.eventStore
	s.mu.Unlock()
	dir, ok := taskBinlogDir(dataDir, task.ID)
	if !ok {
		return legacyFailback{}
	}
	if j, ok := readLegacyJournal(dir); ok {
		return j
	}
	// An earlier build wrote at least A,B before it reused A's names. A
	// task that never switched has one line and costs no event read.
	chainFile := readSourceChainFile(dir)
	if len(chainFile) < 2 {
		return legacyFailback{}
	}
	sig := legacySignature(dir, chainFile)
	if v, ok := legacyCache.Load(dir); ok {
		if entry := v.(legacyCacheEntry); entry.sig == sig {
			return entry.result
		}
	}
	events, err := s.readSourceSwitchEvents(task.ID, store)
	if err != nil {
		return legacyFailback{}
	}
	result := planLegacyFailback(dir, chainFile, events)
	legacyCache.Store(dir, legacyCacheEntry{sig: sig, result: result})
	return result
}

func legacySignature(dir string, chainFile []string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(strings.Join(chainFile, ","))
	for _, entry := range entries {
		if _, ok := binlog.ClassifySegment(entry.Name()); ok {
			b.WriteByte('|')
			b.WriteString(entry.Name())
		}
	}
	return b.String()
}

func readLegacyJournal(dir string) (legacyFailback, bool) {
	body, err := os.ReadFile(filepath.Join(dir, legacyRepairJournal))
	if err != nil {
		return legacyFailback{}, false
	}
	var j legacyFailback
	if err := json.Unmarshal(body, &j); err != nil || len(j.Moves) == 0 {
		return legacyFailback{}, false
	}
	return j, true
}

// legacyCatalog is the file store a repair needs: write, list, and delete one row.
type legacyCatalog interface {
	FileStore
	DeleteBinlogFile(ctx context.Context, taskID, fileName string, epoch int64) error
}

type checkpointWriter interface {
	UpsertCheckpoint(ctx context.Context, taskID string, checkpoint binlog.Checkpoint) error
}

// RepairLegacyFailback runs before a task's runner starts. A healthy
// directory returns nil without a write. A directory from an earlier build
// that can be proven is repaired: the plan is written to a journal first, so
// a crash finishes the same repair on the next Start. A directory that
// cannot be proven returns a permanent STORAGE_INCONSISTENT error.
func (s *Scheduler) RepairLegacyFailback(ctx context.Context, task Task) error {
	plan := s.legacyFailbackFor(task)
	if !plan.found() {
		return nil
	}
	if plan.Problem != "" {
		return NewPermanentError(CodeStorageInconsistent, legacyMessage(plan))
	}
	s.mu.Lock()
	dataDir := s.dataDir
	fileStore := s.fileStore
	cpReader := s.checkpointReader
	s.mu.Unlock()
	dir, _ := taskBinlogDir(dataDir, task.ID)
	var catalog legacyCatalog
	if fileStore != nil {
		c, ok := fileStore.(legacyCatalog)
		if !ok {
			return NewPermanentError(CodeStorageInconsistent, legacyMessage(plan)+" This build's file catalog cannot delete a row, so the repair did not start.")
		}
		catalog = c
	}
	var cpWriter checkpointWriter
	if cpReader != nil {
		w, ok := cpReader.(checkpointWriter)
		if !ok {
			return NewPermanentError(CodeStorageInconsistent, legacyMessage(plan)+" This build's checkpoint store cannot be written, so the repair did not start.")
		}
		cpWriter = w
	}
	if len(plan.Missing) > 0 {
		if err := s.fetchLegacyMissing(ctx, task.ID, dir, plan); err != nil {
			return NewPermanentError(CodeStorageInconsistent, legacyMessage(plan)+" "+err.Error()+" Nothing was changed. Put the file back by hand (troubleshooting \"Task directory from an earlier build after a failback\") and Start again.")
		}
		legacyCache.Delete(dir)
		plan = s.legacyFailbackFor(task)
		if plan.Problem != "" {
			return NewPermanentError(CodeStorageInconsistent, legacyMessage(plan))
		}
		if len(plan.Missing) > 0 {
			return NewPermanentError(CodeStorageInconsistent, legacyMessage(plan)+" The fetched file did not settle the plan; nothing was renamed.")
		}
		if !plan.found() {
			return nil
		}
	}
	if err := writeLegacyJournal(dir, plan); err != nil {
		return err
	}
	orphans := legacyOrphanKeys(ctx, task.ID, plan, catalog)
	if err := applyLegacyRepair(ctx, task.ID, dir, plan, catalog, cpReader, cpWriter); err != nil {
		return NewPermanentError(CodeStorageInconsistent, fmt.Sprintf("repair of the earlier build's failback files stopped: %v. Start again to finish it; the plan is kept in %s.", err, legacyRepairJournal))
	}
	legacyCache.Delete(dir)
	s.mu.Lock()
	detail := legacyRepairDetail(plan)
	if len(orphans) > 0 {
		detail += " orphaned_objects=" + strings.Join(orphans, ";")
	}
	s.appendEventLocked(task.ID, "STORAGE_REPAIRED", "renamed the earlier build's failback files to their stint names", detail)
	s.flushPendingEventsLocked()
	s.mu.Unlock()
	return nil
}

func legacyRepairDetail(plan legacyFailback) string {
	parts := make([]string, 0, len(plan.Moves)+1)
	for _, mv := range plan.Moves {
		parts = append(parts, mv.From+" -> "+mv.To)
	}
	return "chain=" + strings.Join(plan.Chain, ",") + " moves=" + strings.Join(parts, ";")
}

func writeLegacyJournal(dir string, plan legacyFailback) error {
	body, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, legacyRepairJournal+".tmp")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, legacyRepairJournal)); err != nil {
		return err
	}
	return syncDir(dir)
}

// applyLegacyRepair carries out a journal. Each step checks what is already
// done, so running it again after a crash is safe. No file is deleted.
func applyLegacyRepair(ctx context.Context, taskID, dir string, plan legacyFailback, catalog legacyCatalog, cpReader CheckpointReader, cpWriter checkpointWriter) error {
	// 1. Renames. A move whose source is gone and whose target is there is done.
	for _, mv := range plan.Moves {
		if filepath.Base(mv.From) != mv.From || filepath.Base(mv.To) != mv.To {
			return fmt.Errorf("repair plan names a path outside the task directory: %s -> %s", mv.From, mv.To)
		}
		from := filepath.Join(dir, mv.From)
		to := filepath.Join(dir, mv.To)
		_, fromErr := os.Lstat(from)
		_, toErr := os.Lstat(to)
		switch {
		case fromErr == nil && errors.Is(toErr, os.ErrNotExist):
			if err := os.Rename(from, to); err != nil {
				return err
			}
		case errors.Is(fromErr, os.ErrNotExist) && toErr == nil:
		case fromErr == nil && toErr == nil:
			return fmt.Errorf("both %s and %s exist", mv.From, mv.To)
		default:
			return fmt.Errorf("neither %s nor %s is on disk", mv.From, mv.To)
		}
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	// 2. Catalog rows.
	if catalog != nil {
		rows, err := catalog.ListBinlogFiles(ctx, taskID, segmentInventoryLimit)
		if err != nil {
			return err
		}
		byKey := make(map[string]BinlogFile, len(rows))
		for _, row := range rows {
			byKey[catalogKey(row.FileName, row.Epoch)] = row
		}
		for _, mv := range plan.Moves {
			fromNamed, _ := binlog.ClassifySegment(mv.From)
			toNamed, _ := binlog.ClassifySegment(mv.To)
			epoch := rowEpoch(fromNamed)
			row, ok := byKey[catalogKey(fromNamed.Source, epoch)]
			fresh := diskCatalogRow(taskID, dir, mv.To)
			oldKey := ""
			if ok {
				fresh.CreatedAt = row.CreatedAt
				if row.StartPos != 0 {
					fresh.StartPos = row.StartPos
				}
				oldKey = row.ObjectKey
			}
			fresh.ObjectKey = legacyObjectKey(rows, oldKey, plan.Chain[mv.Stint], mv.To)
			markForUpload(&fresh, "renamed from "+mv.From+" by the legacy failback repair")
			if err := catalog.UpsertBinlogFile(ctx, fresh); err != nil {
				return err
			}
			if ok && fromNamed.Source != toNamed.Source {
				if err := catalog.DeleteBinlogFile(ctx, taskID, fromNamed.Source, epoch); err != nil {
					return err
				}
			}
		}
		// The earlier stint's segment that shared a name: its row was
		// pointed at the later file. Write it again from disk.
		for _, name := range plan.Shared {
			named, ok := binlog.ClassifySegment(name)
			if !ok {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				continue
			}
			row, have := byKey[catalogKey(named.Source, rowEpoch(named))]
			if have && filepath.Base(row.FilePath) == name {
				continue
			}
			fresh := diskCatalogRow(taskID, dir, name)
			fresh.ObjectKey = legacyObjectKey(rows, "", plan.Chain[SourceStintOf(named.Source, plan.Chain)], name)
			// The old build may have uploaded the later file to this key.
			markForUpload(&fresh, "listed again by the legacy failback repair")
			if err := catalog.UpsertBinlogFile(ctx, fresh); err != nil {
				return err
			}
		}
	}
	// 3. The checkpoint names the disk file of the open stint.
	if cpReader != nil && cpWriter != nil {
		cp, found, err := cpReader.LoadCheckpoint(ctx, taskID)
		if err != nil {
			return err
		}
		if found {
			for _, mv := range plan.Moves {
				fromNamed, _ := binlog.ClassifySegment(mv.From)
				toNamed, _ := binlog.ClassifySegment(mv.To)
				if strings.TrimSpace(cp.File) == fromNamed.Source && fromNamed.Source != toNamed.Source && movedIsLatest(plan, mv) {
					cp.File = toNamed.Source
					cp.UpdatedAt = time.Now()
					if err := cpWriter.UpsertCheckpoint(ctx, taskID, cp); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	// 4. The chain, then the journal goes.
	if err := writeChainFile(dir, plan.Chain); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, legacyRepairJournal)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}

// movedIsLatest is true when mv moves to the last stint: the checkpoint can
// only name the stint the task was copying.
func movedIsLatest(plan legacyFailback, mv legacyMove) bool {
	return mv.Stint == len(plan.Chain)-1
}

func catalogKey(name string, epoch int64) string {
	return fmt.Sprintf("%s#%d", strings.TrimSpace(name), epoch)
}

func rowEpoch(named binlog.SegmentName) int64 {
	if named.Epoch < 0 {
		return 0
	}
	return named.Epoch
}

func diskCatalogRow(taskID, dir, name string) BinlogFile {
	named, _ := binlog.ClassifySegment(name)
	p := filepath.Join(dir, name)
	row := BinlogFile{
		TaskID:      taskID,
		FileName:    named.Source,
		FilePath:    p,
		Epoch:       rowEpoch(named),
		State:       "SEALED",
		UploadState: "LOCAL_ONLY",
	}
	if named.Open {
		row.State = "OPEN"
	}
	if info, err := os.Stat(p); err == nil {
		row.SizeBytes = info.Size()
		row.CreatedAt = info.ModTime().UTC()
		if !named.Open {
			row.SealedAt = row.CreatedAt
		}
	}
	if start, end, ok := binlog.SegmentPositions(p); ok {
		row.StartPos = FilePos(start)
		row.EndPos = FilePos(end)
	}
	return row
}

// markForUpload makes a sealed row with an object key upload again under
// that key and verify it. An open row uploads when it seals.
func markForUpload(row *BinlogFile, why string) {
	if row.State != "SEALED" || strings.TrimSpace(row.ObjectKey) == "" {
		row.UploadState = "LOCAL_ONLY"
		row.ObjectKey = ""
		return
	}
	row.UploadState = "UPLOAD_FAILED"
	row.UploadError = why
	row.Checksum = ""
	row.UploadedAt = time.Time{}
}

// legacyObjectKey is the key this build gives a sealed segment:
// {prefix}/{cluster}/{identity of the writer}/{disk basename}. The directory
// comes from another row of the same server, else from any row's key with
// the identity replaced. An old build may have filed the later stint under
// the wrong server, so the old key is used only for its prefix.
func legacyObjectKey(rows []BinlogFile, oldKey, identity, base string) string {
	for _, row := range rows {
		key := strings.TrimSpace(row.ObjectKey)
		if key == "" {
			continue
		}
		if dir := path.Dir(key); path.Base(dir) == identity {
			return path.Join(dir, base)
		}
	}
	if key := strings.TrimSpace(oldKey); key != "" {
		return path.Join(path.Dir(path.Dir(key)), identity, base)
	}
	for _, row := range rows {
		if key := strings.TrimSpace(row.ObjectKey); key != "" {
			return path.Join(path.Dir(path.Dir(key)), identity, base)
		}
	}
	return ""
}

func writeChainFile(dir string, chain []string) error {
	tmp := filepath.Join(dir, sourceChainFileName+".tmp")
	if err := os.WriteFile(tmp, []byte(strings.Join(chain, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, sourceChainFileName))
}

func syncFile(p string) error {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	_ = f.Sync()
	return nil
}

// legacyOrphanKeys lists object keys an earlier build may have written for
// the moved files that no catalog row names after the repair: the moved
// row's old key, and the key the old build gives the file under the server
// it was written on. They are listed in STORAGE_REPAIRED, not deleted.
func legacyOrphanKeys(ctx context.Context, taskID string, plan legacyFailback, catalog legacyCatalog) []string {
	if catalog == nil {
		return nil
	}
	rows, err := catalog.ListBinlogFiles(ctx, taskID, segmentInventoryLimit)
	if err != nil {
		return nil
	}
	byKey := make(map[string]BinlogFile, len(rows))
	for _, row := range rows {
		byKey[catalogKey(row.FileName, row.Epoch)] = row
	}
	movedRows := make(map[string]bool, len(plan.Moves))
	for _, mv := range plan.Moves {
		fromNamed, _ := binlog.ClassifySegment(mv.From)
		movedRows[catalogKey(fromNamed.Source, rowEpoch(fromNamed))] = true
	}
	keep := make(map[string]bool)
	for _, row := range rows {
		if k := strings.TrimSpace(row.ObjectKey); k != "" && !movedRows[catalogKey(row.FileName, row.Epoch)] {
			keep[k] = true
		}
	}
	var out []string
	add := func(k string) {
		k = strings.TrimSpace(k)
		if k == "" || keep[k] {
			return
		}
		for _, have := range out {
			if have == k {
				return
			}
		}
		out = append(out, k)
	}
	for _, mv := range plan.Moves {
		fromNamed, _ := binlog.ClassifySegment(mv.From)
		newKey := legacyObjectKey(rows, "", plan.Chain[mv.Stint], mv.To)
		keep[newKey] = true
		if row, ok := byKey[catalogKey(fromNamed.Source, rowEpoch(fromNamed))]; ok && row.ObjectKey != newKey {
			add(row.ObjectKey)
		}
		if fromNamed.Open {
			continue
		}
		add(legacyObjectKey(rows, "", plan.Chain[mv.Stint], mv.From))
	}
	return out
}

// fetchLegacyMissing copies each missing earlier-stint segment back from
// object storage into the task directory, after checking it holds only that
// stint's transactions. Nothing is written when a check fails.
func (s *Scheduler) fetchLegacyMissing(ctx context.Context, taskID, dir string, plan legacyFailback) error {
	s.mu.Lock()
	uploader := s.fileUploader
	fileStore := s.fileStore
	s.mu.Unlock()
	var rows []BinlogFile
	if fileStore != nil {
		listed, err := fileStore.ListBinlogFiles(ctx, taskID, segmentInventoryLimit)
		if err != nil {
			return fmt.Errorf("the catalog could not be read to find the object for %s (%v).", plan.Missing[0].Name, err)
		}
		rows = listed
	}
	opener, _ := uploader.(objectOpener)
	for _, m := range plan.Missing {
		if filepath.Base(m.Name) != m.Name || m.Stint < 0 || m.Stint >= len(plan.Chain) {
			return fmt.Errorf("the plan names %s, which is not a file in the task directory.", m.Name)
		}
		key := legacyObjectKey(rows, "", plan.Chain[m.Stint], m.Name)
		if opener == nil || key == "" {
			return fmt.Errorf("%s is not on disk and no object storage is configured to fetch it from.", m.Name)
		}
		if err := fetchLegacySegment(ctx, opener, dir, m, key); err != nil {
			return err
		}
	}
	return nil
}

func fetchLegacySegment(ctx context.Context, opener objectOpener, dir string, m legacyMissing, key string) error {
	target := filepath.Join(dir, m.Name)
	if _, err := os.Lstat(target); err == nil {
		return nil
	}
	rc, size, err := opener.OpenObject(ctx, key)
	if err != nil || rc == nil {
		if err == nil {
			err = os.ErrNotExist
		}
		return fmt.Errorf("%s is not on disk and object %s could not be read (%v).", m.Name, key, err)
	}
	defer rc.Close()
	tmp := filepath.Join(dir, ".legacy-fetch-"+m.Name)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, rc)
	syncErr := f.Sync()
	closeErr := f.Close()
	fail := func(e error) error {
		_ = os.Remove(tmp)
		return e
	}
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return fail(fmt.Errorf("%s could not be copied from object %s (%v).", m.Name, key, errors.Join(copyErr, syncErr, closeErr)))
	}
	if size >= 0 && n != size {
		return fail(fmt.Errorf("object %s was cut short (%d of %d bytes).", key, n, size))
	}
	log, err := scanSegmentFile(tmp)
	if err != nil {
		return fail(fmt.Errorf("object %s is not a readable binlog segment (%v).", key, err))
	}
	if !log.HasPrevious {
		return fail(fmt.Errorf("object %s has no binlog header, so it is not %s.", key, m.Name))
	}
	in, err := parseGTIDText(m.In)
	if err != nil {
		return fail(err)
	}
	outSet, err := parseGTIDText(m.Out)
	if err != nil {
		return fail(err)
	}
	if m.Stint > 0 {
		if disjoint, err := eventsDisjoint(in, log.Events); err != nil || !disjoint {
			return fail(fmt.Errorf("object %s holds transactions stored before stint %d began, so it is not that stint's %s.", key, m.Stint+1, m.Name))
		}
	}
	if contained, err := eventsContained(outSet, log.Events); err != nil || !contained {
		return fail(fmt.Errorf("object %s holds transactions stint %d had not stored when it left, so it is not that stint's %s (a later stint may have overwritten it).", key, m.Stint+1, m.Name))
	}
	if _, err := os.Lstat(target); err == nil {
		return fail(nil)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fail(err)
	}
	return syncDir(dir)
}
