// Package assurance provides module-level functionality for assurance.
// input: the live ledger fixture, catalog rows copied from it, and in-memory object stores that fail, cancel or change between passes
// output: proof that Coverage records what each pass checked; that Disappeared closes a finding only for a subject checked by the method that detects it; that cancel and errors leave unreached subjects unverified; that last_verified_at advances only on content checks; that open findings force a recheck; that clock skew, missing local copies and missing digests are reported; that concurrent passes share the budget safely; and that the rotation bounds the gap
// pos: coverage-model tests for the phase 3 verifier (docs/design/0009-phase3-verifier.md §6)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// oldDayInSlot returns a day at or after from on which name is in the rotation
// slot, and a following day on which it is not.
func oldDayInSlot(t *testing.T, name string, from time.Time) (in, out time.Time) {
	t.Helper()
	for d := 0; d < 40; d++ {
		day := from.Add(time.Duration(d) * 24 * time.Hour)
		next := day.Add(24 * time.Hour)
		if InRotation(name, day, DefaultPeriod, DefaultRotationPeriods) && !InRotation(name, next, DefaultPeriod, DefaultRotationPeriods) {
			return day, next
		}
	}
	t.Fatalf("%s never in slot", name)
	return
}

// TestVerify_CorruptObjectStaysOpenUntilRehashed is QA r1 F1: a corrupt old
// object found on its slot day must not close on the next day's HEAD-only
// pass. While the local copy exists, an open OBJECT_CORRUPT forces the body to
// be hashed every pass, so a re-upload closes it at once.
func TestVerify_CorruptObjectStaysOpenUntilRehashed(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[:1]
	name, key := rows[0].Name, rows[0].ObjectKey
	objs := objectsFromDir(t, f)
	good := append([]byte(nil), objs.obj[key]...)
	flipStored(t, objs, key)
	slotDay, nextDay := oldDayInSlot(t, name, testNow)
	fresh := verifiedAt(rows, slotDay.Add(-DefaultPeriod))

	found := mustVerify(t, f.dir, rows, objs, nil, Options{Now: slotDay, LastVerified: fresh})
	if !hasFinding(found.Findings, KindObjectCorrupt, name) {
		t.Fatalf("slot day did not hash: %+v %+v", found.Findings, found.Coverage)
	}
	open := found.Findings

	// Next day, caller forgot to pass the open findings: HEAD only, nothing closes.
	unforced := mustVerify(t, f.dir, rows, objs, nil, Options{Now: nextDay, LastVerified: fresh})
	if got := unforced.Coverage[name].Object.Method; got != MethodHead {
		t.Fatalf("next day coverage %s", got)
	}
	if closed := Disappeared(open, unforced); len(closed) != 0 {
		t.Fatalf("HEAD-only pass closed %+v", closed)
	}
	if _, ok := unforced.VerifiedAt[name]; ok {
		t.Fatal("HEAD-only pass advanced last_verified_at")
	}

	// With the open finding passed in, the object is hashed off-slot and still reported.
	before := objs.openCount()
	forced := mustVerify(t, f.dir, rows, objs, nil, Options{Now: nextDay, OpenFindings: open, LastVerified: fresh})
	if objs.openCount() != before+1 || forced.Coverage[name].Object.Method != MethodHashed || forced.Coverage[name].Local.Method != MethodParsed {
		t.Fatalf("open OBJECT_CORRUPT not forced: opens %d->%d cov=%+v", before, objs.openCount(), forced.Coverage[name])
	}
	if !hasFinding(forced.Findings, KindObjectCorrupt, name) || len(forced.Closed) != 0 {
		t.Fatalf("still corrupt: %+v closed %+v", forced.Findings, forced.Closed)
	}

	// Re-uploaded: the forced recheck matches and closes it.
	objs.obj[key] = good
	fixed := mustVerify(t, f.dir, rows, objs, nil, Options{Now: nextDay, OpenFindings: open, LastVerified: fresh})
	if len(fixed.Closed) != 1 || fixed.Closed[0].Finding.Kind != KindObjectCorrupt || fixed.Closed[0].Reason != ReasonHashed {
		t.Fatalf("rehashed and matching, not closed: %+v %+v", fixed.Closed, fixed.Coverage)
	}
	if _, ok := fixed.VerifiedAt[name]; !ok {
		t.Fatal("rehashed object did not advance last_verified_at")
	}
}

// TestVerify_ForcedRecheckWithoutLocalFollowsTheRotation: with the local copy
// gone nothing on this node can repair the object, so an open OBJECT_CORRUPT
// is rehashed on its slot only, not every pass.
func TestVerify_ForcedRecheckWithoutLocalFollowsTheRotation(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[:1]
	name, key := rows[0].Name, rows[0].ObjectKey
	if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
		t.Fatal(err)
	}
	objs := objectsFromDir2(t, f, rows)
	flipStored(t, objs, key)
	open := []Finding{{Kind: KindObjectCorrupt, Subject: name}}
	slots := 0
	for d := 0; d < 45; d++ {
		now := testNow.Add(time.Duration(d) * 24 * time.Hour)
		before := objs.openCount()
		rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: now, OpenFindings: open, LastVerified: verifiedAt(rows, now.Add(-DefaultPeriod))})
		in := InRotation(name, now, DefaultPeriod, DefaultRotationPeriods)
		if in {
			slots++
		}
		if hashed := objs.openCount() - before; (hashed == 1) != in || hashed > 1 {
			t.Fatalf("day %d in slot=%v: %d GETs, cov=%+v", d, in, hashed, rep.Coverage[name])
		}
		if len(rep.Closed) != 0 || len(rep.VerifiedAt) != 0 {
			t.Fatalf("day %d closed %+v advanced %v", d, rep.Closed, rep.VerifiedAt)
		}
	}
	if slots < 2 || slots > 3 {
		t.Fatalf("%d slot days in 45", slots)
	}
}

func objectsFromDir2(t *testing.T, f *fixture, rows []VerifyRow) *memObjects {
	t.Helper()
	m := &memObjects{obj: map[string][]byte{}}
	src := filepath.Join("..", "binlog", "testdata", "ledger", "live")
	for _, row := range rows {
		b, err := os.ReadFile(filepath.Join(src, row.Name))
		if err != nil {
			t.Fatal(err)
		}
		m.obj[row.ObjectKey] = b
	}
	return m
}

func TestVerify_OpenVerifyFailedForcesObjectHash(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[:1]
	name := rows[0].Name
	objs := objectsFromDir(t, f)
	_, off := oldDayInSlot(t, name, testNow)
	fresh := verifiedAt(rows, off.Add(-DefaultPeriod))
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, LastVerified: fresh})
	if c := rep.Coverage[name]; c.Object.Method != MethodHead || c.Local.Method != MethodStat {
		t.Fatalf("off-slot coverage %+v", c)
	}
	open := []Finding{{Kind: KindVerifyFailed, Subject: name}}
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, OpenFindings: open, LastVerified: fresh})
	if c := rep.Coverage[name]; c.Object.Method != MethodHashed || c.Local.Method != MethodParsed {
		t.Fatalf("open VERIFY_FAILED did not force a parse and a hash: %+v", c)
	}
	if len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonParsed {
		t.Fatalf("re-parsed clean, VERIFY_FAILED not closed: %+v", rep.Closed)
	}
	// Other kinds do not force a download.
	for _, kind := range []string{KindObjectMissing, KindObjectSizeMismatch, KindUploadClockSkew, string(I3GTIDHole)} {
		rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, OpenFindings: []Finding{{Kind: kind, Subject: name}}, LastVerified: fresh})
		if rep.Coverage[name].Object.Method != MethodHead || objs.openCount() != 1 {
			t.Fatalf("%s forced a download: %+v", kind, rep.Coverage[name])
		}
	}
}

// TestVerify_ObjectErrorClosesNothingOnThatObject is QA M09: a HEAD that
// fails with something other than not-found leaves the object unverified.
func TestVerify_ObjectErrorClosesNothingOnThatObject(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	name := rows[0].Name
	objs := objectsFromDir(t, f)
	objs.headErr = errors.New("503 slow down")
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1})
	if rep.Coverage[name].Object.Method != MethodError || rep.ObjectErr == nil {
		t.Fatalf("coverage %+v err=%v", rep.Coverage[name], rep.ObjectErr)
	}
	prev := []Finding{
		{Kind: KindObjectMissing, Subject: name},
		{Kind: KindObjectSizeMismatch, Subject: name},
		{Kind: KindObjectCorrupt, Subject: name},
		{Kind: KindVerifyFailed, Subject: name},
	}
	closed := Disappeared(prev, rep)
	if len(closed) != 1 || closed[0].Kind != KindVerifyFailed {
		t.Fatalf("closed %+v; only the parsed local half may close", closed)
	}
	if _, ok := rep.VerifiedAt[name]; ok {
		t.Fatal("object error advanced last_verified_at")
	}
	// A GET error after a good HEAD is the same.
	objs.headErr = nil
	objs.openErr = errors.New("connection reset")
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1})
	if rep.Coverage[name].Object.Method != MethodError || hasFinding(Disappeared(prev, rep), KindObjectCorrupt, name) {
		t.Fatalf("GET error: %+v", rep.Coverage[name])
	}
}

// TestVerify_CancelLeavesUnreachedSubjectsUnverified is QA r1 F3: a cancel
// stops the pass with its error, and what it did not reach closes nothing it
// would need content for. The free half (stat, HEAD, chain) runs first, so
// what it observed is real and closes what it detects.
func TestVerify_CancelLeavesUnreachedSubjectsUnverified(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	objs := objectsFromDir(t, f)
	var prev []Finding
	for _, row := range rows {
		prev = append(prev,
			Finding{Kind: KindVerifyFailed, Subject: row.Name},
			Finding{Kind: KindObjectMissing, Subject: row.Name},
			Finding{Kind: KindObjectCorrupt, Subject: row.Name})
	}

	// Cancelled while the second object body is being opened.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objs.onOpen = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	rep, err := Verify(ctx, f.dir, rows, objs, chainOf(f), Options{Now: testNow, RotationPeriods: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pass returned %v", err)
	}
	if c := rep.Coverage[rows[1].Name]; rep.ObjectErr != nil || c.Object.Method != MethodHead {
		t.Fatalf("a cancel is not a read error: err=%v cov=%+v", rep.ObjectErr, c)
	}
	closed := map[string]bool{}
	for _, c := range Disappeared(prev, rep) {
		closed[c.Kind+" "+c.Subject] = true
	}
	first, rest := rows[0].Name, rows[1:]
	if !closed[KindObjectCorrupt+" "+first] || !closed[KindVerifyFailed+" "+first] {
		t.Fatalf("the first subject was fully checked and should close: %v", closed)
	}
	if _, ok := rep.VerifiedAt[first]; !ok {
		t.Fatalf("first subject checked by content, not advanced: %+v", rep.Coverage[first])
	}
	for _, row := range rest {
		// The queue is local before object per segment: the second segment's
		// local was parsed before its object was opened and the pass cancelled.
		if closed[KindObjectCorrupt+" "+row.Name] || (row.Name != rows[1].Name && closed[KindVerifyFailed+" "+row.Name]) {
			t.Fatalf("unreached content of %s closed: %v", row.Name, closed)
		}
		if !closed[KindObjectMissing+" "+row.Name] {
			t.Fatalf("HEAD found %s before the cancel; I6_MISSING_OBJECT should close: %v", row.Name, closed)
		}
		if _, ok := rep.VerifiedAt[row.Name]; ok {
			t.Fatalf("unreached %s advanced last_verified_at", row.Name)
		}
		if c := rep.Coverage[row.Name]; c.Object.Method == MethodHashed {
			t.Fatalf("unreached %s has object coverage %+v", row.Name, c)
		}
	}

	// Cancelled before the HEADs: nothing at all is covered.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	rep, err = Verify(ctx2, f.dir, rows, objectsFromDir(t, f), chainOf(f), Options{Now: testNow, RotationPeriods: 1})
	if !errors.Is(err, context.Canceled) || len(rep.Coverage) != 0 || rep.ChainChecked || len(Disappeared(prev, rep)) != 0 {
		t.Fatalf("pre-cancelled pass: err=%v coverage=%+v chain=%v", err, rep.Coverage, rep.ChainChecked)
	}

	// Cancelled during the HEADs: the rows after it are unreached.
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	heads := &cancelOnHead{memObjects: objectsFromDir(t, f), at: 2, cancel: cancel3}
	rep, err = Verify(ctx3, f.dir, rows, heads, chainOf(f), Options{Now: testNow, RotationPeriods: 1})
	if !errors.Is(err, context.Canceled) || rep.ChainChecked {
		t.Fatalf("cancelled in HEADs: err=%v chain=%v", err, rep.ChainChecked)
	}
	for _, row := range rows[2:] {
		if c, ok := rep.Coverage[row.Name]; ok {
			t.Fatalf("%s reached after the cancel: %+v", row.Name, c)
		}
	}
	if got := Disappeared(prev, rep); len(got) != 1 || got[0].Subject != rows[0].Name || got[0].Kind != KindObjectMissing {
		t.Fatalf("closed %+v; only the first HEAD answered", got)
	}
}

// cancelOnHead cancels the pass when HEAD number at is asked.
type cancelOnHead struct {
	*memObjects
	n      int
	at     int
	cancel context.CancelFunc
}

func (c *cancelOnHead) Head(ctx context.Context, key string) (int64, error) {
	c.n++
	if c.n == c.at {
		c.cancel()
		return 0, ctx.Err()
	}
	return c.memObjects.Head(ctx, key)
}

func TestDisappeared_ClosesOnlyWithTheDetectingMethod(t *testing.T) {
	const s = "mysql-bin.000004"
	found := func(m Method) Check { return Check{Method: m, Found: m != "" && m != MethodMissing, SizeChecked: true} }
	cov := func(local, object Check, clock bool) Report {
		return Report{Coverage: map[string]Coverage{s: {Local: local, Object: object, ClockChecked: clock}}}
	}
	m := func(method Method) Check { return Check{Method: method} }
	cases := []struct {
		kind   string
		rep    Report
		reason string
	}{
		{KindVerifyFailed, cov(m(MethodParsed), m(""), false), ReasonParsed},
		{KindVerifyFailed, cov(found(MethodStat), found(MethodHashed), true), ""},
		{KindVerifyFailed, cov(m(MethodAbsent), found(MethodHashed), true), ""},
		{KindVerifyFailed, cov(m(MethodAbsent), Check{Method: MethodHashed, Digest: "catalog", Found: true, Parsed: true}, true), ReasonObjectParsed},
		{KindVerifyFailed, cov(m(MethodAbsent), Check{Method: MethodHashed, Digest: "catalog", Found: true}, true), ""},
		{KindVerifyFailed, cov(m(MethodAbsent), Check{Method: MethodHashed, Digest: "local", Found: true, Parsed: true}, true), ""},
		{KindVerifyFailed, cov(m(MethodAbsent), Check{Method: MethodHead, Digest: "catalog", Found: true, Parsed: true}, true), ""},
		{KindVerifyFailed, cov(m(MethodAbsent), Check{Method: MethodHashed, Digest: "local", Found: true}, true), ""},
		{KindVerifyFailed, cov(m(MethodAbsent), Check{Method: MethodHead, Digest: "catalog", Found: true}, true), ""},
		{KindVerifyFailed, cov(m(MethodSkipped), Check{Method: MethodHashed, Digest: "catalog", Found: true}, true), ""},
		{KindVerifyFailed, cov(m(MethodError), found(MethodHashed), true), ""},
		{KindVerifyFailed, cov(m(MethodSkipped), found(MethodHashed), true), ""},
		{KindVerifyFailed, Report{ChainChecked: true}, ""},
		{KindObjectCorrupt, cov(m(MethodParsed), found(MethodHashed), true), ReasonHashed},
		{KindObjectCorrupt, cov(m(MethodParsed), found(MethodHead), true), ""},
		{KindObjectCorrupt, cov(m(MethodParsed), found(MethodUnverifiable), true), ""},
		{KindObjectCorrupt, cov(m(MethodParsed), m(MethodNotApplicable), true), ""},
		{KindObjectCorrupt, cov(m(MethodParsed), found(MethodError), true), ""},
		{KindObjectCorrupt, cov(m(MethodParsed), found(MethodSkipped), true), ""},
		{KindObjectCorrupt, Report{ChainChecked: true}, ""},
		{KindObjectMissing, cov(m(""), found(MethodHead), true), ReasonObjectFound},
		{KindObjectMissing, cov(m(""), found(MethodSkipped), true), ReasonObjectFound},
		{KindObjectMissing, cov(m(""), m(MethodError), true), ""},
		{KindObjectMissing, cov(m(""), m(MethodSkipped), true), ""},
		{KindObjectMissing, cov(m(""), m(MethodNotApplicable), true), ""},
		{KindObjectMissing, cov(m(""), m(MethodMissing), true), ""},
		{KindObjectMissing, cov(m(""), m(MethodHead), true), ""},
		{KindObjectMissing, Report{}, ""},
		{KindObjectMissing, Report{ChainChecked: true}, ReasonChainChecked},
		{KindObjectMissing, Report{ChainChecked: true, Coverage: map[string]Coverage{s: {}}}, ""},
		{KindObjectSizeMismatch, cov(m(""), found(MethodHead), true), ReasonSizeCompared},
		{KindObjectSizeMismatch, cov(m(""), Check{Method: MethodHead, Found: true}, true), ""},
		{KindObjectSizeMismatch, cov(m(""), Check{Method: MethodHashed, Found: true, Digest: "catalog"}, true), ""},
		{KindObjectSizeMismatch, cov(m(""), m(MethodError), true), ""},
		{KindObjectSizeMismatch, cov(m(""), m(MethodMissing), true), ""},
		{KindObjectSizeMismatch, Report{}, ""},
		{KindObjectSizeMismatch, Report{ChainChecked: true}, ReasonChainChecked},
		{KindObjectSizeMismatch, Report{ChainChecked: true, Coverage: map[string]Coverage{s: {}}}, ""},
		{KindLocalMissing, cov(found(MethodParsed), m(""), false), ReasonLocalPresent},
		{KindLocalMissing, cov(found(MethodStat), m(""), false), ReasonLocalPresent},
		{KindLocalMissing, cov(found(MethodSkipped), m(""), false), ReasonLocalPresent},
		{KindLocalMissing, cov(m(MethodAbsent), m(""), false), ""},
		{KindLocalMissing, cov(m(MethodSkipped), m(""), false), ""},
		{KindLocalMissing, cov(m(MethodError), m(""), false), ""},
		{KindLocalMissing, Report{ChainChecked: true}, ReasonChainChecked},
		{KindUploadClockSkew, cov(m(""), m(MethodError), true), ReasonClockChecked},
		{KindUploadClockSkew, cov(m(""), m(""), false), ""},
		{KindUploadClockSkew, Report{ChainChecked: true}, ""},
		{string(I3GTIDHole), cov(found(MethodParsed), found(MethodHashed), true), ""},
		{string(I3GTIDHole), Report{ChainChecked: true}, ReasonChainChecked},
	}
	for i, c := range cases {
		got := Closures([]Finding{{Kind: c.kind, Subject: s}}, c.rep)
		reason := ""
		if len(got) == 1 {
			reason = got[0].Reason
		}
		if reason != c.reason {
			t.Errorf("case %d %s coverage=%+v chain=%v: closed with %q want %q", i, c.kind, c.rep.Coverage, c.rep.ChainChecked, reason, c.reason)
		}
		if (len(Disappeared([]Finding{{Kind: c.kind, Subject: s}}, c.rep)) == 1) != (c.reason != "") {
			t.Errorf("case %d: Disappeared disagrees with Closures", i)
		}
	}
	// Only the sha-mismatch variant closes as "local removed; object matches catalog".
	byHash := cov(m(MethodAbsent), Check{Method: MethodHashed, Digest: "catalog", Found: true, Parsed: true}, true)
	for detail, want := range map[string]string{DetailSHA: ReasonLocalRemoved, DetailParse: ReasonObjectParsed, DetailPositions: ReasonObjectParsed, DetailGTID: ReasonObjectParsed, DetailSize: ReasonObjectParsed} {
		got := Closures([]Finding{{Kind: KindVerifyFailed, Subject: s, Detail: detail}}, byHash)
		if len(got) != 1 || got[0].Reason != want {
			t.Errorf("detail %q: %+v want %q", detail, got, want)
		}
	}
	// A VERIFY_FAILED does not close by the object while OBJECT_CORRUPT is reported on it.
	withCorrupt := cov(m(MethodAbsent), Check{Method: MethodHashed, Digest: "catalog", Found: true, Parsed: true}, true)
	withCorrupt.Findings = []Finding{{Kind: KindObjectCorrupt, Subject: s}}
	if got := Closures([]Finding{{Kind: KindVerifyFailed, Subject: s, Detail: DetailSHA}}, withCorrupt); len(got) != 0 {
		t.Errorf("closed despite OBJECT_CORRUPT: %+v", got)
	}
	// Reported again is never closed, whatever the coverage.
	rep := cov(found(MethodParsed), found(MethodHashed), true)
	rep.Findings = []Finding{{Kind: KindObjectCorrupt, Subject: s}}
	if len(Disappeared([]Finding{{Kind: KindObjectCorrupt, Subject: s}}, rep)) != 0 {
		t.Fatal("closed a finding the pass reported again")
	}
	// Local gone, object hashed against the catalog but corrupt: VERIFY_FAILED stays.
	rep = cov(m(MethodAbsent), Check{Method: MethodHashed, Digest: "catalog", Found: true}, true)
	rep.Findings = []Finding{{Kind: KindObjectCorrupt, Subject: s}}
	if len(Disappeared([]Finding{{Kind: KindVerifyFailed, Subject: s}}, rep)) != 0 {
		t.Fatal("VERIFY_FAILED closed by a corrupt object")
	}
}

// TestVerify_LastVerifiedOnlyForContentChecks is QA F4.
func TestVerify_LastVerifiedOnlyForContentChecks(t *testing.T) {
	f := live(t)
	all := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
	objs := objectsFromDir(t, f)
	// 000003: uploaded, local removed by retention, catalog digest empty -> UNVERIFIABLE.
	// 000004: sealed, not uploaded, local file missing -> I6_MISSING_LOCAL.
	// 000005: not uploaded, local removed by retention -> ABSENT, nothing to check.
	// 000006: uploaded, local present, old, off-slot -> PARSED + HEAD.
	rows := append([]VerifyRow(nil), all...)
	rows[0].SHA256 = ""
	rows[1].UploadState, rows[1].ObjectKey = "UPLOAD_FAILED", ""
	rows[2].UploadState, rows[2].ObjectKey, rows[2].RetentionRemoved = "", "", true
	for _, i := range []int{0, 1, 2} {
		if err := os.Remove(filepath.Join(f.dir, rows[i].Name)); err != nil {
			t.Fatal(err)
		}
	}
	_, off := oldDayInSlot(t, rows[3].Name, testNow)
	for InRotation(rows[0].Name, off, DefaultPeriod, DefaultRotationPeriods) {
		off = off.Add(24 * time.Hour)
	}
	for InRotation(rows[3].Name, off, DefaultPeriod, DefaultRotationPeriods) {
		off = off.Add(24 * time.Hour)
	}
	fresh := verifiedAt(rows, off.Add(-DefaultPeriod))
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, OpenFindings: []Finding{{Kind: KindObjectCorrupt, Subject: rows[0].Name}}, LastVerified: fresh})
	want := map[string][2]Method{
		rows[0].Name: {MethodAbsent, MethodUnverifiable},
		rows[1].Name: {MethodMissing, MethodNotApplicable},
		rows[2].Name: {MethodAbsent, MethodNotApplicable},
		rows[3].Name: {MethodStat, MethodHead},
	}
	for name, w := range want {
		if c := rep.Coverage[name]; c.Local.Method != w[0] || c.Object.Method != w[1] {
			t.Fatalf("%s coverage %+v, want %v", name, c, w)
		}
	}
	if len(rep.VerifiedAt) != 0 {
		t.Fatalf("advanced without a content check: %v", rep.VerifiedAt)
	}
	if objs.openCount() != 0 {
		t.Fatalf("downloaded %d bodies with no digest or off-slot", objs.openCount())
	}
	if rep.UnverifiableNoDigest != 1 {
		t.Fatalf("UnverifiableNoDigest %d", rep.UnverifiableNoDigest)
	}
	got, ok := findingOf(rep.Findings, KindLocalMissing, rows[1].Name)
	if !ok || got.Severity != SeverityCritical {
		t.Fatalf("missing local copy of a not-uploaded segment: %+v", rep.Findings)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("unexpected findings %+v", rep.Findings)
	}
	if len(Disappeared([]Finding{{Kind: KindObjectCorrupt, Subject: rows[0].Name}}, rep)) != 0 {
		t.Fatal("an unverifiable object closed OBJECT_CORRUPT")
	}
	// Uploaded within the window: 000006 is hashed and advances.
	rows[3].UploadedAt = off.Add(-time.Hour)
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, LastVerified: fresh})
	if _, ok := rep.VerifiedAt[rows[3].Name]; !ok || len(rep.VerifiedAt) != 1 {
		t.Fatalf("VerifiedAt %v", rep.VerifiedAt)
	}
}

func TestVerify_DigestSourcesAndBackfillCount(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:2]
	objs := objectsFromDir(t, f)
	// Catalog digest empty, local present: the object is compared with the local file.
	rows[0].SHA256 = ""
	flipStored(t, objs, rows[0].ObjectKey)
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow})
	got, ok := findingOf(rep.Findings, KindObjectCorrupt, rows[0].Name)
	if !ok || rep.Coverage[rows[0].Name].Object.Digest != "local" {
		t.Fatalf("empty catalog digest, flipped object: %+v %+v", rep.Findings, rep.Coverage[rows[0].Name])
	}
	if want := "source=local"; !strings.Contains(got.Evidence, want) {
		t.Fatalf("evidence %q", got.Evidence)
	}
	if rep.NoDigestLocal != 1 || rep.UnverifiableNoDigest != 0 {
		t.Fatalf("NoDigestLocal=%d UnverifiableNoDigest=%d", rep.NoDigestLocal, rep.UnverifiableNoDigest)
	}
	// The local file is corrupt: the object is compared with the catalog row instead.
	local := filepath.Join(f.dir, rows[1].Name)
	b, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	orig := append([]byte(nil), b...)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(local, b, 0o644); err != nil {
		t.Fatal(err)
	}
	rep = mustVerify(t, f.dir, rows[1:], objs, nil, Options{Now: testNow})
	if c := rep.Coverage[rows[1].Name]; c.Object.Method != MethodHashed || c.Object.Digest != "catalog" || hasFinding(rep.Findings, KindObjectCorrupt, rows[1].Name) {
		t.Fatalf("corrupt local, good object: %+v %+v", rep.Findings, c)
	}
	// A wrong catalog digest: VERIFY_FAILED, and the object is still compared with
	// the catalog row, not with the local file that disagrees with it.
	if err := os.WriteFile(local, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	wrong := rows[1]
	wrong.SHA256 = strings.Repeat("a", 64)
	rep = mustVerify(t, f.dir, []VerifyRow{wrong}, objs, nil, Options{Now: testNow})
	if c := rep.Coverage[wrong.Name]; !hasFinding(rep.Findings, KindVerifyFailed, wrong.Name) || c.Object.Digest != "catalog" || !hasFinding(rep.Findings, KindObjectCorrupt, wrong.Name) {
		t.Fatalf("wrong catalog digest: %+v %+v", rep.Findings, c)
	}
	if err := os.WriteFile(local, b, 0o644); err != nil {
		t.Fatal(err)
	}
	// Corrupt local and no catalog digest: nothing to compare with.
	rows[1].SHA256 = ""
	rep = mustVerify(t, f.dir, rows[1:], objs, nil, Options{Now: testNow})
	if c := rep.Coverage[rows[1].Name]; c.Object.Method != MethodUnverifiable || rep.UnverifiableNoDigest != 1 || rep.NoDigestLocal != 1 {
		t.Fatalf("corrupt local, no digest: %+v unverifiable=%d nodigest=%d", c, rep.UnverifiableNoDigest, rep.NoDigestLocal)
	}
}

// TestVerify_LocalReadErrorLeavesChainUnchecked is QA M22.
func TestVerify_LocalReadErrorLeavesChainUnchecked(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	chain := chainOf(f)
	name := rows[1].Name
	p := filepath.Join(f.dir, name)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.dir, "does-not-exist"), p); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, f.dir, rows, objectsFromDir2(t, f, rows), chain, Options{Now: testNow, RotationPeriods: 1})
	if rep.Coverage[name].Local.Method != MethodError || rep.LocalErr == nil {
		t.Fatalf("coverage %+v err=%v", rep.Coverage[name], rep.LocalErr)
	}
	if rep.ChainChecked {
		t.Fatal("chain checked with a local read error")
	}
	if len(Disappeared([]Finding{{Kind: KindVerifyFailed, Subject: name}, {Kind: string(I3GTIDHole), Subject: name}}, rep)) != 0 {
		t.Fatal("closed on an unreadable segment")
	}
	if _, ok := rep.VerifiedAt[name]; ok {
		t.Fatal("unreadable segment advanced")
	}
}

// TestVerify_UnlistedDirectoryIsNotMissing: a node without the directory
// (per_node, not the chain owner) must not report missing local copies.
func TestVerify_UnlistedDirectoryIsNotMissing(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	for i := range rows {
		rows[i].UploadState, rows[i].ObjectKey = "UPLOAD_FAILED", ""
	}
	rep := mustVerify(t, filepath.Join(f.dir, "nope"), rows, nil, chainOf(f), Options{Now: testNow})
	if len(rep.Findings) != 0 || rep.ChainChecked || rep.LocalErr == nil {
		t.Fatalf("unlisted dir: findings=%+v chain=%v err=%v", rep.Findings, rep.ChainChecked, rep.LocalErr)
	}
	for _, row := range rows {
		if rep.Coverage[row.Name].Local.Method != MethodSkipped {
			t.Fatalf("%s coverage %+v", row.Name, rep.Coverage[row.Name])
		}
	}
	if len(Disappeared([]Finding{{Kind: KindLocalMissing, Subject: rows[0].Name}, {Kind: KindVerifyFailed, Subject: rows[0].Name}}, rep)) != 0 {
		t.Fatal("closed with no listing")
	}
}

func TestVerify_LocalMissingClosesWhenTheFileIsBack(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	rows[0].UploadState, rows[0].ObjectKey = "UPLOADING", ""
	p := filepath.Join(f.dir, rows[0].Name)
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	gone := mustVerify(t, f.dir, rows, nil, nil, Options{Now: testNow})
	if !hasFinding(gone.Findings, KindLocalMissing, rows[0].Name) {
		t.Fatalf("findings %+v", gone.Findings)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	back := mustVerify(t, f.dir, rows, nil, nil, Options{Now: testNow})
	if !hasFinding(Disappeared(gone.Findings, back), KindLocalMissing, rows[0].Name) {
		t.Fatalf("not closed: %+v", back.Coverage)
	}
}

// TestVerify_FutureUploadedAtIsClockSkew is QA F5.
func TestVerify_FutureUploadedAtIsClockSkew(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(10*365*24*time.Hour))[:1]
	name := rows[0].Name
	objs := objectsFromDir(t, f)
	// Verified once already: a skewed uploaded_at is not a change, so the
	// row is not read again (a never-verified row is read once, from the tail).
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: -1, LastVerified: verifiedAt(rows, testNow.Add(-48*time.Hour))})
	got, ok := findingOf(rep.Findings, KindUploadClockSkew, name)
	if !ok || got.Severity != SeverityWarning {
		t.Fatalf("findings %+v", rep.Findings)
	}
	if objs.openCount() != 0 || rep.Coverage[name].Object.Method != MethodHead {
		t.Fatalf("a skewed row was downloaded as recent: opens=%d cov=%+v", objs.openCount(), rep.Coverage[name])
	}
	rows[0].UploadedAt = testNow.Add(-2 * time.Hour)
	fixed := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: -1})
	if !hasFinding(Disappeared(rep.Findings, fixed), KindUploadClockSkew, name) {
		t.Fatalf("clock fixed, not closed: %+v", fixed.Findings)
	}
	// Cancelled while the skewed row's body is being read: the skew was
	// observed (no I/O needed) and stays reported; the body is unreached.
	rows[0].UploadedAt = testNow.Add(10 * 365 * 24 * time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objs.onOpen = func(int) { cancel() }
	cut, err := Verify(ctx, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1})
	objs.onOpen = nil
	if err == nil || !hasFinding(cut.Findings, KindUploadClockSkew, name) || cut.Coverage[name].Object.Method != MethodHead {
		t.Fatalf("cancelled skewed row: err=%v findings=%+v cov=%+v", err, cut.Findings, cut.Coverage[name])
	}
	if len(Disappeared(rep.Findings, cut)) != 0 {
		t.Fatal("cancelled pass closed the skew it re-observed")
	}
	// Within the tolerance it is recent and not skew.
	rows[0].UploadedAt = testNow.Add(30 * time.Minute)
	near := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: -1})
	if hasFinding(near.Findings, KindUploadClockSkew, name) || near.Coverage[name].Object.Method != MethodHashed {
		t.Fatalf("30m ahead: %+v %+v", near.Findings, near.Coverage[name])
	}
}

func TestRecentUpload_Window(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want bool
		skew bool
	}{
		{"zero", time.Time{}, false, false},
		{"now", now, true, false},
		{"13h ago", now.Add(-13 * time.Hour), true, false},
		{"exactly 24h", now.Add(-24 * time.Hour), true, false},
		{"24h+1ns", now.Add(-24*time.Hour - 1), false, false},
		{"30m ahead", now.Add(30 * time.Minute), true, false},
		{"exactly 1h ahead", now.Add(time.Hour), true, false},
		{"1h+1ns ahead", now.Add(time.Hour + 1), false, true},
		{"10y ahead", now.Add(10 * 365 * 24 * time.Hour), false, true},
		{"23h ago, CST", now.Add(-23 * time.Hour).In(time.FixedZone("CST", 8*3600)), true, false},
	}
	for _, c := range cases {
		if got := RecentUpload(c.at, now, DefaultRecent); got != c.want {
			t.Errorf("%s: recent=%v want %v", c.name, got, c.want)
		}
		if got := ClockSkewed(c.at, now); got != c.skew {
			t.Errorf("%s: skew=%v want %v", c.name, got, c.skew)
		}
	}
	if !RecentUpload(now.Add(-2*time.Hour), now, 3*time.Hour) || RecentUpload(now.Add(-4*time.Hour), now, 3*time.Hour) {
		t.Error("custom window not honoured")
	}
}

// TestRotation_BoundsTheGap: with the default rotation every name has its
// slot exactly once every 20 periods, about 1/20 of names per period, and the
// worst gap is rotation x period with no rounding.
func TestRotation_BoundsTheGap(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		rotation int
		period   time.Duration
		want     time.Duration
	}{
		{0, 0, 20 * 24 * time.Hour},
		{20, 24 * time.Hour, 480 * time.Hour},
		{7, 48 * time.Hour, 336 * time.Hour},
		{3, 24 * time.Hour, 72 * time.Hour},
		{1, time.Hour, time.Hour},
		{-1, 24 * time.Hour, 0},
	} {
		if got := WorstGap(c.rotation, c.period); got != c.want {
			t.Fatalf("WorstGap(%d, %s)=%s want %s (rotation x period, no rounding)", c.rotation, c.period, got, c.want)
		}
	}
	const names, days = 2000, 365
	last := make([]int, names)
	for i := range last {
		last[i] = -1
	}
	maxGap, minGap := 0, days
	for d := 0; d < days; d++ {
		now := start.Add(time.Duration(d)*24*time.Hour + 13*time.Hour)
		drawn := 0
		for i := 0; i < names; i++ {
			name := fmt.Sprintf("mysql-bin.%06d", i)
			in := InRotation(name, now, DefaultPeriod, DefaultRotationPeriods)
			if in != InRotation(name, now.Add(-13*time.Hour), DefaultPeriod, DefaultRotationPeriods) ||
				in != InRotation(name, now.Add(10*time.Hour+59*time.Minute), DefaultPeriod, DefaultRotationPeriods) {
				t.Fatalf("%s changed inside one UTC day", name)
			}
			if !in {
				continue
			}
			drawn++
			if last[i] >= 0 {
				gap := d - last[i]
				if gap > maxGap {
					maxGap = gap
				}
				if gap < minGap {
					minGap = gap
				}
			} else if d >= 20 {
				t.Fatalf("%s first drawn on day %d", name, d)
			}
			last[i] = d
		}
		if drawn < 60 || drawn > 140 {
			t.Fatalf("day %d drew %d of %d, want about 100", d, drawn, names)
		}
	}
	for i, d := range last {
		if days-1-d >= 20 {
			t.Fatalf("mysql-bin.%06d not drawn in the last 20 days", i)
		}
	}
	if maxGap != 20 || minGap != 20 {
		t.Fatalf("gap min %d max %d, want exactly 20", minGap, maxGap)
	}
	if InRotation("x", start, DefaultPeriod, -1) || !InRotation("x", start, DefaultPeriod, 1) {
		t.Fatal("a negative rotation draws nothing; a rotation of 1 draws everything")
	}
	// Any rotation length is taken as given (no rounding): with 7 periods every
	// name is drawn exactly once in every 7 consecutive periods.
	for _, n := range []int{3, 7, 30, 33} {
		for i := 0; i < 200; i++ {
			name := fmt.Sprintf("x.%06d", i)
			drawn := 0
			for d := 0; d < n; d++ {
				if InRotation(name, start.Add(time.Duration(d)*48*time.Hour), 48*time.Hour, n) {
					drawn++
				}
			}
			if drawn != 1 {
				t.Fatalf("rotation %d: %s drawn %d times in one rotation", n, name, drawn)
			}
		}
	}
}

// TestVerify_ConcurrentPassesShareOneBudget is item 2: run with -race. Each
// Report.Bytes is that pass's reads; the budget's total is both together.
func TestVerify_ConcurrentPassesShareOneBudget(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	single := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: testNow, RotationPeriods: 1})
	budget := NewByteBudget(1 << 30)
	var wg sync.WaitGroup
	got := make([]int64, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rep, err := Verify(context.Background(), f.dir, rows, objectsFromDir2(t, f, rows), nil, Options{Now: testNow, RotationPeriods: 1, Budget: budget})
			got[i], errs[i] = rep.Bytes, err
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if got[i] != single.Bytes {
			t.Fatalf("pass %d Bytes=%d, a lone pass reads %d", i, got[i], single.Bytes)
		}
	}
	if budget.Charged() != 2*single.Bytes {
		t.Fatalf("budget charged %d, want %d", budget.Charged(), 2*single.Bytes)
	}
	again := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: testNow, RotationPeriods: 1, Budget: budget})
	if again.Bytes != single.Bytes {
		t.Fatalf("third pass on a used budget reports %d, want %d", again.Bytes, single.Bytes)
	}
}
