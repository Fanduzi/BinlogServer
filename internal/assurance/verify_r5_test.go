// Package assurance provides module-level functionality for assurance.
// input: a bucket-recovery backlog of not-uploaded segments uploaded together, objects-only queues with a tight pass cap, local files that fail to open or fail once mid-read, object bodies cut short with a clean EOF, sealed_at in the future, and a forced segment checked hourly with Report.Read fed back
// output: proof that recent work past the share is paid from leftover budget instead of skipped, that the slot and overdue tiers keep room for one segment, that unreadable local files raise LOCAL_UNREADABLE and close when read, that a short object body is ERROR, that a future sealed_at is clock skew and does not pin the recent tier, and that a forced segment is re-read at most once per period unless it changed
// pos: QA r4 regression tests for the phase 3 verifier (docs/design/0009-phase3-verifier.md §5.2, §5.3, §6.2, §6.3)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestVerify_BucketRecoveryHashesTheBacklog is QA r4 G1: 30 UPLOAD_FAILED
// rows parsed while pending upload together when the bucket comes back.
// The recent share takes a few; the rest are paid from what the pass has
// left (tail tier), so with C = 630560 every object is hashed within two
// daily passes and no work is skipped while budget is spare.
func TestVerify_BucketRecoveryHashesTheBacklog(t *testing.T) {
	const n, c = 30, 630560
	dir, rows, objs, s := hogRows(t, 0, n)
	if c != 40*s {
		t.Fatalf("segment size %d", s)
	}
	body, err := os.ReadFile(filepath.Join(dir, rows[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	base := testNow
	for i := range rows {
		rows[i].UploadState, rows[i].SealedAt = "UPLOAD_FAILED", base.Add(-time.Hour)
	}
	last := map[string]time.Time{}
	feed := func(rep Report) {
		for k, v := range rep.VerifiedAt {
			last[k] = v
		}
	}
	rep := mustVerify(t, dir, rows, objs, nil, Options{Now: base, PassBytes: c, LastVerified: last})
	feed(rep)
	if len(rep.VerifiedAt) != n {
		t.Fatalf("pending backlog parsed %d of %d", len(rep.VerifiedAt), n)
	}
	up := base.Add(23 * time.Hour)
	for i := range rows {
		rows[i].UploadState, rows[i].ObjectKey, rows[i].UploadedAt = "UPLOADED", "t/"+rows[i].Name, up
		objs.obj[rows[i].ObjectKey] = body
	}
	first := map[string]int{}
	for d := 1; d <= 3; d++ {
		lv := map[string]time.Time{}
		for k, v := range last {
			lv[k] = v
		}
		rep := mustVerify(t, dir, rows, objs, nil, Options{Now: base.Add(time.Duration(d) * DefaultPeriod), PassBytes: c, LastVerified: lv})
		feed(rep)
		hashed := 0
		for name, cov := range rep.Coverage {
			if cov.Object.Method == MethodHashed {
				hashed++
				if _, ok := first[name]; !ok {
					first[name] = d
				}
			}
		}
		if rep.Bytes > c {
			t.Fatalf("day %d paid %d > C", d, rep.Bytes)
		}
		// Every unhashed object left on day 1 is there because the pass
		// is full: what was skipped did not fit what was left.
		if d == 1 && rep.Bytes+2*s <= c && hashed < n {
			t.Fatalf("day 1 paid %d of %d and hashed %d of %d", rep.Bytes, c, hashed, n)
		}
		t.Logf("day %d: paid %d of C=%d, objects hashed %d", d, rep.Bytes, c, hashed)
	}
	for _, row := range rows {
		if d, ok := first[row.Name]; !ok || d > 2 {
			t.Fatalf("%s first hashed on day %d (ok %v)", row.Name, d, ok)
		}
	}
}

// TestVerify_RecentShareLeavesRoomForOneSegment is QA r4 G2: R is at most
// C − F − S, so whenever C − F ≥ S the slot and overdue tiers get at least
// one segment, even at a 100% share; an R smaller than every recent copy
// turns the recent tier off for that pass.
func TestVerify_RecentShareLeavesRoomForOneSegment(t *testing.T) {
	dir, rows, objs := simFixture(t, 0, []int{1000, 1000, 1000})
	old, a, b := rows[0].Name, rows[1].Name, rows[2].Name
	now := testNow
	for InRotation(old, now, DefaultPeriod, 0) || InRotation(a, now, DefaultPeriod, 0) || InRotation(b, now, DefaultPeriod, 0) {
		now = now.Add(DefaultPeriod)
	}
	rows[1].UploadedAt, rows[2].UploadedAt = now.Add(-time.Hour), now.Add(-time.Hour)
	lv := map[string]time.Time{old: now.Add(-100 * DefaultPeriod), a: now.Add(-2 * time.Hour), b: now.Add(-2 * time.Hour)}
	// C = 2S at 100%: R = min(2000, 1000) = 1000.
	rep := mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 2000, LastVerified: lv, RecentSharePercent: 100})
	if c := rep.Coverage[old].Object; c.Method != MethodHashed {
		t.Fatalf("overdue starved at a 100%% share: %+v", c)
	}
	// C = S: R = min(250, 0) = 0, the recent tier is off; the overdue
	// segment is paid and the recent ones wait in the tail.
	rep = mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 1000, LastVerified: lv})
	if c := rep.Coverage[old].Object; c.Method != MethodHashed {
		t.Fatalf("C = S: overdue not paid: %+v", c)
	}
	for _, n := range []string{a, b} {
		if c := rep.Coverage[n].Object; c.Method != MethodSkipped || c.Reason != reasonDeferredBudget {
			t.Fatalf("C = S: %s %+v", n, c)
		}
	}
	if q := queueQ(1000, 0, 1000, DefaultRecentSharePercent); q != 1 {
		t.Fatalf("q = %d", q)
	}
	// Recent work past the share keeps its slot: on a's slot day with
	// R = 0 it is paid as the slot, ahead of the overdue segment.
	day := now
	for !InRotation(a, day, DefaultPeriod, 0) || InRotation(old, day, DefaultPeriod, 0) || InRotation(b, day, DefaultPeriod, 0) {
		day = day.Add(DefaultPeriod)
	}
	day = slotStart(day, DefaultPeriod).Add(2 * time.Hour)
	rows[1].UploadedAt, rows[2].UploadedAt = day.Add(-time.Hour), day.Add(-time.Hour)
	lv = map[string]time.Time{old: day.Add(-100 * DefaultPeriod), a: day.Add(-3 * time.Hour), b: day.Add(-3 * time.Hour)}
	rep = mustVerify(t, dir, rows, objs, nil, Options{Now: day, PassBytes: 1000, LastVerified: lv})
	if c := rep.Coverage[a].Object; c.Method != MethodHashed || c.Reason != tierReason(tierSlot) || rep.Coverage[old].Object.Method != MethodSkipped {
		t.Fatalf("slot day: a %+v old %+v", c, rep.Coverage[old].Object)
	}
	// The recent tier is paid before the slot: with every segment in its
	// slot (rotation 1) and room for two, the recent one is paid first.
	lv3 := map[string]time.Time{old: now.Add(-100 * DefaultPeriod), a: now.Add(-2 * time.Hour), b: now.Add(-100 * DefaultPeriod)}
	rows[1].UploadedAt, rows[2].UploadedAt = now.Add(-time.Hour), rows[0].UploadedAt
	rep = mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 2000, RotationPeriods: 1, LastVerified: lv3, RecentSharePercent: 100})
	if c := rep.Coverage[a].Object; c.Method != MethodHashed || c.Reason != tierReason(tierRecent) {
		t.Fatalf("recent after the slot: a %+v", c)
	}
	// S counts only segments that fit in C - F: a 5000-byte object that
	// never fits does not turn the recent tier off.
	dir2, rows2, objs2 := simFixture(t, 0, []int{5000, 1000})
	huge, fresh := rows2[0].Name, rows2[1].Name
	rows2[1].UploadedAt = now.Add(-time.Hour)
	lv2 := map[string]time.Time{huge: now.Add(-100 * DefaultPeriod), fresh: now.Add(-2 * time.Hour)}
	rep = mustVerify(t, dir2, rows2, objs2, nil, Options{Now: now, PassBytes: 4000, LastVerified: lv2})
	if c := rep.Coverage[fresh].Object; c.Method != MethodHashed || c.Reason != tierReason(tierRecent) {
		t.Fatalf("huge object turned the recent tier off: %+v", c)
	}
}

// failOnce is a local file whose read fails once after skip bytes; every
// other read works, so a reader that ignores the error would go on.
type failOnce struct {
	io.ReadCloser
	skip   int
	failed bool
}

var errDisk = errors.New("read: input/output error")

func (f *failOnce) Read(p []byte) (int, error) {
	if !f.failed && f.skip <= 0 {
		f.failed = true
		return 0, errDisk
	}
	if len(p) > f.skip && f.skip > 0 {
		p = p[:f.skip]
	}
	n, err := f.ReadCloser.Read(p)
	f.skip -= n
	return n, err
}

func withOpenLocal(t *testing.T, open func(path string) (io.ReadCloser, error)) {
	t.Helper()
	prev := openLocal
	openLocal = open
	t.Cleanup(func() { openLocal = prev })
}

// TestVerify_LocalReadErrorIsLocalUnreadable is QA r4 G3 (and Z16): a read
// error in the middle of a local parse is ERROR and LOCAL_UNREADABLE (WARNING,
// CRITICAL when the row is not uploaded), never VERIFY_FAILED; the next
// clean read closes it. A file that cannot be opened at all is the same, and
// the finding closes once the file is gone.
func TestVerify_LocalReadErrorIsLocalUnreadable(t *testing.T) {
	for _, state := range []string{"UPLOADED", "PENDING"} {
		t.Run(state, func(t *testing.T) {
			f := live(t)
			rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
			n := rows[0].Name
			want := SeverityWarning
			if state != "UPLOADED" {
				rows[0].UploadState, rows[0].ObjectKey = state, ""
				want = SeverityCritical
			}
			objs := objectsFromDir2(t, f, rows)
			withOpenLocal(t, func(path string) (io.ReadCloser, error) {
				r, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				return &failOnce{ReadCloser: r, skip: 100}, nil
			})
			opt := Options{Now: testNow, RotationPeriods: 1, LastVerified: verifiedAt(rows, testNow.Add(-DefaultPeriod))}
			rep := mustVerify(t, f.dir, rows, objs, nil, opt)
			fd, ok := findingOf(rep.Findings, KindLocalUnreadable, n)
			if c := rep.Coverage[n].Local; c.Method != MethodError || !ok || fd.Severity != want || hasFinding(rep.Findings, KindVerifyFailed, n) || rep.LocalErr == nil {
				t.Fatalf("local %+v findings %+v", c, rep.Findings)
			}
			if _, adv := rep.VerifiedAt[n]; adv {
				t.Fatal("advanced on a read error")
			}
			openLocal = func(path string) (io.ReadCloser, error) { return os.Open(path) }
			// The open finding forces the re-read: no slot, no recent tier.
			opt.OpenFindings, opt.RotationPeriods = rep.Findings, -1
			rep = mustVerify(t, f.dir, rows, objs, nil, opt)
			if c := rep.Coverage[n].Local; c.Method != MethodParsed || len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonLocalRead {
				t.Fatalf("clean read: local %+v closed %+v findings %+v", c, rep.Closed, rep.Findings)
			}
		})
	}
	t.Run("vanished before open is not unreadable", func(t *testing.T) {
		f := live(t)
		rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
		withOpenLocal(t, func(string) (io.ReadCloser, error) { return nil, os.ErrNotExist })
		rep := mustVerify(t, f.dir, rows, objectsFromDir2(t, f, rows), nil, Options{Now: testNow, RotationPeriods: 1, LastVerified: verifiedAt(rows, testNow.Add(-DefaultPeriod))})
		if rep.Coverage[rows[0].Name].Local.Method != MethodError || len(rep.Findings) != 0 {
			t.Fatalf("local %+v findings %+v", rep.Coverage[rows[0].Name].Local, rep.Findings)
		}
	})
	t.Run("stat fails", func(t *testing.T) {
		f := live(t)
		rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
		objs := objectsFromDir2(t, f, rows)
		// A directory that can be listed but not searched: stat fails.
		if err := os.Chmod(f.dir, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(f.dir, 0o755) })
		rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow})
		if c := rep.Coverage[rows[0].Name].Local; c.Method != MethodError || !hasFinding(rep.Findings, KindLocalUnreadable, rows[0].Name) {
			t.Fatalf("local %+v findings %+v", c, rep.Findings)
		}
	})
	t.Run("cannot open, then gone", func(t *testing.T) {
		f := live(t)
		rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
		n := rows[0].Name
		p := filepath.Join(f.dir, n)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(p, 0o644) })
		objs := objectsFromDir2(t, f, rows)
		opt := Options{Now: testNow, RotationPeriods: 1, LastVerified: verifiedAt(rows, testNow.Add(-DefaultPeriod))}
		var open []Finding
		for d := 0; d < 3; d++ {
			opt.Now, opt.OpenFindings = testNow.Add(time.Duration(d)*DefaultPeriod), open
			rep := mustVerify(t, f.dir, rows, objs, nil, opt)
			if !hasFinding(rep.Findings, KindLocalUnreadable, n) || rep.Coverage[n].Local.Method != MethodError {
				t.Fatalf("day %d: local %+v findings %+v", d, rep.Coverage[n].Local, rep.Findings)
			}
			open = rep.Findings
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		opt.OpenFindings = open
		rep := mustVerify(t, f.dir, rows, objs, nil, opt)
		if len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonLocalGone {
			t.Fatalf("gone: closed %+v cov %+v", rep.Closed, rep.Coverage[n])
		}
	})
}

// shortBody serves each object cut in half with a clean io.EOF.
type shortBody struct{ *memObjects }

func (s shortBody) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.memObjects.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(&eofAfter{b: b[:len(b)/2]}), nil
}

type eofAfter struct{ b []byte }

func (e *eofAfter) Read(p []byte) (int, error) {
	if len(e.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, e.b)
	e.b = e.b[n:]
	return n, nil
}

// TestVerify_ShortObjectBodyIsError is QA r4 G4: an object body that ends
// with a clean io.EOF before the HEAD size is a cut-short read (ERROR), not
// OBJECT_CORRUPT, and closes nothing.
func TestVerify_ShortObjectBodyIsError(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
	n := rows[0].Name
	objs := shortBody{objectsFromDir2(t, f, rows)}
	open := []Finding{{Kind: KindObjectCorrupt, Subject: n, Severity: SeverityCritical}}
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, OpenFindings: open})
	c := rep.Coverage[n].Object
	if c.Method != MethodError || !errors.Is(rep.ObjectErr, io.ErrUnexpectedEOF) || hasFinding(rep.Findings, KindObjectCorrupt, n) || len(rep.Closed) != 0 {
		t.Fatalf("object %+v err %v findings %+v closed %+v", c, rep.ObjectErr, rep.Findings, rep.Closed)
	}
}

// TestVerify_FutureSealedAtIsClockSkew is QA r4 G5: a sealed_at more than an
// hour ahead is UPLOAD_CLOCK_SKEW and is not a change, so the row is read
// once, not on every hourly pass; the finding does not hold back
// last_verified_at and closes once sealed_at is sane.
func TestVerify_FutureSealedAtIsClockSkew(t *testing.T) {
	dir, rows, objs, _ := hogRows(t, 0, 1)
	n := rows[0].Name
	rows[0].SealedAt = testNow.Add(72 * time.Hour)
	last := map[string]time.Time{}
	var open []Finding
	reads := 0
	for h := 0; h < 96; h++ {
		lv := map[string]time.Time{}
		for k, v := range last {
			lv[k] = v
		}
		rep := mustVerify(t, dir, rows, objs, nil, Options{Now: testNow.Add(time.Duration(h) * time.Hour), RotationPeriods: 1000, LastVerified: lv, OpenFindings: open})
		for k, v := range rep.VerifiedAt {
			last[k] = v
		}
		// Once sealed_at is no longer skewed (h 71) it is a real change and
		// the row is read again; before that, only the first pass reads it.
		if rep.Coverage[n].Local.Method == MethodParsed && h < 71 {
			reads++
		}
		if h < 71 {
			fd, ok := findingOf(rep.Findings, KindUploadClockSkew, n)
			if !ok || fd.Severity != SeverityWarning || !rep.Coverage[n].ClockChecked {
				t.Fatalf("h %d: findings %+v", h, rep.Findings)
			}
		}
		open = rep.Findings
	}
	if reads != 1 {
		t.Fatalf("future sealed_at read %d times in 71 hourly passes", reads)
	}
	// An open UPLOAD_CLOCK_SKEW does not keep a never-verified row out of
	// the recent tier, and does not hold back last_verified_at.
	skew := []Finding{{Kind: KindUploadClockSkew, Subject: n, Severity: SeverityWarning}}
	rep := mustVerify(t, dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1000, OpenFindings: skew})
	if _, adv := rep.VerifiedAt[n]; rep.Coverage[n].Local.Method != MethodParsed || !adv {
		t.Fatalf("open skew: local %+v advanced %v", rep.Coverage[n].Local, adv)
	}
	rows[0].SealedAt = testNow.Add(-time.Hour)
	rep = mustVerify(t, dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1000, LastVerified: last, OpenFindings: []Finding{{Kind: KindUploadClockSkew, Subject: n}}})
	if len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonClockChecked {
		t.Fatalf("sane sealed_at: closed %+v", rep.Closed)
	}
}

// TestVerify_ForcedSegmentReadOncePerPeriod is QA r4 G6: with Report.Read fed
// back, a forced segment is re-read at most once per period, unless its
// local size or mtime, or its object size, changed.
func TestVerify_ForcedSegmentReadOncePerPeriod(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
	n := rows[1].Name
	objs := objectsFromDir(t, f)
	objs.obj["t/"+n][100] ^= 1
	open := []Finding{{Kind: KindObjectCorrupt, Subject: n, Severity: SeverityCritical}}
	start := slotStart(testNow, DefaultPeriod)
	read := map[string]ReadMark{}
	pass := func(at time.Time) Coverage {
		t.Helper()
		lr := map[string]ReadMark{}
		for k, v := range read {
			lr[k] = v
		}
		rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: at, OpenFindings: open, LastRead: lr})
		for k, v := range rep.Read {
			read[k] = v
		}
		if !hasFinding(rep.Findings, KindObjectCorrupt, n) && rep.Coverage[n].Object.Method == MethodHashed {
			t.Fatalf("corrupt object not reported: %+v", rep.Findings)
		}
		return rep.Coverage[n]
	}
	hashed := 0
	for h := 0; h < 48; h++ {
		c := pass(start.Add(time.Duration(h) * time.Hour))
		if c.Object.Method == MethodHashed {
			hashed++
		} else if c.Object.Reason != reasonHeldForced || c.Local.Reason != reasonHeldForced {
			t.Fatalf("h %d held without the reason: %+v", h, c)
		}
	}
	if hashed != 2 {
		t.Fatalf("forced segment hashed %d times in 48 hourly passes (two periods)", hashed)
	}
	at := start.Add(50 * time.Hour)
	if c := pass(at); c.Object.Method != MethodHashed {
		t.Fatalf("new period: %+v", c)
	}
	if c := pass(at.Add(time.Hour)); c.Object.Method == MethodHashed {
		t.Fatalf("same period, nothing changed: %+v", c)
	}
	p := filepath.Join(f.dir, n)
	mt := time.Now().Add(-time.Minute)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	if c := pass(at.Add(2 * time.Hour)); c.Local.Method != MethodParsed || c.Object.Method != MethodHashed {
		t.Fatalf("local mtime changed: %+v", c)
	}
	objs.obj["t/"+n] = append(objs.obj["t/"+n], 0)
	if c := pass(at.Add(3 * time.Hour)); c.Local.Method != MethodParsed {
		t.Fatalf("object size changed: %+v", c)
	}
}
