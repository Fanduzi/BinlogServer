// Package assurance provides module-level functionality for assurance.
// input: the live ledger fixture, catalog rows copied from it, and an in-memory object store
// output: proof that one pass reports a deleted object, a one-byte object change and a one-byte local change, that a clean chain reports nothing, and that the byte budget binds
// pos: acceptance tests for the phase 3 verifier (docs/design/0009-phase3-verifier.md)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

type memObjects struct {
	obj     map[string][]byte
	opens   int
	headErr error
}

func (m *memObjects) Head(_ context.Context, key string) (int64, error) {
	if m.headErr != nil {
		return 0, m.headErr
	}
	b, ok := m.obj[key]
	if !ok {
		return 0, os.ErrNotExist
	}
	return int64(len(b)), nil
}

func (m *memObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.opens++
	b, ok := m.obj[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func verifyRows(t *testing.T, f *fixture, uploadedAt time.Time) []VerifyRow {
	t.Helper()
	var rows []VerifyRow
	for _, row := range f.in.Rows {
		if !row.Sealed {
			continue
		}
		p := filepath.Join(f.dir, row.Name)
		facts, err := binlog.ReadSealFacts(p)
		if err != nil {
			t.Fatal(err)
		}
		start, end, ok := binlog.EventSpan(p)
		if !ok {
			t.Fatalf("%s has no event span", row.Name)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		contents, err := binlog.ReadSealedSegmentContents(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, VerifyRow{
			Name: row.Name, Sealed: true, Size: facts.Size, StartPos: start, EndPos: end,
			SHA256: facts.SHA256, GTIDSet: contents.GTIDs.String(),
			UploadState: "UPLOADED", ObjectKey: row.ObjectKey, UploadedAt: uploadedAt,
		})
	}
	return rows
}

func objectsFromDir(t *testing.T, f *fixture) *memObjects {
	t.Helper()
	m := &memObjects{obj: map[string][]byte{}}
	for _, row := range f.in.Rows {
		if !row.Sealed {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.dir, row.Name))
		if err != nil {
			t.Fatal(err)
		}
		m.obj[row.ObjectKey] = b
	}
	return m
}

func chainOf(f *fixture) *Chain {
	f.in.Local = binlog.LedgerAt(f.dir)
	return ChainOf(f.in)
}

func hasFinding(fs []Finding, kind, subject string) bool {
	for _, f := range fs {
		if f.Kind == kind && f.Subject == subject {
			return true
		}
	}
	return false
}

func TestVerify_CleanChainHasNoFindings(t *testing.T) {
	f := live(t)
	if vs := f.check(t); len(vs) != 0 {
		t.Fatalf("fixture chain %v", vs)
	}
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	objs := objectsFromDir(t, f)
	budget := NewByteBudget(1 << 30)
	rep, err := Verify(context.Background(), f.dir, rows, objs, chainOf(f), Options{Now: testNow, SamplePercent: 100, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("findings %+v", rep.Findings)
	}
	if budget.Charged() < 15000 {
		t.Fatalf("budget charged %d bytes", budget.Charged())
	}
	for _, row := range rows {
		if _, ok := rep.VerifiedAt[row.Name]; !ok {
			t.Fatalf("last_verified_at missing for %s", row.Name)
		}
	}
}

func TestVerify_ReportsDeletedObjectFlippedObjectAndFlippedLocal(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	objs := objectsFromDir(t, f)
	opt := Options{Now: testNow, SamplePercent: 100}
	clean, err := Verify(context.Background(), f.dir, rows, objs, chainOf(f), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Findings) != 0 {
		t.Fatalf("clean pass %+v", clean.Findings)
	}
	const deleted = "mysql-bin.000003"
	const flippedObj = "mysql-bin.000004"
	const flippedLocal = "mysql-bin.000005"
	deletedBytes := append([]byte(nil), objs.obj["t/"+deleted]...)
	objectBytes := append([]byte(nil), objs.obj["t/"+flippedObj]...)
	delete(objs.obj, "t/"+deleted)
	flipStored(t, objs, "t/"+flippedObj)
	localPath := filepath.Join(f.dir, flippedLocal)
	original, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	flipped := append([]byte(nil), original...)
	flipped[len(flipped)/2] ^= 0xff
	if err := os.WriteFile(localPath, flipped, 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := Verify(context.Background(), f.dir, rows, objs, chainOf(f), opt)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(bad.Findings, KindObjectMissing, deleted) {
		t.Fatalf("deleted object not reported: %+v", bad.Findings)
	}
	if !hasFinding(bad.Findings, KindObjectCorrupt, flippedObj) {
		t.Fatalf("flipped object not reported: %+v", bad.Findings)
	}
	if !hasFinding(bad.Findings, KindVerifyFailed, flippedLocal) {
		t.Fatalf("flipped local segment not reported: %+v", bad.Findings)
	}
	if _, ok := bad.VerifiedAt[deleted]; ok {
		t.Fatal("deleted segment advanced last_verified_at")
	}
	objs.obj["t/"+deleted] = deletedBytes
	objs.obj["t/"+flippedObj] = objectBytes
	if err := os.WriteFile(localPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	binlog.ForgetFile(localPath)
	good, err := Verify(context.Background(), f.dir, rows, objs, chainOf(f), opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []Finding{
		{Kind: KindObjectMissing, Subject: deleted},
		{Kind: KindObjectCorrupt, Subject: flippedObj},
		{Kind: KindVerifyFailed, Subject: flippedLocal},
	} {
		if !hasFinding(Disappeared(bad.Findings, good), want.Kind, want.Subject) {
			t.Fatalf("%s %s still open: bad=%+v good=%+v", want.Kind, want.Subject, bad.Findings, good.Findings)
		}
	}
}

func flipStored(t *testing.T, m *memObjects, key string) {
	t.Helper()
	b := append([]byte(nil), m.obj[key]...)
	if len(b) == 0 {
		t.Fatalf("no object %s", key)
	}
	b[len(b)/2] ^= 0xff
	m.obj[key] = b
}

func TestVerify_RecentUploadIsHashedWhenSampleIsOff(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	rows = rows[:1]
	objs := objectsFromDir(t, f)
	flipStored(t, objs, rows[0].ObjectKey)
	rep, err := Verify(context.Background(), f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(rep.Findings, KindObjectCorrupt, rows[0].Name) {
		t.Fatalf("recent upload not hashed: %+v opens will not tell us; findings %+v", rows[0].Name, rep.Findings)
	}
	if objs.opens == 0 {
		t.Fatal("object body was not read")
	}
}

func TestVerify_OldObjectOutsideSampleIsNotRead(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-48*time.Hour))
	rows = rows[:1]
	objs := objectsFromDir(t, f)
	flipStored(t, objs, rows[0].ObjectKey)
	rep, err := Verify(context.Background(), f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: -1})
	if err != nil {
		t.Fatal(err)
	}
	if hasFinding(rep.Findings, KindObjectCorrupt, rows[0].Name) {
		t.Fatalf("old object was judged by body: %+v", rep.Findings)
	}
	if objs.opens != 0 {
		t.Fatalf("old object body was read %d times", objs.opens)
	}
	delete(objs.obj, rows[0].ObjectKey)
	rep, err = Verify(context.Background(), f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(rep.Findings, KindObjectMissing, rows[0].Name) {
		t.Fatalf("HEAD miss not reported: %+v", rep.Findings)
	}
}

func TestDisappeared_SkipsIncompleteObject(t *testing.T) {
	prev := []Finding{{Kind: KindObjectMissing, Subject: "mysql-bin.000003", Severity: SeverityCritical}}
	got := Disappeared(prev, Report{ObjectIncomplete: true})
	if len(got) != 0 {
		t.Fatalf("closed an object finding without a complete object pass: %+v", got)
	}
}

func TestSampleObject_IsStableWithinAPeriod(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	n := 0
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("mysql-bin.%06d", i)
		a := SampleObject(name, time.Time{}, now, DefaultRecent, DefaultPeriod, 5)
		b := SampleObject(name, time.Time{}, now, DefaultRecent, DefaultPeriod, 5)
		if a != b {
			t.Fatalf("%s was not stable", name)
		}
		if a {
			n++
		}
	}
	if n < 50 || n > 150 {
		t.Fatalf("sampled %d of 2000, want about 100", n)
	}
	later := now.Add(48 * time.Hour)
	changed := 0
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("mysql-bin.%06d", i)
		if SampleObject(name, time.Time{}, now, DefaultRecent, DefaultPeriod, 5) !=
			SampleObject(name, time.Time{}, later, DefaultRecent, DefaultPeriod, 5) {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("the next period drew the same set")
	}
	if !SampleObject("mysql-bin.000001", now.Add(time.Hour), now, DefaultRecent, DefaultPeriod, -1) {
		t.Fatal("a future upload was not treated as recent")
	}
}

func TestByteBudget_IsRespected(t *testing.T) {
	b := NewByteBudget(1000)
	start := time.Now()
	if err := b.Wait(context.Background(), 2500); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 1200*time.Millisecond {
		t.Fatalf("2500 bytes at 1000 B/s took %s", elapsed)
	}
	if b.Charged() != 2500 {
		t.Fatalf("charged %d", b.Charged())
	}
}
