// Package assurance provides module-level functionality for assurance.
// input: Inputs: the task's chain order, catalog rows, task directory ledger, object listing snapshot, checkpoint, start set and recorded source switches
// output: the backup chain: its segments with where each one is readable, and the I1-I7 violations of ADR 0009 plus the order self-check, each a closed Kind with the segments involved and the evidence; an unset upload grace is 15 minutes; since P2-3 I4 includes open copies on the replay order (same content only: GTIDs intersect or one file is a byte prefix of the other) and an empty leftover open file is INFO I4_EMPTY_LEFTOVER
// note P2-5.1 (design 0009 §20.2): a local size is compared with a sealed row only for a legacy segment with no .seal-sha256 line and not while a legacy failback is planned; an object size mismatch is I6_OBJECT_SIZE_MISMATCH
// pos: the one answer to "is this backup complete, and where is each part" (ADR 0009 phase 1); depends on binlog only
// note #405: an open file of only header events is never an I4 copy (every copy of a source file starts with the same header)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

// Checkpoint is the task's checkpoint (C in ADR 0009).
type Checkpoint struct {
	GTIDSet string
	File    string
	Pos     uint32
}

// Row is the dependency-free projection of one binlog_files row.
type Row struct {
	Name        string
	Source      string
	Epoch       int64
	Sealed      bool
	Size        int64
	UploadState string
	Checksum    string
	ObjectKey   string
	CreatedAt   time.Time
	SealedAt    time.Time
}

// Switch is one continued source switch: the server the chain moved to and
// the GTID set it recorded as stored at that point.
type Switch struct {
	Identity string
	Stored   string
}

// Inputs is everything the chain knows; see docs/design/0009-phase1-backup-chain.md.
type Inputs struct {
	Flavor     string
	StartGTID  string
	Checkpoint Checkpoint
	Order      []string
	Rows       []Row
	// NoCatalog says there is no catalog at all (standalone without meta):
	// a segment without a row is then not I6_MISSING_ROW.
	NoCatalog bool
	Local     *binlog.Ledger
	Objects   map[string]int64
	// ObjectPrefixes, when set, are the listing prefixes (ObjectPrefixOf)
	// and whether each was listed completely (design 0009 §21.3): a segment
	// whose prefix is missing or incomplete is not judged against Objects.
	// nil keeps phase 1: a non-nil Objects is a full listing.
	ObjectPrefixes map[string]bool
	ObjectStorage  bool
	UploadGrace    time.Duration
	Switches       []Switch
	// Planned are conclusions the tasks planners already reached (stored
	// GTID hole, legacy failback; I3 subclasses). Check returns them in
	// chain order with its own.
	Planned []Violation
	// Plan, when set, is asked once, lazily, for more planner conclusions in
	// priority order. Health only asks it when no segment is damaged.
	Plan func() []Violation
}

// Kind is a closed set of invariant violations.
type Kind string

const (
	I1Regression       Kind = "I1_REGRESSION"
	I1Unreadable       Kind = "I1_UNREADABLE"
	I2IndexGap         Kind = "I2_INDEX_GAP"
	I2SwitchUnrecorded Kind = "I2_SWITCH_UNRECORDED"
	I2OrderInvalid     Kind = "I2_ORDER_INVALID"
	I3GTIDHole         Kind = "I3_GTID_HOLE"
	I3StoredHole       Kind = "I3_STORED_HOLE"
	I3LegacyFailback   Kind = "I3_LEGACY_FAILBACK"
	I4Overlap          Kind = "I4_OVERLAP"
	// I4EmptyLeftover: an open file with only the magic, only a header, or
	// less than one event header and no complete event (#357). INFO, never a
	// verdict, never cuts replay.
	I4EmptyLeftover   Kind = "I4_EMPTY_LEFTOVER"
	I5CheckpointAhead Kind = "I5_CHECKPOINT_AHEAD"
	I6MissingLocal    Kind = "I6_MISSING_LOCAL"
	I6MissingObject   Kind = "I6_MISSING_OBJECT"
	I6OrphanRow       Kind = "I6_ORPHAN_ROW"
	I6MissingRow      Kind = "I6_MISSING_ROW"
	// I6SizeMismatch: a legacy segment (no .seal-sha256 line) whose sealed
	// catalog row disagrees with the local file size (design 0009 §20.2).
	I6SizeMismatch Kind = "I6_SIZE_MISMATCH"
	// I6ObjectSizeMismatch: the stored object's size disagrees with the row.
	I6ObjectSizeMismatch Kind = "I6_OBJECT_SIZE_MISMATCH"
	I7UploadLate         Kind = "I7_UPLOAD_LATE"
	I7Unverified         Kind = "I7_UNVERIFIED"
)

// AllKinds lists every Kind.
func AllKinds() []Kind {
	return []Kind{I1Regression, I1Unreadable, I2IndexGap, I2SwitchUnrecorded, I2OrderInvalid, I3GTIDHole, I3StoredHole, I3LegacyFailback,
		I4Overlap, I4EmptyLeftover, I5CheckpointAhead, I6MissingLocal, I6MissingObject, I6OrphanRow, I6MissingRow,
		I6SizeMismatch, I6ObjectSizeMismatch, I7UploadLate, I7Unverified}
}

// Violation is one broken invariant.
type Violation struct {
	Kind     Kind
	Segments []string
	Evidence string
	Detail   string
	// Problem is the full storage problem for the kinds storage health
	// reports (I1 from Ledger.Damage, the planner I3 subclasses).
	Problem *binlog.StorageProblem
	// Since is when the condition began, when the chain knows it
	// (I7_UPLOAD_LATE: the row's sealed_at), for severity by age.
	Since time.Time
}

// Segment is one segment on the chain and where it can be read.
type Segment struct {
	Name  string
	Named binlog.SegmentName
	// Row is the catalog row; nil when there is none.
	Row *Row
	// Local reports the file is in the task directory; LocalSize is its size.
	Local     bool
	LocalSize int64
	// Object is the listed object size; nil when the object was not listed
	// (no listing, no key) or is not there. ObjectListed tells which.
	Object       *int64
	ObjectListed bool
	// Contents is what the local file stores; nil when it is not readable.
	Contents *binlog.SegmentContents
	ReadErr  error
}

// Chain is one task's backup chain. It is not safe for concurrent use.
type Chain struct {
	in   Inputs
	segs []Segment
	read bool

	planned  []Violation
	planDone bool
}

// DefaultUploadGrace is the upload grace when Inputs leaves it unset (ADR
// 0009 decision 2).
const DefaultUploadGrace = 15 * time.Minute

// ChainOf is the chain for in. Nothing is read until a question is asked.
// An unset UploadGrace is DefaultUploadGrace.
func ChainOf(in Inputs) *Chain {
	if in.UploadGrace <= 0 {
		in.UploadGrace = DefaultUploadGrace
	}
	return &Chain{in: in}
}

func (c *Chain) mysql() bool {
	f := strings.TrimSpace(c.in.Flavor)
	return f == "" || strings.EqualFold(f, "mysql")
}

// Segments is the chain in restore order.
func (c *Chain) Segments() []Segment {
	if c.read {
		return c.segs
	}
	c.read = true
	rows := map[string]*Row{}
	for i := range c.in.Rows {
		rows[c.in.Rows[i].Name] = &c.in.Rows[i]
	}
	for _, name := range c.in.Order {
		seg := Segment{Name: name, Row: rows[name]}
		seg.Named, _ = binlog.ClassifySegment(name)
		if c.in.Local != nil && c.in.Local.Dir() != "" {
			if info, err := os.Stat(filepath.Join(c.in.Local.Dir(), name)); err == nil && info.Mode().IsRegular() {
				seg.Local, seg.LocalSize = true, info.Size()
				contents, err := c.in.Local.Contents(name)
				if err != nil {
					seg.ReadErr = err
				} else {
					seg.Contents = &contents
				}
			}
		}
		if seg.Row != nil && seg.Row.ObjectKey != "" && c.in.Objects != nil && c.prefixListed(seg.Row.ObjectKey) {
			seg.ObjectListed = true
			if size, ok := c.in.Objects[seg.Row.ObjectKey]; ok {
				seg.Object = &size
			}
		}
		c.segs = append(c.segs, seg)
	}
	return c.segs
}

// Check reports every violation of I1-I7, in chain order: a violation sorts
// by the first segment it names; one that names no chain segment comes last.
// The same Inputs and now give the same result.
func (c *Chain) Check(now time.Time) []Violation {
	segs := c.Segments()
	var out []Violation
	add := func(k Kind, evidence, detail string, names ...string) {
		out = append(out, Violation{Kind: k, Segments: names, Evidence: evidence, Detail: detail})
	}
	switched := map[string]bool{}
	for _, sw := range c.in.Switches {
		switched[strings.ToLower(strings.TrimSpace(sw.Identity))] = true
	}
	unrecorded := map[string]bool{}
	held := binlog.GTIDSet{}
	type group struct {
		prefix string
		seq    uint64
		log    binlog.SegmentGTIDLog
		first  string
	}
	var prev *group
	var before []binlog.GTIDEventRef
	stored := c.storedSet(segs)
	repeats := c.sealedRepeats(segs, nil)
	for i := range segs {
		seg := &segs[i]
		// I1
		if seg.Local && c.in.Local != nil {
			if detail, bad := c.in.Local.SegmentDamage(seg.Name); bad {
				add(I1Regression, detail, detail, seg.Name)
			} else if reason, unreadable := c.in.Local.SegmentUnreadable(seg.Name); unreadable {
				detail := "segment " + seg.Name + " is not readable: " + reason
				add(I1Unreadable, detail, detail, seg.Name)
			}
		}
		prefix, idOK := sourcePrefix(seg.Named.Source)
		// I2: a switch to a server the chain has no record for.
		if id := identityOf(seg.Named.Source); id != "" && !switched[id] && !unrecorded[id] {
			unrecorded[id] = true
			add(I2SwitchUnrecorded, id, fmt.Sprintf("segment %s is from server %s but no source switch to it is recorded", seg.Name, id), seg.Name)
		}
		// I4: an empty leftover open file (#357) is INFO and takes no part in I4.
		if c.emptyLeftover(segs, i) {
			detail := "open segment " + seg.Name + " holds no complete event; it is a leftover and replay is not cut for it"
			add(I4EmptyLeftover, seg.Name, detail, seg.Name)
		}
		// I4: the same source file twice on the chain (sealed or open copies).
		if oi, ok := repeats[i]; ok {
			other := &segs[oi]
			add(I4Overlap, other.Name+" "+seg.Name, sealedTwiceDetail(other, seg), seg.Name, other.Name)
		}
		if seg.Contents != nil && c.mysql() {
			log := seg.Contents.Log
			if uuid, n, hole := intraSegmentHole(log.Events); hole {
				add(I3GTIDHole, fmt.Sprintf("%s:%d", uuid, n), fmt.Sprintf("gtid hole in %s at %s:%d", seg.Name, uuid, n), seg.Name)
			}
			// I4: transactions stored twice.
			mine := seg.Contents.GTIDs
			if both := mine.Minus(mine.Minus(held)); !both.IsEmpty() {
				add(I4Overlap, both.String(), fmt.Sprintf("segment %s stores %s again", seg.Name, both.String()), seg.Name)
			}
			held.AddAll(mine)
		}
		// I2/I3 between neighbouring source files.
		if idOK {
			var log binlog.SegmentGTIDLog
			if seg.Contents != nil {
				log = seg.Contents.Log
			}
			if prev != nil && prev.prefix == prefix && prev.seq == seg.Named.Seq {
				if !prev.log.HasPrevious && log.HasPrevious {
					prev.log.HasPrevious, prev.log.Previous = true, log.Previous
				}
				prev.log.Events = append(prev.log.Events, log.Events...)
			} else {
				if prev != nil {
					if prev.prefix == prefix && seg.Named.Seq > prev.seq+1 {
						add(I2IndexGap, fmt.Sprintf("%d..%d", prev.seq+1, seg.Named.Seq-1),
							fmt.Sprintf("source files between %s and %s are missing", prev.first, seg.Name), seg.Name, prev.first)
					}
					before = append(before, prev.log.Events...)
					if c.mysql() && seg.Contents != nil {
						var hole bool
						if prev.prefix != prefix {
							hole = switchHole(stored, before, log)
						} else {
							hole = gtidHole(prev.log, log)
						}
						if hole {
							add(I3GTIDHole, prev.first+" "+seg.Name, fmt.Sprintf("gtid hole between %s and %s", prev.first, seg.Name), seg.Name, prev.first)
						}
					}
				}
				prev = &group{prefix: prefix, seq: seg.Named.Seq, log: log, first: seg.Name}
			}
		}
		c.checkCopies(seg, now, add)
	}
	if c.mysql() {
		c.checkInteriorHole(segs, &out, add)
		c.checkCheckpointGTID(segs, stored, add)
	}
	c.checkCheckpointPos(segs, add)
	c.checkOrder(segs, add)
	c.checkOffChainRows(add)
	out = append(out, c.in.Planned...)
	out = append(out, c.plan()...)
	out = dropSizeUnderLegacyFailback(out)
	for i := range out {
		if out[i].Kind != I7UploadLate || len(out[i].Segments) == 0 {
			continue
		}
		for j := range segs {
			if segs[j].Name == out[i].Segments[0] && segs[j].Row != nil {
				out[i].Since = segs[j].Row.SealedAt
				break
			}
		}
	}
	pos := map[string]int{}
	for i, s := range segs {
		pos[s.Name] = i
	}
	rank := func(v Violation) int {
		if len(v.Segments) > 0 {
			if p, ok := pos[v.Segments[0]]; ok {
				return p
			}
		}
		return len(segs)
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// emptyLeftover: a local open segment with only the magic, only header
// events, or fewer bytes than the magic plus one event header and no
// complete event (#357, SEGMENT_EMPTY_LEFTOVER). The last segment on the
// order is the live open file right after a rotate, never a leftover.
func (c *Chain) emptyLeftover(segs []Segment, i int) bool {
	seg := &segs[i]
	if !seg.Named.Open || !seg.Local || c.in.Local == nil || i == len(segs)-1 {
		return false
	}
	if seg.LocalSize < 4+19 {
		return true
	}
	if seg.Contents != nil && (!seg.Contents.GTIDs.IsEmpty() || len(seg.Contents.Log.Events) > 0) {
		return false
	}
	return binlog.PreambleOnly(filepath.Join(c.in.Local.Dir(), seg.Name))
}

// sealedRepeats maps the chain position of each segment that repeats an
// earlier segment of the same source file (I4) to the position of
// the first such earlier segment. Epochs of one source file that continue
// each other are not a repeat; only a pair that cannot be told apart, or
// whose GTIDs (MySQL) or event positions overlap, is. A non-nil counts
// narrows which pairs count.
func (c *Chain) sealedRepeats(segs []Segment, counts func(earlier, later *Segment) bool) map[int]int {
	out := map[int]int{}
	seen := map[string][]int{} // prefix\x00seq of a sealed segment -> chain positions
	for i := range segs {
		seg := &segs[i]
		prefix, ok := sourcePrefix(seg.Named.Source)
		// P2-3: open copies count too (#372 openstale/openhand), except an
		// empty leftover, which holds nothing to apply twice.
		if !ok || c.emptyLeftover(segs, i) {
			continue
		}
		key := fmt.Sprintf("%s\x00%d", prefix, seg.Named.Seq)
		for _, oi := range seen[key] {
			same := c.sameBytesSpan
			if segs[oi].Named.Open || seg.Named.Open {
				same = c.sameOpenCopy // P2-3: an open copy counts only when it holds the same content
			}
			if (counts == nil || counts(&segs[oi], seg)) && same(&segs[oi], seg) {
				out[i] = oi
				break
			}
		}
		seen[key] = append(seen[key], i)
	}
	return out
}

func sealedTwiceDetail(earlier, later *Segment) string {
	return fmt.Sprintf("source file %s is on the chain twice: %s and %s", later.Named.Source, earlier.Name, later.Name)
}

// sameBytesSpan reports that two sealed segments of one source file may
// hold the same events: either is unreadable (nothing proves otherwise), or
// their GTIDs intersect (MySQL, both sides with GTIDs), or their event
// position ranges overlap. A side with no GTIDs (junk, magic-only,
// header-only, zeros) is judged by positions, never by an empty intersection.
func (c *Chain) sameBytesSpan(a, b *Segment) bool {
	if a.Contents == nil || b.Contents == nil {
		return true
	}
	if c.mysql() && !a.Contents.GTIDs.IsEmpty() && !b.Contents.GTIDs.IsEmpty() {
		both := a.Contents.GTIDs.Minus(a.Contents.GTIDs.Minus(b.Contents.GTIDs))
		return !both.IsEmpty()
	}
	if c.in.Local == nil {
		return true
	}
	as, ae, aok := binlog.SegmentPositions(filepath.Join(c.in.Local.Dir(), a.Name))
	bs, be, bok := binlog.SegmentPositions(filepath.Join(c.in.Local.Dir(), b.Name))
	if !aok || !bok {
		return true
	}
	return as < be && bs < ae
}

// sameOpenCopy reports that a pair with an open member holds the same
// content (#372 openstale/openhand): their GTIDs intersect (MySQL, both
// sides with GTIDs), or one file's bytes are a prefix of the other's. An
// open file of a later epoch that continues the sealed one is not a copy.
func (c *Chain) sameOpenCopy(a, b *Segment) bool {
	if a.Contents != nil && b.Contents != nil && c.mysql() && !a.Contents.GTIDs.IsEmpty() && !b.Contents.GTIDs.IsEmpty() {
		both := a.Contents.GTIDs.Minus(a.Contents.GTIDs.Minus(b.Contents.GTIDs))
		return !both.IsEmpty()
	}
	if c.in.Local == nil || !a.Local || !b.Local {
		return false
	}
	// A preamble (format description, previous GTIDs) starts every copy of
	// a source file, including an open file that has just resumed it on a
	// later epoch and holds no transaction yet: that is not a second copy
	// (QA #404, after the legacy failback repair).
	for _, s := range []*Segment{a, b} {
		if s.Named.Open && binlog.HeaderEventsOnly(filepath.Join(c.in.Local.Dir(), s.Name)) {
			return false
		}
	}
	return bytePrefix(filepath.Join(c.in.Local.Dir(), a.Name), filepath.Join(c.in.Local.Dir(), b.Name))
}

// bytePrefix: the shorter file's bytes are the start of the longer one.
// minCopyBytes is the magic plus one v4 event header.
const minCopyBytes = 4 + 19

var binlogMagic = []byte{0xfe, 'b', 'i', 'n'}

func bytePrefix(pa, pb string) bool {
	fa, err := os.Open(pa)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(pb)
	if err != nil {
		return false
	}
	defer fb.Close()
	// Both files must hold the magic and at least one event header: anything
	// shorter is an empty leftover or not a binlog, never a copy.
	for _, f := range []*os.File{fa, fb} {
		st, err := f.Stat()
		if err != nil || st.Size() < minCopyBytes {
			return false
		}
	}
	magic := make([]byte, 4)
	if _, err := fa.ReadAt(magic, 0); err != nil || !bytes.Equal(magic, binlogMagic) {
		return false
	}
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		n := na
		if nb < n {
			n = nb
		}
		if !bytes.Equal(ba[:n], bb[:n]) {
			return false
		}
		if ea != nil || eb != nil {
			return n > 0 || (na == 0 && nb == 0)
		}
	}
}

// checkCopies is I6 and I7 for one segment.
func (c *Chain) checkCopies(seg *Segment, now time.Time, add func(Kind, string, string, ...string)) {
	if seg.Named.Open {
		return
	}
	row := seg.Row
	if row == nil && c.in.NoCatalog {
		return
	}
	if row == nil {
		if seg.Local {
			add(I6MissingRow, seg.Name, fmt.Sprintf("sealed segment %s has no catalog row", seg.Name), seg.Name)
		} else {
			add(I6MissingRow, seg.Name, fmt.Sprintf("chain segment %s has no catalog row and no local file", seg.Name), seg.Name)
		}
		return
	}
	uploaded := strings.EqualFold(row.UploadState, "UPLOADED")
	match := strings.EqualFold(row.Checksum, "match")
	switch {
	case !seg.Local && !uploaded:
		add(I6MissingLocal, row.UploadState, fmt.Sprintf("segment %s is %s but not on local disk", seg.Name, row.UploadState), seg.Name)
	case !seg.Local && seg.ObjectListed && seg.Object == nil:
		add(I6OrphanRow, row.ObjectKey, fmt.Sprintf("segment %s is neither on local disk nor in object storage (%s)", seg.Name, row.ObjectKey), seg.Name)
	case seg.Local && uploaded && seg.ObjectListed && seg.Object == nil:
		add(I6MissingObject, row.ObjectKey, fmt.Sprintf("segment %s is UPLOADED but object %s is missing", seg.Name, row.ObjectKey), seg.Name)
	}
	if uploaded && strings.TrimSpace(row.ObjectKey) == "" && (c.in.Objects != nil || c.in.ObjectStorage) {
		add(I6MissingObject, "object_key=", fmt.Sprintf("segment %s is UPLOADED but its row has no object key", seg.Name), seg.Name)
	}
	// §20.2: a ledger line is the size at seal and wins over the row (a
	// disagreement with the file is I1_UNREADABLE); only a legacy segment
	// with a sealed row is compared.
	if seg.Local && row.Sealed && row.Size > 0 && seg.LocalSize != row.Size && !c.ledgerHas(seg.Name) {
		add(I6SizeMismatch, fmt.Sprintf("local=%d row=%d", seg.LocalSize, row.Size), fmt.Sprintf("segment %s is %d bytes on disk, the catalog says %d", seg.Name, seg.LocalSize, row.Size), seg.Name)
	}
	if seg.Object != nil && row.Size > 0 && *seg.Object != row.Size {
		add(I6ObjectSizeMismatch, fmt.Sprintf("object=%d row=%d", *seg.Object, row.Size), fmt.Sprintf("object %s is %d bytes, the catalog says %d", row.ObjectKey, *seg.Object, row.Size), seg.Name)
	}
	if !c.in.ObjectStorage {
		return
	}
	if uploaded && !match {
		add(I7Unverified, "checksum="+row.Checksum, fmt.Sprintf("segment %s is UPLOADED without a matching checksum", seg.Name), seg.Name)
		return
	}
	grace := c.in.UploadGrace
	if !uploaded && !row.SealedAt.IsZero() && now.Sub(row.SealedAt) > grace {
		add(I7UploadLate, fmt.Sprintf("%s for %s", row.UploadState, now.Sub(row.SealedAt).Truncate(time.Second)),
			fmt.Sprintf("segment %s was sealed %s ago and is still %s", seg.Name, now.Sub(row.SealedAt).Truncate(time.Second), row.UploadState), seg.Name)
	}
}

// checkOffChainRows is I6 for sealed rows that are not on the chain: a row
// whose bytes are nowhere (#209).
func (c *Chain) checkOffChainRows(add func(Kind, string, string, ...string)) {
	on := map[string]bool{}
	for _, n := range c.in.Order {
		on[n] = true
	}
	for _, row := range c.in.Rows {
		if on[row.Name] || !row.Sealed || c.in.Objects == nil {
			continue
		}
		if c.in.Local != nil && c.in.Local.Dir() != "" {
			if _, err := os.Stat(filepath.Join(c.in.Local.Dir(), row.Name)); err == nil {
				continue
			}
		}
		if _, ok := c.in.Objects[row.ObjectKey]; ok && row.ObjectKey != "" {
			continue
		}
		add(I6OrphanRow, row.ObjectKey, fmt.Sprintf("catalog row %s has no local file and no object", row.Name), row.Name)
	}
}

// storedSet is B, the first header's history and every stored transaction.
func (c *Chain) storedSet(segs []Segment) binlog.GTIDSet {
	set := binlog.GTIDSet{}
	for _, s := range segs {
		if s.Contents != nil && s.Contents.Log.HasPrevious {
			if prev, err := binlog.ParseGTIDSet(s.Contents.Log.Previous); err == nil {
				set.AddAll(prev)
			}
			break
		}
	}
	if start, err := binlog.ParseGTIDSet(c.in.StartGTID); err == nil {
		set.AddAll(start)
	}
	for _, s := range segs {
		if s.Contents != nil {
			set.AddAll(s.Contents.GTIDs)
		}
	}
	return set
}

// checkInteriorHole is I3 for sequences missing between stored ones that no
// neighbour check already named.
func (c *Chain) checkInteriorHole(segs []Segment, out *[]Violation, add func(Kind, string, string, ...string)) {
	named := map[string]bool{}
	for _, v := range *out {
		if v.Kind == I3GTIDHole && len(v.Segments) > 0 {
			named[v.Segments[0]] = true
		}
	}
	stored := binlog.GTIDSet{}
	for _, s := range segs {
		if s.Contents == nil {
			if s.Local {
				return // an unreadable segment may hold it
			}
			continue
		}
		stored.AddAll(s.Contents.GTIDs)
	}
	start, err := binlog.ParseGTIDSet(c.in.StartGTID)
	if err != nil {
		start = nil
	}
	gaps := binlog.StoredHoles(stored, start)
	keys := make([]string, 0, len(gaps))
	for k := range gaps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, iv := range gaps[k] {
			after := ""
			for _, s := range segs {
				if s.Contents == nil || after != "" {
					continue
				}
				for _, r := range s.Contents.GTIDs[k] {
					if r.Lo > iv.Hi {
						after = s.Name
						break
					}
				}
			}
			// A neighbour check already named the hole before this segment.
			if named[after] {
				continue
			}
			ev := fmt.Sprintf("%s:%d-%d", k, iv.Lo, iv.Hi)
			add(I3GTIDHole, ev, fmt.Sprintf("gtid hole: %s is stored in no segment", ev), after)
		}
	}
}

// checkCheckpointGTID is I5 for the checkpoint's GTID set.
func (c *Chain) checkCheckpointGTID(segs []Segment, stored binlog.GTIDSet, add func(Kind, string, string, ...string)) {
	last := ""
	if len(segs) > 0 {
		last = segs[len(segs)-1].Name
	}
	if cp := strings.TrimSpace(c.in.Checkpoint.GTIDSet); cp != "" {
		if set, err := binlog.ParseGTIDSet(cp); err == nil {
			if ahead := set.Minus(stored); !ahead.IsEmpty() {
				add(I5CheckpointAhead, ahead.String(), fmt.Sprintf("checkpoint lists %s, which no segment holds", ahead.String()), last)
			}
		}
	}
}

// checkCheckpointPos is I5 for the checkpoint's file and position: past the
// durable end of the open tail, past the end of the sealed file it names, or
// at a source file after the last one on the chain.
func (c *Chain) checkCheckpointPos(segs []Segment, add func(Kind, string, string, ...string)) {
	file, pos := strings.TrimSpace(c.in.Checkpoint.File), c.in.Checkpoint.Pos
	if file == "" {
		return
	}
	if c.in.Local != nil {
		if tail, ok := c.in.Local.Tail(); ok && tail.File == file {
			if pos > tail.Pos {
				add(I5CheckpointAhead, fmt.Sprintf("%s:%d > %d", tail.File, pos, tail.Pos),
					fmt.Sprintf("checkpoint %s:%d is past the durable end %d of the open segment", tail.File, pos, tail.Pos), filepath.Base(tail.Path))
			}
			return
		}
	}
	var sealed *Segment
	for i := range segs {
		if segs[i].Named.Source == file && !segs[i].Named.Open {
			sealed = &segs[i]
		}
	}
	if sealed != nil {
		size := int64(0)
		switch {
		case sealed.Local:
			size = sealed.LocalSize
		case sealed.Row != nil:
			size = sealed.Row.Size
		}
		if size > 0 && int64(pos) > size {
			add(I5CheckpointAhead, fmt.Sprintf("%s:%d > %d", file, pos, size),
				fmt.Sprintf("checkpoint %s:%d is past the end %d of sealed segment %s", file, pos, size, sealed.Name), sealed.Name)
		}
		return
	}
	prefix, seq, ok := sourceIndex(file)
	if !ok {
		return
	}
	lastName, lastSeq, found := "", uint64(0), false
	for _, s := range segs {
		if p, _ := sourcePrefix(s.Named.Source); p == prefix && s.Named.Seq >= lastSeq {
			lastName, lastSeq, found = s.Name, s.Named.Seq, true
		}
	}
	if found && seq > lastSeq {
		add(I5CheckpointAhead, fmt.Sprintf("%s:%d after %s", file, pos, lastName),
			fmt.Sprintf("checkpoint %s:%d is at a source file after the last chain segment %s", file, pos, lastName), lastName)
	}
}

// checkOrder is the chain order's self-check (I2): every local segment is
// on it, and the source index never goes back inside one prefix.
func (c *Chain) checkOrder(segs []Segment, add func(Kind, string, string, ...string)) {
	on := map[string]bool{}
	last := map[string]uint64{}
	for _, s := range segs {
		on[s.Name] = true
		prefix, ok := sourcePrefix(s.Named.Source)
		if !ok {
			continue
		}
		if prev, seen := last[prefix]; seen && s.Named.Seq < prev {
			add(I2OrderInvalid, fmt.Sprintf("%d after %d", s.Named.Seq, prev),
				fmt.Sprintf("chain order puts %s after index %d of %s", s.Name, prev, prefix), s.Name)
			continue
		}
		last[prefix] = s.Named.Seq
	}
	if c.in.Local == nil {
		return
	}
	local, err := c.in.Local.Segments()
	if err != nil {
		return
	}
	// SelectReplayFiles keeps only the highest open epoch of a source file.
	topOpen := map[string]int64{}
	for _, ls := range local {
		if ls.Named.Open && ls.Named.Epoch > topOpen[ls.Named.Source] {
			topOpen[ls.Named.Source] = ls.Named.Epoch
		}
	}
	for _, ls := range local {
		if ls.Named.Open && ls.Named.Epoch < topOpen[ls.Named.Source] {
			continue
		}
		if !on[ls.Name] {
			add(I2OrderInvalid, ls.Name, fmt.Sprintf("local segment %s is not on the chain order", ls.Name), ls.Name)
		}
	}
}

// sourceIndex splits a source file name into prefix and index.
func sourceIndex(source string) (string, uint64, bool) {
	prefix, ok := sourcePrefix(source)
	if !ok {
		return "", 0, false
	}
	n, err := strconv.ParseUint(source[len(prefix)+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return prefix, n, true
}

// sourcePrefix is the source name without its numeric index.
func sourcePrefix(source string) (string, bool) {
	dot := strings.LastIndex(source, ".")
	if dot <= 0 || dot == len(source)-1 {
		return "", false
	}
	return source[:dot], true
}

// identityOf is the server identity a failback name starts with
// ({uuid}.mysql-bin.000001), "" for a plain name.
func identityOf(source string) string {
	first, _, ok := strings.Cut(source, ".")
	if !ok || len(first) != 36 || strings.Count(first, "-") != 4 {
		return ""
	}
	return strings.ToLower(first)
}

func (c *Chain) ledgerHas(name string) bool {
	if c.in.Local == nil || c.in.Local.Dir() == "" {
		return false
	}
	_, ok := binlog.SealSumAt(c.in.Local.Dir(), name)
	return ok
}

// dropSizeUnderLegacyFailback: the legacy failback repair rewrites rows
// from disk, so the row sizes are not judged meanwhile (§20.2).
func dropSizeUnderLegacyFailback(vs []Violation) []Violation {
	legacy := false
	for _, v := range vs {
		if v.Kind == I3LegacyFailback {
			legacy = true
		}
	}
	if !legacy {
		return vs
	}
	out := vs[:0]
	for _, v := range vs {
		if v.Kind != I6SizeMismatch {
			out = append(out, v)
		}
	}
	return out
}

// ObjectPrefixOf is the listing prefix of an object key: its directory with
// the trailing slash, or "" for a key with no directory.
func ObjectPrefixOf(key string) string {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return ""
	}
	return key[:i+1]
}

func (c *Chain) prefixListed(key string) bool {
	if c.in.ObjectPrefixes == nil {
		return true
	}
	return c.in.ObjectPrefixes[ObjectPrefixOf(key)]
}
