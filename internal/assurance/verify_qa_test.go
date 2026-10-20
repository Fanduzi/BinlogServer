// Package assurance provides module-level functionality for assurance.
// input: pass state fed back the way the worker does (last_verified_at, ReadMark, open findings with severity and detail), catalog rows, a temp directory and an in-memory object store
// output: proof that a backlog sorts as never verified, that LOCAL_UNREADABLE escalates and closes across passes, that collapse and split keep severity and detail, that a future uploaded_at within tolerance is not a change, and that a dangling symlink on a pending segment is LOCAL_MISSING
// pos: QA r5 follow-ups for the phase 3 verifier (docs/design/0009-phase3-verifier.md §5.2, §5.3, §6.2)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// passState is what a worker carries between passes (design §10):
// last_verified_at, Report.Read fed back as Options.LastRead, and the open
// findings. A finding reported again is upserted (severity and detail
// replaced, as task_findings does); a closed one is removed.
type passState struct {
	last map[string]time.Time
	read map[string]ReadMark
	open []Finding
}

func newPassState() *passState {
	return &passState{last: map[string]time.Time{}, read: map[string]ReadMark{}}
}

func (z *passState) opt(o Options) Options {
	o.LastVerified, o.LastRead = map[string]time.Time{}, map[string]ReadMark{}
	for k, v := range z.last {
		o.LastVerified[k] = v
	}
	for k, v := range z.read {
		o.LastRead[k] = v
	}
	o.OpenFindings = append([]Finding(nil), z.open...)
	return o
}

func (z *passState) feed(rep Report) (opened int) {
	for k, v := range rep.VerifiedAt {
		z.last[k] = v
	}
	for k, v := range rep.Read {
		z.read[k] = v
	}
	key := func(f Finding) string { return f.Kind + "\x00" + f.Subject }
	gone := map[string]bool{}
	for _, c := range rep.Closed {
		gone[key(c.Finding)] = true
	}
	idx := map[string]int{}
	var next []Finding
	for _, f := range z.open {
		if !gone[key(f)] {
			idx[key(f)] = len(next)
			next = append(next, f)
		}
	}
	for _, f := range rep.Findings {
		if i, ok := idx[key(f)]; ok {
			next[i] = f
			continue
		}
		idx[key(f)] = len(next)
		next = append(next, f)
		opened++
	}
	z.open = next
	return opened
}

func (z *passState) finding(kind, subject string) (Finding, bool) {
	return findingOf(z.open, kind, subject)
}

func qaBody(n int, fill byte) ([]byte, string) {
	b := bytes.Repeat([]byte{fill}, n)
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:])
}

// TestVerify_StaleBacklogSortsAsNeverVerified is QA r5 P2-1: a backlog
// verified locally before its upload (last_verified_at before uploaded_at)
// sorts as never verified, ahead of verified overdue work. With 120 and 400
// overdue segments at C = 6S (k > q·N, so the overdue queue alone has no
// bound), every backlog object is still hashed within one rotation (N
// periods) and the corrupt one is found within that bound. On r5 the corrupt
// object took 23 days at 120 and 29 of 30 objects were never hashed at 400.
func TestVerify_StaleBacklogSortsAsNeverVerified(t *testing.T) {
	for _, overdue := range []int{120, 400} {
		t.Run(fmt.Sprintf("overdue %d at C=6S", overdue), func(t *testing.T) {
			const S, C = 4096, 6 * 4096
			n := rotationLength(0)
			objs := &memObjects{obj: map[string][]byte{}}
			z := newPassState()
			var rows []VerifyRow
			for i := 0; i < overdue; i++ {
				b, sum := qaBody(S, byte(i%250+1))
				name := fmt.Sprintf("mysql-bin.%06d", 1000+i)
				rows = append(rows, VerifyRow{Name: name, Sealed: true, Size: S, SHA256: sum, UploadState: "UPLOADED", ObjectKey: "t/" + name,
					UploadedAt: testNow.Add(-200 * 24 * time.Hour), SealedAt: testNow.Add(-200 * 24 * time.Hour), RetentionRemoved: true})
				objs.obj["t/"+name] = b
				z.last[name] = testNow.Add(-100 * 24 * time.Hour)
			}
			corrupt := ""
			for i := 0; i < 30; i++ {
				b, sum := qaBody(S, byte(i%250+3))
				name := fmt.Sprintf("mysql-bin.%06d", 5000+i)
				rows = append(rows, VerifyRow{Name: name, Sealed: true, Size: S, SHA256: sum, UploadState: "UPLOADED", ObjectKey: "t/" + name,
					UploadedAt: testNow.Add(-time.Hour), SealedAt: testNow.Add(-25 * time.Hour), RetentionRemoved: true})
				obj := append([]byte(nil), b...)
				if i == 29 {
					obj[10] ^= 1
					corrupt = name
				}
				objs.obj["t/"+name] = obj
				z.last[name] = testNow.Add(-24 * time.Hour) // parsed while pending, before the upload
			}
			first, found := map[string]int{}, -1
			for d := 0; d < n; d++ {
				rep := mustVerify(t, t.TempDir(), rows, objs, nil, z.opt(Options{Now: testNow.Add(time.Duration(d) * DefaultPeriod), PassBytes: C, CatalogComplete: true}))
				if rep.Bytes > C {
					t.Fatalf("day %d paid %d > C", d, rep.Bytes)
				}
				for name, c := range rep.Coverage {
					if _, ok := first[name]; !ok && c.Object.Method == MethodHashed {
						first[name] = d
					}
				}
				if found < 0 && hasFinding(rep.Findings, KindObjectCorrupt, corrupt) {
					found = d
				}
				z.feed(rep)
			}
			for _, r := range rows[overdue:] {
				if _, ok := first[r.Name]; !ok {
					t.Fatalf("%s not hashed within %d periods", r.Name, n)
				}
			}
			if found < 0 {
				t.Fatalf("corrupt %s not found within %d periods", corrupt, n)
			}
		})
	}
}

// TestVerify_RotationOffHashesNeverVerifiedOnce is QA r5 P3: with the
// rotation off, an upload outside the recent window that was never verified
// is hashed once, from the tail, like one verified before its upload; one
// verified after its upload is not read.
func TestVerify_RotationOffHashesNeverVerifiedOnce(t *testing.T) {
	const S = 4096
	objs := &memObjects{obj: map[string][]byte{}}
	z := newPassState()
	var rows []VerifyRow
	for i := 0; i < 6; i++ {
		b, sum := qaBody(S, byte(i+1))
		name := fmt.Sprintf("mysql-bin.%06d", 100+i)
		rows = append(rows, VerifyRow{Name: name, Sealed: true, Size: S, SHA256: sum, UploadState: "UPLOADED", ObjectKey: "t/" + name,
			UploadedAt: testNow.Add(-48 * time.Hour), SealedAt: testNow.Add(-49 * time.Hour), RetentionRemoved: true})
		objs.obj["t/"+name] = b
		switch {
		case i >= 4:
			z.last[name] = testNow.Add(-47 * time.Hour) // after the upload
		case i >= 2:
			z.last[name] = testNow.Add(-50 * time.Hour) // before the upload
		}
	}
	hashed := map[string]int{}
	for d := 0; d < 10; d++ {
		rep := mustVerify(t, t.TempDir(), rows, objs, nil, z.opt(Options{Now: testNow.Add(time.Duration(d) * DefaultPeriod), RotationPeriods: -1}))
		for name, c := range rep.Coverage {
			if c.Object.Method == MethodHashed {
				hashed[name]++
				if c.Object.Reason != reasonTail {
					t.Fatalf("%s: %+v", name, c.Object)
				}
			}
		}
		z.feed(rep)
	}
	for i, r := range rows {
		want := 1
		if i >= 4 {
			want = 0
		}
		if hashed[r.Name] != want {
			t.Fatalf("%s hashed %d times in 10 passes, want %d", r.Name, hashed[r.Name], want)
		}
	}
}

// TestVerify_FutureSealedAtWithinToleranceReadsOnce is QA r5 P3: a sealed_at
// in the future but within ClockSkewTolerance is not a change until it
// passes, so ten-minute passes read the segment once, not on every pass.
func TestVerify_FutureSealedAtWithinToleranceReadsOnce(t *testing.T) {
	const S = 4096
	for _, ahead := range []time.Duration{30 * time.Minute, 3 * time.Hour} {
		objs := &memObjects{obj: map[string][]byte{}}
		b, sum := qaBody(S, 9)
		name := "mysql-bin.000200"
		row := VerifyRow{Name: name, Sealed: true, Size: S, SHA256: sum, UploadState: "UPLOADED", ObjectKey: "t/" + name,
			UploadedAt: testNow.Add(-48 * time.Hour), SealedAt: testNow.Add(ahead), RetentionRemoved: true}
		objs.obj["t/"+name] = b
		z := newPassState()
		z.last[name] = testNow.Add(-47 * time.Hour)
		reads := 0
		for m := 0; m < 6*60; m += 10 {
			at := testNow.Add(time.Duration(m) * time.Minute)
			rep := mustVerify(t, t.TempDir(), []VerifyRow{row}, objs, nil, z.opt(Options{Now: at, RotationPeriods: 1000}))
			if rep.Coverage[name].Object.Method == MethodHashed {
				reads++
				if at.Before(row.SealedAt) {
					t.Fatalf("sealed_at +%s: read at +%dm, before sealed_at", ahead, m)
				}
			}
			z.feed(rep)
		}
		if reads != 1 {
			t.Fatalf("sealed_at +%s: hashed %d times in 6h of 10-minute passes", ahead, reads)
		}
	}
}

// TestVerify_ReadMarkComparesEveryField: a forced segment read this period
// is held only while every ReadMark field matches; a change in any of them,
// a same-size re-upload (uploaded_at, QA r5 P3) included, reads it again.
func TestVerify_ReadMarkComparesEveryField(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
	n := rows[0].Name
	objs := objectsFromDir2(t, f, rows)
	open := []Finding{{Kind: KindObjectCorrupt, Subject: n, Severity: SeverityCritical}}
	start := slotStart(testNow, DefaultPeriod).Add(time.Hour)
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: start, OpenFindings: open})
	mk, ok := rep.Read[n]
	if !ok || !mk.UploadedAt.Equal(rows[0].UploadedAt) {
		t.Fatalf("read mark %+v %v", mk, ok)
	}
	at := start.Add(time.Hour)
	try := func(what string, rows []VerifyRow, m ReadMark, want Method) {
		t.Helper()
		rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: at, OpenFindings: open, LastRead: map[string]ReadMark{n: m}})
		if got := rep.Coverage[n].Object.Method; got != want {
			t.Fatalf("%s: object %s, want %s", what, got, want)
		}
	}
	try("nothing changed", rows, mk, MethodHead)
	m := mk
	m.LocalSize++
	try("local size", rows, m, MethodHashed)
	m = mk
	m.LocalModTime = m.LocalModTime.Add(time.Second)
	try("local mtime", rows, m, MethodHashed)
	m = mk
	m.ObjectSize++
	try("object size", rows, m, MethodHashed)
	re := append([]VerifyRow(nil), rows...)
	re[0].UploadedAt = at.Add(-10 * time.Minute)
	try("same-size re-upload", re, mk, MethodHashed)
}

// TestVerify_ReadMarkNeedsEveryScheduledCopy: no ReadMark unless the local
// copy parsed and the object concluded; one copy failing keeps the segment
// due.
func TestVerify_ReadMarkNeedsEveryScheduledCopy(t *testing.T) {
	setup := func(t *testing.T) (string, []VerifyRow, *memObjects, Options) {
		f := live(t)
		rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
		open := []Finding{{Kind: KindObjectCorrupt, Subject: rows[0].Name, Severity: SeverityCritical}}
		return f.dir, rows, objectsFromDir2(t, f, rows), Options{Now: testNow, OpenFindings: open}
	}
	t.Run("both read", func(t *testing.T) {
		dir, rows, objs, opt := setup(t)
		if rep := mustVerify(t, dir, rows, objs, nil, opt); len(rep.Read) != 1 {
			t.Fatalf("read %+v", rep.Read)
		}
	})
	t.Run("object fails", func(t *testing.T) {
		dir, rows, objs, opt := setup(t)
		objs.openErr = errors.New("connection reset")
		rep := mustVerify(t, dir, rows, objs, nil, opt)
		c := rep.Coverage[rows[0].Name]
		if c.Local.Method != MethodParsed || c.Object.Method != MethodError || len(rep.Read) != 0 {
			t.Fatalf("cov %+v read %+v", c, rep.Read)
		}
	})
	t.Run("local fails", func(t *testing.T) {
		dir, rows, objs, opt := setup(t)
		withOpenLocal(t, func(path string) (io.ReadCloser, error) {
			r, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			return &failOnce{ReadCloser: r, skip: 100}, nil
		})
		rep := mustVerify(t, dir, rows, objs, nil, opt)
		c := rep.Coverage[rows[0].Name]
		if c.Local.Method != MethodError || c.Object.Method != MethodHashed || len(rep.Read) != 0 {
			t.Fatalf("cov %+v read %+v", c, rep.Read)
		}
	})
}

// cutBody serves each object cut bytes short, with a clean io.EOF.
type cutBody struct {
	*memObjects
	cut int
}

func (s cutBody) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.memObjects.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(&eofAfter{b: b[:len(b)-s.cut]}), nil
}

// TestVerify_ObjectOneByteShortIsError: a body one byte shorter than the
// HEAD size is a cut-short read too.
func TestVerify_ObjectOneByteShortIsError(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
	n := rows[0].Name
	objs := cutBody{objectsFromDir2(t, f, rows), 1}
	open := []Finding{{Kind: KindObjectCorrupt, Subject: n, Severity: SeverityCritical}}
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, OpenFindings: open})
	if c := rep.Coverage[n].Object; c.Method != MethodError || !errors.Is(rep.ObjectErr, io.ErrUnexpectedEOF) || len(rep.Closed) != 0 {
		t.Fatalf("object %+v err %v closed %+v", c, rep.ObjectErr, rep.Closed)
	}
}

// TestVerify_LocalUnreadableClosesWhenSoleCopyGone: a not-uploaded segment's
// file that disappears is LOCAL_MISSING, and LOCAL_UNREADABLE closes.
func TestVerify_LocalUnreadableClosesWhenSoleCopyGone(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
	rows[0].UploadState, rows[0].ObjectKey = "PENDING", ""
	n := rows[0].Name
	if err := os.Remove(filepath.Join(f.dir, n)); err != nil {
		t.Fatal(err)
	}
	open := []Finding{{Kind: KindLocalUnreadable, Subject: n, Severity: SeverityCritical, Detail: DetailUnreadable}}
	rep := mustVerify(t, f.dir, rows, nil, nil, Options{Now: testNow, OpenFindings: open})
	if !hasFinding(rep.Findings, KindLocalMissing, n) || len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonLocalGone {
		t.Fatalf("findings %+v closed %+v", rep.Findings, rep.Closed)
	}
}

// TestVerify_LocalUnreadableClosesOnSizeMismatch is QA r5 P3: once the file
// stats again with a size that differs from the row, VERIFY_FAILED takes
// over and LOCAL_UNREADABLE closes.
func TestVerify_LocalUnreadableClosesOnSizeMismatch(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
	n := rows[0].Name
	if err := os.Truncate(filepath.Join(f.dir, n), rows[0].Size-1); err != nil {
		t.Fatal(err)
	}
	open := []Finding{{Kind: KindLocalUnreadable, Subject: n, Severity: SeverityWarning, Detail: DetailUnreadable}}
	rep := mustVerify(t, f.dir, rows, objectsFromDir2(t, f, rows), nil, Options{Now: testNow, OpenFindings: open})
	if !hasFinding(rep.Findings, KindVerifyFailed, n) || len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonLocalSizeDiffers {
		t.Fatalf("findings %+v closed %+v", rep.Findings, rep.Closed)
	}
}

// TestVerify_LocalUnreadableHysteresis is QA r5 P2-2: LOCAL_UNREADABLE is
// WARNING; a not-uploaded segment's escalates to CRITICAL on the second
// consecutive failure and stays CRITICAL until it closes; it closes after
// two clean reads in a row, so an error every other pass neither escalates
// nor opens and closes it each time.
func TestVerify_LocalUnreadableHysteresis(t *testing.T) {
	const W, C, OK = SeverityWarning, SeverityCritical, ""
	cases := []struct {
		name, state string
		fail        []bool
		want        []string // severity of the open finding after each pass; "" is closed
	}{
		{"pending: two failures escalate, two clean reads close", "PENDING",
			[]bool{true, true, false, true, false, false}, []string{W, C, C, C, C, OK}},
		{"pending: every other pass", "PENDING",
			[]bool{true, false, true, false, true, false, false}, []string{W, W, W, W, W, W, OK}},
		{"uploaded: never CRITICAL", "UPLOADED",
			[]bool{true, true, true, false, false}, []string{W, W, W, W, OK}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := live(t)
			rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))[1:2]
			n := rows[0].Name
			if tc.state != "UPLOADED" {
				rows[0].UploadState, rows[0].ObjectKey = tc.state, ""
			}
			objs := objectsFromDir2(t, f, rows)
			fail := false
			withOpenLocal(t, func(path string) (io.ReadCloser, error) {
				r, err := os.Open(path)
				if err != nil || !fail {
					return r, err
				}
				return &failOnce{ReadCloser: r, skip: 100}, nil
			})
			z := newPassState()
			opened := 0
			for i, fl := range tc.fail {
				fail = fl
				rep := mustVerify(t, f.dir, rows, objs, nil, z.opt(Options{Now: testNow.Add(time.Duration(i) * time.Hour)}))
				if !fl && rep.Coverage[n].Local.Method != MethodParsed {
					t.Fatalf("pass %d: not read: %+v", i, rep.Coverage[n].Local)
				}
				opened += z.feed(rep)
				got, ok := z.finding(KindLocalUnreadable, n)
				if ok != (tc.want[i] != OK) || ok && got.Severity != tc.want[i] {
					t.Fatalf("pass %d: open %v %+v, want %q", i, ok, got, tc.want[i])
				}
				if ok && got.Detail == DetailUnreadableRecovering && fl {
					t.Fatalf("pass %d: recovering after a failure", i)
				}
			}
			if opened != 1 {
				t.Fatalf("opened %d times", opened)
			}
		})
	}
}

// TestVerify_LocalUnreadableTaskLevel is QA r5 P2-2: when every stat in the
// task fails with the same error the pass reports one task-level
// LOCAL_UNREADABLE (empty Subject), which covers the per-file ones and is
// not closed as a removed row. An open per-file failure that is not
// recovering makes that task-level finding start CRITICAL; it closes after
// two passes in which the files stat again.
func TestVerify_LocalUnreadableTaskLevel(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
	if len(rows) < 2 {
		t.Fatal("fixture has one row")
	}
	for i := range rows {
		rows[i].UploadState, rows[i].ObjectKey = "PENDING", ""
	}
	if err := os.Chmod(f.dir, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.dir, 0o755) })
	z := newPassState()
	z.open = []Finding{{Kind: KindLocalUnreadable, Subject: rows[0].Name, Severity: SeverityWarning, Detail: DetailUnreadable}}
	pass := func(i int) Report {
		rep := mustVerify(t, f.dir, rows, nil, nil, z.opt(Options{Now: testNow.Add(time.Duration(i) * time.Hour), CatalogComplete: true}))
		z.feed(rep)
		return rep
	}
	rep := pass(0)
	var un []Finding
	for _, fd := range rep.Findings {
		if fd.Kind == KindLocalUnreadable {
			un = append(un, fd)
		}
	}
	// The planted per-file finding is already a failure (not recovering), so
	// the task-level one starts CRITICAL instead of dropping to WARNING.
	if len(un) != 1 || un[0].Subject != "" || un[0].Severity != SeverityCritical || len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonUnreadableTask {
		t.Fatalf("findings %+v closed %+v", rep.Findings, rep.Closed)
	}
	pass(1)
	if fd, ok := z.finding(KindLocalUnreadable, ""); !ok || fd.Severity != SeverityCritical {
		t.Fatalf("second failure: %+v %v", fd, ok)
	}
	if err := os.Chmod(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if rep := pass(2); len(rep.Closed) != 0 {
		t.Fatalf("closed on the first clean pass: %+v", rep.Closed)
	}
	if fd, ok := z.finding(KindLocalUnreadable, ""); !ok || fd.Detail != DetailUnreadableRecovering {
		t.Fatalf("first clean pass: %+v %v", fd, ok)
	}
	if rep := pass(3); len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonLocalStat {
		t.Fatalf("second clean pass: closed %+v", rep.Closed)
	}
}

// TestTierOf_NotVerifiedSinceItsLastChange: where a row that is not
// content-verified since its last change lands. A not-uploaded row is
// recent whatever its age (Y10); an upload is recent only inside the window;
// past it, the rotation decides between the overdue tier (empty order) and
// the tail. A row verified since its last change is in none.
func TestTierOf_NotVerifiedSinceItsLastChange(t *testing.T) {
	old := testNow.Add(-48 * time.Hour)
	pending := VerifyRow{Name: "mysql-bin.000001", Sealed: true, UploadState: "PENDING", SealedAt: old}
	upOld := VerifyRow{Name: "mysql-bin.000001", Sealed: true, UploadState: "UPLOADED", ObjectKey: "k", SealedAt: old, UploadedAt: old}
	upNew := upOld
	upNew.UploadedAt = testNow.Add(-time.Hour)
	for _, tc := range []struct {
		name     string
		row      VerifyRow
		last     time.Time
		rotation int
		want     int
	}{
		{"pending, never verified, rotation off", pending, time.Time{}, -1, tierRecent},
		{"pending, sealed after the last verify", pending, old.Add(-time.Hour), -1, tierRecent},
		{"pending, verified since", pending, old.Add(time.Hour), -1, tierNone},
		{"recent upload, verified before it", upNew, old.Add(time.Hour), -1, tierRecent},
		{"old upload, never verified, rotation off", upOld, time.Time{}, -1, tierTail},
		{"old upload, verified before it, rotation off", upOld, old.Add(-time.Hour), -1, tierTail},
		{"old upload, verified after it, rotation off", upOld, old.Add(time.Hour), -1, tierNone},
		{"old upload, verified before it, rotation on", upOld, old.Add(-time.Hour), 1000, tierOverdue},
	} {
		p := &pass{now: testNow, opt: Options{Now: testNow, RotationPeriods: tc.rotation}}
		s := &seg{row: tc.row, last: tc.last}
		if got := p.tierOf(s); got != tc.want {
			t.Fatalf("%s: tier %d, want %d", tc.name, got, tc.want)
		}
		if p.stale(s) != s.order.IsZero() {
			t.Fatalf("%s: stale %v, order %v", tc.name, p.stale(s), s.order)
		}
	}
}

func withStatPath(t *testing.T, stat func(string) (os.FileInfo, error)) {
	t.Helper()
	prev := statPath
	statPath = stat
	t.Cleanup(func() { statPath = prev })
}

func statPathErr(path string, err error) error {
	return &os.PathError{Op: "stat", Path: path, Err: err}
}

// pendingStatRows is four not-uploaded sealed names whose files are one byte.
// A successful stat with Size 2 is a size mismatch, so it does not parse.
func pendingStatRows(t *testing.T) (string, []VerifyRow) {
	t.Helper()
	dir := t.TempDir()
	var rows []VerifyRow
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("mysql-bin.%06d", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, VerifyRow{Name: name, Sealed: true, Size: 2, UploadState: "PENDING", SealedAt: testNow.Add(-48 * time.Hour)})
	}
	return dir, rows
}

func statByName(fail map[string]error) func(string) (os.FileInfo, error) {
	return func(path string) (os.FileInfo, error) {
		if err, ok := fail[filepath.Base(path)]; ok {
			return nil, statPathErr(path, err)
		}
		return os.Stat(path)
	}
}

// TestVerify_LocalUnreadableCollapseKeepsSeverity is QA r5 P3-1: switching
// between per-file and task-level LOCAL_UNREADABLE keeps the severity (and,
// on a split, the detail). A sole-copy CRITICAL must not come back as
// WARNING for one pass, and a split must not leave the task-level finding
// open beside new WARNINGs.
func TestVerify_LocalUnreadableCollapseKeepsSeverity(t *testing.T) {
	dir, rows := pendingStatRows(t)
	names := []string{rows[0].Name, rows[1].Name, rows[2].Name, rows[3].Name}
	eloop := map[string]error{}
	for _, n := range names {
		eloop[n] = syscall.ELOOP
	}
	three := map[string]error{}
	for _, n := range names[:3] {
		three[n] = syscall.ELOOP
	}
	z := newPassState()
	pass := func(i int, fail map[string]error) Report {
		withStatPath(t, statByName(fail))
		rep := mustVerify(t, dir, rows, nil, nil, z.opt(Options{Now: testNow.Add(time.Duration(i) * time.Hour), CatalogComplete: true}))
		z.feed(rep)
		return rep
	}
	openUnreadable := func() []Finding {
		var out []Finding
		for _, f := range z.open {
			if f.Kind == KindLocalUnreadable {
				out = append(out, f)
			}
		}
		return out
	}
	rep := pass(0, eloop)
	if got := openUnreadable(); len(got) != 1 || got[0].Subject != "" || got[0].Severity != SeverityWarning {
		t.Fatalf("first collapse: %+v findings %+v", got, rep.Findings)
	}
	pass(1, eloop)
	if fd, ok := z.finding(KindLocalUnreadable, ""); !ok || fd.Severity != SeverityCritical {
		t.Fatalf("second collapse: %+v %v", fd, ok)
	}
	rep = pass(2, three)
	if len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonSplit || rep.Closed[0].Finding.Subject != "" {
		t.Fatalf("split close: %+v", rep.Closed)
	}
	got := openUnreadable()
	if len(got) != 3 {
		t.Fatalf("split open: %+v", got)
	}
	for _, f := range got {
		if f.Subject == "" || f.Severity != SeverityCritical || f.Detail != DetailUnreadable {
			t.Fatalf("split finding: %+v", f)
		}
	}
	rep = pass(3, eloop)
	if fd, ok := z.finding(KindLocalUnreadable, ""); !ok || fd.Severity != SeverityCritical {
		t.Fatalf("collapse again: %+v %v", fd, ok)
	}
	if len(openUnreadable()) != 1 {
		t.Fatalf("per-file still open: %+v", openUnreadable())
	}
	for _, c := range rep.Closed {
		if c.Finding.Kind == KindLocalUnreadable && c.Finding.Subject != "" && c.Reason != ReasonUnreadableTask {
			t.Fatalf("per-file close: %+v", c)
		}
	}
	rep = pass(4, three)
	got = openUnreadable()
	if len(got) != 3 {
		t.Fatalf("second split: %+v", got)
	}
	for _, f := range got {
		if f.Severity != SeverityCritical || f.Detail != DetailUnreadable {
			t.Fatalf("second split finding: %+v", f)
		}
	}
	_ = rep
}

// TestVerify_PartialStatFailureDoesNotCollapse kills H06: three of four
// stats failing the same way stay per-file. Collapsing when one file still
// stats would hide which names failed.
func TestVerify_PartialStatFailureDoesNotCollapse(t *testing.T) {
	dir, rows := pendingStatRows(t)
	fail := map[string]error{}
	for _, row := range rows[:3] {
		fail[row.Name] = syscall.ELOOP
	}
	withStatPath(t, statByName(fail))
	rep := mustVerify(t, dir, rows, nil, nil, Options{Now: testNow})
	if hasFinding(rep.Findings, KindLocalUnreadable, "") {
		t.Fatalf("collapsed a partial failure: %+v", rep.Findings)
	}
	for _, row := range rows[:3] {
		if !hasFinding(rep.Findings, KindLocalUnreadable, row.Name) {
			t.Fatalf("missing %s: %+v", row.Name, rep.Findings)
		}
	}
	if hasFinding(rep.Findings, KindLocalUnreadable, rows[3].Name) {
		t.Fatalf("healthy name reported: %+v", rep.Findings)
	}
}

// TestVerify_MixedStatErrorsDoNotCollapse kills H07: two errno values are
// not one task-level finding.
func TestVerify_MixedStatErrorsDoNotCollapse(t *testing.T) {
	dir, rows := pendingStatRows(t)
	fail := map[string]error{
		rows[0].Name: syscall.ELOOP, rows[1].Name: syscall.ELOOP,
		rows[2].Name: syscall.EACCES, rows[3].Name: syscall.EACCES,
	}
	withStatPath(t, statByName(fail))
	rep := mustVerify(t, dir, rows, nil, nil, Options{Now: testNow})
	if hasFinding(rep.Findings, KindLocalUnreadable, "") {
		t.Fatalf("collapsed mixed errno: %+v", rep.Findings)
	}
	for _, row := range rows {
		if !hasFinding(rep.Findings, KindLocalUnreadable, row.Name) {
			t.Fatalf("missing %s: %+v", row.Name, rep.Findings)
		}
	}
}

// TestVerify_DanglingSymlinkPendingIsMissingLocal kills H08: a directory
// entry that stat cannot follow (ENOENT) is absent. A pending segment with
// no other copy is LOCAL_MISSING, not a stat error and not LOCAL_UNREADABLE.
func TestVerify_DanglingSymlinkPendingIsMissingLocal(t *testing.T) {
	dir := t.TempDir()
	name := "mysql-bin.000001"
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	row := VerifyRow{Name: name, Sealed: true, UploadState: "PENDING", SealedAt: testNow.Add(-48 * time.Hour)}
	rep := mustVerify(t, dir, []VerifyRow{row}, nil, nil, Options{Now: testNow, CatalogComplete: true})
	if rep.Coverage[name].Local.Method != MethodMissing {
		t.Fatalf("local %+v", rep.Coverage[name].Local)
	}
	if !hasFinding(rep.Findings, KindLocalMissing, name) || hasFinding(rep.Findings, KindLocalUnreadable, name) || hasFinding(rep.Findings, KindLocalUnreadable, "") {
		t.Fatalf("findings %+v", rep.Findings)
	}
}

// TestVerify_FutureUploadedAtWithinToleranceIsNotAChange is QA r5 P3 and
// kills H23: uploaded_at up to an hour in the future is not a change and
// not UPLOAD_CLOCK_SKEW, so a corrupt object stays unread until that time
// passes. Past the hour it is clock skew immediately and is hashed once
// the timestamp is no longer in the future. A row whose uploaded_at is
// already past is hashed on the first pass.
func TestVerify_FutureUploadedAtWithinToleranceIsNotAChange(t *testing.T) {
	body, sum := qaBody(32, 7)
	bad := append([]byte(nil), body...)
	bad[0] ^= 1
	objs := &memObjects{obj: map[string][]byte{}}
	sealed := testNow.Add(-48 * time.Hour)
	row := func(name string, uploaded time.Time) VerifyRow {
		key := "t/" + name
		objs.obj[key] = append([]byte(nil), bad...)
		return VerifyRow{Name: name, Sealed: true, Size: 32, SHA256: sum, UploadState: "UPLOADED", ObjectKey: key,
			UploadedAt: uploaded, SealedAt: sealed, RetentionRemoved: true}
	}
	control := row("mysql-bin.000010", testNow.Add(-time.Hour))
	for _, tc := range []struct {
		name     string
		uploaded time.Time
		hashAt   time.Duration
		skew     bool
	}{
		{"plus 30m", testNow.Add(30 * time.Minute), 30 * time.Minute, false},
		{"plus 59m", testNow.Add(59 * time.Minute), time.Hour, false},
		{"plus 61m", testNow.Add(61 * time.Minute), 70 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			future := row("mysql-bin.000011", tc.uploaded)
			rows := []VerifyRow{control, future}
			last := testNow.Add(-time.Hour) // after slot start, after sealed
			z := newPassState()
			z.last[future.Name] = last
			var hashedAt time.Duration = -1
			for _, step := range []time.Duration{0, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 40 * time.Minute, 50 * time.Minute, time.Hour, 70 * time.Minute} {
				rep := mustVerify(t, t.TempDir(), rows, objs, nil, z.opt(Options{Now: testNow.Add(step), RotationPeriods: 1}))
				z.feed(rep)
				if step == 0 && rep.Coverage[control.Name].Object.Method != MethodHashed {
					t.Fatalf("control at 0: %s", rep.Coverage[control.Name].Object.Method)
				}
				got := rep.Coverage[future.Name].Object.Method
				skewed := hasFinding(rep.Findings, KindUploadClockSkew, future.Name)
				if step == 0 && tc.skew != skewed {
					t.Fatalf("skew at 0: %v, want %v; findings %+v", skewed, tc.skew, rep.Findings)
				}
				if step == 0 && !tc.skew && skewed {
					t.Fatal("clock skew inside the tolerance")
				}
				if got == MethodHashed {
					if hashedAt < 0 {
						hashedAt = step
					}
					if !hasFinding(rep.Findings, KindObjectCorrupt, future.Name) {
						t.Fatalf("hashed at %s without OBJECT_CORRUPT: %+v", step, rep.Findings)
					}
				}
			}
			if hashedAt != tc.hashAt {
				t.Fatalf("hashed at %s, want %s", hashedAt, tc.hashAt)
			}
		})
	}
}

// TestVerify_NamedStaleRowUsesOrderNotLastVerified kills H19: a named row
// that is stale (order empty) still takes its slot. Using last_verified_at,
// which is inside the slot, would skip it.
func TestVerify_NamedStaleRowUsesOrderNotLastVerified(t *testing.T) {
	body, sum := qaBody(32, 9)
	bad := append([]byte(nil), body...)
	bad[3] ^= 1
	name := "mysql-bin.000020"
	key := "t/" + name
	objs := &memObjects{obj: map[string][]byte{key: bad}}
	slot := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	row := VerifyRow{Name: name, Sealed: true, Size: 32, SHA256: sum, UploadState: "UPLOADED", ObjectKey: key,
		UploadedAt: slot.Add(2 * time.Hour), SealedAt: slot.Add(-24 * time.Hour), RetentionRemoved: true}
	opt := Options{Now: testNow, RotationPeriods: 1, CatalogComplete: true,
		LastVerified: map[string]time.Time{name: slot.Add(time.Hour)},
		OpenFindings: []Finding{{Kind: KindObjectMissing, Subject: name, Severity: SeverityCritical, Detail: "uploaded object is missing"}}}
	rep := mustVerify(t, t.TempDir(), []VerifyRow{row}, objs, nil, opt)
	if rep.Coverage[name].Object.Method != MethodHashed || !hasFinding(rep.Findings, KindObjectCorrupt, name) {
		t.Fatalf("coverage %+v findings %+v", rep.Coverage[name].Object, rep.Findings)
	}
}
