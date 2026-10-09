// Package binlog provides module-level functionality for binlog.
// input: a task directory of binlog segments, the checkpoint gtid_set, and the task's start gtid_set
// output: a storage problem when one segment rotates to its own or an older file, or when an event position goes backwards inside that segment, naming that segment, the segments before it that still restore, the MySQL GTIDs the checkpoint lists that no segment holds, and the GTID set a new task starts from; out-of-order GTIDs, a hole in one UUID, and a missing file are not a problem
// pos: detect a backup already damaged by a stale GTID re-dump before another start drops more transactions
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
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

// DetectStorageProblem scans taskDir for the re-dump damage signature.
// A rotate to the segment's own file or an older one, or an event position
// that goes backwards inside one segment, is a problem.
// GTID order is not. A replica can commit out of order, interleave several
// UUIDs, or omit a number that lives in a later segment, the start set, or
// gtid_purged. checkpointGTID and startGTID only describe a problem already
// found: the transactions the checkpoint lists that no segment holds, and
// the set a new task starts from. A missing directory is not a problem.
func DetectStorageProblem(taskDir, checkpointGTID, startGTID, flavor string) (StorageProblem, bool) {
	taskDir = strings.TrimSpace(taskDir)
	if taskDir == "" {
		return StorageProblem{}, false
	}
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return StorageProblem{}, false
	}
	segs := make([]storageSegment, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		named, ok := ClassifySegment(entry.Name())
		if !ok {
			continue
		}
		segs = append(segs, storageSegment{name: entry.Name(), named: named, path: filepath.Join(taskDir, entry.Name())})
	}
	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].named.Seq != segs[j].named.Seq {
			return segs[i].named.Seq < segs[j].named.Seq
		}
		if segs[i].named.Epoch != segs[j].named.Epoch {
			return segs[i].named.Epoch < segs[j].named.Epoch
		}
		return segs[i].name < segs[j].name
	})
	for i, seg := range segs {
		detail, bad := segmentDamage(seg.path, seg.name, seg.named.Source)
		if !bad {
			continue
		}
		problem := StorageProblem{Segment: seg.name, Detail: detail}
		var valid []storageSegment
		for _, prev := range segs[:i] {
			if prev.named.Seq < seg.named.Seq {
				valid = append(valid, prev)
				problem.Valid = append(problem.Valid, prev.name)
			}
		}
		if !isMariaDBStorage(flavor) {
			problem.Missing, problem.Restart = storageRecoverySets(segs, valid, checkpointGTID, startGTID)
		}
		problem.Message = storageMessage(problem)
		return problem, true
	}
	return StorageProblem{}, false
}

type storageSegment struct {
	name  string
	named SegmentName
	path  string
}

func isMariaDBStorage(flavor string) bool {
	return strings.EqualFold(strings.TrimSpace(flavor), "mariadb")
}

// storageRecoverySets is checkpoint minus base minus every stored GTID, and
// base plus the GTIDs of the valid segments. base is the task's start set
// plus the previous-GTIDs header of the first segment, which is what the
// source had executed before the first stored transaction. Restart is empty
// when base is unknown, because the valid segments alone would re-dump the
// source's whole history.
func storageRecoverySets(all, valid []storageSegment, checkpointGTID, startGTID string) (string, string) {
	base := newGTIDIntervals()
	if start, ok := parseGTIDIntervals(startGTID); ok {
		base.addAll(start)
	}
	stored := newGTIDIntervals()
	for i, seg := range all {
		if previous := addSegmentGTIDs(stored, seg.path); i == 0 {
			if prev, ok := parseGTIDIntervals(previous); ok {
				base.addAll(prev)
			}
		}
	}
	missing := ""
	if claimed, ok := parseGTIDIntervals(checkpointGTID); ok {
		claimed.subtract(base)
		claimed.subtract(stored)
		missing = claimed.String()
	}
	if len(base) == 0 {
		return missing, ""
	}
	restart := newGTIDIntervals()
	restart.addAll(base)
	for _, seg := range valid {
		addSegmentGTIDs(restart, seg.path)
	}
	return missing, restart.String()
}

// addSegmentGTIDs adds every GTID event of one segment and returns its
// previous-GTIDs header text.
func addSegmentGTIDs(set gtidIntervals, path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	log, err := ScanSegmentGTIDs(f)
	if err != nil {
		return ""
	}
	for _, ev := range log.Events {
		set.add(ev.UUID, ev.Seq, ev.Seq)
	}
	return log.Previous
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
	if i := strings.Index(name, "."); i > 0 && looksUUID(name[:i]) {
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
