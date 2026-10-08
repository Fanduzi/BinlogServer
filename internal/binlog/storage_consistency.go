// Package binlog provides module-level functionality for binlog.
// input: a task directory of binlog segments and the checkpoint gtid_set
// output: a storage problem when one segment rotates to its own or an older file, or when an event position goes backwards inside that segment; out-of-order GTIDs, a hole in one UUID, and a missing file are not a problem
// pos: detect a backup already damaged by a stale GTID re-dump before another start drops more transactions
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

// storageRecoveryText is the operator step for a backup that already lost transactions.
// The missing transactions are not in these files. Rewriting the checkpoint cannot put them back.
const storageRecoveryText = "Stop the task and do not start it again. Do not restore from these files. Create a new task from a GTID the source still has. The checkpoint gtid_set is not proof of what was stored. Leave these files for forensics. No in-place rewrite puts the missing transactions back."

// StorageProblem is one on-disk disagreement between the checkpoint and the segments.
// Message names what was found and the recovery step.
type StorageProblem struct {
	Message string
}

// DetectStorageProblem scans taskDir for the re-dump damage signature.
// A rotate to the segment's own file or an older one, or an event position
// that goes backwards inside one segment, is a problem.
// GTID order is not. A replica can commit out of order, interleave several
// UUIDs, or omit a number that lives in a later segment, the start set, or
// gtid_purged. checkpointGTID and flavor stay in the signature so callers
// can pass the stored set; they do not change this result. A missing
// directory is not a problem.
func DetectStorageProblem(taskDir, checkpointGTID, flavor string) (StorageProblem, bool) {
	taskDir = strings.TrimSpace(taskDir)
	if taskDir == "" {
		return StorageProblem{}, false
	}
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return StorageProblem{}, false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		named, ok := ClassifySegment(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(taskDir, entry.Name())
		if msg, bad := segmentDamage(path, entry.Name(), named.Source); bad {
			return StorageProblem{Message: msg}, true
		}
	}
	return StorageProblem{}, false
}

func segmentDamage(path, diskName, segmentSource string) (string, bool) {
	if next, stray := strayRotate(path, segmentSource); stray {
		return "segment " + diskName + " contains a rotate to " + next + ", which is not a newer binlog file. " + storageRecoveryText, true
	}
	if pos, prev, back := positionRegresses(path); back {
		return "event end pos " + strconv.FormatUint(uint64(pos), 10) + " is behind earlier end pos " + strconv.FormatUint(uint64(prev), 10) + " in " + diskName + ". " + storageRecoveryText, true
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
