// Package binlog provides module-level functionality for binlog.
// input: one task segment directory (or, for bytes that are not in one, a single segment reader)
// output: the segment ledger: the segments in binlog order, each segment's GTID ranges and previous-GTIDs header, the stored GTID set and the sealed segments' set, holes inside it, the first damaged segment with its recovery sets, and the resumable open tail with the GTID set a resume continues from (seed, sealed segments and tail, #391)
// pos: the one answer to "what does this task directory durably hold"; the runner's resume, /window, /replay, storage alerts, stored-hole detection, legacy failback repair and observability all ask it (ADR 0006)
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// Ledger is what one task directory durably holds. A Ledger reads the
// directory listing at most once and each segment at most once; build a new
// one (LedgerAt) for each question so the answer reflects the disk now.
// A Ledger is not safe for concurrent use.
type Ledger struct {
	dir      string
	listed   bool
	entries  []os.DirEntry
	segs     []LedgerSegment
	listErr  error
	contents map[string]ledgerScan
}

// LedgerSegment is one segment file in the directory.
type LedgerSegment struct {
	// Name is the disk name; Named is how ClassifySegment reads it.
	Name  string
	Named SegmentName
	Path  string
}

// SegmentContents is what one segment stores.
type SegmentContents struct {
	// Log is every GTID event with its offset and time, and the header.
	Log SegmentGTIDLog
	// GTIDs is the set of those events (the header is not stored there).
	GTIDs GTIDSet
}

type ledgerScan struct {
	c   SegmentContents
	err error
}

// LedgerAt is the ledger of dir. Nothing is read until a question is asked.
func LedgerAt(dir string) *Ledger {
	return &Ledger{dir: strings.TrimSpace(dir)}
}

// Dir is the directory the ledger reads.
func (l *Ledger) Dir() string { return l.dir }

func (l *Ledger) list() error {
	if l.listed {
		return l.listErr
	}
	l.listed = true
	if l.dir == "" {
		l.listErr = os.ErrNotExist
		return l.listErr
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		l.listErr = err
		return err
	}
	l.entries = entries
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		named, ok := ClassifySegment(entry.Name())
		if !ok {
			continue
		}
		l.segs = append(l.segs, LedgerSegment{Name: entry.Name(), Named: named, Path: filepath.Join(l.dir, entry.Name())})
	}
	sort.SliceStable(l.segs, func(i, j int) bool {
		a, b := l.segs[i], l.segs[j]
		if a.Named.Seq != b.Named.Seq {
			return a.Named.Seq < b.Named.Seq
		}
		if a.Named.Epoch != b.Named.Epoch {
			return a.Named.Epoch < b.Named.Epoch
		}
		return a.Name < b.Name
	})
	return nil
}

// Segments lists every segment file in binlog order: source index, then
// epoch, then name. An unreadable directory is an error.
func (l *Ledger) Segments() ([]LedgerSegment, error) {
	if err := l.list(); err != nil {
		return nil, err
	}
	return l.segs, nil
}

// Contents is what the segment called name stores. The file does not have
// to be in the listing (a catalog row may name it). A file that cannot be
// opened or whose previous-GTIDs header does not decode is an error; a torn
// tail keeps the events before it.
func (l *Ledger) Contents(name string) (SegmentContents, error) {
	if s, ok := l.contents[name]; ok {
		return s.c, s.err
	}
	c, err := segmentContentsAt(filepath.Join(l.dir, name))
	if l.contents == nil {
		l.contents = map[string]ledgerScan{}
	}
	l.contents[name] = ledgerScan{c: c, err: err}
	return c, err
}

// Stored is every transaction a segment in the directory holds. A segment
// that cannot be read adds nothing. Previous-GTIDs headers are history, not
// stored transactions.
func (l *Ledger) Stored() GTIDSet {
	out := GTIDSet{}
	segs, _ := l.Segments()
	for _, seg := range segs {
		if c, err := l.Contents(seg.Name); err == nil {
			out.AddAll(c.GTIDs)
		}
	}
	return out
}

// SealedStored is every transaction a sealed segment holds. The open tail
// is left out: its trailing transaction may be unfinished, and Tail reads
// only its complete ones. A resume unions this with the tail so a checkpoint
// row that lags the segments (for example after a source switchover sealed
// the old source's file) does not make the source send stored GTIDs again
// (ADR 0005, #391).
func (l *Ledger) SealedStored() GTIDSet {
	out := GTIDSet{}
	segs, _ := l.Segments()
	for _, seg := range segs {
		if seg.Named.Open {
			continue
		}
		if c, err := l.Contents(seg.Name); err == nil {
			out.AddAll(c.GTIDs)
		}
	}
	return out
}

// Holes is every sequence missing between the lowest and highest stored
// sequence of one source that start does not cover.
func (l *Ledger) Holes(start GTIDSet) GTIDSet {
	return StoredHoles(l.Stored(), start)
}

// StoredHoles is Holes for a stored set gathered elsewhere (for example
// from segments read out of object storage).
func StoredHoles(stored, start GTIDSet) GTIDSet {
	return stored.InteriorGaps().Minus(start)
}

// Tail is the open segment a resume continues from: the highest open
// segment with a complete event, its complete transactions, and the cut
// before a trailing partial one. ok is false when there is none.
func (l *Ledger) Tail() (OpenTail, bool) {
	if err := l.list(); err != nil {
		return OpenTail{}, false
	}
	seg, ok := durableOpenSegmentIn(l.dir, l.entries)
	if !ok {
		return OpenTail{}, false
	}
	tail, ok := ScanOpenTail(seg.path)
	if !ok {
		return OpenTail{}, false
	}
	tail.File = seg.source
	return tail, true
}

// ResumeGTID is seed plus every sealed segment's transactions plus
// everything the open tail durably holds (its previous-GTIDs text and its
// complete transactions), in flavor's GTID
// syntax. A text that does not parse for flavor is skipped. ok is false when
// there is no open tail; then the set is "".
func (l *Ledger) ResumeGTID(flavor, seed string) (OpenTail, string, bool) {
	tail, ok := l.Tail()
	if !ok {
		return OpenTail{}, "", false
	}
	union := unionDurable(flavor, seed, OpenTail{Previous: []string{l.SealedText(flavor)}})
	return tail, unionDurable(flavor, union, tail), true
}

// SealedText is SealedStored as text, or "" for a flavor other than MySQL
// (the segment scan reads MySQL GTID events only).
func (l *Ledger) SealedText(flavor string) string {
	if f := strings.TrimSpace(flavor); f != "" && f != gomysql.MySQLFlavor {
		return ""
	}
	return l.SealedStored().String()
}

// Durable is every GTID text the tail durably holds, in the order a resume
// absorbs them: previous-GTIDs first, then each complete transaction. The
// trailing partial transaction is not in it.
func (t OpenTail) Durable() []string {
	out := make([]string, 0, len(t.Previous)+len(t.GTIDs))
	out = append(out, t.Previous...)
	return append(out, t.GTIDs...)
}

func unionDurable(flavor, seed string, tail OpenTail) string {
	if strings.TrimSpace(flavor) == "" {
		flavor = gomysql.MySQLFlavor
	}
	var set gomysql.GTIDSet
	add := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if set == nil {
			parsed, err := gomysql.ParseGTIDSet(flavor, text)
			if err != nil {
				return
			}
			set = parsed
			return
		}
		_ = set.Update(text)
	}
	add(seed)
	for _, g := range tail.Durable() {
		add(g)
	}
	if set == nil || set.IsEmpty() {
		return ""
	}
	return set.String()
}

// ReadSegmentContents is Contents for segment bytes that are not in a
// local task directory. The caller owns r.
func ReadSegmentContents(r io.Reader) (SegmentContents, error) {
	return readSegmentContents(r, false)
}

// ReadSealedSegmentContents is ReadSegmentContents for a sealed segment: an
// event size past the end of r is ErrCorruptEventSize, not a torn tail.
func ReadSealedSegmentContents(r io.Reader) (SegmentContents, error) {
	return readSegmentContents(r, true)
}

func readSegmentContents(r io.Reader, sealed bool) (SegmentContents, error) {
	log, err := scanSegmentGTIDsBounded(r, readerRemaining(r), sealed)
	if err != nil {
		return SegmentContents{}, err
	}
	set := GTIDSet{}
	set.AddEvents(log.Events)
	return SegmentContents{Log: log, GTIDs: set}, nil
}

var contentsCache stampCache[SegmentContents] // segment path -> contents

var sealedCorruptCache stampCache[string] // path -> UnreadableReason, "" when readable

// sealedSegmentCorrupt is why the sealed segment at p is not readable: an
// event size above 1 GiB or past the end of the file, a CRC32 mismatch, or
// a file cut short (ErrSegmentTruncated). Contents keeps reading such a
// file as a torn tail (the re-dump damage fixtures end that way); this is
// the stricter check for a file that was sealed whole. Cached per size and
// mtime. A file that cannot be opened is not corrupt.
func sealedSegmentCorrupt(p string) (string, bool) {
	info, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	st := StampOf(info)
	if reason, ok := sealedCorruptCache.get(p, st); ok {
		return reason, reason != ""
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	_, scanErr := scanSegmentGTIDsBounded(f, info.Size(), true)
	reason := ""
	if errors.Is(scanErr, ErrCorruptEventSize) {
		reason = UnreadableReason(scanErr)
	}
	sealedCorruptCache.put(p, st, reason)
	return reason, reason != ""
}

// segmentContentsAt reads one segment file once per size and mtime.
func segmentContentsAt(p string) (SegmentContents, error) {
	info, err := os.Stat(p)
	if err != nil {
		return SegmentContents{}, err
	}
	st := StampOf(info)
	if c, ok := contentsCache.get(p, st); ok {
		return c, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return SegmentContents{}, err
	}
	defer f.Close()
	c, err := ReadSegmentContents(f)
	if err != nil {
		return SegmentContents{}, err
	}
	contentsCache.put(p, st, c)
	return c, nil
}
