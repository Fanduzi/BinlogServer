// Package assurance provides module-level functionality for assurance.
// input: catalog rows, a task directory of sealed segments, an object store that can HEAD and GET, an optional Chain, the findings open before this pass, and a byte budget
// output: a Report whose Coverage records, per segment, what this pass did to the local copy and to the object (parsed, hashed, HEAD only, skipped, error) and why; the findings it wants open; the segments whose last_verified_at may advance (content-verified only); per-pass bytes; counts of objects with no digest. Disappeared closes a previous finding only for a subject this pass checked with the method that detects that kind
// pos: ADR 0009 phase 3 pure verifier (docs/design/0009-phase3-verifier.md); the caller must already be the task owner — this file does not ask
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"binlog_server/internal/binlog"

	"golang.org/x/time/rate"
)

const (
	// DefaultSamplePercent is the share of older uploaded objects whose
	// bodies are hashed on one pass. It sets the rotation length: 5% is a
	// rotation of 20 periods, so every object is hashed at least once every
	// 20 periods. Recent uploads and objects with an open OBJECT_CORRUPT or
	// VERIFY_FAILED are hashed on every pass.
	DefaultSamplePercent = 5
	// DefaultRecent is the window in which every upload is hashed.
	DefaultRecent = 24 * time.Hour
	// DefaultPeriod is the length of one rotation slot.
	DefaultPeriod = 24 * time.Hour
	// ClockSkewTolerance is how far in the future uploaded_at may be and
	// still count as recent. Beyond it the row is UPLOAD_CLOCK_SKEW.
	ClockSkewTolerance = time.Hour
	// DefaultBudgetBytesPerSec is the process-wide read budget.
	DefaultBudgetBytesPerSec int64 = 4 << 20
	// maxBurst caps the token bucket so a large file cannot borrow more
	// than this many bytes at once.
	maxBurst = 64 << 10

	// KindVerifyFailed is a sealed local segment that does not parse or does
	// not match its catalog row (size, SHA-256, positions, GTID text).
	KindVerifyFailed = "VERIFY_FAILED"
	// KindObjectMissing is an UPLOADED object that HEAD or GET did not find.
	KindObjectMissing = "I6_MISSING_OBJECT"
	// KindObjectSizeMismatch is an UPLOADED object whose HEAD size is not the row size.
	KindObjectSizeMismatch = "I6_OBJECT_SIZE_MISMATCH"
	// KindObjectCorrupt is an object whose SHA-256 is not the local file's or the recorded digest.
	KindObjectCorrupt = "OBJECT_CORRUPT"
	// KindLocalMissing is a sealed row that is not UPLOADED and has no local
	// file: no copy of those bytes exists. Same name as the chain's I6.
	KindLocalMissing = string(I6MissingLocal)
	// KindUploadClockSkew is an UPLOADED row whose uploaded_at is more than
	// ClockSkewTolerance in the future.
	KindUploadClockSkew = "UPLOAD_CLOCK_SKEW"

	SeverityCritical = "CRITICAL"
	SeverityWarning  = "WARNING"
	SeverityInfo     = "INFO"
)

// Method is what one pass did to one copy of a segment.
type Method string

const (
	// MethodParsed: the local file was read end to end (magic, event
	// lengths, CRC32, GTID events), hashed, and compared with the row.
	MethodParsed Method = "PARSED"
	// MethodStat: the local file's size differs from the row; it was not parsed.
	MethodStat Method = "STAT"
	// MethodAbsent: there is no local file and none is required (the row is
	// UPLOADED, or local retention removed it).
	MethodAbsent Method = "ABSENT"
	// MethodMissing: a copy that must exist does not (local: sealed and not
	// UPLOADED; object: HEAD or GET answered not found).
	MethodMissing Method = "MISSING"
	// MethodHashed: the object body was read and its SHA-256 compared with a digest.
	MethodHashed Method = "HASHED"
	// MethodHead: the object exists and its size was compared; the body was not read.
	MethodHead Method = "HEAD"
	// MethodUnverifiable: the object exists and its size was compared, but
	// there is no digest to compare a body with (empty catalog sha256 and no
	// clean local parse). Never counts as verified.
	MethodUnverifiable Method = "UNVERIFIABLE"
	// MethodNotApplicable: the row is not UPLOADED, so there is no object to check.
	MethodNotApplicable Method = "NOT_APPLICABLE"
	// MethodError: a read failed with something other than not-found.
	// Nothing about this copy is known.
	MethodError Method = "ERROR"
	// MethodSkipped: this copy was not looked at; Reason says why.
	MethodSkipped Method = "SKIPPED"
)

// Check is one copy's outcome. A zero Check (empty Method) means the pass
// never reached this copy.
type Check struct {
	Method Method
	Reason string
	// Digest is the source of the digest a HASHED body was compared with:
	// "local" or "catalog".
	Digest string
}

// Coverage is what one pass established about one segment.
type Coverage struct {
	Local  Check
	Object Check
	// ClockChecked is true when uploaded_at was compared with now.
	ClockChecked bool
}

// ObjectStore is the object half of one pass. A missing object is os.ErrNotExist.
// Any other error leaves that object unverified.
type ObjectStore interface {
	Head(ctx context.Context, key string) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// VerifyRow is one catalog row. Name is the on-disk segment name.
// GTIDSet is optional: the catalog has no such column, and an empty value is not compared.
// StartPos and EndPos of 0 are unknown and are not compared.
// RetentionRemoved is the design 0009 §26 marker: local retention removed the file.
type VerifyRow struct {
	Name             string
	Sealed           bool
	Size             int64
	StartPos         uint32
	EndPos           uint32
	SHA256           string
	GTIDSet          string
	UploadState      string
	ObjectKey        string
	UploadedAt       time.Time
	RetentionRemoved bool
}

// Finding is one conclusion a pass wants open. It is not a stored row.
type Finding struct {
	Kind     string
	Subject  string
	Severity string
	Detail   string
	Evidence string
}

// Report is one pass.
//
// Coverage has an entry for every sealed row the pass reached, and each
// copy's Method says what was done. A copy the pass never reached (it
// returned early on cancel or error) has a zero Check. Closing a finding and
// advancing last_verified_at read only Coverage, so an early return cannot
// close or advance anything it did not check.
type Report struct {
	Findings   []Finding
	Coverage   map[string]Coverage
	VerifiedAt map[string]time.Time
	// ChainChecked is true when Chain.Check ran on a fully listed and read directory.
	ChainChecked bool
	// Bytes is what this pass read: LocalBytes plus ObjectBytes. It is not
	// the shared budget's running total.
	Bytes       int64
	LocalBytes  int64
	ObjectBytes int64
	// UnverifiableNoDigest counts UPLOADED rows this pass had no digest for:
	// the catalog sha256 is empty and the local file is gone or did not parse
	// cleanly. Their objects get HEAD only.
	UnverifiableNoDigest int
	// NoDigestLocal counts sealed rows with an empty catalog sha256 whose local
	// file still exists (rows sealed before migration 000008). A digest can be
	// backfilled from that file now; after retention it cannot.
	NoDigestLocal int
	LocalErr      error
	ObjectErr     error
}

// Options selects the sample and the budget. A nil Budget is unlimited.
// SamplePercent 0 means DefaultSamplePercent. Negative means older objects
// are not downloaded unless forced. Recent and Period of 0 mean the defaults.
// OpenFindings are the findings open before this pass: a subject with an open
// OBJECT_CORRUPT or VERIFY_FAILED has its object hashed on every pass until a
// pass rechecks it.
type Options struct {
	Now           time.Time
	SamplePercent int
	Recent        time.Duration
	Period        time.Duration
	Budget        *ByteBudget
	OpenFindings  []Finding
}

// ErrBudgetDeadline is a budget wait that cannot finish before the context
// deadline. The pass stops there, as it does on cancel.
var ErrBudgetDeadline = errors.New("byte budget wait would pass the context deadline")

// ByteBudget paces reads. A nil budget does not pace. It is safe to share
// between concurrent passes.
type ByteBudget struct {
	lim     *rate.Limiter
	charged atomic.Int64
}

// NewByteBudget paces at bytesPerSec. bytesPerSec <= 0 returns nil (unlimited).
func NewByteBudget(bytesPerSec int64) *ByteBudget {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := bytesPerSec
	if burst > maxBurst {
		burst = maxBurst
	}
	return &ByteBudget{lim: rate.NewLimiter(rate.Limit(bytesPerSec), int(burst))}
}

// Wait blocks until n bytes are allowed. n larger than the burst is split.
func (b *ByteBudget) Wait(ctx context.Context, n int64) error {
	if b == nil || n <= 0 {
		return nil
	}
	burst := int64(b.lim.Burst())
	for n > 0 {
		step := n
		if step > burst {
			step = burst
		}
		if err := b.lim.WaitN(ctx, int(step)); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("%w: %v", ErrBudgetDeadline, err)
		}
		b.charged.Add(step)
		n -= step
	}
	return nil
}

// Charged is the number of bytes Wait has allowed across every pass sharing b.
func (b *ByteBudget) Charged() int64 {
	if b == nil {
		return 0
	}
	return b.charged.Load()
}

// ClockSkewed reports whether uploadedAt is more than ClockSkewTolerance after now.
func ClockSkewed(uploadedAt, now time.Time) bool {
	return !uploadedAt.IsZero() && uploadedAt.Sub(now) > ClockSkewTolerance
}

// RecentUpload reports whether uploadedAt is inside the recent window.
// An empty time is not recent. A time up to ClockSkewTolerance in the future
// is recent; further in the future is clock skew and is not.
func RecentUpload(uploadedAt, now time.Time, recent time.Duration) bool {
	if recent <= 0 {
		recent = DefaultRecent
	}
	if uploadedAt.IsZero() || ClockSkewed(uploadedAt, now) {
		return false
	}
	return now.Sub(uploadedAt) <= recent
}

// RotationLength is the number of periods in one rotation for percent:
// ceil(100/percent). 1 means every period, 0 means never.
func RotationLength(percent int) int {
	if percent == 0 {
		percent = DefaultSamplePercent
	}
	if percent < 0 {
		return 0
	}
	if percent >= 100 {
		return 1
	}
	return (100 + percent - 1) / percent
}

// InRotation reports whether name's slot is the current period. Each name
// has one fixed slot, sha256(name) mod RotationLength, so it is drawn exactly
// once every RotationLength periods.
func InRotation(name string, now time.Time, period time.Duration, percent int) bool {
	n := RotationLength(percent)
	if n == 0 {
		return false
	}
	if period < time.Second {
		period = DefaultPeriod
	}
	slot := uint64(now.Unix()) / uint64(period/time.Second)
	sum := sha256.Sum256([]byte(name))
	h := binary.BigEndian.Uint64(sum[:8])
	return h%uint64(n) == slot%uint64(n)
}

// SampleObject reports whether this pass downloads the object body without
// being forced: a recent upload, or the object's rotation slot.
func SampleObject(name string, uploadedAt, now time.Time, recent, period time.Duration, percent int) bool {
	return RecentUpload(uploadedAt, now, recent) || InRotation(name, now, period, percent)
}

// passCounter counts the bytes one pass read. One pass runs on one goroutine.
type passCounter struct {
	local  int64
	object int64
}

// Verify checks sealed segments under dir against rows, objects and chain.
// The caller must already be the owner of this task. A store of nil leaves
// every object SKIPPED. A chain of nil leaves the chain unchecked.
// On an early return the Report holds only what was checked before it.
func Verify(ctx context.Context, dir string, rows []VerifyRow, objects ObjectStore, chain *Chain, opt Options) (rep Report, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rep = Report{Coverage: map[string]Coverage{}, VerifiedAt: map[string]time.Time{}}
	var count passCounter
	defer func() { rep.finish(now, &count) }()
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	items := sealedRows(rows)
	forced := forcedSubjects(opt.OpenFindings)

	present := map[string]bool{}
	entries, listErr := os.ReadDir(dir)
	listed := listErr == nil
	if listed {
		for _, e := range entries {
			if !e.IsDir() {
				present[e.Name()] = true
			}
		}
	} else {
		rep.LocalErr = listErr
	}
	localClean := listed
	localSHA := map[string]string{}
	for _, row := range items {
		var cov Coverage
		switch {
		case !listed:
			cov.Local = Check{Method: MethodSkipped, Reason: "task directory not listed: " + listErr.Error()}
		case !present[row.Name]:
			cov.Local = absentLocal(row)
			if cov.Local.Method == MethodMissing {
				rep.Findings = append(rep.Findings, Finding{Kind: KindLocalMissing, Subject: row.Name, Severity: SeverityCritical,
					Detail: "sealed segment is not UPLOADED and has no local file", Evidence: "upload_state=" + strings.TrimSpace(row.UploadState)})
			}
		default:
			if strings.TrimSpace(row.SHA256) == "" {
				rep.NoDigestLocal++
			}
			res, readErr := checkLocal(ctx, filepath.Join(dir, row.Name), row, opt.Budget, &count)
			if readErr != nil {
				if stop := abort(ctx, readErr); stop != nil {
					return rep, stop
				}
				cov.Local = Check{Method: MethodError, Reason: readErr.Error()}
				localClean = false
				if rep.LocalErr == nil {
					rep.LocalErr = readErr
				}
				break
			}
			cov.Local = res.check
			if res.finding != nil {
				rep.Findings = append(rep.Findings, *res.finding)
			} else if res.sum != "" {
				localSHA[row.Name] = res.sum
			}
		}
		rep.Coverage[row.Name] = cov
	}

	for _, row := range items {
		cov := rep.Coverage[row.Name]
		if !uploaded(row) {
			cov.Object = Check{Method: MethodNotApplicable, Reason: "upload_state=" + strings.TrimSpace(row.UploadState)}
			rep.Coverage[row.Name] = cov
			continue
		}
		// The clock observation needs no I/O, so it is complete and recorded before the object is touched.
		if ClockSkewed(row.UploadedAt, now) {
			rep.Findings = append(rep.Findings, Finding{Kind: KindUploadClockSkew, Subject: row.Name, Severity: SeverityWarning,
				Detail:   "uploaded_at is in the future; the row is not treated as recent",
				Evidence: fmt.Sprintf("uploaded_at=%s now=%s", row.UploadedAt.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))})
		}
		cov.ClockChecked = true
		rep.Coverage[row.Name] = cov
		digest, source := localSHA[row.Name], "local"
		if digest == "" {
			digest, source = strings.TrimSpace(row.SHA256), "catalog"
		}
		if digest == "" {
			rep.UnverifiableNoDigest++
		}
		if objects == nil {
			cov.Object = Check{Method: MethodSkipped, Reason: "no object store"}
		} else {
			why := hashReason(row, now, opt, forced)
			res, objErr := checkObject(ctx, row, digest, source, why, objects, opt.Budget, &count)
			if objErr != nil {
				if stop := abort(ctx, objErr); stop != nil {
					return rep, stop
				}
				cov.Object = Check{Method: MethodError, Reason: objErr.Error()}
				if rep.ObjectErr == nil {
					rep.ObjectErr = objErr
				}
			} else {
				cov.Object = res.check
				if res.finding != nil {
					rep.Findings = append(rep.Findings, *res.finding)
				}
			}
		}
		rep.Coverage[row.Name] = cov
	}

	if chain != nil && localClean {
		for _, v := range chain.Check(now) {
			rep.Findings = append(rep.Findings, findingFromViolation(v))
		}
		rep.ChainChecked = true
	}
	return rep, nil
}

// abort is the error that ends the pass early: a cancelled context, or a
// budget wait that cannot finish before the deadline. Other read errors leave
// one copy as ERROR and the pass goes on.
func abort(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrBudgetDeadline) {
		return err
	}
	return nil
}

// finish sorts and merges findings, totals this pass's bytes, and derives
// VerifiedAt from Coverage. It runs on every return, early or not.
func (rep *Report) finish(now time.Time, count *passCounter) {
	rep.Findings = dedupe(rep.Findings)
	rep.LocalBytes = count.local
	rep.ObjectBytes = count.object
	rep.Bytes = count.local + count.object
	named := map[string]bool{}
	for _, f := range rep.Findings {
		named[f.Subject] = true
	}
	for name, cov := range rep.Coverage {
		if !named[name] && contentVerified(cov) {
			rep.VerifiedAt[name] = now
		}
	}
}

// contentVerified is true when every copy that exists was checked by content
// this pass: the local file PARSED (or no local copy is required) and the
// object HASHED (or there is no object), and at least one of the two was.
// HEAD only, UNVERIFIABLE, SKIPPED, ERROR and unreached copies never count.
func contentVerified(cov Coverage) bool {
	local := cov.Local.Method == MethodParsed || cov.Local.Method == MethodAbsent
	object := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable
	read := cov.Local.Method == MethodParsed || cov.Object.Method == MethodHashed
	return local && object && read
}

// Disappeared is the previous findings this pass is positive evidence against:
// not reported again, and the subject was checked with the method that
// detects that kind. A copy that was skipped, errored or not reached closes nothing.
func Disappeared(previous []Finding, rep Report) []Finding {
	have := map[string]bool{}
	for _, f := range rep.Findings {
		have[f.Kind+"\x00"+f.Subject] = true
	}
	var out []Finding
	for _, f := range previous {
		if have[f.Kind+"\x00"+f.Subject] {
			continue
		}
		if closes(f, rep) {
			out = append(out, f)
		}
	}
	return out
}

// closes is the per-kind detector table (design §6).
func closes(f Finding, rep Report) bool {
	cov, ok := rep.Coverage[f.Subject]
	switch f.Kind {
	case KindVerifyFailed:
		return ok && cov.Local.Method == MethodParsed
	case KindLocalMissing:
		return (ok && localPresent(cov.Local.Method)) || rep.ChainChecked
	case KindObjectMissing:
		if ok {
			return objectFound(cov.Object.Method)
		}
		return rep.ChainChecked
	case KindObjectSizeMismatch:
		if ok {
			return objectFound(cov.Object.Method)
		}
		return rep.ChainChecked
	case KindObjectCorrupt:
		return ok && cov.Object.Method == MethodHashed
	case KindUploadClockSkew:
		return ok && cov.ClockChecked
	default:
		return rep.ChainChecked
	}
}

func localPresent(m Method) bool {
	return m == MethodParsed || m == MethodStat
}

// objectFound is a HEAD that answered with a size; each of these compared it with the row.
func objectFound(m Method) bool {
	return m == MethodHead || m == MethodUnverifiable || m == MethodHashed
}

func sealedRows(rows []VerifyRow) []VerifyRow {
	idx := map[string]int{}
	var out []VerifyRow
	for _, row := range rows {
		if !row.Sealed || row.Name == "" {
			continue
		}
		if named, ok := binlog.ClassifySegment(row.Name); ok && named.Open {
			continue
		}
		if i, ok := idx[row.Name]; ok {
			out[i] = row
			continue
		}
		idx[row.Name] = len(out)
		out = append(out, row)
	}
	return out
}

func forcedSubjects(open []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range open {
		if f.Kind == KindObjectCorrupt || f.Kind == KindVerifyFailed {
			out[f.Subject] = f.Kind
		}
	}
	return out
}

func absentLocal(row VerifyRow) Check {
	switch {
	case uploaded(row):
		return Check{Method: MethodAbsent, Reason: "no local file; the object is the remaining copy"}
	case row.RetentionRemoved:
		return Check{Method: MethodAbsent, Reason: "local retention removed the file"}
	default:
		return Check{Method: MethodMissing, Reason: "sealed, upload_state=" + strings.TrimSpace(row.UploadState) + ", no local file"}
	}
}

// hashReason is why this pass reads the object body, or "" when it does not.
func hashReason(row VerifyRow, now time.Time, opt Options, forced map[string]string) string {
	if kind, ok := forced[row.Name]; ok {
		return "open " + kind + " forces a recheck"
	}
	if RecentUpload(row.UploadedAt, now, opt.Recent) {
		return "uploaded within the recent window"
	}
	if InRotation(row.Name, now, opt.Period, opt.SamplePercent) {
		return "rotation slot"
	}
	return ""
}

func uploaded(row VerifyRow) bool {
	return strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOADED") && strings.TrimSpace(row.ObjectKey) != ""
}

type localResult struct {
	check   Check
	finding *Finding
	sum     string
}

func checkLocal(ctx context.Context, path string, row VerifyRow, budget *ByteBudget, count *passCounter) (localResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return localResult{}, err
	}
	if row.Size > 0 && info.Size() != row.Size {
		return localResult{
			check:   Check{Method: MethodStat, Reason: "size differs from the catalog row; not parsed"},
			finding: localFinding(row.Name, "size differs from the catalog row", fmt.Sprintf("local=%d row=%d", info.Size(), row.Size)),
		}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return localResult{}, err
	}
	defer f.Close()
	br := &budgetReader{ctx: ctx, r: f, b: budget, n: &count.local}
	h := sha256.New()
	contents, parseErr := binlog.ReadSealedSegmentContents(io.TeeReader(br, h))
	if parseErr == nil {
		_, parseErr = io.Copy(io.Discard, io.TeeReader(br, h))
		if errors.Is(parseErr, io.EOF) {
			parseErr = nil
		}
	}
	if br.err != nil {
		return localResult{}, br.err
	}
	if ctx.Err() != nil {
		return localResult{}, ctx.Err()
	}
	parsed := func(reason string, finding *Finding, sum string) (localResult, error) {
		return localResult{check: Check{Method: MethodParsed, Reason: reason}, finding: finding, sum: sum}, nil
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if parseErr != nil {
		return parsed("did not parse", localFinding(row.Name, "sealed segment did not parse", parseErr.Error()), "")
	}
	want := strings.TrimSpace(row.SHA256)
	if want != "" && !strings.EqualFold(sum, want) {
		return parsed("sha256 differs from the catalog row", localFinding(row.Name, "sha256 differs from the catalog row", fmt.Sprintf("local=%s row=%s", sum, want)), sum)
	}
	if row.StartPos > 0 || row.EndPos > 0 {
		// EventSpan seeks, so the second read is paid up front instead of paced.
		if err := budget.Wait(ctx, info.Size()); err != nil {
			return localResult{}, err
		}
		count.local += info.Size()
		start, end, ok := binlog.EventSpan(path)
		if !ok || (row.StartPos > 0 && start != row.StartPos) || (row.EndPos > 0 && end != row.EndPos) {
			return parsed("event positions differ from the catalog row", localFinding(row.Name, "event positions differ from the catalog row", fmt.Sprintf("start local=%d row=%d end local=%d row=%d", start, row.StartPos, end, row.EndPos)), sum)
		}
	}
	if gtid := strings.TrimSpace(row.GTIDSet); gtid != "" && contents.GTIDs.String() != gtid {
		return parsed("gtid set differs from the catalog row", localFinding(row.Name, "gtid set differs from the catalog row", fmt.Sprintf("parsed=%s row=%s", contents.GTIDs.String(), gtid)), sum)
	}
	reason := "parsed; matches the catalog row"
	if want == "" {
		reason = "parsed; catalog row has no sha256 to compare"
	}
	return parsed(reason, nil, sum)
}

func localFinding(name, detail, evidence string) *Finding {
	return &Finding{Kind: KindVerifyFailed, Subject: name, Severity: SeverityCritical, Detail: detail, Evidence: evidence}
}

type objectResult struct {
	check   Check
	finding *Finding
}

// checkObject HEADs the object and, when why is not empty and there is a
// digest, hashes the body. digest empty means UNVERIFIABLE: the body is not read.
func checkObject(ctx context.Context, row VerifyRow, digest, source, why string, objects ObjectStore, budget *ByteBudget, count *passCounter) (objectResult, error) {
	key := strings.TrimSpace(row.ObjectKey)
	missing := func(how string) (objectResult, error) {
		return objectResult{
			check:   Check{Method: MethodMissing, Reason: how + " answered not found"},
			finding: objectFinding(KindObjectMissing, row.Name, "uploaded object is missing", "key="+key),
		}, nil
	}
	size, err := objects.Head(ctx, key)
	if errors.Is(err, os.ErrNotExist) {
		return missing("HEAD")
	}
	if err != nil {
		return objectResult{}, err
	}
	if row.Size > 0 && size != row.Size {
		return objectResult{
			check:   Check{Method: MethodHead, Reason: "size differs from the catalog row; body not read"},
			finding: objectFinding(KindObjectSizeMismatch, row.Name, "object size differs from the catalog row", fmt.Sprintf("object=%d row=%d", size, row.Size)),
		}, nil
	}
	if digest == "" {
		return objectResult{check: Check{Method: MethodUnverifiable, Reason: "no digest: catalog sha256 is empty and there is no clean local parse"}}, nil
	}
	if why == "" {
		return objectResult{check: Check{Method: MethodHead, Reason: "not in this period's rotation slot"}}, nil
	}
	body, err := objects.Open(ctx, key)
	if errors.Is(err, os.ErrNotExist) {
		return missing("GET")
	}
	if err != nil {
		return objectResult{}, err
	}
	defer body.Close()
	sum, err := hashReader(ctx, body, budget, &count.object)
	if errors.Is(err, os.ErrNotExist) {
		return missing("GET")
	}
	if err != nil {
		return objectResult{}, err
	}
	res := objectResult{check: Check{Method: MethodHashed, Reason: why, Digest: source}}
	if !strings.EqualFold(sum, digest) {
		res.finding = objectFinding(KindObjectCorrupt, row.Name, "object sha256 differs", fmt.Sprintf("object=%s want=%s source=%s", sum, digest, source))
	}
	return res, nil
}

func objectFinding(kind, name, detail, evidence string) *Finding {
	return &Finding{Kind: kind, Subject: name, Severity: SeverityCritical, Detail: detail, Evidence: evidence}
}

func findingFromViolation(v Violation) Finding {
	sev := SeverityCritical
	if v.Kind == I4EmptyLeftover {
		sev = SeverityInfo
	}
	subject := ""
	if len(v.Segments) > 0 {
		subject = v.Segments[0]
	}
	return Finding{Kind: string(v.Kind), Subject: subject, Severity: sev, Detail: v.Detail, Evidence: v.Evidence}
}

func dedupe(in []Finding) []Finding {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Kind != in[j].Kind {
			return in[i].Kind < in[j].Kind
		}
		if in[i].Subject != in[j].Subject {
			return in[i].Subject < in[j].Subject
		}
		return in[i].Evidence < in[j].Evidence
	})
	out := in[:0]
	prev := ""
	for _, f := range in {
		key := f.Kind + "\x00" + f.Subject
		if key == prev {
			continue
		}
		prev = key
		out = append(out, f)
	}
	return out
}

// budgetReader paces reads on the shared budget and counts them for this pass.
type budgetReader struct {
	ctx context.Context
	r   io.Reader
	b   *ByteBudget
	n   *int64
	err error
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if err := b.ctx.Err(); err != nil {
		b.err = err
		return 0, err
	}
	n, err := b.r.Read(p)
	*b.n += int64(n)
	if n > 0 {
		if werr := b.b.Wait(b.ctx, int64(n)); werr != nil {
			b.err = werr
			return n, werr
		}
	}
	return n, err
}

func hashReader(ctx context.Context, r io.Reader, budget *ByteBudget, n *int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, &budgetReader{ctx: ctx, r: r, b: budget, n: n}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
