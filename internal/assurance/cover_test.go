// Package assurance provides module-level functionality for assurance.
// input: synthetic chain segments with event times and MySQL GTIDs, plus breaks
// output: coverage inside, outside, and on segment boundaries; a 250-segment prefix is not truncated; full-backup precheck is red on a hole or an overlap
// pos: unit coverage for ADR 0009 phases 4 and 5
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

const coverUUID = "3e11fa47-71ca-11e1-9e33-c80aa9429562"

func TestCover_TimeInsideOutsideAndBoundary(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	segs := []CoverSegment{
		coverSeg("mysql-bin.000001", base, 10, 1),
		coverSeg("mysql-bin.000002", base.Add(100*time.Second), 10, 5),
	}
	// First and last copied event of each segment, and a second strictly between.
	boundary := []time.Time{
		base, base.Add(30 * time.Second),
		base.Add(100 * time.Second), base.Add(130 * time.Second),
	}
	for _, at := range boundary {
		got := mustCoverTime(t, segs, at)
		if !got.Covered || got.StopAt == nil {
			t.Fatalf("boundary %s %+v", at, got)
		}
	}
	inside := base.Add(10 * time.Second)
	got := mustCoverTime(t, segs, inside)
	if !got.Covered || len(got.Segments) != 1 || got.Segments[0].Name != "mysql-bin.000001" {
		t.Fatalf("inside %+v", got)
	}
	if got.StopAt.Pos != 4+2*20 { // two events of 20 bytes, magic already in the offsets
		t.Fatalf("inside stop %+v", got.StopAt)
	}
	gap := base.Add(50 * time.Second)
	got = mustCoverTime(t, segs, gap)
	if !got.Covered || len(got.Segments) != 1 || got.StopAt.Segment != "mysql-bin.000001" {
		t.Fatalf("gap %+v", got)
	}
	if got := mustCoverTime(t, segs, base.Add(-time.Second)); got.Covered || got.Reason != coverBefore {
		t.Fatalf("before %+v", got)
	}
	if got := mustCoverTime(t, segs, base.Add(191*time.Second)); got.Covered || got.Reason != coverAfter {
		t.Fatalf("after %+v", got)
	}
	second := mustCoverTime(t, segs, base.Add(110*time.Second))
	if !second.Covered || len(second.Segments) != 2 || second.StopAt.Segment != "mysql-bin.000002" {
		t.Fatalf("second segment %+v", second)
	}
}

func TestCover_GTIDInsideOutsideAndBoundary(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	segs := []CoverSegment{
		coverSeg("mysql-bin.000001", base, 10, 1),
		coverSeg("mysql-bin.000002", base.Add(time.Minute), 10, 5),
	}
	first := mustCoverGTID(t, segs, coverUUID+":1")
	if !first.Covered || len(first.Segments) != 1 || !strings.Contains(first.Applied, ":1") || strings.Contains(first.Applied, ":2") {
		t.Fatalf("first %+v applied %s", first, first.Applied)
	}
	mid := mustCoverGTID(t, segs, coverUUID+":2")
	if !mid.Covered || len(mid.Segments) != 1 || !strings.Contains(mid.Applied, ":1-2") && !strings.Contains(mid.Applied, ":1:2") {
		t.Fatalf("mid applied %s", mid.Applied)
	}
	last := mustCoverGTID(t, segs, coverUUID+":8")
	if !last.Covered || len(last.Segments) != 2 || last.StopAt.Segment != "mysql-bin.000002" {
		t.Fatalf("last %+v", last)
	}
	missing := mustCoverGTID(t, segs, coverUUID+":99")
	if missing.Covered || missing.Reason != coverGTIDMissing {
		t.Fatalf("missing %+v", missing)
	}
	if _, err := Cover(segs, CoverQuery{GTID: "not-a-gtid"}, nil); err == nil {
		t.Fatal("expected invalid gtid")
	}
}

func TestCover_BreakCutsTheLaterSegmentOnly(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	segs := []CoverSegment{
		coverSeg("mysql-bin.000001", base, 10, 1),
		coverSeg("mysql-bin.000003", base.Add(time.Minute), 10, 5),
	}
	breaks := []Break{{Files: []string{"mysql-bin.000002", "mysql-bin.000001", "mysql-bin.000003"}, Reason: "missing source file mysql-bin.000002 between mysql-bin.000001 and mysql-bin.000003"}}
	early := mustCover(t, segs, CoverQuery{At: &base}, breaks)
	if !early.Covered || len(early.Segments) != 1 {
		t.Fatalf("early %+v", early)
	}
	laterAt := base.Add(time.Minute)
	later := mustCover(t, segs, CoverQuery{At: &laterAt}, breaks)
	if later.Covered || !strings.Contains(later.Reason, "missing source file") {
		t.Fatalf("later %+v", later)
	}
}

func TestCover_DoesNotTruncate(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	segs := make([]CoverSegment, 250)
	for i := range segs {
		segs[i] = coverSeg(fmt.Sprintf("mysql-bin.%06d", i+1), base.Add(time.Duration(i)*time.Minute), 1, int64(i+1))
	}
	at := base.Add(249 * time.Minute)
	got := mustCover(t, segs, CoverQuery{At: &at}, nil)
	if !got.Covered || len(got.Segments) != 250 {
		t.Fatalf("len %d covered %v", len(got.Segments), got.Covered)
	}
	if got.Segments[0].Name != "mysql-bin.000001" || got.Segments[249].Name != "mysql-bin.000250" {
		t.Fatalf("ends %s %s", got.Segments[0].Name, got.Segments[249].Name)
	}
}

func TestFullBackupPrecheck_HoleAndOverlapAreRed(t *testing.T) {
	start := coverUUID + ":1-4"
	applied := coverUUID + ":5-8"
	hole := FullBackupPrecheck(start, applied, coverUUID+":1-3")
	if hole.Result != "red" || !strings.Contains(hole.Description, "hole") {
		t.Fatalf("hole %+v", hole)
	}
	overlap := FullBackupPrecheck(start, applied, coverUUID+":1-6")
	if overlap.Result != "red" || !strings.Contains(overlap.Description, "overlap") {
		t.Fatalf("overlap %+v", overlap)
	}
	both := FullBackupPrecheck(start, applied, coverUUID+":5-6")
	if both.Result != "red" || !strings.Contains(both.Description, "hole") || !strings.Contains(both.Description, "overlap") {
		t.Fatalf("both %+v", both)
	}
	ok := FullBackupPrecheck(start, applied, start)
	if ok.Result != "green" {
		t.Fatalf("green %+v", ok)
	}
	manual := FullBackupPrecheck(start, applied, "")
	if manual.Result != "manual" {
		t.Fatalf("manual %+v", manual)
	}
}

func TestChecksumPrecheck_MismatchIsRed(t *testing.T) {
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	cov := CoverResult{Covered: true, Segments: []CoverageSegment{{Name: "a", Checksum: "mismatch", LocalPath: "/a"}}}
	if got := ChecksumPrecheck(cov, 0, now); got.Result != "red" {
		t.Fatalf("%+v", got)
	}
	fresh := now.Add(-time.Hour)
	cov.Segments[0].Checksum = ""
	cov.Segments[0].LastVerifiedAt = &fresh
	if got := ChecksumPrecheck(cov, 0, now); got.Result != "green" {
		t.Fatalf("fresh %+v", got)
	}
	cov.Segments[0].Checksum = "match"
	cov.Segments[0].LocalPath = ""
	cov.Segments[0].LastVerifiedAt = nil
	if got := ChecksumPrecheck(cov, 0, now); got.Result != "green" {
		t.Fatalf("match %+v", got)
	}
}

func mustCoverTime(t *testing.T, segs []CoverSegment, at time.Time) CoverResult {
	t.Helper()
	return mustCover(t, segs, CoverQuery{At: &at}, nil)
}

func mustCoverGTID(t *testing.T, segs []CoverSegment, gtid string) CoverResult {
	t.Helper()
	got, err := Cover(segs, CoverQuery{GTID: gtid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustCover(t *testing.T, segs []CoverSegment, q CoverQuery, breaks []Break) CoverResult {
	t.Helper()
	got, err := Cover(segs, q, breaks)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// coverSeg builds n copied events of 20 bytes starting at offset 4.
// Sequences start at seq0. Times are base, base+10s, ...
func coverSeg(name string, base time.Time, n int, seq0 int64) CoverSegment {
	seg := CoverSegment{Name: name, Source: name, Readable: true, LocalPath: "/data/" + name, SHA256: "abc", Size: int64(4 + n*20)}
	offset := uint64(4)
	for i := 0; i < n; i++ {
		ev := binlog.CoverEvent{
			When: base.Add(time.Duration(i*10) * time.Second), Offset: offset, End: offset + 20,
			UUID: coverUUID, Seq: seq0 + int64(i), Counts: true,
		}
		seg.Events = append(seg.Events, ev)
		offset += 20
	}
	seg.End = offset
	return seg
}
