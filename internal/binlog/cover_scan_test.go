// Package binlog provides module-level functionality for binlog.
// input: crafted binlog segments with timed events and MySQL GTID bodies
// output: assertions that ScanCover reports event offsets, copied-event times, and GTID identity, and skips the format description
// pos: unit coverage for the coverage scan
// note: if this file changes, update this header and module README.md.
package binlog

import (
	"bytes"
	"testing"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"
)

func TestScanCover_TimesAndGTIDOffsets(t *testing.T) {
	when := time.Date(2024, 1, 1, 1, 2, 3, 0, time.UTC)
	raw, second := gtidFile(t, when,
		gtidPiece{seq: 1, logPos: 9000},
		gtidPiece{seq: 2, logPos: 9500},
	)
	got, err := ScanCover(bytes.NewReader(raw), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.End != uint64(len(raw)) || len(got.Events) != 2 {
		t.Fatalf("end %d events %d len %d", got.End, len(got.Events), len(raw))
	}
	if got.Events[1].Offset != second || got.Events[1].Seq != 2 || got.Events[1].UUID != "3e11fa47-71ca-11e1-9e33-c80aa9429562" {
		t.Fatalf("second %+v want offset %d", got.Events[1], second)
	}
	if !got.Events[1].Counts || !got.Events[1].When.Equal(when) || got.Events[0].End != second {
		t.Fatalf("events %+v", got.Events)
	}
	header := typedBinlog(t,
		typedEvent{goreplication.FORMAT_DESCRIPTION_EVENT, when},
		typedEvent{goreplication.QUERY_EVENT, when.Add(time.Minute)},
	)
	got, err = ScanCover(bytes.NewReader(header), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 || got.Events[0].Counts || !got.Events[1].Counts || !got.Events[1].When.Equal(when.Add(time.Minute)) {
		t.Fatalf("header events %+v", got.Events)
	}
}
