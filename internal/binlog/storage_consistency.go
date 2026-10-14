// Package binlog provides module-level functionality for binlog.
// input: a task directory of binlog segments, the checkpoint gtid_set, and the task's start gtid_set
// output: a storage problem when one segment rotates to its own or an older file, or when an event position goes backwards inside that segment, naming that segment, the segments before it that still restore, the MySQL GTIDs the checkpoint lists that no segment holds, and the GTID set a new task starts from; out-of-order GTIDs, a hole in one UUID, and a missing file are not a problem
// pos: the ledger's damage question (Ledger.Damage); detect a backup already damaged by a stale GTID re-dump before another start drops more transactions
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

// StorageProblem is one on-disk disagreement between the checkpoint and the segments.
// Message names what was found and the recovery step.
type StorageProblem struct {
	Message string
	// Segment is the disk name of the first damaged segment.
	Segment string
	// Detail is what was found in that segment, without the recovery text.
	Detail string
	// Missing is the MySQL GTID set the checkpoint lists that no segment
	// holds and the task's start set does not cover. Empty when unknown.
	Missing string
	// Valid lists the segments before the damaged one, in binlog order.
	// Their bytes were written before the damage and still restore.
	Valid []string
	// Restart is the GTID set a new task starts from: the task's start set
	// plus every transaction in Valid. That dump sends everything after the
	// valid segments, including Missing. Empty when it cannot be computed.
	Restart string
}

// Damage is the first segment in binlog order with the re-dump damage
// signature: a rotate to the segment's own file or an older one, or an
// event position that goes backwards. Valid lists the segments before it.
// For MySQL, Missing is checkpointGTID minus the start set, the first
// segment's previous-GTIDs header and every stored GTID, and Restart is
// that base plus the GTIDs of Valid (empty when the base is unknown,
// because the valid segments alone would re-dump the source's whole
// history). Out-of-order GTIDs, a hole in one UUID and a missing file are
// not damage. An unreadable or missing directory is not damage.
func (l *Ledger) Damage(checkpointGTID, startGTID, flavor string) (StorageProblem, bool) {
	segs, err := l.Segments()
	if err != nil {
		return StorageProblem{}, false
	}
	for i, seg := range segs {
		detail, bad := segmentDamage(seg.Path, seg.Name, seg.Named.Source)
		if !bad {
			continue
		}
		problem := StorageProblem{Segment: seg.Name, Detail: detail}
		var valid []LedgerSegment
		for _, prev := range segs[:i] {
			if prev.Named.Seq < seg.Named.Seq {
				valid = append(valid, prev)
				problem.Valid = append(problem.Valid, prev.Name)
			}
		}
		if !isMariaDBStorage(flavor) {
			problem.Missing, problem.Restart = l.recoverySets(segs, valid, checkpointGTID, startGTID)
		}
		problem.Message = storageMessage(problem)
		return problem, true
	}
	return StorageProblem{}, false
}

// SegmentDamage is the re-dump damage signature of the segment called name
// (a rotate to its own or an older file, or an event position that goes
// backwards), with what was found. A file that cannot be read is not damage.
func (l *Ledger) SegmentDamage(name string) (string, bool) {
	named, ok := ClassifySegment(name)
	if !ok {
		return "", false
	}
	return segmentDamage(filepath.Join(l.dir, name), name, named.Source)
}

// IsDamaged reports that Damage would find a segment, without computing the
// recovery sets.
func (l *Ledger) IsDamaged() bool {
	segs, err := l.Segments()
	if err != nil {
		return false
	}
	for _, seg := range segs {
		if _, bad := segmentDamage(seg.Path, seg.Name, seg.Named.Source); bad {
			return true
		}
	}
	return false
}

func isMariaDBStorage(flavor string) bool {
	return strings.EqualFold(strings.TrimSpace(flavor), "mariadb")
}

// recoverySets is checkpoint minus base minus every stored GTID, and base
// plus the GTIDs of the valid segments. base is the task's start set plus
// the previous-GTIDs header of the first segment.
func (l *Ledger) recoverySets(all, valid []LedgerSegment, checkpointGTID, startGTID string) (string, string) {
	base := GTIDSet{}
	if start, ok := ParseGTIDSetLenient(startGTID); ok {
		base.AddAll(start)
	}
	stored := GTIDSet{}
	for i, seg := range all {
		c, err := l.Contents(seg.Name)
		if err != nil {
			continue
		}
		stored.AddAll(c.GTIDs)
		if i == 0 {
			if prev, ok := ParseGTIDSetLenient(c.Log.Previous); ok {
				base.AddAll(prev)
			}
		}
	}
	missing := ""
	if claimed, ok := ParseGTIDSetLenient(checkpointGTID); ok {
		missing = claimed.Minus(base).Minus(stored).String()
	}
	if len(base) == 0 {
		return missing, ""
	}
	restart := base.Clone()
	for _, seg := range valid {
		if c, err := l.Contents(seg.Name); err == nil {
			restart.AddAll(c.GTIDs)
		}
	}
	return missing, restart.String()
}

// storageRecoveryText is the operator step for a backup that already lost transactions.
const storageRecoveryText = "Do not start this task again and do not restore the damaged segment or any later one; replay from this task stops before it. Create a new task from a GTID the source still has"

func storageMessage(p StorageProblem) string {
	var b strings.Builder
	b.WriteString(p.Detail)
	b.WriteString(" This is the re-dump damage that loses transactions.")
	if p.Missing != "" {
		b.WriteString(" Transactions the checkpoint lists that no segment holds: ")
		b.WriteString(p.Missing)
		b.WriteString(".")
	}
	switch len(p.Valid) {
	case 0:
		b.WriteString(" No segment before " + p.Segment + " is left to restore.")
	case 1:
		b.WriteString(" The segment before " + p.Segment + " (" + p.Valid[0] + ") was written before the damage and still restores.")
	default:
		b.WriteString(" The " + strconv.Itoa(len(p.Valid)) + " segments before " + p.Segment + " (" + p.Valid[0] + " to " + p.Valid[len(p.Valid)-1] + ") were written before the damage and still restore.")
	}
	b.WriteString(" ")
	b.WriteString(storageRecoveryText)
	if p.Restart != "" {
		b.WriteString(": start mode GTID with gtid_set " + p.Restart + " continues right after those segments and fetches the missing transactions again. Do it before the source purges them.")
	} else {
		b.WriteString(".")
	}
	b.WriteString(" Leave these files for forensics.")
	return b.String()
}

func segmentDamage(path, diskName, segmentSource string) (string, bool) {
	if next, stray := strayRotate(path, segmentSource); stray {
		return "Segment " + diskName + " contains a rotate to " + next + ", which is not a newer binlog file.", true
	}
	if pos, prev, back := positionRegresses(path); back {
		return "Segment " + diskName + " has event end pos " + strconv.FormatUint(uint64(pos), 10) + " behind earlier end pos " + strconv.FormatUint(uint64(prev), 10) + ".", true
	}
	return "", false
}

func positionRegresses(path string) (uint32, uint32, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, false
	}
	size := info.Size()
	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil || string(magic) != string(durableMagic) {
		return 0, 0, false
	}
	offset := int64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	var prev uint32
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := f.Read(hdr); err != nil {
			return 0, 0, false
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		if eventSize < int64(goreplication.EventHeaderSize) || offset+eventSize > size {
			return 0, 0, false
		}
		bodyLen := eventSize - int64(goreplication.EventHeaderSize)
		if bodyLen > 0 {
			if _, err := f.Seek(bodyLen, 1); err != nil {
				return 0, 0, false
			}
		}
		offset += eventSize
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if logPos == 0 {
			continue
		}
		if prev != 0 && logPos < prev {
			return logPos, prev, true
		}
		if logPos > prev {
			prev = logPos
		}
	}
	return 0, 0, false
}

func strayRotate(path, segmentSource string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false
	}
	size := info.Size()
	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil || string(magic) != string(durableMagic) {
		return "", false
	}
	offset := int64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	crc32 := false
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := f.Read(hdr); err != nil {
			return "", false
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		if eventSize < int64(goreplication.EventHeaderSize) || offset+eventSize > size {
			return "", false
		}
		bodyLen := eventSize - int64(goreplication.EventHeaderSize)
		eventType := hdr[4]
		var body []byte
		keep := eventType == byte(goreplication.FORMAT_DESCRIPTION_EVENT) || eventType == byte(goreplication.ROTATE_EVENT)
		if keep && bodyLen > 0 && bodyLen <= 1<<20 {
			body = make([]byte, bodyLen)
			if _, err := f.Read(body); err != nil {
				return "", false
			}
		} else if bodyLen > 0 {
			if _, err := f.Seek(bodyLen, 1); err != nil {
				return "", false
			}
		}
		if eventType == byte(goreplication.FORMAT_DESCRIPTION_EVENT) && len(body) >= 57 {
			fde := &goreplication.FormatDescriptionEvent{}
			if err := fde.Decode(body); err == nil && fde.ChecksumAlgorithm == goreplication.BINLOG_CHECKSUM_ALG_CRC32 {
				crc32 = true
			}
		}
		offset += eventSize
		if eventType != byte(goreplication.ROTATE_EVENT) || len(body) < 8 {
			continue
		}
		raw := body
		if crc32 && len(raw) >= goreplication.BinlogChecksumLength {
			raw = raw[:len(raw)-goreplication.BinlogChecksumLength]
		}
		rot := &goreplication.RotateEvent{}
		if err := rot.Decode(raw); err != nil {
			continue
		}
		next := strings.TrimRight(string(rot.NextLogName), "\x00")
		next = strings.TrimSpace(next)
		segSeq, nextSeq, same := sameBinlogStream(segmentSource, next)
		if same && nextSeq <= segSeq {
			return next, true
		}
	}
	return "", false
}

func sameBinlogStream(segmentSource, rotateNext string) (uint64, uint64, bool) {
	segBase, segSeq, ok := plainBinlog(segmentSource)
	nextBase, nextSeq, okNext := plainBinlog(rotateNext)
	if !ok || !okNext || segBase != nextBase {
		return 0, 0, false
	}
	return segSeq, nextSeq, true
}

func plainBinlog(name string) (string, uint64, bool) {
	name = strings.TrimRight(strings.TrimSpace(name), "\x00")
	if i := strings.Index(name, "."); i > 0 && looksStintPrefix(name[:i]) {
		name = name[i+1:]
	}
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot == len(name)-1 {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(name[dot+1:], 10, 64)
	if err != nil || name[:dot] == "" {
		return "", 0, false
	}
	return name[:dot], seq, true
}

// looksStintPrefix is a server identity prefix on a later stint's file:
// {uuid}, or {uuid}~{n} when the chain returned to that server.
func looksStintPrefix(s string) bool {
	if i := strings.Index(s, "~"); i > 0 {
		n := s[i+1:]
		if n == "" {
			return false
		}
		for _, c := range n {
			if c < '0' || c > '9' {
				return false
			}
		}
		s = s[:i]
	}
	return looksUUID(s)
}

func looksUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
