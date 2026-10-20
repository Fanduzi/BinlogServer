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
		if InRotation(name, day, DefaultPeriod, DefaultSamplePercent) && !InRotation(name, next, DefaultPeriod, DefaultSamplePercent) {
			return day, next
		}
	}
	t.Fatalf("%s never in slot", name)
	return
}

// TestVerify_CorruptObjectStaysOpenUntilRehashed is QA F1: a corrupt old
// object found on its slot day must not close on the next day's HEAD-only
// pass, and an open OBJECT_CORRUPT forces the body to be hashed every pass.
func TestVerify_CorruptObjectStaysOpenUntilRehashed(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[:1]
	name, key := rows[0].Name, rows[0].ObjectKey
	if err := os.Remove(filepath.Join(f.dir, name)); err != nil { // retention: the object is the only copy
		t.Fatal(err)
	}
	objs := objectsFromDir2(t, f, rows)
	good := append([]byte(nil), objs.obj[key]...)
	flipStored(t, objs, key)
	slotDay, nextDay := oldDayInSlot(t, name, testNow)

	found := mustVerify(t, f.dir, rows, objs, nil, Options{Now: slotDay})
	if !hasFinding(found.Findings, KindObjectCorrupt, name) {
		t.Fatalf("slot day did not hash: %+v %+v", found.Findings, found.Coverage)
	}
	open := found.Findings

	// Next day, caller forgot to pass the open findings: HEAD only, nothing closes.
	unforced := mustVerify(t, f.dir, rows, objs, nil, Options{Now: nextDay})
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
	forced := mustVerify(t, f.dir, rows, objs, nil, Options{Now: nextDay, OpenFindings: open})
	if objs.openCount() != before+1 || forced.Coverage[name].Object.Method != MethodHashed {
		t.Fatalf("open OBJECT_CORRUPT not forced: opens %d->%d cov=%+v", before, objs.openCount(), forced.Coverage[name])
	}
	if !hasFinding(forced.Findings, KindObjectCorrupt, name) || len(Disappeared(open, forced)) != 0 {
		t.Fatalf("still corrupt: %+v", forced.Findings)
	}

	// Re-uploaded: the forced recheck matches and closes it.
	objs.obj[key] = good
	fixed := mustVerify(t, f.dir, rows, objs, nil, Options{Now: nextDay, OpenFindings: open})
	if !hasFinding(Disappeared(open, fixed), KindObjectCorrupt, name) {
		t.Fatalf("rehashed and matching, not closed: %+v %+v", fixed.Findings, fixed.Coverage)
	}
	if _, ok := fixed.VerifiedAt[name]; !ok {
		t.Fatal("rehashed object did not advance last_verified_at")
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
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: off})
	if rep.Coverage[name].Object.Method != MethodHead {
		t.Fatalf("off-slot coverage %+v", rep.Coverage[name])
	}
	open := []Finding{{Kind: KindVerifyFailed, Subject: name}}
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, OpenFindings: open})
	if rep.Coverage[name].Object.Method != MethodHashed {
		t.Fatalf("open VERIFY_FAILED did not force a hash: %+v", rep.Coverage[name])
	}
	if !hasFinding(Disappeared(open, rep), KindVerifyFailed, name) {
		t.Fatal("re-parsed clean, VERIFY_FAILED not closed")
	}
	// Other kinds do not force a download.
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, OpenFindings: []Finding{{Kind: KindObjectMissing, Subject: name}}})
	if rep.Coverage[name].Object.Method != MethodHead {
		t.Fatalf("I6_MISSING_OBJECT forced a download: %+v", rep.Coverage[name])
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
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: 100})
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
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: 100})
	if rep.Coverage[name].Object.Method != MethodError || hasFinding(Disappeared(prev, rep), KindObjectCorrupt, name) {
		t.Fatalf("GET error: %+v", rep.Coverage[name])
	}
}

// TestVerify_CancelLeavesUnreachedSubjectsUnverified is QA F3: an early
// return closes nothing it did not reach, whatever the caller does with err.
func TestVerify_CancelLeavesUnreachedSubjectsUnverified(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	objs := objectsFromDir(t, f)
	var prev []Finding
	for _, row := range rows {
		prev = append(prev,
			Finding{Kind: KindVerifyFailed, Subject: row.Name},
			Finding{Kind: KindObjectMissing, Subject: row.Name},
			Finding{Kind: KindObjectCorrupt, Subject: row.Name},
			Finding{Kind: string(I3GTIDHole), Subject: row.Name})
	}

	// Cancelled while the second object body is being opened.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objs.onOpen = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	rep, err := Verify(ctx, f.dir, rows, objs, chainOf(f), Options{Now: testNow, SamplePercent: 100})
	if err == nil {
		t.Fatal("cancelled pass returned no error")
	}
	if rep.ChainChecked {
		t.Fatal("chain marked checked on a cancelled pass")
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
		if closed[KindObjectCorrupt+" "+row.Name] || closed[KindObjectMissing+" "+row.Name] {
			t.Fatalf("unreached object %s closed: %v", row.Name, closed)
		}
		if _, ok := rep.VerifiedAt[row.Name]; ok {
			t.Fatalf("unreached %s advanced last_verified_at", row.Name)
		}
		if rep.Coverage[row.Name].Object.Method != "" {
			t.Fatalf("unreached %s has object coverage %+v", row.Name, rep.Coverage[row.Name])
		}
	}
	for k := range closed {
		if k[:len(I3GTIDHole)] == string(I3GTIDHole) {
			t.Fatalf("chain finding closed on a cancelled pass: %s", k)
		}
	}

	// Cancelled during the local parse: nothing at all is covered.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	rep, err = Verify(ctx2, f.dir, rows, objectsFromDir(t, f), chainOf(f), Options{Now: testNow, SamplePercent: 100, Budget: NewByteBudget(1000)})
	if err == nil {
		t.Fatal("timed-out pass returned no error")
	}
	if got := Disappeared(prev, rep); len(got) != 0 || len(rep.VerifiedAt) != 0 || len(rep.Coverage) != 0 {
		t.Fatalf("timed-out local scan closed %+v, advanced %v, covered %+v", got, rep.VerifiedAt, rep.Coverage)
	}
}

func TestDisappeared_ClosesOnlyWithTheDetectingMethod(t *testing.T) {
	const s = "mysql-bin.000004"
	cov := func(local, object Method, clock bool) Report {
		return Report{Coverage: map[string]Coverage{s: {Local: Check{Method: local}, Object: Check{Method: object}, ClockChecked: clock}}}
	}
	cases := []struct {
		kind  string
		rep   Report
		close bool
	}{
		{KindVerifyFailed, cov(MethodParsed, "", false), true},
		{KindVerifyFailed, cov(MethodStat, MethodHashed, true), false},
		{KindVerifyFailed, cov(MethodAbsent, MethodHashed, true), false},
		{KindVerifyFailed, cov(MethodError, MethodHashed, true), false},
		{KindVerifyFailed, cov(MethodSkipped, MethodHashed, true), false},
		{KindVerifyFailed, Report{ChainChecked: true}, false},
		{KindObjectCorrupt, cov(MethodParsed, MethodHashed, true), true},
		{KindObjectCorrupt, cov(MethodParsed, MethodHead, true), false},
		{KindObjectCorrupt, cov(MethodParsed, MethodUnverifiable, true), false},
		{KindObjectCorrupt, cov(MethodParsed, MethodNotApplicable, true), false},
		{KindObjectCorrupt, cov(MethodParsed, MethodError, true), false},
		{KindObjectCorrupt, Report{ChainChecked: true}, false},
		{KindObjectMissing, cov("", MethodHead, true), true},
		{KindObjectMissing, cov("", MethodUnverifiable, true), true},
		{KindObjectMissing, cov("", MethodHashed, true), true},
		{KindObjectMissing, cov("", MethodError, true), false},
		{KindObjectMissing, cov("", MethodSkipped, true), false},
		{KindObjectMissing, cov("", MethodNotApplicable, true), false},
		{KindObjectMissing, Report{}, false},
		{KindObjectMissing, Report{ChainChecked: true}, true},
		{KindObjectSizeMismatch, cov("", MethodHead, true), true},
		{KindObjectSizeMismatch, cov("", MethodError, true), false},
		{KindObjectSizeMismatch, cov("", MethodMissing, true), false},
		{KindObjectSizeMismatch, Report{}, false},
		{KindObjectSizeMismatch, Report{ChainChecked: true}, true},
		{KindLocalMissing, cov(MethodParsed, "", false), true},
		{KindLocalMissing, cov(MethodStat, "", false), true},
		{KindLocalMissing, cov(MethodSkipped, "", false), false},
		{KindLocalMissing, cov(MethodError, "", false), false},
		{KindLocalMissing, Report{ChainChecked: true}, true},
		{KindUploadClockSkew, cov("", MethodError, true), true},
		{KindUploadClockSkew, cov("", "", false), false},
		{KindUploadClockSkew, Report{ChainChecked: true}, false},
		{string(I3GTIDHole), cov(MethodParsed, MethodHashed, true), false},
		{string(I3GTIDHole), Report{ChainChecked: true}, true},
	}
	for i, c := range cases {
		got := len(Disappeared([]Finding{{Kind: c.kind, Subject: s}}, c.rep)) == 1
		if got != c.close {
			t.Errorf("case %d %s coverage=%+v chain=%v: closed=%v want %v", i, c.kind, c.rep.Coverage, c.rep.ChainChecked, got, c.close)
		}
	}
	// Reported again is never closed, whatever the coverage.
	rep := cov(MethodParsed, MethodHashed, true)
	rep.Findings = []Finding{{Kind: KindObjectCorrupt, Subject: s}}
	if len(Disappeared([]Finding{{Kind: KindObjectCorrupt, Subject: s}}, rep)) != 0 {
		t.Fatal("closed a finding the pass reported again")
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
	for InRotation(rows[0].Name, off, DefaultPeriod, DefaultSamplePercent) {
		off = off.Add(24 * time.Hour)
	}
	for InRotation(rows[3].Name, off, DefaultPeriod, DefaultSamplePercent) {
		off = off.Add(24 * time.Hour)
	}
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, OpenFindings: []Finding{{Kind: KindObjectCorrupt, Subject: rows[0].Name}}})
	want := map[string][2]Method{
		rows[0].Name: {MethodAbsent, MethodUnverifiable},
		rows[1].Name: {MethodMissing, MethodNotApplicable},
		rows[2].Name: {MethodAbsent, MethodNotApplicable},
		rows[3].Name: {MethodParsed, MethodHead},
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
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off})
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
	rep := mustVerify(t, f.dir, rows, objectsFromDir2(t, f, rows), chain, Options{Now: testNow, SamplePercent: 100})
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
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: -1})
	got, ok := findingOf(rep.Findings, KindUploadClockSkew, name)
	if !ok || got.Severity != SeverityWarning {
		t.Fatalf("findings %+v", rep.Findings)
	}
	if objs.openCount() != 0 || rep.Coverage[name].Object.Method != MethodHead {
		t.Fatalf("a skewed row was downloaded as recent: opens=%d cov=%+v", objs.openCount(), rep.Coverage[name])
	}
	rows[0].UploadedAt = testNow.Add(-2 * time.Hour)
	fixed := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: -1})
	if !hasFinding(Disappeared(rep.Findings, fixed), KindUploadClockSkew, name) {
		t.Fatalf("clock fixed, not closed: %+v", fixed.Findings)
	}
	// Cancelled while the skewed row's body is being read: the skew was
	// observed (no I/O needed) and stays reported; the object is unreached.
	rows[0].UploadedAt = testNow.Add(10 * 365 * 24 * time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objs.onOpen = func(int) { cancel() }
	cut, err := Verify(ctx, f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: 100})
	objs.onOpen = nil
	if err == nil || !hasFinding(cut.Findings, KindUploadClockSkew, name) || cut.Coverage[name].Object.Method != "" {
		t.Fatalf("cancelled skewed row: err=%v findings=%+v cov=%+v", err, cut.Findings, cut.Coverage[name])
	}
	if len(Disappeared(rep.Findings, cut)) != 0 {
		t.Fatal("cancelled pass closed the skew it re-observed")
	}
	// Within the tolerance it is recent and not skew.
	rows[0].UploadedAt = testNow.Add(30 * time.Minute)
	near := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, SamplePercent: -1})
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
		if got := SampleObject("x", c.at, now, 0, 0, -1); got != c.want {
			t.Errorf("%s: SampleObject=%v want %v", c.name, got, c.want)
		}
	}
	if !RecentUpload(now.Add(-2*time.Hour), now, 3*time.Hour) || RecentUpload(now.Add(-4*time.Hour), now, 3*time.Hour) {
		t.Error("custom window not honoured")
	}
}

// TestRotation_BoundsTheGap is the (a) bound: at 5% every object is hashed
// exactly once every 20 periods, about 5% of objects per period.
func TestRotation_BoundsTheGap(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct{ percent, n int }{{0, 20}, {5, 20}, {10, 10}, {3, 34}, {50, 2}, {100, 1}, {150, 1}, {-1, 0}} {
		if got := RotationLength(c.percent); got != c.n {
			t.Fatalf("RotationLength(%d)=%d want %d", c.percent, got, c.n)
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
			in := InRotation(name, now, DefaultPeriod, DefaultSamplePercent)
			if in != InRotation(name, now.Add(-13*time.Hour), DefaultPeriod, DefaultSamplePercent) ||
				in != InRotation(name, now.Add(10*time.Hour+59*time.Minute), DefaultPeriod, DefaultSamplePercent) {
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
	if InRotation("x", start, DefaultPeriod, -1) || !InRotation("x", start, DefaultPeriod, 100) {
		t.Fatal("negative percent draws nothing; 100 draws everything")
	}
}

// TestVerify_ConcurrentPassesShareOneBudget is item 2: run with -race. Each
// Report.Bytes is that pass's reads; the budget's total is both together.
func TestVerify_ConcurrentPassesShareOneBudget(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	single := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: testNow, SamplePercent: 100})
	budget := NewByteBudget(1 << 30)
	var wg sync.WaitGroup
	got := make([]int64, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rep, err := Verify(context.Background(), f.dir, rows, objectsFromDir2(t, f, rows), nil, Options{Now: testNow, SamplePercent: 100, Budget: budget})
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
	again := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: testNow, SamplePercent: 100, Budget: budget})
	if again.Bytes != single.Bytes {
		t.Fatalf("third pass on a used budget reports %d, want %d", again.Bytes, single.Bytes)
	}
}
