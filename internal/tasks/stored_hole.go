// Package tasks provides module-level functionality for tasks.
// input: a MySQL task directory that switched source at least once, its SOURCE_SWITCHOVER events (the gtid_set each continued switch recorded as stored), the task's start gtid_set, and the GTIDs of its local segments
// output: a storage problem when a switch recorded transactions as stored that no local segment holds, between transactions the segments do hold, or right after the task's start set while the task's first file is still on disk (a stored GTID hole), naming the first segment after the hole so /replay and /window stop before it; per-UUID GTID interval arithmetic shared with the window's interior-hole check
// pos: safety net for a failback chain whose segment was lost (a hole is not reported while a sealed UPLOADED catalog row before the cut is only in object storage: replay reads it from there) (an earlier build's first-stint file kept only in object storage, or a file deleted by hand); read-only, Start is not refused by it
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/binlog"
)

const storedHoleDetailPrefix = "stored gtid hole:"

// gtidRanges is a GTID set as closed intervals per source key (uuid, or
// uuid:tag), sorted and merged.
type gtidRanges map[string][][2]int64

func (r gtidRanges) add(key string, lo, hi int64) {
	if key == "" || lo <= 0 || hi < lo {
		return
	}
	r[key] = append(r[key], [2]int64{lo, hi})
}

func (r gtidRanges) addEvents(events []binlog.GTIDEventRef) {
	for _, ev := range events {
		if ev.UUID == "" || ev.Seq <= 0 {
			continue
		}
		r.add(strings.ToLower(ev.UUID), ev.Seq, ev.Seq)
	}
}

func (r gtidRanges) addAll(o gtidRanges) {
	for k, list := range o {
		for _, iv := range list {
			r.add(k, iv[0], iv[1])
		}
	}
}

// normalize sorts and merges touching intervals.
func (r gtidRanges) normalize() gtidRanges {
	for k, list := range r {
		sort.Slice(list, func(i, j int) bool { return list[i][0] < list[j][0] })
		merged := make([][2]int64, 0, len(list))
		for _, iv := range list {
			if n := len(merged); n > 0 && iv[0] <= merged[n-1][1]+1 {
				if iv[1] > merged[n-1][1] {
					merged[n-1][1] = iv[1]
				}
				continue
			}
			merged = append(merged, iv)
		}
		r[k] = merged
	}
	return r
}

// minus is r without o. Both must be normalized.
func (r gtidRanges) minus(o gtidRanges) gtidRanges {
	out := gtidRanges{}
	for k, list := range r {
		cut := o[k]
		for _, iv := range list {
			lo, hi := iv[0], iv[1]
			for _, c := range cut {
				if c[1] < lo || c[0] > hi {
					continue
				}
				if c[0] > lo {
					out.add(k, lo, c[0]-1)
				}
				lo = c[1] + 1
				if lo > hi {
					break
				}
			}
			if lo <= hi {
				out.add(k, lo, hi)
			}
		}
	}
	return out.normalize()
}

func (r gtidRanges) has(key string, seq int64) bool {
	for _, iv := range r[key] {
		if seq >= iv[0] && seq <= iv[1] {
			return true
		}
	}
	return false
}

// String is the MySQL text form, keys sorted.
func (r gtidRanges) String() string {
	keys := make([]string, 0, len(r))
	for k, list := range r {
		if len(list) > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		var b strings.Builder
		b.WriteString(k)
		for _, iv := range r[k] {
			b.WriteByte(':')
			b.WriteString(strconv.FormatInt(iv[0], 10))
			if iv[1] != iv[0] {
				b.WriteByte('-')
				b.WriteString(strconv.FormatInt(iv[1], 10))
			}
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, ",")
}

// parseGTIDRanges reads a MySQL gtid_set. Tagged sets (uuid:tag:n) keep the
// tag in the key. An empty text is an empty set.
func parseGTIDRanges(text string) (gtidRanges, error) {
	out := gtidRanges{}
	text = strings.TrimSpace(text)
	if text == "" {
		return out, nil
	}
	set, err := parseGTIDText(text)
	if err != nil {
		return nil, err
	}
	for _, item := range strings.Split(strings.ReplaceAll(set.String(), "\n", ""), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		fields := strings.Split(item, ":")
		key := strings.ToLower(fields[0])
		for _, f := range fields[1:] {
			lo, hi, ok := parseGTIDInterval(f)
			if !ok {
				key = strings.ToLower(fields[0]) + ":" + f
				continue
			}
			out.add(key, lo, hi)
		}
	}
	return out.normalize(), nil
}

func parseGTIDInterval(f string) (int64, int64, bool) {
	lo, hi := f, f
	if i := strings.IndexByte(f, '-'); i > 0 {
		lo, hi = f[:i], f[i+1:]
	}
	a, err1 := strconv.ParseInt(lo, 10, 64)
	b, err2 := strconv.ParseInt(hi, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return a, b, true
}

// interiorGaps lists, per key, the sequences missing between the lowest and
// highest sequence of set. Leading history (before the lowest) is not a gap.
func interiorGaps(set gtidRanges) gtidRanges {
	out := gtidRanges{}
	for k, list := range set {
		for i := 1; i < len(list); i++ {
			out.add(k, list[i-1][1]+1, list[i][0]-1)
		}
	}
	return out.normalize()
}

type holeFile struct {
	name string
	set  gtidRanges
	prev string
}

type holeCacheEntry struct {
	sig     string
	problem binlog.StorageProblem
	found   bool
}

var storedHoleCache sync.Map // task dir -> holeCacheEntry

type segmentRangesEntry struct {
	size  int64
	mtime time.Time
	set   gtidRanges
	prev  string
}

var segmentRangesCache sync.Map // segment path -> segmentRangesEntry

// segmentRanges is the GTID set one local segment stores and its
// previous-GTIDs header. A file is read once per size and mtime.
func segmentRanges(p string) (gtidRanges, string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, "", err
	}
	if v, ok := segmentRangesCache.Load(p); ok {
		e := v.(segmentRangesEntry)
		if e.size == info.Size() && e.mtime.Equal(info.ModTime()) {
			return e.set, e.prev, nil
		}
	}
	log, err := scanSegmentFile(p)
	if err != nil {
		return nil, "", err
	}
	set := gtidRanges{}
	set.addEvents(log.Events)
	set.normalize()
	segmentRangesCache.Store(p, segmentRangesEntry{size: info.Size(), mtime: info.ModTime(), set: set, prev: log.Previous})
	return set, log.Previous, nil
}

// storedHoleFor finds transactions a continued switch recorded as stored that
// no local segment holds, with stored transactions of the same source both
// before and after them (or the start set before them, while the task's
// first file is still on disk). Retention removes the oldest files first,
// so it never makes such a hole. A task that never switched is not read.
func (s *Scheduler) storedHoleFor(task Task) (binlog.StorageProblem, bool) {
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
	dir, ok := taskBinlogDir(dataDir, task.ID)
	if !ok {
		return binlog.StorageProblem{}, false
	}
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
	problem, found := planStoredHole(dir, chainFile, events, StartGTIDText(task), chosen)
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
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// planStoredHole is storedHoleFor on a listing: chosen is the replay order.
func planStoredHole(dir string, chainFile []string, events []TaskEvent, startGTID string, chosen []BinlogFile) (binlog.StorageProblem, bool) {
	chainFile = cleanIdents(chainFile)
	if len(chainFile) == 0 {
		return binlog.StorageProblem{}, false
	}
	stints := deriveStints(chainFile[0], events)
	recorded := gtidRanges{}
	switchTo := map[string]string{} // key -> identity of the first switch that recorded it
	for _, st := range stints[1:] {
		set, err := parseGTIDRanges(st.in)
		if err != nil {
			continue
		}
		recorded.addAll(set)
		for k := range set {
			if _, ok := switchTo[k]; !ok {
				switchTo[k] = st.id
			}
		}
	}
	recorded.normalize()
	if len(recorded) == 0 {
		return binlog.StorageProblem{}, false
	}
	held, err := parseGTIDRanges(startGTID)
	if err != nil {
		held = gtidRanges{}
	}
	parts := make([]holeFile, 0, len(chosen))
	for _, f := range chosen {
		name := segmentInventoryBasename(f)
		if name == "" {
			continue
		}
		set, prev, err := segmentRanges(filepath.Join(dir, name))
		if err != nil {
			// An unreadable file is reported by the window; it is not a hole.
			return binlog.StorageProblem{}, false
		}
		parts = append(parts, holeFile{name: name, set: set, prev: prev})
		held.addAll(set)
	}
	held.normalize()
	// The start set anchors a hole from below only while the task's first
	// file is still here: an unprefixed first file whose header is inside
	// the start set. After retention the first local file starts later, and
	// the transactions before it still restore from object storage.
	start := gtidRanges{}
	if len(parts) > 0 {
		if named, ok := binlog.ClassifySegment(parts[0].name); ok && SourceStintOf(named.Source, cleanIdents(chainFile)) == 0 {
			seed, seedErr := parseGTIDRanges(startGTID)
			header, headerErr := parseGTIDRanges(parts[0].prev)
			if seedErr == nil && headerErr == nil && len(seed) > 0 && len(header.minus(seed)) == 0 {
				start = seed
			}
		}
	}
	missing := recorded.minus(held)
	cut := -1
	lost := gtidRanges{}
	for k, list := range missing {
		for _, iv := range list {
			below := -2 // index of the last local file holding k below iv; -1: only the start set
			above := false
			for _, r := range start[k] {
				if r[0] < iv[0] {
					below = -1
				}
			}
			for i, p := range parts {
				for _, r := range p.set[k] {
					if r[0] < iv[0] {
						below = i
					}
					if r[1] > iv[1] {
						above = true
					}
				}
			}
			if below < -1 || !above {
				continue
			}
			lost.add(k, iv[0], iv[1])
			next := below + 1
			if below == -1 {
				// Only the start set is below: cut at the first file that
				// is after the hole (holds a later sequence, or a header
				// that already lists part of it).
				next = len(parts)
				one := gtidRanges{}
				one.add(k, iv[0], iv[1])
				for i, p := range parts {
					later := false
					for _, r := range p.set[k] {
						if r[1] > iv[1] {
							later = true
						}
					}
					if header, err := parseGTIDRanges(p.prev); err == nil && !rangesEqual(one.minus(header), one) {
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
	lost.normalize()
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

func rangesEqual(a, b gtidRanges) bool {
	return a.String() == b.String()
}
