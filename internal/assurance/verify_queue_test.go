// Package assurance provides module-level functionality for assurance.
// input: the live ledger fixture, copies of it under new names, synthetic object-only segments, in-memory object stores, fixed deadlines and per-pass byte caps
// output: proof that every object is HEAD'd before any content is paid for; that content is paid in queue order (forced, recent, slot, oldest last_verified_at); that a deadline or an exhausted budget ends the pass normally with the rest SKIPPED and only cancel aborts; that repeated short passes reach every item within the documented bound; that Report.Bytes is what was paid; that positions come from the parse stream; and the closing rules for a removed local copy, a removed row and an unknown row size
// pos: scheduling, budget and closing tests for the phase 3 verifier (docs/design/0009-phase3-verifier.md §5.2, §6, §7)
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

// queueBound is the design §5.3 bound P: the most passes an overdue segment
// waits, given k segments in the queue, q segments completed per pass, and a
// rotation of n periods. It is the smallest t with
// q*t >= (k-1)*(1+ceil(t/n)) + 1, or -1 when q*n <= k-1 (no bound).
func queueBound(k, q, n int) int {
	if q <= 0 || q*n <= k-1 {
		return -1
	}
	for t := 1; ; t++ {
		if q*t >= (k-1)*(1+(t+n-1)/n)+1 {
			return t
		}
	}
}

func TestQueueBound(t *testing.T) {
	for _, c := range []struct{ k, q, n, want int }{
		{4, 1, 20, 7}, {3, 1, 20, 5}, {1, 1, 20, 1}, {24, 3, 20, 16}, {24, 2, 20, 35}, {13, 2, 20, 13}, {21, 1, 20, -1}, {10, 0, 20, -1},
	} {
		if got := queueBound(c.k, c.q, c.n); got != c.want {
			t.Errorf("queueBound(%d,%d,%d)=%d want %d", c.k, c.q, c.n, got, c.want)
		}
	}
}

// TestVerify_StarvationUnderAFixedDeadline is QA r2 F1 with QA's numbers:
// 64 KiB/s, a 450ms deadline, 25 daily passes, one open OBJECT_CORRUPT. The
// caller feeds VerifiedAt back as LastVerified and the findings back as
// OpenFindings, as the worker will.
//
// Bound (design §5.3): C = burst + rate x (deadline - 150ms slack) = 85196
// bytes; the forced segment costs F = 2 x 15764; a segment's content is S =
// 2 x 15764; the recent share is Rc = C x 25% = 21299; q = floor((C-F-Rc)/S)
// = 1 segment per pass; with k = 3 other
// segments and a rotation of 20, P = queueBound(3, 1, 20) = 5. So every
// copy of every other segment is content-checked within the first 5 passes,
// and the forced segment's two copies on every pass.
func TestVerify_StarvationUnderAFixedDeadline(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
	objs := objectsFromDir(t, f)
	first := rows[0].Name
	objs.obj["t/"+first][10] ^= 0xff
	open := []Finding{{Kind: KindObjectCorrupt, Subject: first}}

	const rate, deadline, slack = 64 << 10, 450 * time.Millisecond, 150 * time.Millisecond
	seg := 2 * fileSize(t, filepath.Join(f.dir, first))
	c := int64(rate) + int64(float64(rate)*(deadline-slack).Seconds())
	q := queueQ(c, seg, seg, DefaultRecentSharePercent)
	bound := queueBound(len(rows)-1, q, DefaultRotationPeriods)
	if bound != 5 {
		t.Fatalf("derived bound %d (C=%d S=%d q=%d)", bound, c, seg, q)
	}

	last := map[string]time.Time{}
	firstLocal, firstObject, firstVerified := map[string]int{}, map[string]int{}, map[string]int{}
	for pass := 0; pass < 25; pass++ {
		now := testNow.Add(time.Duration(pass) * 24 * time.Hour)
		lv := map[string]time.Time{}
		for k, v := range last {
			lv[k] = v
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		rep, err := Verify(ctx, f.dir, rows, objs, chainOf(f), Options{Now: now, Budget: NewByteBudget(rate), OpenFindings: open, LastVerified: lv})
		cancel()
		if err != nil {
			t.Fatalf("pass %d: a deadline must end the pass normally: %v", pass, err)
		}
		for _, row := range rows {
			cov := rep.Coverage[row.Name]
			if !cov.Object.Found {
				t.Fatalf("pass %d: %s not HEAD'd: %+v", pass, row.Name, cov)
			}
			if _, ok := firstLocal[row.Name]; !ok && cov.Local.Method == MethodParsed {
				firstLocal[row.Name] = pass
			}
			if _, ok := firstObject[row.Name]; !ok && cov.Object.Method == MethodHashed {
				firstObject[row.Name] = pass
			}
		}
		if c := rep.Coverage[first]; c.Local.Method != MethodParsed || c.Object.Method != MethodHashed || !hasFinding(rep.Findings, KindObjectCorrupt, first) {
			t.Fatalf("pass %d: forced segment %+v findings %+v", pass, c, rep.Findings)
		}
		for name, at := range rep.VerifiedAt {
			last[name] = at
			if _, ok := firstVerified[name]; !ok {
				firstVerified[name] = pass
			}
		}
		open = rep.Findings
		if pass < 6 {
			t.Logf("pass %d bytes=%d deferred=%d verified=%v", pass, rep.Bytes, rep.Deferred, rep.VerifiedAt)
		}
	}
	for _, row := range rows[1:] {
		for what, seen := range map[string]map[string]int{"local parse": firstLocal, "object hash": firstObject, "last_verified_at": firstVerified} {
			p, ok := seen[row.Name]
			if !ok || p >= bound {
				t.Fatalf("%s: first %s at pass %d (seen=%v), bound %d passes", row.Name, what, p, ok, bound)
			}
		}
	}
	if _, ok := firstVerified[first]; ok {
		t.Fatal("the corrupt segment advanced last_verified_at")
	}
}

// simFixture is n copies of the live segments with local files and objects,
// plus object-only segments (local removed by retention) of the given sizes.
func simFixture(t *testing.T, copies int, objectSizes []int) (string, []VerifyRow, *memObjects) {
	t.Helper()
	f := live(t)
	base := verifyRows(t, f, testNow.Add(-400*24*time.Hour))
	dir := t.TempDir()
	objs := &memObjects{obj: map[string][]byte{}}
	var rows []VerifyRow
	for i := 0; i < copies; i++ {
		src := base[i%len(base)]
		body, err := os.ReadFile(filepath.Join(f.dir, src.Name))
		if err != nil {
			t.Fatal(err)
		}
		row := src
		row.Name = fmt.Sprintf("sim-local.%06d", i)
		row.ObjectKey = "t/" + row.Name
		if err := os.WriteFile(filepath.Join(dir, row.Name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		objs.obj[row.ObjectKey] = body
		rows = append(rows, row)
	}
	rng := rand.New(rand.NewSource(7))
	for i, size := range objectSizes {
		body := make([]byte, size)
		rng.Read(body)
		sum := sha256.Sum256(body)
		row := VerifyRow{Name: fmt.Sprintf("sim-object.%06d", i), Sealed: true, Size: int64(size), SHA256: hex.EncodeToString(sum[:]),
			UploadState: "UPLOADED", UploadedAt: testNow.Add(-400 * 24 * time.Hour), RetentionRemoved: true}
		row.ObjectKey = "t/" + row.Name
		objs.obj[row.ObjectKey] = body
		rows = append(rows, row)
	}
	return dir, rows, objs
}

// TestVerify_QueueBoundsTheGap runs 120 daily passes capped at PassBytes and
// checks the design §5.3 bound for every segment: first content check within
// P passes, then a gap of at most RotationPeriods + P passes. An item larger
// than the whole pass is deferred every pass and does not hold up the rest.
// With no cap the gap is at most RotationPeriods (rotation x period).
func TestVerify_QueueBoundsTheGap(t *testing.T) {
	const n = 20
	rng := rand.New(rand.NewSource(3))
	var sizes []int
	for i := 0; i < 20; i++ {
		sizes = append(sizes, 2048+rng.Intn(15764-2048))
	}
	const oversized = 300 << 10
	sizes = append(sizes, oversized)
	dir, rows, objs := simFixture(t, 4, sizes)
	big := rows[len(rows)-1].Name

	for _, c := range []struct {
		name      string
		passBytes int64
	}{{"capped", 3 * 2 * 15764}, {"unlimited", 0}} {
		t.Run(c.name, func(t *testing.T) {
			k, s := len(rows)-1, int64(2*15764)
			bound := n
			if c.passBytes > 0 {
				// q = floor((C - C x 25%) / S) = 2 with the recent share counted.
				bound = queueBound(k, queueQ(c.passBytes, 0, s, DefaultRecentSharePercent), n)
				if bound != 35 {
					t.Fatalf("derived bound %d", bound)
				}
			}
			last := map[string]time.Time{}
			hits := map[string][]int{}
			for pass := 0; pass < 120; pass++ {
				now := testNow.Add(time.Duration(pass) * 24 * time.Hour)
				lv := map[string]time.Time{}
				for k, v := range last {
					lv[k] = v
				}
				rep := mustVerify(t, dir, rows, objs, nil, Options{Now: now, RotationPeriods: n, PassBytes: c.passBytes, LastVerified: lv})
				if c.passBytes > 0 && rep.Bytes > c.passBytes {
					t.Fatalf("pass %d read %d over the cap %d", pass, rep.Bytes, c.passBytes)
				}
				if len(rep.Findings) != 0 {
					t.Fatalf("findings %+v", rep.Findings)
				}
				for name, at := range rep.VerifiedAt {
					last[name] = at
					hits[name] = append(hits[name], pass)
				}
				if c.passBytes > 0 {
					if cov := rep.Coverage[big]; cov.Object.Method != MethodSkipped || !strings.Contains(cov.Object.Reason, "does not fit") {
						t.Fatalf("pass %d oversized coverage %+v", pass, cov)
					}
				}
			}
			worst := 0
			for _, row := range rows {
				if c.passBytes > 0 && row.Name == big {
					if len(hits[big]) != 0 {
						t.Fatal("oversized item verified under the cap")
					}
					continue
				}
				h := hits[row.Name]
				if len(h) == 0 || h[0] >= bound {
					t.Fatalf("%s first verified at %v, bound %d", row.Name, h, bound)
				}
				prev := h[0]
				for _, p := range h[1:] {
					if p-prev > worst {
						worst = p - prev
					}
					prev = p
				}
				if 119-prev > n+bound {
					t.Fatalf("%s last verified at pass %d", row.Name, prev)
				}
			}
			limit := n + bound
			if c.passBytes == 0 {
				limit = n
			}
			if worst > limit {
				t.Fatalf("worst gap %d passes, bound %d", worst, limit)
			}
			t.Logf("%s: worst gap %d passes, bound %d", c.name, worst, limit)
		})
	}
}

// slotDayFor finds a day on which want is in its slot and none of avoid is.
func slotDayFor(t *testing.T, want string, avoid ...string) time.Time {
	t.Helper()
	for d := 0; d < 400; d++ {
		day := testNow.Add(time.Duration(d) * 24 * time.Hour)
		if !InRotation(want, day, DefaultPeriod, DefaultRotationPeriods) {
			continue
		}
		ok := true
		for _, a := range avoid {
			if InRotation(a, day, DefaultPeriod, DefaultRotationPeriods) {
				ok = false
			}
		}
		if ok {
			return day
		}
	}
	t.Fatalf("no slot day for %s", want)
	return time.Time{}
}

// TestVerify_QueueOrder pays forced, then recent, then slot, then overdue,
// oldest last_verified_at first, one segment (two copies) per 31528 bytes.
func TestVerify_QueueOrder(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-300*24*time.Hour))
	// 000003 and 000004 share slot 18, so the slot role goes to 000005.
	overdue, recent, slot, forced := rows[0].Name, rows[1].Name, rows[2].Name, rows[3].Name
	day := slotDayFor(t, slot, overdue)
	rows[1].UploadedAt = day.Add(-time.Hour)
	lv := verifiedAt(rows, day.Add(-DefaultPeriod))
	delete(lv, overdue)
	open := []Finding{{Kind: KindObjectCorrupt, Subject: forced}}
	order := []string{forced, recent, slot, overdue}
	seg := 2 * fileSize(t, filepath.Join(f.dir, forced))
	// R is at most C - F - S (QA r4 G2): with room for two segments the
	// forced one takes one and the recent one may not take the other, so it
	// drops to the tail and the slot is paid. From three segments on the
	// tiers are paid in order.
	paid := [][]string{nil, {forced}, {forced, slot}, {forced, recent, slot}, order}
	for i := 0; i <= len(order); i++ {
		want := map[string]bool{}
		for _, name := range paid[i] {
			want[name] = true
		}
		// The recent share is lifted here; TestVerify_RecentShareCapsTheRecentTier covers it.
		rep := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: day, OpenFindings: open, LastVerified: lv, PassBytes: int64(i) * seg, RecentSharePercent: 100})
		if i == 0 {
			rep = mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: day, OpenFindings: open, LastVerified: lv, PassBytes: 1, RecentSharePercent: 100})
		}
		for j, name := range order {
			cov := rep.Coverage[name]
			done := cov.Local.Method == MethodParsed && cov.Object.Method == MethodHashed
			if done != want[name] {
				t.Fatalf("cap %d segments: %s (rank %d) coverage %+v", i, name, j, cov)
			}
			if !want[name] && (cov.Local.Method != MethodSkipped || cov.Object.Method != MethodSkipped) {
				t.Fatalf("cap %d: deferred %s not SKIPPED: %+v", i, name, cov)
			}
		}
		if rep.Deferred != 2*(len(order)-i) {
			t.Fatalf("cap %d: deferred %d", i, rep.Deferred)
		}
	}
	// Two overdue, nothing else due: the older last_verified_at goes first.
	rows[1].UploadedAt = rows[0].UploadedAt
	var other time.Time
	for d := 0; d < 400; d++ {
		at := testNow.Add(time.Duration(d) * 24 * time.Hour)
		free := true
		for _, row := range rows {
			if InRotation(row.Name, at, DefaultPeriod, DefaultRotationPeriods) {
				free = false
			}
		}
		if free {
			other = at
			break
		}
	}
	// The older one has the later name, so name order alone would pick the other.
	lv = verifiedAt(rows, other.Add(-DefaultPeriod))
	lv[overdue] = other.Add(-100 * 24 * time.Hour)
	lv[slot] = other.Add(-200 * 24 * time.Hour)
	rep := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: other, LastVerified: lv, PassBytes: seg})
	if _, ok := rep.VerifiedAt[slot]; !ok || len(rep.VerifiedAt) != 1 {
		t.Fatalf("older overdue segment not paid first: %v", rep.VerifiedAt)
	}
	// An overdue segment an open finding names cannot advance, so it is not
	// paid for from the overdue queue (it would hold the head forever).
	rep = mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: other, LastVerified: lv, PassBytes: seg,
		OpenFindings: []Finding{{Kind: string(I3GTIDHole), Subject: slot}}})
	if _, ok := rep.VerifiedAt[overdue]; !ok || rep.Coverage[slot].Local.Method != MethodStat || rep.Coverage[slot].Object.Method != MethodHead {
		t.Fatalf("named overdue segment was paid for: %v %+v", rep.VerifiedAt, rep.Coverage[slot])
	}
	rep = mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: other, LastVerified: lv, PassBytes: 2 * seg})
	if len(rep.VerifiedAt) != 2 {
		t.Fatalf("both overdue segments fit: %v", rep.VerifiedAt)
	}
}

// TestVerify_HeadsEveryObjectBeforeAnyContent: with nothing to spend, every
// object is still HEAD'd, so a deleted object is found on every pass.
func TestVerify_HeadsEveryObjectBeforeAnyContent(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	objs := objectsFromDir(t, f)
	gone := rows[2]
	delete(objs.obj, gone.ObjectKey)
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, PassBytes: 1})
	if !hasFinding(rep.Findings, KindObjectMissing, gone.Name) || rep.Bytes != 0 || objs.openCount() != 0 {
		t.Fatalf("findings %+v bytes %d opens %d", rep.Findings, rep.Bytes, objs.openCount())
	}
	for _, row := range rows {
		c := rep.Coverage[row.Name]
		if row.Name == gone.Name {
			if c.Object.Method != MethodMissing {
				t.Fatalf("%s %+v", row.Name, c)
			}
			continue
		}
		if !c.Object.Found || !c.Object.SizeChecked || c.Object.Method != MethodSkipped || !c.Local.Found || c.Local.Method != MethodSkipped {
			t.Fatalf("%s coverage %+v", row.Name, c)
		}
	}
	if rep.Deferred != 2*len(rows)-1 || len(rep.VerifiedAt) != 0 {
		t.Fatalf("deferred %d verified %v", rep.Deferred, rep.VerifiedAt)
	}
}

// TestVerify_DeadlineEndsThePassNormally: a passed deadline or a budget that
// cannot be paid in time defers the rest and returns no error; only cancel aborts.
func TestVerify_DeadlineEndsThePassNormally(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))
	prev := []Finding{{Kind: KindObjectMissing, Subject: rows[0].Name}, {Kind: KindObjectCorrupt, Subject: rows[0].Name}, {Kind: KindVerifyFailed, Subject: rows[1].Name}}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	rep, err := Verify(ctx, f.dir, rows, objectsFromDir(t, f), chainOf(f), Options{Now: testNow, RotationPeriods: 1, OpenFindings: prev})
	if err != nil {
		t.Fatalf("passed deadline returned %v", err)
	}
	if rep.ChainChecked || rep.Deferred != 2*len(rows) || len(rep.VerifiedAt) != 0 {
		t.Fatalf("chain=%v deferred=%d verified=%v", rep.ChainChecked, rep.Deferred, rep.VerifiedAt)
	}
	for _, row := range rows {
		c := rep.Coverage[row.Name]
		if c.Local.Method != MethodSkipped || c.Object.Method != MethodSkipped || !strings.Contains(c.Local.Reason, "deadline") {
			t.Fatalf("%s coverage %+v", row.Name, c)
		}
	}
	if len(rep.Closed) != 1 || rep.Closed[0].Finding.Kind != KindObjectMissing {
		t.Fatalf("closed %+v; only what HEAD saw may close", rep.Closed)
	}

	// 1000 B/s for 50ms cannot pay for a 15764-byte file: deferred up front.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	budget := NewByteBudget(1000)
	rep, err = Verify(ctx2, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: testNow, RotationPeriods: 1, Budget: budget})
	if err != nil || rep.Bytes != 0 || budget.Charged() != 0 || rep.Deferred != 2*len(rows) {
		t.Fatalf("slow budget: err=%v bytes=%d charged=%d deferred=%d", err, rep.Bytes, budget.Charged(), rep.Deferred)
	}
	if c := rep.Coverage[rows[0].Name]; !strings.Contains(c.Local.Reason, "does not fit") {
		t.Fatalf("coverage %+v", c)
	}
}

// drainOnRead is a concurrent pass taking the whole burst just after this
// pass started reading an object.
type drainOnRead struct {
	*memObjects
	budget  *ByteBudget
	drained int64
}

func (d *drainOnRead) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := d.memObjects.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	return &drainReader{ReadCloser: rc, d: d}, nil
}

type drainReader struct {
	io.ReadCloser
	d    *drainOnRead
	done bool
}

func (r *drainReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		burst := int64(r.d.budget.lim.Burst())
		if err := r.d.budget.Wait(context.Background(), burst); err != nil {
			return 0, err
		}
		r.d.drained += burst
	}
	return r.ReadCloser.Read(p)
}

// TestVerify_BytesAreWhatThePassPaid is QA r2 F6: a read whose payment fails
// is not counted, so Report.Bytes equals what this pass charged.
func TestVerify_BytesAreWhatThePassPaid(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	if err := os.Remove(filepath.Join(f.dir, rows[0].Name)); err != nil {
		t.Fatal(err)
	}
	budget := NewByteBudget(64 << 10)
	store := &drainOnRead{memObjects: objectsFromDir2(t, f, rows), budget: budget}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	rep, err := Verify(ctx, f.dir, rows, store, nil, Options{Now: testNow, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	if store.drained == 0 || rep.Bytes != budget.Charged()-store.drained || rep.Bytes != 0 || rep.ObjectBytes != 0 {
		t.Fatalf("bytes=%d object=%d charged=%d drained=%d", rep.Bytes, rep.ObjectBytes, budget.Charged(), store.drained)
	}
	if c := rep.Coverage[rows[0].Name]; c.Object.Method != MethodSkipped || rep.Deferred != 1 {
		t.Fatalf("coverage %+v deferred %d", c, rep.Deferred)
	}

	// A read larger than one burst is cut to one burst, so a payment that
	// fails half way through a large read cannot leave paid bytes uncounted:
	// 4000 B/s, a 6000-byte object, another pass takes 2500 bytes first.
	body := bytes.Repeat([]byte("x"), 6000)
	sum := sha256.Sum256(body)
	row := VerifyRow{Name: "big.000001", Sealed: true, Size: 6000, SHA256: hex.EncodeToString(sum[:]), UploadState: "UPLOADED", ObjectKey: "t/big.000001", UploadedAt: testNow.Add(-time.Hour), RetentionRemoved: true}
	budget = NewByteBudget(4000)
	part := &partDrain{memObjects: &memObjects{obj: map[string][]byte{row.ObjectKey: body}}, budget: budget, take: 2500}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	rep, err = Verify(ctx2, t.TempDir(), []VerifyRow{row}, part, nil, Options{Now: testNow, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bytes != budget.Charged()-part.take || rep.Bytes != 4000 {
		t.Fatalf("bytes=%d charged=%d taken=%d", rep.Bytes, budget.Charged(), part.take)
	}
}

// partDrain takes part of the burst just after the pass opens the object.
type partDrain struct {
	*memObjects
	budget *ByteBudget
	take   int64
}

func (d *partDrain) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := d.memObjects.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := d.budget.Wait(context.Background(), d.take); err != nil {
		return nil, err
	}
	return rc, nil
}

// TestSpanTracker_MatchesEventSpan: the streamed span equals binlog.EventSpan
// on the fixture files and on damaged copies, fed in random chunk sizes.
func TestSpanTracker_MatchesEventSpan(t *testing.T) {
	src := filepath.Join("..", "binlog", "testdata", "ledger", "live")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(11))
	tmp := t.TempDir()
	check := func(label string, body []byte) {
		p := filepath.Join(tmp, "seg")
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		ws, we, wok := binlog.EventSpan(p)
		tr := &spanTracker{size: int64(len(body))}
		rest := body
		for len(rest) > 0 {
			k := 1 + rng.Intn(64)
			if rng.Intn(4) == 0 {
				k = 1 + rng.Intn(8192)
			}
			k = min(k, len(rest))
			if _, err := tr.Write(rest[:k]); err != nil {
				t.Fatal(err)
			}
			rest = rest[k:]
		}
		gs, ge, gok := tr.result()
		if gs != ws || ge != we || gok != wok {
			t.Fatalf("%s: tracker (%d,%d,%v), EventSpan (%d,%d,%v)", label, gs, ge, gok, ws, we, wok)
		}
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "mysql-bin.") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		check(e.Name(), body)
		for i := 0; i < 150; i++ {
			b := append([]byte(nil), body...)
			switch i % 3 {
			case 0:
				b[rng.Intn(min(len(b), 4096))] ^= byte(1 + rng.Intn(255))
			case 1:
				b = b[:rng.Intn(len(b))]
			default:
				for j := 0; j < 4; j++ {
					b[rng.Intn(len(b))] = byte(rng.Intn(256))
				}
			}
			check(fmt.Sprintf("%s damage %d", e.Name(), i), b)
		}
		checked++
	}
	check("empty", nil)
	check("magic only", []byte("\xfebin"))
	check("bad magic", bytes.Repeat([]byte{1}, 64))
	if checked < 4 {
		t.Fatalf("checked %d files", checked)
	}
}

// TestVerify_VerifyFailedWithLocalRemoved is QA r2 F2: an open VERIFY_FAILED
// whose local copy retention removed closes when the object hashes clean
// against the catalog digest, with that reason; otherwise it stays open and
// the object is rehashed on its slot only. last_verified_at never advances
// while the finding is open.
func TestVerify_VerifyFailedWithLocalRemoved(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			f := live(t)
			rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
			n := rows[1].Name
			if err := os.Remove(filepath.Join(f.dir, n)); err != nil {
				t.Fatal(err)
			}
			binlog.ForgetFile(filepath.Join(f.dir, n))
			objs := objectsFromDir2(t, f, rows)
			if corrupt {
				flipStored(t, objs, "t/"+n)
			}
			open := []Finding{{Kind: KindVerifyFailed, Subject: n, Detail: DetailSHA}}
			gets, closedOn := 0, -1
			for d := 0; d < 45; d++ {
				now := testNow.Add(time.Duration(d) * 24 * time.Hour)
				before := objs.opened["t/"+n]
				rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: now, OpenFindings: open, LastVerified: verifiedAt(rows, now.Add(-DefaultPeriod))})
				got := objs.opened["t/"+n] - before
				gets += got
				in := InRotation(n, now, DefaultPeriod, DefaultRotationPeriods)
				if got != 0 && !in {
					t.Fatalf("day %d: %d GETs off the slot", d, got)
				}
				_, advanced := rep.VerifiedAt[n]
				for _, c := range rep.Closed {
					if c.Finding.Kind == KindVerifyFailed {
						if corrupt || c.Reason != ReasonLocalRemoved {
							t.Fatalf("day %d closed %+v (corrupt=%v)", d, c, corrupt)
						}
						closedOn = d
					}
				}
				if closedOn == d {
					if !advanced || !in {
						t.Fatalf("day %d: closed on slot=%v, advanced=%v", d, in, advanced)
					}
					open = nil
				} else if advanced && closedOn < 0 {
					t.Fatalf("day %d: advanced with VERIFY_FAILED open: %+v", d, rep.Coverage[n])
				}
				if corrupt {
					// The caller keeps every finding open; OBJECT_CORRUPT does not force a GET without a local copy.
					open = append([]Finding{{Kind: KindVerifyFailed, Subject: n, Detail: DetailSHA}}, rep.Findings...)
				}
			}
			if corrupt && (closedOn >= 0 || gets < 2 || gets > 3) {
				t.Fatalf("corrupt: closed on %d, %d GETs in 45 days", closedOn, gets)
			}
			if !corrupt && closedOn < 0 {
				t.Fatal("never closed")
			}
		})
	}
}

// TestVerify_OpenFindingsBlockLastVerified: a segment an open finding names
// does not advance unless this pass closes that finding.
func TestVerify_OpenFindingsBlockLastVerified(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	n := rows[0].Name
	objs := objectsFromDir(t, f)
	for _, c := range []struct {
		kind    string
		advance bool
	}{
		{string(I3GTIDHole), false}, // chain not checked: stays open
		{KindLocalMissing, true},    // the file is there
		{KindObjectMissing, true},   // HEAD found it
		{KindUploadClockSkew, true}, // clock compared
		{KindObjectCorrupt, true},   // hashed and matching
		{KindVerifyFailed, true},    // parsed and matching
	} {
		// A named segment gets content work on its slot only; rotation 1 makes every period its slot.
		rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, RotationPeriods: 1, LastVerified: verifiedAt(rows, testNow.Add(-DefaultPeriod)), OpenFindings: []Finding{{Kind: c.kind, Subject: n}}})
		if c := rep.Coverage[n]; c.Local.Method != MethodParsed || c.Object.Method != MethodHashed {
			t.Fatalf("coverage %+v", c)
		}
		if _, ok := rep.VerifiedAt[n]; ok != c.advance {
			t.Fatalf("open %s: advanced=%v want %v (closed %+v)", c.kind, ok, c.advance, rep.Closed)
		}
	}
}

// TestVerify_RowRemovedClosesEveryKind is QA r2 F3: once a complete catalog
// has no row for a subject, every row-scoped kind on it closes as "row
// removed", and it is no longer forced. I6_MISSING_ROW is the exception (no
// row is what it reports), and so are chain kinds (QA r3 F3): removing a row
// does not fill a GTID hole, so they close only when the chain is checked.
func TestVerify_RowRemovedClosesEveryKind(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-30*24*time.Hour))
	n := rows[1].Name
	kept := append(append([]VerifyRow(nil), rows[:1]...), rows[2:]...)
	var prev []Finding
	for _, kind := range []string{KindVerifyFailed, KindObjectMissing, KindObjectSizeMismatch, KindObjectCorrupt, KindLocalMissing, KindUploadClockSkew, string(I3GTIDHole), string(I6MissingRow)} {
		prev = append(prev, Finding{Kind: kind, Subject: n})
	}
	objs := objectsFromDir(t, f)
	lv := verifiedAt(rows, testNow.Add(-DefaultPeriod))
	rep := mustVerify(t, f.dir, kept, objs, nil, Options{Now: testNow, OpenFindings: prev, CatalogComplete: true, RotationPeriods: -1, LastVerified: lv})
	if len(rep.Closed) != len(prev)-2 {
		t.Fatalf("closed %+v", rep.Closed)
	}
	for _, c := range rep.Closed {
		if c.Reason != ReasonRowRemoved || c.Finding.Kind == string(I6MissingRow) || c.Finding.Kind == string(I3GTIDHole) {
			t.Fatalf("closure %+v", c)
		}
	}
	if objs.opened["t/"+n] != 0 || objs.openCount() != 0 {
		t.Fatalf("a removed row was still forced: %v", objs.opened)
	}
	// Not known to be the whole catalog: nothing closes without evidence.
	rep = mustVerify(t, f.dir, kept, objs, nil, Options{Now: testNow, OpenFindings: prev, RotationPeriods: -1, LastVerified: lv})
	if len(rep.Closed) != 0 {
		t.Fatalf("partial catalog closed %+v", rep.Closed)
	}
	// A row that is still there (even unsealed) is not removed.
	unsealed := rows[1]
	unsealed.Sealed = false
	rep = mustVerify(t, f.dir, append(kept, unsealed), objs, nil, Options{Now: testNow, OpenFindings: prev, CatalogComplete: true, RotationPeriods: -1, LastVerified: lv})
	for _, c := range rep.Closed {
		if c.Reason == ReasonRowRemoved {
			t.Fatalf("present row closed as removed: %+v", c)
		}
	}
}

// TestVerify_UnknownRowSize is QA r2 F4: with row size 0 a HEAD cannot
// compare the size; only a hash that matches the local file does.
func TestVerify_UnknownRowSize(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	n, key := rows[0].Name, rows[0].ObjectKey
	rows[0].Size = 0
	prev := []Finding{{Kind: KindObjectSizeMismatch, Subject: n}}

	objs := objectsFromDir(t, f)
	good := objs.obj[key]
	objs.obj[key] = good[:100]
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, OpenFindings: prev})
	if c := rep.Coverage[n]; c.Object.Method != MethodHashed || c.Object.SizeChecked || len(rep.Closed) != 0 || !hasFinding(rep.Findings, KindObjectCorrupt, n) || rep.UnverifiableSize != 1 {
		t.Fatalf("truncated object, size 0: %+v closed %+v findings %+v unverifiable %d", c, rep.Closed, rep.Findings, rep.UnverifiableSize)
	}
	objs.obj[key] = good
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, OpenFindings: prev})
	if len(rep.Closed) != 1 || rep.Closed[0].Reason != ReasonSizeCompared || rep.UnverifiableSize != 0 || !rep.Coverage[n].Object.SizeChecked {
		t.Fatalf("matching local hash: closed %+v cov %+v", rep.Closed, rep.Coverage[n])
	}

	// Old and off-slot: a HEAD alone closes nothing; the open finding forces the hash.
	rows[0].UploadedAt = testNow.Add(-30 * 24 * time.Hour)
	_, off := oldDayInSlot(t, n, testNow)
	lv := verifiedAt(rows, off.Add(-DefaultPeriod))
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, LastVerified: lv})
	if c := rep.Coverage[n]; c.Object.Method != MethodHead || c.Object.SizeChecked || len(Disappeared(prev, rep)) != 0 || rep.UnverifiableSize != 1 {
		t.Fatalf("HEAD with size 0: %+v", c)
	}
	rep = mustVerify(t, f.dir, rows, objs, nil, Options{Now: off, LastVerified: lv, OpenFindings: prev})
	if c := rep.Coverage[n]; c.Object.Method != MethodHashed || len(rep.Closed) != 1 {
		t.Fatalf("open size mismatch on size 0 not forced: %+v %+v", c, rep.Closed)
	}

	// No local file: a catalog-digest hash does not compare the size.
	if err := os.Remove(filepath.Join(f.dir, n)); err != nil {
		t.Fatal(err)
	}
	in, _ := oldDayInSlot(t, n, testNow)
	rep = mustVerify(t, f.dir, rows, objectsFromDir2(t, f, rows), nil, Options{Now: in, LastVerified: verifiedAt(rows, in.Add(-DefaultPeriod)), OpenFindings: prev})
	if c := rep.Coverage[n]; c.Object.Method != MethodHashed || c.Object.Digest != "catalog" || c.Object.SizeChecked || len(rep.Closed) != 0 || rep.UnverifiableSize != 1 {
		t.Fatalf("no local, size 0: %+v closed %+v", c, rep.Closed)
	}
}

// TestVerify_RetentionMarkerDoesNotCloseMissingLocal is QA r2 X14: a row that
// was never uploaded and whose file retention removed is ABSENT, which does
// not show the file exists, so I6_MISSING_LOCAL stays open.
func TestVerify_RetentionMarkerDoesNotCloseMissingLocal(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	rows[0].UploadState, rows[0].ObjectKey, rows[0].RetentionRemoved = "UPLOAD_FAILED", "", true
	if err := os.Remove(filepath.Join(f.dir, rows[0].Name)); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, f.dir, rows, nil, nil, Options{Now: testNow, OpenFindings: []Finding{{Kind: KindLocalMissing, Subject: rows[0].Name}}})
	if rep.Coverage[rows[0].Name].Local.Method != MethodAbsent || len(rep.Closed) != 0 || len(rep.Findings) != 0 {
		t.Fatalf("cov %+v closed %+v findings %+v", rep.Coverage[rows[0].Name], rep.Closed, rep.Findings)
	}
}

// TestVerify_OverdueAtRotationTimesPeriod: a segment whose last check is
// exactly rotation x period old is overdue; one second younger it is not.
func TestVerify_OverdueAtRotationTimesPeriod(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-300*24*time.Hour))[:1]
	n := rows[0].Name
	const rotation = 2
	now := testNow
	for InRotation(n, now, DefaultPeriod, rotation) {
		now = now.Add(24 * time.Hour)
	}
	gap := WorstGap(rotation, DefaultPeriod)
	rep := mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: now, RotationPeriods: rotation, LastVerified: map[string]time.Time{n: now.Add(-gap)}})
	if _, ok := rep.VerifiedAt[n]; !ok {
		t.Fatalf("exactly %s old, not overdue: %+v", gap, rep.Coverage[n])
	}
	rep = mustVerify(t, f.dir, rows, objectsFromDir(t, f), nil, Options{Now: now, RotationPeriods: rotation, LastVerified: map[string]time.Time{n: now.Add(-gap + time.Second)}})
	if c := rep.Coverage[n]; c.Local.Method != MethodStat || c.Object.Method != MethodHead {
		t.Fatalf("younger than %s, still scheduled: %+v", gap, c)
	}
}

// TestVerify_DeferredLocalParseDefersTheDigestlessHash: with no catalog
// digest the object needs this pass's local digest; when the parse was
// deferred, the hash is deferred too, not counted as unverifiable.
func TestVerify_DeferredLocalParseDefersTheDigestlessHash(t *testing.T) {
	f := live(t)
	rows := verifyRows(t, f, testNow.Add(-time.Hour))[:1]
	n, key := rows[0].Name, rows[0].ObjectKey
	rows[0].SHA256, rows[0].Size = "", 0
	objs := objectsFromDir(t, f)
	objs.obj[key] = objs.obj[key][:100]
	rep := mustVerify(t, f.dir, rows, objs, nil, Options{Now: testNow, PassBytes: 1000})
	c := rep.Coverage[n]
	if c.Local.Method != MethodSkipped || c.Object.Method != MethodSkipped || rep.UnverifiableNoDigest != 0 || rep.Deferred != 2 || objs.openCount() != 0 {
		t.Fatalf("cov %+v unverifiable %d deferred %d opens %d", c, rep.UnverifiableNoDigest, rep.Deferred, objs.openCount())
	}
}

// TestVerify_WorkThatDoesNotFitIsSkippedNotBlocking: an item at the head of
// the queue that does not fit is deferred, and later work that does fit is
// still paid for in the same pass.
func TestVerify_WorkThatDoesNotFitIsSkippedNotBlocking(t *testing.T) {
	dir, rows, objs := simFixture(t, 0, []int{50000, 1000})
	big, small := rows[0].Name, rows[1].Name
	now := testNow
	for InRotation(small, now, DefaultPeriod, DefaultRotationPeriods) || InRotation(big, now, DefaultPeriod, DefaultRotationPeriods) {
		now = now.Add(24 * time.Hour)
	}
	lv := map[string]time.Time{big: now.Add(-100 * 24 * time.Hour), small: now.Add(-50 * 24 * time.Hour)}
	rep := mustVerify(t, dir, rows, objs, nil, Options{Now: now, PassBytes: 10000, LastVerified: lv})
	if c := rep.Coverage[big]; c.Object.Method != MethodSkipped || !strings.Contains(c.Object.Reason, "does not fit") {
		t.Fatalf("big %+v", c)
	}
	if c := rep.Coverage[small]; c.Object.Method != MethodHashed {
		t.Fatalf("small after a deferred item was not paid for: %+v", c)
	}
	if _, ok := rep.VerifiedAt[small]; !ok || rep.Deferred != 1 || rep.Bytes != 1000 {
		t.Fatalf("verified %v deferred %d bytes %d", rep.VerifiedAt, rep.Deferred, rep.Bytes)
	}
}
