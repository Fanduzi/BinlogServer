// Package tasks provides module-level functionality for tasks.
// input: in-memory segment views (catalog row, event span, GTID log) the older window tests build by hand
// output: the RecoveryWindow assurance.WindowOf derives for them, through the same row projection RecoveryWindow uses
// pos: test adapter so the pre-ADR-0009 window tests keep asserting the moved derivation unchanged
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"binlog_server/internal/assurance"
	"binlog_server/internal/binlog"
)

type segmentView struct {
	File     BinlogFile
	Span     EventSpan
	SpanOK   bool
	Log      binlog.SegmentGTIDLog
	Readable bool
}

func assessRecovery(views []segmentView, flavor string, objectStorage bool) (RecoveryWindow, error) {
	return assessRecoveryFrom(views, flavor, objectStorage, "")
}

func assessRecoveryFrom(views []segmentView, flavor string, objectStorage bool, startGTID string) (RecoveryWindow, error) {
	segs := make([]assurance.WindowSegment, 0, len(views))
	for _, v := range views {
		seg := windowSegmentOf(v.File)
		seg.Readable, seg.SpanOK, seg.First, seg.Last, seg.Log = v.Readable, v.SpanOK, v.Span.First, v.Span.Last, v.Log
		segs = append(segs, seg)
	}
	w, err := assurance.WindowOf(segs, flavor, objectStorage, startGTID)
	if err != nil {
		return RecoveryWindow{}, err
	}
	return recoveryWindowOf(w), nil
}
