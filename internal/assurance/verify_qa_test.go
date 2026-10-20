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
// not closed as a removed row; it escalates like a file's and closes after
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
	if len(un) != 1 || un[0].Subject != "" || un[0].Severity != SeverityWarning || len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonUnreadableTask {
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
