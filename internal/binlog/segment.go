// Package binlog provides module-level functionality for binlog.
// input: an on-disk binlog segment basename
// output: the source file, numeric index, epoch, and whether the name is an open segment, plus OpenName and SealedName; a rejected name including .takeover-* is neither
// pos: the one segment-name classifier shared by the runner, durable resume, and the tasks disk scan
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"strconv"
	"strings"
)

const (
	openEpochMark   = ".open.e"
	sealedEpochMark = ".sealed.e"
)

// SegmentName is one durable binlog segment name.
// Epoch -1 is a plain sealed name such as mysql-bin.000001.
// Open is true only for name.open.e<epoch>.
// A later seal of a source name that already has a sealed file is name.sealed.e<epoch>.
type SegmentName struct {
	Source string
	Seq    uint64
	Epoch  int64
	Open   bool
}

// ClassifySegment parses a segment basename.
// The replication runner, durable resume, and the tasks disk scan all use this.
func ClassifySegment(name string) (SegmentName, bool) {
	if name == "" || strings.HasPrefix(name, ".") {
		return SegmentName{}, false
	}
	epoch := int64(-1)
	source := name
	open := false
	if idx := strings.LastIndex(name, openEpochMark); idx > 0 {
		n, ok := parseEpochSuffix(name[idx+len(openEpochMark):])
		if !ok {
			return SegmentName{}, false
		}
		source = name[:idx]
		epoch = n
		open = true
	} else if idx := strings.LastIndex(name, sealedEpochMark); idx > 0 {
		n, ok := parseEpochSuffix(name[idx+len(sealedEpochMark):])
		if !ok {
			return SegmentName{}, false
		}
		source = name[:idx]
		epoch = n
	}
	dot := strings.LastIndex(source, ".")
	if dot <= 0 || dot == len(source)-1 {
		return SegmentName{}, false
	}
	seq, err := strconv.ParseUint(source[dot+1:], 10, 64)
	if err != nil || source[:dot] == "" {
		return SegmentName{}, false
	}
	return SegmentName{Source: source, Seq: seq, Epoch: epoch, Open: open}, true
}

// OpenName reports that basename is an open segment.
// A name ClassifySegment rejects is not open.
func OpenName(name string) bool {
	named, ok := ClassifySegment(name)
	return ok && named.Open
}

// SealedName reports that basename is a sealed segment.
// A name ClassifySegment rejects, including .takeover-*, is not a binlog.
func SealedName(name string) bool {
	named, ok := ClassifySegment(name)
	return ok && !named.Open
}

func parseEpochSuffix(text string) (int64, bool) {
	if text == "" || strings.ContainsAny(text, "./\\") {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
