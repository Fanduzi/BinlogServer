// Package assurance provides module-level functionality for assurance.
// input: the live ledger fixture, synthetic object-only segments, a backlog of not-uploaded local segments, hourly arrivals, objects whose bytes match a wrong catalog row, and stores that stall on HEAD
// output: proof that the recent tier holds a row only until it is content-verified after its last change, that its share of a pass is capped so a backlog cannot starve the slot and overdue tiers (design §5.3 bound with the share counted), that a new upload is read at most twice in its first day, that a VERIFY_FAILED with no local file closes only when the object parses and matches the row, that a removed row leaves chain kinds to the chain, and that the HEAD phase stops at the deadline
// pos: QA r3 regression tests for the phase 3 verifier (docs/design/0009-phase3-verifier.md §5.2, §5.3, §6.2)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

// queueQ is design §5.3's q: what is left of C after the forced work and
// the recent share R = min(⌊C × share⌋, max(0, C − F − S)), in whole
// segments of S.
func queueQ(c, f, s int64, sharePct int) int {
	r := min(c*int64(sharePct)/100, max(0, c-f-s))
	return int((c - f - r) / s)
}

// hogRows is QA r3's pending_local_hog: object-only segments of the
// template size that were uploaded long ago, plus not-uploaded (PENDING)
// local segments.
func hogRows(t *testing.T, objects, pending int) (string, []VerifyRow, *memObjects, int64) {
	t.Helper()
	f := live(t)
	tpl := verifyRows(t, f, testNow)[1]
	body, err := os.ReadFile(filepath.Join(f.dir, tpl.Name))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	objs := &memObjects{obj: map[string][]byte{}}
	var rows []VerifyRow
	for i := 0; i < objects; i++ {
		b := bytes.Repeat([]byte{byte(i + 1)}, len(body))
		sum := sha256.Sum256(b)
		name := fmt.Sprintf("mysql-bin.%06d", 101+i)
		row := VerifyRow{Name: name, Sealed: true, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), UploadState: "UPLOADED",
			ObjectKey: "t/" + name, UploadedAt: testNow.Add(-30 * 24 * time.Hour), RetentionRemoved: true}
		objs.obj[row.ObjectKey] = b
		rows = append(rows, row)
	}
	for i := 0; i < pending; i++ {
		row := tpl
		row.Name = fmt.Sprintf("mysql-bin.%06d", 201+i)
		row.UploadState, row.ObjectKey, row.UploadedAt = "PENDING", "", time.Time{}
		if err := os.WriteFile(filepath.Join(dir, row.Name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return dir, rows, objs, int64(len(body))
}

// TestVerify_PendingBacklogDoesNotStarveTheRotation is QA r3 F1
// (pending_local_hog_3): C=47292, F=0, S=15764, k=13, N=20, VerifiedAt fed
// back. Not-uploaded rows leave the recent tier once parsed, and the recent
// share is counted in q, so every segment is first checked within P passes
// and then at most N+P periods apart.
func TestVerify_PendingBacklogDoesNotStarveTheRotation(t *testing.T) {
	const n, passes = 20, 200
	dir, rows, objs, s := hogRows(t, 10, 3)
	c := 3 * s
	if c != 47292 || s != 15764 || len(rows) != 13 {
		t.Fatalf("setup C=%d S=%d k=%d", c, s, len(rows))
	}
	q := queueQ(c, 0, s, DefaultRecentSharePercent)
	bound := queueBound(len(rows), q, n)
	if q != 2 || bound != 13 {
		t.Fatalf("derived q=%d P=%d", q, bound)
	}
	last := map[string]time.Time{}
	hits := map[string][]int{}
	for pass := 0; pass < passes; pass++ {
		now := testNow.Add(time.Duration(pass) * DefaultPeriod)
		lv := map[string]time.Time{}
		for k, v := range last {
			lv[k] = v
		}
		rep := mustVerify(t, dir, rows, objs, nil, Options{Now: now, RotationPeriods: n, PassBytes: c, LastVerified: lv, CatalogComplete: true})
		if rep.Bytes > c || len(rep.Findings) != 0 {
			t.Fatalf("pass %d: bytes %d findings %+v", pass, rep.Bytes, rep.Findings)
		}
		for name, at := range rep.VerifiedAt {
			last[name] = at
			hits[name] = append(hits[name], pass)
		}
	}
	worst := 0
	for _, row := range rows {
		h := append(append([]int(nil), hits[row.Name]...), passes-1) // the tail to the last pass counts as a gap
		if len(hits[row.Name]) == 0 || hits[row.Name][0] >= bound {
			t.Fatalf("%s first checked at %v, bound %d passes", row.Name, hits[row.Name], bound)
		}
		for i := 1; i < len(h); i++ {
			if g := h[i] - h[i-1]; g > worst {
				worst = g
			}
		}
	}
	if worst > n+bound {
		t.Fatalf("worst gap %d periods > N+P = %d", worst, n+bound)
	}
	t.Logf("q=%d P=%d bound first<=%d gap<=%d; worst gap %d", q, bound, bound, n+bound, worst)
}

// TestVerify_NotUploadedRowIsRecentUntilParsed: a not-uploaded row is in
// the recent tier until it is parsed after its last change, then follows the
// rotation like any other; a later change (sealed_at) makes it recent again.
func TestVerify_NotUploadedRowIsRecentUntilParsed(t *testing.T) {
	dir, rows, objs, _ := hogRows(t, 0, 1)
	n := rows[0].Name
	opt := Options{Now: testNow, RotationPeriods: -1}
	rep := mustVerify(t, dir, rows, objs, nil, opt)
	if c := rep.Coverage[n].Local; c.Method != MethodParsed {
		t.Fatalf("never parsed, not uploaded: %+v", c)
	}
	at, ok := rep.VerifiedAt[n]
	if !ok {
		t.Fatalf("not verified: %+v", rep.Coverage[n])
	}
	opt.Now, opt.LastVerified = testNow.Add(time.Hour), map[string]time.Time{n: at}
	if c := mustVerify(t, dir, rows, objs, nil, opt).Coverage[n].Local; c.Method != MethodStat {
		t.Fatalf("parsed again with nothing changed: %+v", c)
	}
	rows[0].SealedAt = testNow.Add(30 * time.Minute)
	if c := mustVerify(t, dir, rows, objs, nil, opt).Coverage[n].Local; c.Method != MethodParsed {
		t.Fatalf("changed after the last parse but not recent: %+v", c)
	}
	// Parsed while pending, then uploaded: the upload is a change, so the object is read.
	body, err := os.ReadFile(filepath.Join(dir, n))
	if err != nil {
		t.Fatal(err)
	}
	rows[0].SealedAt = time.Time{}
	rows[0].UploadState, rows[0].ObjectKey, rows[0].UploadedAt = "UPLOADED", "t/"+n, testNow.Add(50*time.Minute)
	objs.obj["t/"+n] = body
	if c := mustVerify(t, dir, rows, objs, nil, opt).Coverage[n].Object; c.Method != MethodHashed {
		t.Fatalf("uploaded after the last parse but not read: %+v", c)
	}
}

// TestVerify_NamedSegmentGetsNoRecentWork: a recent upload that an open
// finding names does not advance, so the recent tier would read it on every
// pass; it gets content work only from its slot (or when forced).
func TestVerify_NamedSegmentGetsNoRecentWork(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	n := rows[0].Name
	rep := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: testNow, RotationPeriods: -1, OpenFindings: []Finding{{Kind: string(I3GTIDHole), Subject: n}}})
	if c := rep.Coverage[n]; c.Local.Method != MethodStat || c.Object.Method != MethodHead {
		t.Fatalf("named recent upload read: %+v", c)
	}
}

// TestVerify_NewUploadIsReadAtMostTwiceInItsFirstDay is QA r3's hourly
// arrivals: with passes every hour and spare budget, a new upload is read
// once for the recent tier and at most once more for its slot.
func TestVerify_NewUploadIsReadAtMostTwiceInItsFirstDay(t *testing.T) {
	dir, rows, objs, s := hogRows(t, 10, 0)
	body := objs.obj[rows[0].ObjectKey]
	last := map[string]time.Time{}
	arrived := map[string]int{}
	reads := map[string]int{}
	for pass := 0; pass < 96; pass++ {
		now := testNow.Add(time.Duration(pass) * time.Hour)
		if pass < 48 {
			b := append([]byte(nil), body...)
			b[0] = byte(pass)
			sum := sha256.Sum256(b)
			name := fmt.Sprintf("mysql-bin.%06d", 500+pass)
			row := VerifyRow{Name: name, Sealed: true, Size: s, SHA256: hex.EncodeToString(sum[:]), UploadState: "UPLOADED", ObjectKey: "t/" + name, UploadedAt: now, RetentionRemoved: true}
			objs.obj[row.ObjectKey] = b
			rows = append(rows, row)
			arrived[name] = pass
		}
		lv := map[string]time.Time{}
		for k, v := range last {
			lv[k] = v
		}
		rep := mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 100 * s, LastVerified: lv})
		for name, at := range rep.VerifiedAt {
			last[name] = at
		}
		for name, c := range rep.Coverage {
			if a, ok := arrived[name]; ok && pass-a < 24 && c.Object.Method == MethodHashed {
				reads[name]++
			}
		}
	}
	for name := range arrived {
		if reads[name] < 1 || reads[name] > 2 {
			t.Fatalf("%s read %d times in its first 24 hourly passes", name, reads[name])
		}
	}
}

// TestVerify_RecentShareCapsTheRecentTier: recent work past the share falls
// to its fallback tier, or to the tail tier that is paid only from what the
// pass has left, so spare budget is not left unused (QA r4 G1). R is also
// capped at C − F − S (QA r4 G2). With the rotation on, a recent object past
// the share has never been verified since its upload, so it is overdue and
// sorts ahead of verified overdue work (QA r5 P2-1); with the rotation off it
// goes to the tail.
func TestVerify_RecentShareCapsTheRecentTier(t *testing.T) {
	dir, rows, objs := simFixture(t, 0, []int{1000, 1000, 1000})
	old, a, b := rows[0].Name, rows[1].Name, rows[2].Name
	now := testNow
	for InRotation(old, now, DefaultPeriod, 0) || InRotation(a, now, DefaultPeriod, 0) || InRotation(b, now, DefaultPeriod, 0) {
		now = now.Add(DefaultPeriod)
	}
	rows[1].UploadedAt, rows[2].UploadedAt = now.Add(-time.Hour), now.Add(-time.Hour)
	lv := map[string]time.Time{old: now.Add(-100 * DefaultPeriod), a: now.Add(-2 * time.Hour), b: now.Add(-2 * time.Hour)}
	overdue := tierReason(tierOverdue)
	// R = min(500, 2000-0-1000) = 500: neither recent object fits the
	// share. Both are overdue and never verified since their upload, so they
	// sort ahead of the verified overdue object, which is deferred.
	rep := mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 2000, LastVerified: lv})
	if ca, cb, co := rep.Coverage[a].Object, rep.Coverage[b].Object, rep.Coverage[old].Object; ca.Method != MethodHashed || ca.Reason != overdue || cb.Method != MethodHashed || cb.Reason != overdue || co.Method != MethodSkipped || co.Reason != reasonDeferredBudget {
		t.Fatalf("share 25 of 2000: a %+v b %+v old %+v", ca, cb, co)
	}
	// Rotation off, C = 1500: R = min(375, 500) = 375. a is paid from the
	// tail; b does not fit what is left; old was verified after its upload
	// and is not read.
	rep = mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 1500, LastVerified: lv, RotationPeriods: -1})
	if ca, cb, co := rep.Coverage[a].Object, rep.Coverage[b].Object, rep.Coverage[old].Object; ca.Method != MethodHashed || ca.Reason != reasonTail || cb.Method != MethodSkipped || cb.Reason != reasonDeferredBudget || co.Method != MethodHead {
		t.Fatalf("rotation off: a %+v b %+v old %+v", ca, cb, co)
	}
	// share 100 and 50: R = min(2000 or 1000, 1000) = 1000, so a is recent,
	// b is overdue and paid ahead of the verified overdue object.
	for _, pct := range []int{100, 50} {
		rep = mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 2000, LastVerified: lv, RecentSharePercent: pct})
		ca, cb, co := rep.Coverage[a].Object, rep.Coverage[b].Object, rep.Coverage[old].Object
		if ca.Method != MethodHashed || ca.Reason != tierReason(tierRecent) || cb.Method != MethodHashed || cb.Reason != overdue || co.Method != MethodSkipped || co.Reason != reasonDeferredBudget {
			t.Fatalf("share %d: a %+v b %+v old %+v", pct, ca, cb, co)
		}
	}
	// Spare budget. With no PassBytes, C is what the budget can pay before
	// the deadline: 2000 tokens + 2000 B/s x 2s, so R is about 1500 bytes:
	// a is recent, b and old are overdue, and all three are paid.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rep, err := Verify(ctx, dir, rows, objs, nil, Options{Now: now, Budget: NewByteBudget(2000), LastVerified: lv})
	if err != nil {
		t.Fatal(err)
	}
	if ca, cb, co := rep.Coverage[a].Object, rep.Coverage[b].Object, rep.Coverage[old].Object; ca.Method != MethodHashed || ca.Reason != tierReason(tierRecent) || cb.Method != MethodHashed || cb.Reason != overdue || co.Method != MethodHashed {
		t.Fatalf("budget and deadline: a %+v b %+v old %+v", ca, cb, co)
	}
	for _, pct := range []int{-1, 101} {
		if _, err := Verify(context.Background(), dir, rows, objs, nil, Options{Now: now, RecentSharePercent: pct}); err == nil {
			t.Fatalf("share %d accepted", pct)
		}
	}
}

// TestVerify_LocalRemovedVerifyFailedNeedsTheObjectToParse is QA r3 F2: with
// the local file gone, an object that matches the catalog digest closes a
// VERIFY_FAILED only when it also parses and matches the row's positions and
// GTID set. The sha variant closes as "local removed; object matches
// catalog". Otherwise the finding stays open on the object (CRITICAL, sole
// copy). The parse costs no extra bytes.
func TestVerify_LocalRemovedVerifyFailedNeedsTheObjectToParse(t *testing.T) {
	cases := []struct {
		name, detail string
		edit         func(row *VerifyRow, obj []byte) []byte
		closeReason  string
	}{
		{"parse failure, catalog digest of the bad bytes", DetailParse, func(row *VerifyRow, obj []byte) []byte {
			bad := append([]byte(nil), obj...)
			bad[len(bad)/2] ^= 0xff
			sum := sha256.Sum256(bad)
			row.SHA256 = hex.EncodeToString(sum[:])
			return bad
		}, ""},
		{"end_log_pos differs", DetailPositions, func(row *VerifyRow, obj []byte) []byte { row.EndPos++; return obj }, ""},
		{"gtid set differs", DetailGTID, func(row *VerifyRow, obj []byte) []byte {
			row.GTIDSet = "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5"
			return obj
		}, ""},
		{"sha differs, object good", DetailSHA, func(row *VerifyRow, obj []byte) []byte { return obj }, ReasonLocalRemoved},
		{"parse failure, object good", DetailParse, func(row *VerifyRow, obj []byte) []byte { return obj }, ReasonObjectParsed},
	}
	t.Run("no open VERIFY_FAILED: the object is only hashed", func(t *testing.T) {
		f := live(t)
		rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
		n := rows[0].Name
		objs := objectsFromDir2(t, f, rows)
		if err := os.Remove(filepath.Join(f.dir, n)); err != nil {
			t.Fatal(err)
		}
		binlog.ForgetFile(filepath.Join(f.dir, n))
		rows[0].EndPos++
		rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1, LastVerified: verifiedAt(rows, testNow.Add(-DefaultPeriod)), OpenFindings: []Finding{{Kind: KindObjectMissing, Subject: n}}})
		if c := rep.Coverage[n].Object; c.Method != MethodHashed || c.Parsed || hasFinding(rep.Findings, KindVerifyFailed, n) {
			t.Fatalf("cov %+v findings %+v", c, rep.Findings)
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := live(t)
			rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
			n := rows[0].Name
			objs := objectsFromDir2(t, f, rows)
			if err := os.Remove(filepath.Join(f.dir, n)); err != nil {
				t.Fatal(err)
			}
			binlog.ForgetFile(filepath.Join(f.dir, n))
			objs.obj[rows[0].ObjectKey] = c.edit(&rows[0], objs.obj[rows[0].ObjectKey])
			size := int64(len(objs.obj[rows[0].ObjectKey]))
			open := []Finding{{Kind: KindVerifyFailed, Subject: n, Severity: SeverityCritical, Detail: c.detail}}
			rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1, LastVerified: verifiedAt(rows, testNow.Add(-DefaultPeriod)), OpenFindings: open})
			cov := rep.Coverage[n]
			if cov.Object.Method != MethodHashed || cov.Object.Digest != "catalog" || rep.ObjectBytes != size || rep.Bytes != size {
				t.Fatalf("cov %+v object bytes %d want %d", cov, rep.ObjectBytes, size)
			}
			_, advanced := rep.VerifiedAt[n]
			if c.closeReason != "" {
				if len(rep.Closed) != 1 || rep.Closed[0].Reason != c.closeReason || !cov.Object.Parsed || !advanced {
					t.Fatalf("closed %+v parsed %v advanced %v", rep.Closed, cov.Object.Parsed, advanced)
				}
				return
			}
			fd, ok := findingOf(rep.Findings, KindVerifyFailed, n)
			if len(rep.Closed) != 0 || advanced || cov.Object.Parsed || !ok || fd.Severity != SeverityCritical || fd.Detail != DetailObjectPrefix+c.detail {
				t.Fatalf("closed %+v advanced %v finding %+v (ok %v)", rep.Closed, advanced, fd, ok)
			}
		})
	}
}

// TestVerify_RowRemovedLeavesChainKindsToTheChain is QA r3 F3: a removed row
// does not close I1–I5 or I7 kinds; only a chain check in the same pass does.
func TestVerify_RowRemovedLeavesChainKindsToTheChain(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
	n := rows[1].Name
	kept := append(append([]VerifyRow(nil), rows[:1]...), rows[2:]...)
	var prev []Finding
	for _, kind := range []Kind{I1Regression, I2IndexGap, I3GTIDHole, I4Overlap, I5CheckpointAhead, I7UploadLate} {
		prev = append(prev, Finding{Kind: string(kind), Subject: n})
	}
	opt := Options{Now: testNow, OpenFindings: prev, CatalogComplete: true, RotationPeriods: -1, LastVerified: verifiedAt(rows, testNow.Add(-DefaultPeriod))}
	rep := mustVerify(t, f.dir, kept, objectsFromDir(t, f), nil, opt)
	if len(rep.Closed) != 0 {
		t.Fatalf("row removed closed chain kinds without a chain check: %+v", rep.Closed)
	}
	rep = mustVerify(t, f.dir, kept, objectsFromDir(t, f), chainOf(f), opt)
	if !rep.ChainChecked || len(rep.Closed) != len(prev) {
		t.Fatalf("chain checked %v closed %+v findings %+v", rep.ChainChecked, rep.Closed, rep.Findings)
	}
	for _, c := range rep.Closed {
		if c.Reason != ReasonChainChecked {
			t.Fatalf("closure %+v", c)
		}
	}
}

// stallHead blocks the first HEAD until the context is done; later HEADs
// answer at once and ignore the context, as a store that does not honour it.
type stallHead struct {
	*memObjects
	heads int
}

func (s *stallHead) Head(ctx context.Context, key string) (int64, error) {
	s.heads++
	if s.heads == 1 {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.memObjects.Head(ctx, key)
}

// TestVerify_HeadPhaseStopsAtTheDeadline is QA r3 F4 (Y14): once a HEAD runs
// into the deadline no further HEAD is sent; the rest are SKIPPED and the
// pass ends normally.
func TestVerify_HeadPhaseStopsAtTheDeadline(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
	store := &stallHead{memObjects: objectsFromDir(t, f)}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rep, err := Verify(ctx, f.dir, rows, store, nil, Options{Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if store.heads != 1 {
		t.Fatalf("%d HEADs sent; the deadline passed on the first", store.heads)
	}
	for _, row := range rows {
		if c := rep.Coverage[row.Name].Object; c.Method != MethodSkipped || c.Reason != reasonDeferredTime {
			t.Fatalf("%s: %+v", row.Name, c)
		}
	}
}
