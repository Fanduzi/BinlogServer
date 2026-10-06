// Package binlog provides module-level functionality for binlog.
// input: a task data directory and on-disk binlog segment bytes
// output: the source file and end log_pos of the last complete event in a task directory or in one segment, the cursor that finds where that event ends (skipping an artificial event whose end log_pos is 0), the start and end binlog positions of the contiguous event chain in one segment, whether a segment is only magic or a log_pos 0 header, and the next file named by a sealed rotate
// pos: shared durable-position reader used by the replication runner and the task resume API
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

var durableMagic = []byte{0xfe, 'b', 'i', 'n'}

type durableSegment struct {
	source string
	seq    uint64
	epoch  int64
	path   string
}

// DurableResume is the source file and end log_pos of the last complete event
// in the highest open segment. Sealed names are not resume points. File size
// is not that position. A segment with no complete event is skipped.
// ok is false when none exists.
func DurableResume(dataDir, taskID string) (file string, pos uint32, ok bool) {
	dir, okDir := durableTaskDir(dataDir, taskID)
	if !okDir {
		return "", 0, false
	}
	return DurableResumeDir(dir)
}

// DurableResumeDir is DurableResume for a segment directory the catalog already
// recorded. The directory is used as given; it is not joined with a task id.
func DurableResumeDir(taskDir string) (file string, pos uint32, ok bool) {
	taskDir = strings.TrimSpace(taskDir)
	if taskDir == "" {
		return "", 0, false
	}
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return "", 0, false
	}
	cands := make([]durableSegment, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		named, ok := ClassifySegment(entry.Name())
		if !ok || !named.Open {
			continue
		}
		cands = append(cands, durableSegment{
			source: named.Source,
			seq:    named.Seq,
			epoch:  named.Epoch,
			path:   filepath.Join(taskDir, entry.Name()),
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].seq != cands[j].seq {
			return cands[i].seq > cands[j].seq
		}
		return cands[i].epoch > cands[j].epoch
	})
	for _, seg := range cands {
		pos, _, _, ok := DurableCursor(seg.path)
		if !ok {
			continue
		}
		return seg.source, pos, true
	}
	return "", 0, false
}

// DurableCursor walks complete events. pos is the last event's end log_pos.
// An event whose end log_pos is 0 is artificial (a source restart's Rotate,
// or a format description that was not given a position). It is not a resume
// position. When a real event precedes it, pos and end stay on that event so
// the artificial bytes can be dropped. end is the file offset of the first
// byte after that event: a torn tail, or a trailing artificial event, sits
// at or after end. A segment with no event whose end log_pos is greater than
// 0 is not a resume point.
func DurableCursor(path string) (pos uint32, end int64, size int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, 0, false
	}
	size = info.Size()
	if size < 4 {
		return 0, 0, size, false
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != string(durableMagic) {
		return 0, 0, size, false
	}
	offset := int64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	var lastPos uint32
	var lastEnd int64
	found := false
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if eventSize < int64(goreplication.EventHeaderSize) || offset+eventSize > size {
			break
		}
		if _, err := f.Seek(eventSize-int64(goreplication.EventHeaderSize), io.SeekCurrent); err != nil {
			break
		}
		offset += eventSize
		if logPos == 0 {
			continue
		}
		lastPos = logPos
		lastEnd = offset
		found = true
	}
	if !found {
		return 0, 0, size, false
	}
	return lastPos, lastEnd, size, true
}

// EventSpan is the binlog position span of the contiguous event chain that ends
// at the last complete event whose end log_pos is greater than 0. start is
// that chain's first event position (end log_pos minus the event size). end is
// the last event's end log_pos. An event whose end log_pos is 0 is ignored.
// When the next event starts after the chain, the chain restarts there, so a
// leading format description does not pull start back across a gap. ok is
// false when the file has no such event.
func EventSpan(path string) (start, end uint32, ok bool) {
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
	if size < 4 {
		return 0, 0, false
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != string(durableMagic) {
		return 0, 0, false
	}
	offset := int64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	var chainStart, chainEnd uint32
	found := false
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if eventSize < int64(goreplication.EventHeaderSize) || offset+eventSize > size {
			break
		}
		if _, err := f.Seek(eventSize-int64(goreplication.EventHeaderSize), io.SeekCurrent); err != nil {
			break
		}
		offset += eventSize
		if logPos == 0 || int64(logPos) < eventSize {
			continue
		}
		evStart := logPos - uint32(eventSize)
		if !found || evStart > chainEnd {
			chainStart = evStart
			chainEnd = logPos
			found = true
			continue
		}
		if evStart < chainStart {
			chainStart = evStart
		}
		if logPos > chainEnd {
			chainEnd = logPos
		}
	}
	if !found {
		return 0, 0, false
	}
	return chainStart, chainEnd, true
}

// PreambleOnly reports that path has no resume event. DurableResume skips it.
// The 4-byte magic header alone qualifies. A header of complete events whose
// end log_pos is 0 also qualifies when the file ends on an event boundary.
// A torn tail does not. An event whose end log_pos is greater than 0 does not.
func PreambleOnly(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	size := info.Size()
	if size < 4 {
		return false
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != string(durableMagic) {
		return false
	}
	if size == 4 {
		return true
	}
	offset := int64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := io.ReadFull(f, hdr); err != nil {
			return false
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if eventSize < int64(goreplication.EventHeaderSize) || offset+eventSize > size || logPos != 0 {
			return false
		}
		if _, err := f.Seek(eventSize-int64(goreplication.EventHeaderSize), io.SeekCurrent); err != nil {
			return false
		}
		offset += eventSize
	}
	return offset == size
}

// LastRotateTarget is the next file named by the last complete event when
// that event is a rotate. endPos is that event's end log_pos. A torn tail
// is ignored. ok is false when the last complete event is not a rotate.
// A CRC32 trailer, when the format description says so, is not part of the name.
func LastRotateTarget(path string) (nextFile string, nextPos uint32, endPos uint32, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, 0, false
	}
	size := info.Size()
	if size < 4 {
		return "", 0, 0, false
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != string(durableMagic) {
		return "", 0, 0, false
	}
	offset := int64(4)
	hdr := make([]byte, goreplication.EventHeaderSize)
	crc32 := false
	var lastType byte
	var lastBody []byte
	var lastPos uint32
	seen := false
	for offset+int64(goreplication.EventHeaderSize) <= size {
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		eventSize := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		logPos := binary.LittleEndian.Uint32(hdr[13:17])
		if eventSize < int64(goreplication.EventHeaderSize) || offset+eventSize > size {
			break
		}
		bodyLen := eventSize - int64(goreplication.EventHeaderSize)
		eventType := hdr[4]
		var body []byte
		keep := eventType == byte(goreplication.FORMAT_DESCRIPTION_EVENT) || eventType == byte(goreplication.ROTATE_EVENT)
		if keep && bodyLen > 0 && bodyLen <= 1<<20 {
			body = make([]byte, bodyLen)
			if _, err := io.ReadFull(f, body); err != nil {
				break
			}
		} else if _, err := f.Seek(bodyLen, io.SeekCurrent); err != nil {
			break
		}
		if eventType == byte(goreplication.FORMAT_DESCRIPTION_EVENT) && len(body) >= 57 {
			fde := &goreplication.FormatDescriptionEvent{}
			if err := fde.Decode(body); err == nil && fde.ChecksumAlgorithm == goreplication.BINLOG_CHECKSUM_ALG_CRC32 {
				crc32 = true
			}
		}
		offset += eventSize
		lastType = eventType
		lastPos = logPos
		seen = true
		if eventType == byte(goreplication.ROTATE_EVENT) {
			lastBody = body
		} else {
			lastBody = nil
		}
	}
	if !seen || lastType != byte(goreplication.ROTATE_EVENT) || len(lastBody) < 8 {
		return "", 0, 0, false
	}
	body := lastBody
	if crc32 && len(body) >= 4 {
		body = body[:len(body)-goreplication.BinlogChecksumLength]
	}
	if len(body) < 8 {
		return "", 0, 0, false
	}
	rot := &goreplication.RotateEvent{}
	if err := rot.Decode(body); err != nil {
		return "", 0, 0, false
	}
	name := strings.TrimRight(string(rot.NextLogName), "\x00")
	name = strings.TrimSpace(name)
	if !rotateNameOK(name) {
		return "", 0, 0, false
	}
	pos := uint32(rot.Position)
	if pos == 0 {
		pos = 4
	}
	return name, pos, lastPos, true
}

func rotateNameOK(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func durableTaskDir(dataDir, taskID string) (string, bool) {
	dataDir = strings.TrimSpace(dataDir)
	taskID = strings.TrimSpace(taskID)
	if dataDir == "" || taskID == "" || taskID != filepath.Base(taskID) || strings.HasPrefix(taskID, ".") {
		return "", false
	}
	return filepath.Join(dataDir, taskID), true
}
