// Package assurance provides module-level functionality for assurance.
// input: copies of the segment-ledger test directories, mutated the way past incidents left them (#223 overlap, #209 orphan row, #173 unverified upload), with catalog rows and an object listing built from them
// output: proof that Check reports each invariant violation kind (I1-I7) on at least two directories and nothing on a healthy chain
// pos: acceptance tests for the backup chain (ADR 0009 phase 1, P1-1)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

const (
	liveUUID  = "e2e732df-c5fa-11f1-9e48-822b383dbcd0"
	liveStart = liveUUID + ":1-9"
	liveCP    = liveUUID + ":1-184"
)

var testNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// fixture is a task directory plus the catalog and object listing that
// match it when nothing is wrong.
type fixture struct {
	dir string
	in  Inputs
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		in, err := os.Open(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(filepath.Join(dst, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
	return dst
}

// live is the healthy live chain: four sealed UPLOADED+match segments with
// their objects, and the open tail.
func live(t *testing.T) *fixture {
	t.Helper()
	dir := copyDir(t, filepath.Join("..", "binlog", "testdata", "ledger", "live"))
	f := &fixture{dir: dir}
	f.in = Inputs{
		Flavor:        "mysql",
		StartGTID:     liveStart,
		Checkpoint:    Checkpoint{GTIDSet: liveCP},
		ObjectStorage: true,
		UploadGrace:   15 * time.Minute,
		Objects:       map[string]int64{},
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if named, ok := binlog.ClassifySegment(e.Name()); ok {
			names = append(names, e.Name())
			info, _ := e.Info()
			row := Row{Name: e.Name(), Source: named.Source, Epoch: named.Epoch, Size: info.Size(),
				CreatedAt: testNow.Add(-2 * time.Hour)}
			if !named.Open {
				row.Sealed = true
				row.SealedAt = testNow.Add(-time.Hour)
				row.UploadState = "UPLOADED"
				row.Checksum = "match"
				row.ObjectKey = "t/" + e.Name()
				f.in.Objects[row.ObjectKey] = info.Size()
			}
			f.in.Rows = append(f.in.Rows, row)
		}
	}
	sort.Strings(names)
	f.in.Order = names
	return f
}

func (f *fixture) row(t *testing.T, name string) *Row {
	t.Helper()
	for i := range f.in.Rows {
		if f.in.Rows[i].Name == name {
			return &f.in.Rows[i]
		}
	}
	t.Fatalf("no row %s", name)
	return nil
}

func (f *fixture) dropRow(name string) {
	out := f.in.Rows[:0]
	for _, r := range f.in.Rows {
		if r.Name != name {
			out = append(out, r)
		}
	}
	f.in.Rows = out
}

func (f *fixture) dropOrder(name string) {
	out := f.in.Order[:0]
	for _, n := range f.in.Order {
		if n != name {
			out = append(out, n)
		}
	}
	f.in.Order = out
}

func (f *fixture) removeFile(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
		t.Fatal(err)
	}
}

// corruptSize sets the high byte of the second event's size in name.
func (f *fixture) corruptSize(t *testing.T, name string, high byte) {
	t.Helper()
	p := filepath.Join(f.dir, name)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	off := 4 + int(binary.LittleEndian.Uint32(raw[4+9:4+13]))
	raw[off+12] = high
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// regress appends the body of src to dst: the v0.5.57 re-dump signature.
func (f *fixture) regress(t *testing.T, dst, src string, n int64) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, src))
	if err != nil {
		t.Fatal(err)
	}
	raw = raw[4:]
	if int64(len(raw)) > n {
		raw = raw[:n]
	}
	out, err := os.OpenFile(filepath.Join(f.dir, dst), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := out.Write(raw); err != nil {
		t.Fatal(err)
	}
	info, _ := out.Stat()
	f.row(t, dst).Size = info.Size()
	f.in.Objects["t/"+dst] = info.Size()
}

// duplicate copies src to a new sealed epoch name and puts it on the chain
// right after src, like the #223 double seal.
func (f *fixture) duplicate(t *testing.T, src, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, src))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	named, _ := binlog.ClassifySegment(name)
	f.in.Rows = append(f.in.Rows, Row{Name: name, Source: named.Source, Epoch: named.Epoch, Sealed: true,
		Size: int64(len(raw)), UploadState: "UPLOADED", Checksum: "match", ObjectKey: "t/" + name,
		CreatedAt: testNow.Add(-2 * time.Hour), SealedAt: testNow.Add(-time.Hour)})
	f.in.Objects["t/"+name] = int64(len(raw))
	var order []string
	for _, n := range f.in.Order {
		order = append(order, n)
		if n == src {
			order = append(order, name)
		}
	}
	f.in.Order = order
}

func (f *fixture) check(t *testing.T) []Violation {
	t.Helper()
	f.in.Local = binlog.LedgerAt(f.dir)
	return ChainOf(f.in).Check(testNow)
}

func has(vs []Violation, kind Kind, segment string) bool {
	for _, v := range vs {
		if v.Kind != kind {
			continue
		}
		if segment == "" {
			return true
		}
		if len(v.Segments) > 0 && v.Segments[0] == segment {
			return true
		}
	}
	return false
}

func kinds(vs []Violation) string {
	var out []string
	for _, v := range vs {
		out = append(out, string(v.Kind)+"@"+strings.Join(v.Segments, ","))
	}
	return strings.Join(out, " ")
}

func TestHealthyChainHasNoViolation(t *testing.T) {
	if vs := live(t).check(t); len(vs) != 0 {
		t.Fatalf("healthy chain: %s", kinds(vs))
	}
	// No object storage and no listing: nothing to say about objects.
	f := live(t)
	f.in.ObjectStorage = false
	f.in.Objects = nil
	if vs := f.check(t); len(vs) != 0 {
		t.Fatalf("healthy chain without object storage: %s", kinds(vs))
	}
}

func TestCheckIsDeterministic(t *testing.T) {
	f := live(t)
	f.removeFile(t, "mysql-bin.000004")
	f.dropOrder("mysql-bin.000004")
	a := kinds(f.check(t))
	b := kinds(f.check(t))
	if a == "" || a != b {
		t.Fatalf("Check not deterministic or empty: %q vs %q", a, b)
	}
}

type violationCase struct {
	name    string
	kind    Kind
	segment string
	mutate  func(*testing.T, *fixture)
}

// leftover writes an open segment of raw bytes after src on the order.
func (f *fixture) leftover(t *testing.T, src, name string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, n := range f.in.Order {
		order = append(order, n)
		if n == src {
			order = append(order, name)
		}
	}
	f.in.Order = order
}

func violationCases() []violationCase {
	return []violationCase{
		// I4 (P2-3): a stale open copy holding the same events as a sealed one (#372 openstale).
		{"I4/open-copy-of-sealed-000006", I4Overlap, "mysql-bin.000006.open.e4", func(t *testing.T, f *fixture) {
			f.duplicate(t, "mysql-bin.000006", "mysql-bin.000006.open.e4")
		}},
		// I4 (P2-3, #357): an open leftover with only the magic, or shorter than one event header.
		{"I4/empty-leftover-magic", I4EmptyLeftover, "mysql-bin.000005.open.e3", func(t *testing.T, f *fixture) {
			f.leftover(t, "mysql-bin.000005", "mysql-bin.000005.open.e3", []byte{0xfe, 'b', 'i', 'n'})
		}},
		{"I4/empty-leftover-short", I4EmptyLeftover, "mysql-bin.000004.open.e2", func(t *testing.T, f *fixture) {
			f.leftover(t, "mysql-bin.000004", "mysql-bin.000004.open.e2", []byte{0xfe, 'b', 'i', 'n', 1, 2, 3, 4, 5, 6})
		}},
		// I1: the v0.5.57 re-dump signature inside a sealed segment.
		{"I1/regress-into-next", I1Regression, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.regress(t, "mysql-bin.000005", "mysql-bin.000004", 2000)
		}},
		// I1: a corrupt event size in a sealed segment (#358/#361).
		{"I1/unreadable-size-4GiB", I1Unreadable, "mysql-bin.000004", func(t *testing.T, f *fixture) {
			f.corruptSize(t, "mysql-bin.000004", 0xff)
		}},
		{"I1/unreadable-size-past-eof", I1Unreadable, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.corruptSize(t, "mysql-bin.000005", 0x3f)
		}},
		{"I2/order-leaves-out-local", I2OrderInvalid, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.dropOrder("mysql-bin.000005")
		}},
		{"I2/order-goes-back", I2OrderInvalid, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.in.Order = []string{"mysql-bin.000003", "mysql-bin.000004", "mysql-bin.000006", "mysql-bin.000005", "mysql-bin.000007.open.e1"}
		}},
		{"I1/regress-into-self", I1Regression, "mysql-bin.000003", func(t *testing.T, f *fixture) {
			f.regress(t, "mysql-bin.000003", "mysql-bin.000003", 5000)
		}},
		// I2: a source file missing between two stored ones.
		{"I2/missing-000004", I2IndexGap, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.removeFile(t, "mysql-bin.000004")
			f.dropRow("mysql-bin.000004")
			f.dropOrder("mysql-bin.000004")
		}},
		{"I2/missing-000005", I2IndexGap, "mysql-bin.000006", func(t *testing.T, f *fixture) {
			f.removeFile(t, "mysql-bin.000005")
			f.dropRow("mysql-bin.000005")
			f.dropOrder("mysql-bin.000005")
		}},
		// I3: the transactions of a lost segment are a hole between neighbours.
		{"I3/hole-000004", I3GTIDHole, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.removeFile(t, "mysql-bin.000004")
			f.dropRow("mysql-bin.000004")
			f.dropOrder("mysql-bin.000004")
		}},
		{"I3/hole-000005", I3GTIDHole, "mysql-bin.000006", func(t *testing.T, f *fixture) {
			f.removeFile(t, "mysql-bin.000005")
			f.dropRow("mysql-bin.000005")
			f.dropOrder("mysql-bin.000005")
		}},
		// I4: #223, the same file sealed twice and both on the chain.
		{"I4/double-seal-000004", I4Overlap, "mysql-bin.000004.sealed.e1", func(t *testing.T, f *fixture) {
			f.duplicate(t, "mysql-bin.000004", "mysql-bin.000004.sealed.e1")
		}},
		{"I4/double-seal-000006", I4Overlap, "mysql-bin.000006.sealed.e2", func(t *testing.T, f *fixture) {
			f.duplicate(t, "mysql-bin.000006", "mysql-bin.000006.sealed.e2")
		}},
		// I5: the checkpoint claims transactions no segment holds.
		{"I5/beyond-stored", I5CheckpointAhead, "", func(t *testing.T, f *fixture) {
			f.in.Checkpoint.GTIDSet = liveUUID + ":1-500"
		}},
		{"I5/foreign-uuid", I5CheckpointAhead, "", func(t *testing.T, f *fixture) {
			f.in.Checkpoint.GTIDSet = liveCP + ",3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5"
		}},
		// I6: the row says there is a local copy and there is none.
		{"I6/missing-local-000003", I6MissingLocal, "mysql-bin.000003", func(t *testing.T, f *fixture) {
			r := f.row(t, "mysql-bin.000003")
			r.UploadState, r.Checksum = "UPLOAD_FAILED", ""
			delete(f.in.Objects, r.ObjectKey)
			f.removeFile(t, "mysql-bin.000003")
		}},
		{"I6/missing-local-000006", I6MissingLocal, "mysql-bin.000006", func(t *testing.T, f *fixture) {
			f.in.ObjectStorage = false
			f.in.Objects = nil
			r := f.row(t, "mysql-bin.000006")
			r.UploadState, r.Checksum, r.ObjectKey = "LOCAL_ONLY", "", ""
			f.removeFile(t, "mysql-bin.000006")
		}},
		// I6: UPLOADED, the local file is there, the object is gone.
		{"I6/missing-object-000004", I6MissingObject, "mysql-bin.000004", func(t *testing.T, f *fixture) {
			delete(f.in.Objects, "t/mysql-bin.000004")
		}},
		{"I6/missing-object-000006", I6MissingObject, "mysql-bin.000006", func(t *testing.T, f *fixture) {
			delete(f.in.Objects, "t/mysql-bin.000006")
		}},
		// I6: #209, a row whose bytes are nowhere.
		{"I6/orphan-row-000005", I6OrphanRow, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.removeFile(t, "mysql-bin.000005")
			delete(f.in.Objects, "t/mysql-bin.000005")
		}},
		{"I6/orphan-row-off-chain", I6OrphanRow, "mysql-bin.000002", func(t *testing.T, f *fixture) {
			f.in.Rows = append(f.in.Rows, Row{Name: "mysql-bin.000002", Source: "mysql-bin.000002", Epoch: -1, Sealed: true,
				Size: 100, UploadState: "UPLOADED", Checksum: "match", ObjectKey: "t/mysql-bin.000002", SealedAt: testNow.Add(-3 * time.Hour)})
		}},
		// I6: a sealed segment on disk and on the chain with no catalog row.
		{"I6/missing-row-000003", I6MissingRow, "mysql-bin.000003", func(t *testing.T, f *fixture) {
			f.dropRow("mysql-bin.000003")
		}},
		{"I6/missing-row-000006", I6MissingRow, "mysql-bin.000006", func(t *testing.T, f *fixture) {
			f.dropRow("mysql-bin.000006")
		}},
		// I6: sizes disagree.
		{"I6/size-local", I6SizeMismatch, "mysql-bin.000004", func(t *testing.T, f *fixture) {
			f.row(t, "mysql-bin.000004").Size = 1
			f.in.Objects["t/mysql-bin.000004"] = 1
		}},
		{"I6/size-object", I6SizeMismatch, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			f.in.Objects["t/mysql-bin.000005"] = 7
		}},
		// I7: sealed long ago and still not UPLOADED+match.
		{"I7/late-upload-failed", I7UploadLate, "mysql-bin.000005", func(t *testing.T, f *fixture) {
			r := f.row(t, "mysql-bin.000005")
			r.UploadState, r.Checksum = "UPLOAD_FAILED", ""
		}},
		{"I7/late-local-only", I7UploadLate, "mysql-bin.000006", func(t *testing.T, f *fixture) {
			r := f.row(t, "mysql-bin.000006")
			r.UploadState, r.Checksum = "LOCAL_ONLY", ""
		}},
		// I7: #173, UPLOADED without a matching checksum.
		{"I7/uploaded-mismatch", I7Unverified, "mysql-bin.000003", func(t *testing.T, f *fixture) {
			f.row(t, "mysql-bin.000003").Checksum = "mismatch"
		}},
		{"I7/uploaded-unchecked-local-gone", I7Unverified, "mysql-bin.000004", func(t *testing.T, f *fixture) {
			f.row(t, "mysql-bin.000004").Checksum = ""
			f.removeFile(t, "mysql-bin.000004")
		}},
	}
}

func TestEveryViolationKind(t *testing.T) {
	seen := map[Kind]int{}
	for _, c := range violationCases() {
		t.Run(c.name, func(t *testing.T) {
			f := live(t)
			c.mutate(t, f)
			vs := f.check(t)
			if !has(vs, c.kind, c.segment) {
				t.Fatalf("want %s at %q, got %s", c.kind, c.segment, kinds(vs))
			}
		})
		seen[c.kind]++
	}
	for _, k := range AllKinds() {
		if k == I2SwitchUnrecorded || k == I3StoredHole || k == I3LegacyFailback {
			continue // need a source switch; see TestSwitchKinds
		}
		if seen[k] < 2 {
			t.Errorf("kind %s has %d cases, want at least 2", k, seen[k])
		}
	}
}

// A fresh seal inside the grace is not late.
func TestUploadInsideGraceIsNotLate(t *testing.T) {
	f := live(t)
	r := f.row(t, "mysql-bin.000006")
	r.UploadState, r.Checksum = "UPLOAD_FAILED", ""
	r.SealedAt = testNow.Add(-time.Minute)
	if vs := f.check(t); has(vs, I7UploadLate, "") {
		t.Fatalf("1 minute old seal reported late: %s", kinds(vs))
	}
}

// The order of violations follows the chain.
func TestViolationsFollowChainOrder(t *testing.T) {
	f := live(t)
	f.dropRow("mysql-bin.000006")
	f.dropRow("mysql-bin.000003")
	vs := f.check(t)
	if len(vs) < 2 || vs[0].Segments[0] != "mysql-bin.000003" {
		t.Fatalf("want mysql-bin.000003 first: %s", kinds(vs))
	}
}

const failbackB = "5e86458b-c3b5-11f1-8415-822b383dbcd0"

// A segment from a server the chain has no recorded switch to (I2), and the
// I3 subclasses the tasks planners decide, kept in chain order.
func TestSwitchKinds(t *testing.T) {
	dir := copyDir(t, filepath.Join("..", "binlog", "testdata", "ledger", "failback"))
	bName := failbackB + ".mysql-bin.000001"
	in := Inputs{Flavor: "mysql", Order: []string{"mysql-bin.000002", bName, "mysql-bin.000003.open.e1"}, Local: binlog.LedgerAt(dir)}
	for _, sw := range [][]Switch{nil, {{Identity: "5b4bc6b5-c3b5-11f1-84bd-822b383dbcd0"}}} {
		in.Switches = sw
		if vs := ChainOf(in).Check(testNow); !has(vs, I2SwitchUnrecorded, bName) {
			t.Fatalf("switches %v: want I2_SWITCH_UNRECORDED at %s, got %s", sw, bName, kinds(vs))
		}
	}
	in.Switches = []Switch{{Identity: failbackB}}
	if vs := ChainOf(in).Check(testNow); has(vs, I2SwitchUnrecorded, "") {
		t.Fatalf("recorded switch reported: %s", kinds(vs))
	}
	in.Planned = []Violation{
		{Kind: I3LegacyFailback, Segments: []string{"mysql-bin.000003.open.e1"}},
		{Kind: I3StoredHole, Segments: []string{bName}},
	}
	vs := ChainOf(in).Check(testNow)
	if !has(vs, I3StoredHole, bName) || !has(vs, I3LegacyFailback, "mysql-bin.000003.open.e1") {
		t.Fatalf("planned conclusions lost: %s", kinds(vs))
	}
	var order []Kind
	for _, v := range vs {
		if v.Kind == I3StoredHole || v.Kind == I3LegacyFailback {
			order = append(order, v.Kind)
		}
	}
	if len(order) != 2 || order[0] != I3StoredHole {
		t.Fatalf("planned conclusions not in chain order: %v", order)
	}
}

// Every Segment says where it can be read.
func TestSegmentsLocations(t *testing.T) {
	f := live(t)
	delete(f.in.Objects, "t/mysql-bin.000004")
	f.removeFile(t, "mysql-bin.000005")
	f.in.Local = binlog.LedgerAt(f.dir)
	segs := ChainOf(f.in).Segments()
	if len(segs) != 5 {
		t.Fatalf("%d segments", len(segs))
	}
	by := map[string]Segment{}
	for _, s := range segs {
		by[s.Name] = s
	}
	if s := by["mysql-bin.000004"]; !s.Local || s.Object != nil || !s.ObjectListed || s.Contents == nil {
		t.Fatalf("000004 %+v", s)
	}
	if s := by["mysql-bin.000005"]; s.Local || s.Object == nil || s.Contents != nil {
		t.Fatalf("000005 %+v", s)
	}
	if s := by["mysql-bin.000007.open.e1"]; !s.Local || s.Row == nil || !s.Named.Open {
		t.Fatalf("open %+v", s)
	}
}
