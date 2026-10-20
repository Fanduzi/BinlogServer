// Package assurance provides module-level functionality for assurance.
// input: the live ledger fixture, catalog rows copied from it, and an in-memory object store
// output: proof that one pass reports a deleted object, a one-byte object change, a truncated object and a one-byte local change; that local size, digest, position and GTID mismatches are VERIFY_FAILED; that a clean chain reports nothing; and that the byte budget binds and every file is charged once
// pos: acceptance tests for the phase 3 verifier (docs/design/0009-phase3-verifier.md); coverage, closing and rotation tests are in verify_coverage_test.go, scheduling and budget tests in verify_queue_test.go
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

type memObjects struct {
	mu      sync.Mutex
	obj     map[string][]byte
	opens   int
	opened  map[string]int
	headErr error
	openErr error
	onOpen  func(n int)
}

func (m *memObjects) Head(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	m.opens++
	n := m.opens
	if m.opened == nil {
		m.opened = map[string]int{}
	}
	m.opened[key]++
	b, ok := m.obj[key]
	hook, openErr := m.onOpen, m.openErr
	m.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if openErr != nil {
		return nil, openErr
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func (m *memObjects) openCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opens
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

func findingOf(fs []Finding, kind, subject string) (Finding, bool) {
	for _, f := range fs {
		if f.Kind == kind && f.Subject == subject {
			return f, true
		}
	}
	return Finding{}, false
}

func mustVerify(t *testing.T, dir string, rows []VerifyRow, objs ObjectStore, chain *Chain, opt Options) Report {
	t.Helper()
	rep, err := Verify(context.Background(), dir, rows, objs, chain, opt)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// verifiedAt is a LastVerified map that marks every row verified at at, so
// that only forced, recent and slot work is scheduled. A segment verified
// since its slot period began gets no slot work, so tests pass a time one
// period back.
func verifiedAt(rows []VerifyRow, at time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for _, row := range rows {
		out[row.Name] = at
	}
	return out
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestFindingKindNames(t *testing.T) {
	// The names are the ADR 0009 phase 3 table; the worker registers them as written.
	for got, want := range map[string]string{
		KindVerifyFailed:       "VERIFY_FAILED",
		KindObjectMissing:      "I6_MISSING_OBJECT",
		KindObjectSizeMismatch: "I6_OBJECT_SIZE_MISMATCH",
		KindObjectCorrupt:      "OBJECT_CORRUPT",
		KindLocalMissing:       "I6_MISSING_LOCAL",
		KindUploadClockSkew:    "UPLOAD_CLOCK_SKEW",
	} {
		if got != want {
			t.Fatalf("kind %q, want %q", got, want)
		}
	}
}

func TestVerify_CleanChainHasNoFindings(t *testing.T) {
	f := live(t)
	if vs := f.check(t); len(vs) != 0 {
		t.Fatalf("fixture chain %v", vs)
	}
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	objs := objectsFromDir(t, f)
	budget := NewByteBudget(1 << 30)
	rep := mustVerify(t, f.dir, rows, objs, chainOf(f), Options{Now: testNow, RotationPeriods: 1, Budget: budget})
	if len(rep.Findings) != 0 {
		t.Fatalf("findings %+v", rep.Findings)
	}
	if !rep.ChainChecked {
		t.Fatal("chain not checked on a clean directory")
	}
	if budget.Charged() != rep.Bytes || rep.Bytes < 15000 {
		t.Fatalf("budget charged %d, pass read %d", budget.Charged(), rep.Bytes)
	}
	for _, row := range rows {
		cov := rep.Coverage[row.Name]
		if cov.Local.Method != MethodParsed || cov.Object.Method != MethodHashed || cov.Object.Digest != "local" {
			t.Fatalf("%s coverage %+v", row.Name, cov)
		}
		if _, ok := rep.VerifiedAt[row.Name]; !ok {
			t.Fatalf("last_verified_at missing for %s", row.Name)
		}
	}
}

func TestVerify_ReportsDeletedObjectFlippedObjectAndFlippedLocal(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	objs := objectsFromDir(t, f)
	opt := Options{Now: testNow, RotationPeriods: 1}
	clean := mustVerify(t, f.dir, rows, objs, chainOf(f), opt)
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
	bad := mustVerify(t, f.dir, rows, objs, chainOf(f), opt)
	if !hasFinding(bad.Findings, KindObjectMissing, deleted) {
		t.Fatalf("deleted object not reported: %+v", bad.Findings)
	}
	if !hasFinding(bad.Findings, KindObjectCorrupt, flippedObj) {
		t.Fatalf("flipped object not reported: %+v", bad.Findings)
	}
	if !hasFinding(bad.Findings, KindVerifyFailed, flippedLocal) {
		t.Fatalf("flipped local segment not reported: %+v", bad.Findings)
	}
	for _, name := range []string{deleted, flippedObj, flippedLocal} {
		if _, ok := bad.VerifiedAt[name]; ok {
			t.Fatalf("%s advanced last_verified_at with a finding open", name)
		}
	}
	objs.obj["t/"+deleted] = deletedBytes
	objs.obj["t/"+flippedObj] = objectBytes
	if err := os.WriteFile(localPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	binlog.ForgetFile(localPath)
	good := mustVerify(t, f.dir, rows, objs, chainOf(f), opt)
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

func TestVerify_ObjectSizeMismatch(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	name := rows[0].Name
	objs := objectsFromDir(t, f)
	key := rows[0].ObjectKey
	objs.obj[key] = objs.obj[key][:len(objs.obj[key])-1]
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1})
	if !hasFinding(rep.Findings, KindObjectSizeMismatch, name) || hasFinding(rep.Findings, KindObjectCorrupt, name) {
		t.Fatalf("truncated object: %+v", rep.Findings)
	}
	if cov := rep.Coverage[name]; cov.Object.Method != MethodHead || objs.openCount() != 0 {
		t.Fatalf("a size mismatch read the body: %+v opens=%d", cov, objs.openCount())
	}

	// A wrong catalog size is wrong for both copies.
	objs = objectsFromDir(t, f)
	rows[0].Size++
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1})
	if !hasFinding(rep.Findings, KindObjectSizeMismatch, name) || !hasFinding(rep.Findings, KindVerifyFailed, name) {
		t.Fatalf("catalog size +1: %+v", rep.Findings)
	}
	if rep.Coverage[name].Local.Method != MethodStat {
		t.Fatalf("local coverage %+v", rep.Coverage[name].Local)
	}
}

func TestVerify_LocalMismatchesAgainstTheCatalogRow(t *testing.T) {
	f := live(t)
	base := verifyRows(t, f, testNow.Add(-time.Hour))[0]
	base.UploadState, base.ObjectKey = "", ""
	cases := []struct {
		name   string
		edit   func(*VerifyRow)
		detail string
		method Method
	}{
		{"size", func(r *VerifyRow) { r.Size-- }, "size differs", MethodStat},
		{"sha256", func(r *VerifyRow) { r.SHA256 = strings.Repeat("0", 64) }, "sha256 differs", MethodParsed},
		{"start_pos", func(r *VerifyRow) { r.StartPos++ }, "event positions differ", MethodParsed},
		{"end_pos", func(r *VerifyRow) { r.EndPos++ }, "event positions differ", MethodParsed},
		{"gtid", func(r *VerifyRow) { r.GTIDSet = "00000000-0000-0000-0000-000000000001:1-2" }, "gtid set differs", MethodParsed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := base
			c.edit(&row)
			rep := mustVerify(t, f.dir, []VerifyRow{row}, nil, nil, Options{Now: testNow})
			got, ok := findingOf(rep.Findings, KindVerifyFailed, row.Name)
			if !ok || !strings.Contains(got.Detail, c.detail) {
				t.Fatalf("findings %+v, want %q", rep.Findings, c.detail)
			}
			if rep.Coverage[row.Name].Local.Method != c.method {
				t.Fatalf("coverage %+v", rep.Coverage[row.Name])
			}
			if _, adv := rep.VerifiedAt[row.Name]; adv {
				t.Fatal("a mismatching segment advanced last_verified_at")
			}
		})
	}
	rep := mustVerify(t, f.dir, []VerifyRow{base}, nil, nil, Options{Now: testNow})
	if len(rep.Findings) != 0 || rep.Coverage[base.Name].Local.Method != MethodParsed {
		t.Fatalf("unchanged row: %+v %+v", rep.Findings, rep.Coverage)
	}
	if _, ok := rep.VerifiedAt[base.Name]; !ok {
		t.Fatal("a parsed, not uploaded segment did not advance last_verified_at")
	}
}

func TestVerify_RecentUploadIsHashedWithRotationOff(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	rows = rows[:1]
	objs := objectsFromDir(t, f)
	flipStored(t, objs, rows[0].ObjectKey)
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: -1})
	if !hasFinding(rep.Findings, KindObjectCorrupt, rows[0].Name) {
		t.Fatalf("recent upload not hashed: %+v", rep.Findings)
	}
	if objs.openCount() == 0 {
		t.Fatal("object body was not read")
	}
}

func TestVerify_OldObjectOutsideRotationIsNotRead(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-48*time.Hour))
	rows = rows[:1]
	objs := objectsFromDir(t, f)
	flipStored(t, objs, rows[0].ObjectKey)
	// Verified after its upload: with the rotation off nothing reads it again.
	lv := verifiedAt(rows, testNow.Add(-47*time.Hour))
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: -1, LastVerified: lv})
	if hasFinding(rep.Findings, KindObjectCorrupt, rows[0].Name) {
		t.Fatalf("old object was judged by body: %+v", rep.Findings)
	}
	if objs.openCount() != 0 {
		t.Fatalf("old object body was read %d times", objs.openCount())
	}
	if rep.Coverage[rows[0].Name].Object.Method != MethodHead {
		t.Fatalf("coverage %+v", rep.Coverage[rows[0].Name])
	}
	if _, ok := rep.VerifiedAt[rows[0].Name]; ok {
		t.Fatal("a HEAD-only object advanced last_verified_at")
	}
	delete(objs.obj, rows[0].ObjectKey)
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: -1, LastVerified: lv})
	if !hasFinding(rep.Findings, KindObjectMissing, rows[0].Name) {
		t.Fatalf("HEAD miss not reported: %+v", rep.Findings)
	}
	if rep.ObjectErr != nil || rep.Coverage[rows[0].Name].Object.Method != MethodMissing {
		t.Fatalf("a 404 is not an error: err=%v cov=%+v", rep.ObjectErr, rep.Coverage[rows[0].Name])
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

func TestByteBudget_BurstIsCapped(t *testing.T) {
	if b := NewByteBudget(DefaultBudgetBytesPerSec); b.lim.Burst() != 64<<10 {
		t.Fatalf("burst %d at 4 MiB/s, want 64 KiB", b.lim.Burst())
	}
	if b := NewByteBudget(1000); b.lim.Burst() != 1000 {
		t.Fatalf("burst %d at 1000 B/s", b.lim.Burst())
	}
	if NewByteBudget(0) != nil || NewByteBudget(-1) != nil {
		t.Fatal("a non-positive rate is unlimited (nil)")
	}
}

// TestVerify_ChargesEveryReadToTheBudget counts what each read costs: the
// local file once (the positions come from the same stream as the parse),
// and every streamed object byte. HEAD and stat are free.
func TestVerify_ChargesEveryReadToTheBudget(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	var local int64
	for _, row := range rows {
		local += fileSize(t, filepath.Join(f.dir, row.Name))
	}
	objectBytes := local

	budget := NewByteBudget(1 << 30)
	noPos := append([]VerifyRow(nil), rows...)
	for i := range noPos {
		noPos[i].StartPos, noPos[i].EndPos = 0, 0
	}
	rep := mustVerify(t, f.dir, noPos, objectsFromDir(t, f), nil, Options{Now: testNow, RotationPeriods: 1, Budget: budget})
	if rep.LocalBytes != local || rep.ObjectBytes != objectBytes || rep.Bytes != local+objectBytes {
		t.Fatalf("pass read local=%d object=%d total=%d, want %d %d", rep.LocalBytes, rep.ObjectBytes, rep.Bytes, local, objectBytes)
	}
	if budget.Charged() != local+objectBytes {
		t.Fatalf("budget charged %d, want %d", budget.Charged(), local+objectBytes)
	}

	// Positions set: still one read of each file.
	budget = NewByteBudget(1 << 30)
	rep = mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: testNow, RotationPeriods: 1, Budget: budget})
	if rep.LocalBytes != local || budget.Charged() != local+objectBytes || len(rep.Findings) != 0 {
		t.Fatalf("with positions: local=%d charged=%d findings=%+v, want %d and %d", rep.LocalBytes, budget.Charged(), rep.Findings, local, local+objectBytes)
	}

	// Nothing scheduled: stat and HEAD only, nothing charged.
	budget = NewByteBudget(1 << 30)
	rep = mustVerify(t, f.dir, noPos, objectsFromDir(t, f), nil, Options{Now: testNow.Add(72 * time.Hour), RotationPeriods: -1, Budget: budget, LastVerified: verifiedAt(noPos, testNow.Add(time.Hour))})
	if rep.Bytes != 0 || budget.Charged() != 0 {
		t.Fatalf("stat and HEAD only: bytes=%d charged=%d", rep.Bytes, budget.Charged())
	}
	for _, row := range noPos {
		if c := rep.Coverage[row.Name]; c.Local.Method != MethodStat || c.Object.Method != MethodHead || !c.Local.Found || !c.Object.Found {
			t.Fatalf("%s coverage %+v", row.Name, c)
		}
	}
}
