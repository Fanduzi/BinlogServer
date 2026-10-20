// Package assurance provides module-level functionality for assurance.
// input: catalog rows (with sealed_at and uploaded_at), a task directory of sealed segments, an object store that can HEAD and GET, an optional Chain, the findings open before this pass, each segment's last_verified_at and last ReadMark, a byte budget and the recent tier's share of it (1–100)
// output: a Report whose Coverage records, per segment, what this pass did to the local copy and to the object (parsed, hashed, HEAD only, skipped, error) and why; the findings it wants open; the open findings it closes and why; the segments whose last_verified_at may advance (content-verified and named by no finding other than UPLOAD_CLOCK_SKEW); a ReadMark per segment read in full (fed back so a forced segment is read once per period); paid bytes; counts of objects with no digest or no comparable size
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
	// DefaultRotationPeriods is the rotation length: every segment has one
	// slot in each run of this many periods. The worst gap between two
	// content checks of a segment with spare budget is RotationPeriods x
	// Period (20 x 24h by default); WorstGap computes it.
	DefaultRotationPeriods = 20
	// DefaultRecent is the window in which every upload is checked by content.
	DefaultRecent = 24 * time.Hour
	// DefaultRecentSharePercent is the most of one pass's capacity the
	// recent tier may spend (design §5.3), further capped so the slot and
	// overdue tiers keep room for one segment. Recent work past it falls back
	// to the rotation slot, the overdue tier, or the tail tier that is paid
	// only from what the pass has left.
	DefaultRecentSharePercent = 25
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
	// KindUploadClockSkew is a row whose uploaded_at (UPLOADED rows) or
	// sealed_at is more than ClockSkewTolerance in the future.
	KindUploadClockSkew = "UPLOAD_CLOCK_SKEW"
	// KindLocalUnreadable is a local segment file that exists but could not
	// be read (stat, open or read failed with something other than not-found,
	// a deadline or a cancel). WARNING, CRITICAL when the row is not uploaded
	// and the file is the only copy.
	KindLocalUnreadable = "LOCAL_UNREADABLE"

	SeverityCritical = "CRITICAL"
	SeverityWarning  = "WARNING"
	SeverityInfo     = "INFO"
)

// Closing reasons (Closure.Reason).
const (
	ReasonParsed         = "local parsed and matches the catalog row"
	ReasonLocalRemoved   = "local removed; object matches catalog"
	ReasonObjectParsed   = "local removed; object parses and matches the catalog row"
	ReasonLocalPresent   = "local file present"
	ReasonObjectFound    = "object found by HEAD"
	ReasonSizeCompared   = "object size compared"
	ReasonHashed         = "object hashed and matches"
	ReasonClockChecked   = "uploaded_at compared with now"
	ReasonChainChecked   = "chain checked"
	ReasonRowRemoved     = "row removed"
	ReasonLocalRead      = "local file read to the end"
	ReasonLocalGone      = "local file no longer present"
	reasonTail           = "not verified since its last change, past the recent share or window; paid from what the pass had left"
	reasonHeldForced     = "forced recheck already done this period and nothing changed since"
	reasonNotScheduled   = "not scheduled for a content check this pass"
	reasonDeferredBudget = "deferred: does not fit in what is left of this pass's budget"
	reasonDeferredTime   = "deferred: the pass deadline ran out"
)

// VERIFY_FAILED details. A finding on the object, the copy left once local
// retention removed the file, carries DetailObjectPrefix before the detail.
const (
	DetailParse        = "sealed segment did not parse"
	DetailSHA          = "sha256 differs from the catalog row"
	DetailPositions    = "event positions differ from the catalog row"
	DetailGTID         = "gtid set differs from the catalog row"
	DetailSize         = "size differs from the catalog row"
	DetailObjectPrefix = "object (sole copy): "
)

// Method is what one pass did to one copy of a segment.
type Method string

const (
	// MethodParsed: the local file was read end to end (magic, event
	// lengths, CRC32, GTID events, positions), hashed, and compared with the row.
	MethodParsed Method = "PARSED"
	// MethodStat: the local file exists and its size was compared with the
	// row; it was not parsed (the size differs, or it was not scheduled).
	MethodStat Method = "STAT"
	// MethodAbsent: there is no local file and none is required (the row is
	// UPLOADED, or local retention removed it).
	MethodAbsent Method = "ABSENT"
	// MethodMissing: a copy that must exist does not (local: sealed and not
	// UPLOADED; object: HEAD or GET answered not found).
	MethodMissing Method = "MISSING"
	// MethodHashed: the object body was read and its SHA-256 compared with a digest.
	MethodHashed Method = "HASHED"
	// MethodHead: the object exists; the body was not read.
	MethodHead Method = "HEAD"
	// MethodUnverifiable: the object exists, but there is no digest to compare
	// a body with (empty catalog sha256 and no clean local parse). Never
	// counts as verified.
	MethodUnverifiable Method = "UNVERIFIABLE"
	// MethodNotApplicable: the row is not UPLOADED, so there is no object to check.
	MethodNotApplicable Method = "NOT_APPLICABLE"
	// MethodError: a read failed with something other than not-found.
	MethodError Method = "ERROR"
	// MethodSkipped: this copy was not looked at, or its scheduled content
	// check was deferred (budget or deadline); Reason says why.
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
	// Found is true when this pass saw the copy exist (stat or HEAD answered).
	Found bool
	// SizeChecked is true when this pass compared the copy's size: with the
	// catalog row (row size known), or, for an object whose row size is
	// unknown, by a hash that matched the local file.
	SizeChecked bool
	// Parsed is set on an object HASHED against the catalog digest whose
	// bytes were also parsed while hashing and matched the row's positions
	// and GTID set. Only an open VERIFY_FAILED with no local file asks for it.
	Parsed bool
}

// Coverage is what one pass established about one segment.
type Coverage struct {
	Local  Check
	Object Check
	// ClockChecked is true when sealed_at, and for an UPLOADED row
	// uploaded_at, were compared with now.
	ClockChecked bool
}

// ReadMark is what a segment looked like when a pass last read every copy
// it scheduled. The caller feeds Report.Read back as Options.LastRead so a
// forced segment is re-read at most once per period unless its local size,
// local mtime or object size changed (design §6.3).
type ReadMark struct {
	At           time.Time
	LocalSize    int64
	LocalModTime time.Time
	ObjectSize   int64
}

// ObjectStore is the object half of one pass. A missing object is os.ErrNotExist.
// Any other error leaves that object unverified.
type ObjectStore interface {
	Head(ctx context.Context, key string) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// VerifyRow is one catalog row. Name is the on-disk segment name.
// GTIDSet is optional: the catalog has no such column, and an empty value is not compared.
// Size, StartPos and EndPos of 0 are unknown and are not compared.
// RetentionRemoved is the design 0009 §26 marker: local retention removed the file.
type VerifyRow struct {
	Name        string
	Sealed      bool
	Size        int64
	StartPos    uint32
	EndPos      uint32
	SHA256      string
	GTIDSet     string
	UploadState string
	ObjectKey   string
	UploadedAt  time.Time
	// SealedAt is the row's last change before upload (sealed_at); zero is unknown.
	SealedAt         time.Time
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

// Closure is an open finding a pass is positive evidence against, and why.
type Closure struct {
	Finding Finding
	Reason  string
}

// Report is one pass.
//
// Coverage has an entry for every sealed row the pass reached, and each
// copy's Method says what was done. Closing a finding and advancing
// last_verified_at read only Coverage, so nothing the pass did not check can
// close or advance.
type Report struct {
	Findings []Finding
	// Closed is Closures(Options.OpenFindings, this report).
	Closed     []Closure
	Coverage   map[string]Coverage
	VerifiedAt map[string]time.Time
	// Read is a ReadMark for each segment this pass read every scheduled
	// copy of (at least one by content). Feed it back as Options.LastRead.
	Read map[string]ReadMark
	// ChainChecked is true when Chain.Check ran on a fully listed directory.
	ChainChecked bool
	// CatalogComplete is Options.CatalogComplete: rows was the whole catalog.
	CatalogComplete bool
	// Bytes is what this pass paid for: LocalBytes plus ObjectBytes. With a
	// budget it equals what this pass charged to it.
	Bytes       int64
	LocalBytes  int64
	ObjectBytes int64
	// Deferred counts scheduled content checks (one per copy) this pass did
	// not do because the budget or the deadline ran out.
	Deferred int
	// UnverifiableNoDigest counts UPLOADED rows this pass had no digest for:
	// the catalog sha256 is empty and the local file is gone or did not parse
	// cleanly. Their objects get HEAD only.
	UnverifiableNoDigest int
	// UnverifiableSize counts objects found whose size this pass could not
	// compare: the catalog size is unknown and no hash matched a local file.
	UnverifiableSize int
	// NoDigestLocal counts sealed rows with an empty catalog sha256 whose local
	// file still exists (rows sealed before migration 000008). A digest can be
	// backfilled from that file now; after retention it cannot.
	NoDigestLocal int
	LocalErr      error
	ObjectErr     error

	catalog map[string]bool
}

// Options schedules one pass.
//
// RotationPeriods is the rotation length in periods; 0 means
// DefaultRotationPeriods, negative turns the rotation and the overdue queue
// off. Recent and Period of 0 mean the defaults. A nil Budget is unlimited.
// PassBytes > 0 caps what one pass reads. OpenFindings are the findings open
// before this pass. LastVerified is each segment's last_verified_at (missing
// means never). CatalogComplete says rows is the whole catalog of the task,
// so a subject with no row has had its row removed. RecentSharePercent caps
// what the recent tier may spend of the pass's capacity: 0 means
// DefaultRecentSharePercent, 1–100 is a percentage, anything else makes
// Verify return an error (CheckRecentSharePercent). LastRead is the previous
// passes' Report.Read.
type Options struct {
	Now                time.Time
	RotationPeriods    int
	Recent             time.Duration
	RecentSharePercent int
	Period             time.Duration
	Budget             *ByteBudget
	PassBytes          int64
	OpenFindings       []Finding
	LastVerified       map[string]time.Time
	LastRead           map[string]ReadMark
	CatalogComplete    bool
}

// CheckRecentSharePercent rejects a recent share outside 1–100. 0 is the
// default and is accepted here; a config loader should reject it too, since
// the key has a default and 0 would read as "off".
func CheckRecentSharePercent(pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("verify: recent share percent %d is outside 1-100", pct)
	}
	return nil
}

// ErrBudgetDeadline is a budget wait that cannot finish before the context
// deadline. The copy being read is deferred; the pass goes on with work that fits.
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

// Wait blocks until n bytes are allowed. n larger than the burst is split,
// and each step is charged when it is allowed.
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

// chunk is the most one budgeted read may take, so a read is paid in one step.
func (b *ByteBudget) chunk() int {
	if b == nil {
		return 0
	}
	return b.lim.Burst()
}

// fits reports whether n bytes can be paid before deadline at the current
// rate. Without a budget or a deadline it cannot tell and says yes.
func (b *ByteBudget) fits(n int64, deadline time.Time, ok bool) bool {
	if b == nil || !ok {
		return true
	}
	now := time.Now()
	need := float64(n) - b.lim.TokensAt(now)
	if need <= 0 {
		return true
	}
	wait := time.Duration(need / float64(b.lim.Limit()) * float64(time.Second))
	return !now.Add(wait).After(deadline)
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

// rotationLength is rotation with the default applied; 0 means off.
func rotationLength(rotation int) int {
	if rotation == 0 {
		return DefaultRotationPeriods
	}
	if rotation < 0 {
		return 0
	}
	return rotation
}

func periodOf(period time.Duration) time.Duration {
	if period < time.Second {
		return DefaultPeriod
	}
	return period
}

// WorstGap is the longest a segment waits for its slot: rotation x period,
// with the defaults applied. 0 when the rotation is off.
func WorstGap(rotation int, period time.Duration) time.Duration {
	return time.Duration(rotationLength(rotation)) * periodOf(period)
}

// InRotation reports whether name's slot is the current period. Each name
// has one fixed slot, sha256(name) mod rotation, so it is drawn exactly once
// in every run of rotation periods. rotation 0 is the default; negative is off.
func InRotation(name string, now time.Time, period time.Duration, rotation int) bool {
	n := rotationLength(rotation)
	if n == 0 {
		return false
	}
	slot := uint64(now.Unix()) / uint64(periodOf(period)/time.Second)
	sum := sha256.Sum256([]byte(name))
	h := binary.BigEndian.Uint64(sum[:8])
	return h%uint64(n) == slot%uint64(n)
}

// Scheduling tiers, best first (design §5.2).
const (
	tierForced = iota
	tierRecent
	tierSlot
	tierOverdue
	tierTail // recent work past the share whose fallback is none: leftover budget only
	tierNone
)

// passCounter counts the bytes one pass paid for. One pass runs on one goroutine.
type passCounter struct {
	local  int64
	object int64
}

func (c *passCounter) total() int64 { return c.local + c.object }

// seg is one sealed row during a pass.
type seg struct {
	row      VerifyRow
	cov      Coverage
	path     string
	size     int64 // local size from stat
	mtime    time.Time
	parse    bool  // the local file may be parsed
	objSize  int64 // object size from HEAD
	hash     bool  // the object may be hashed
	localSHA string
	last     time.Time
	tier     int
	demoted  bool // its recent work went past the recent share
	held     bool // forced, but already read this period with nothing changed
}

// work is one scheduled content check: a local parse or an object hash.
type work struct {
	s     *seg
	local bool
	tier  int
}

// pass is one Verify call.
type pass struct {
	ctx      context.Context
	now      time.Time
	opt      Options
	objects  ObjectStore
	rep      *Report
	count    passCounter
	deadline time.Time
	hasDL    bool
	forced   map[string][]string
	named    map[string]bool
}

// Verify checks sealed segments under dir against rows, objects and chain.
// The caller must already be the owner of this task. A store of nil leaves
// every object SKIPPED. A chain of nil leaves the chain unchecked.
//
// The pass is free work first (stat every local file, HEAD every object,
// the chain), then content checks from one queue in priority order: forced
// (at most once per period with Options.LastRead), recent (not
// content-verified since the last change; capped at min(share × C,
// C − F − S)), rotation slot, overdue by oldest last_verified_at, and a tail
// paid from what is left (recent overflow, and verified rows not verified
// since an upload outside the recent window). When the budget
// or the deadline runs out, what is left is SKIPPED and the pass ends
// normally. Only a cancelled context returns an error.
func Verify(ctx context.Context, dir string, rows []VerifyRow, objects ObjectStore, chain *Chain, opt Options) (rep Report, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := CheckRecentSharePercent(opt.RecentSharePercent); err != nil {
		return Report{}, err
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rep = Report{Coverage: map[string]Coverage{}, VerifiedAt: map[string]time.Time{}, Read: map[string]ReadMark{}, CatalogComplete: opt.CatalogComplete, catalog: map[string]bool{}}
	for _, row := range rows {
		rep.catalog[row.Name] = true
	}
	p := &pass{ctx: ctx, now: now, opt: opt, objects: objects, rep: &rep}
	p.deadline, p.hasDL = ctx.Deadline()
	defer func() { rep.finish(now, &p.count, opt.OpenFindings) }()
	if cancelled(ctx) {
		return rep, ctx.Err()
	}
	p.forced, p.named = openSubjects(opt.OpenFindings)

	segs, localClean, err := p.free(dir, sealedRows(rows))
	if err != nil {
		return rep, err
	}
	if chain != nil && localClean && ctx.Err() == nil {
		for _, v := range chain.Check(now) {
			rep.Findings = append(rep.Findings, findingFromViolation(v))
		}
		rep.ChainChecked = true
	}
	if err := p.content(segs); err != nil {
		return rep, err
	}
	p.mark(segs)
	return rep, nil
}

// mark records a ReadMark for every segment whose scheduled copies were all
// read to a conclusion this pass, at least one by content.
func (p *pass) mark(segs []*seg) {
	for _, s := range segs {
		l, o := s.cov.Local.Method, s.cov.Object.Method
		local := !s.parse || l == MethodParsed
		object := !s.hash || o == MethodHashed || o == MethodUnverifiable || o == MethodMissing
		if local && object && (l == MethodParsed || o == MethodHashed) {
			p.rep.Read[s.row.Name] = ReadMark{At: p.now, LocalSize: s.size, LocalModTime: s.mtime, ObjectSize: s.objSize}
		}
	}
}

// cancelled is a context cancelled by its owner. A passed deadline is not:
// it ends the pass normally.
func cancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// outOfTime is a passed deadline, or a budget wait that cannot finish before it.
func (p *pass) outOfTime(err error) bool {
	return errors.Is(p.ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrBudgetDeadline)
}

// free is everything that costs no budget: list and stat the local files,
// HEAD every object. It fills Coverage with what is known without reading
// a body and marks what may be read.
func (p *pass) free(dir string, rows []VerifyRow) ([]*seg, bool, error) {
	rep := p.rep
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
	timeUp := false
	var segs []*seg
	for _, row := range rows {
		s := &seg{row: row, path: filepath.Join(dir, row.Name), last: p.opt.LastVerified[row.Name]}
		noDigest := strings.TrimSpace(row.SHA256) == ""
		switch {
		case !listed:
			s.cov.Local = Check{Method: MethodSkipped, Reason: "task directory not listed: " + listErr.Error()}
		case !present[row.Name]:
			s.cov.Local = absentLocal(row)
			if s.cov.Local.Method == MethodMissing {
				rep.Findings = append(rep.Findings, Finding{Kind: KindLocalMissing, Subject: row.Name, Severity: SeverityCritical,
					Detail: "sealed segment is not UPLOADED and has no local file", Evidence: "upload_state=" + strings.TrimSpace(row.UploadState)})
			}
		default:
			info, statErr := os.Stat(s.path)
			if statErr != nil {
				s.cov.Local = Check{Method: MethodError, Reason: statErr.Error()}
				localClean = false
				if rep.LocalErr == nil {
					rep.LocalErr = statErr
				}
				p.unreadable(row, statErr)
				break
			}
			s.mtime = info.ModTime()
			if noDigest {
				rep.NoDigestLocal++
			}
			s.size = info.Size()
			s.cov.Local = Check{Method: MethodStat, Reason: reasonNotScheduled, Found: true, SizeChecked: row.Size > 0}
			if row.Size > 0 && s.size != row.Size {
				s.cov.Local.Reason = "size differs from the catalog row; not parsed"
				rep.Findings = append(rep.Findings, *localFinding(row.Name, DetailSize, fmt.Sprintf("local=%d row=%d", s.size, row.Size)))
				break
			}
			s.parse = true
		}

		if uploaded(row) && noDigest && !s.parse {
			rep.UnverifiableNoDigest++
		}
		if ClockSkewed(row.SealedAt, p.now) {
			rep.Findings = append(rep.Findings, Finding{Kind: KindUploadClockSkew, Subject: row.Name, Severity: SeverityWarning,
				Detail:   "sealed_at is in the future; it is not used for the recent tier",
				Evidence: fmt.Sprintf("sealed_at=%s now=%s", row.SealedAt.UTC().Format(time.RFC3339Nano), p.now.UTC().Format(time.RFC3339Nano))})
		}
		s.cov.ClockChecked = true
		switch {
		case !uploaded(row):
			s.cov.Object = Check{Method: MethodNotApplicable, Reason: "upload_state=" + strings.TrimSpace(row.UploadState)}
		default:
			if ClockSkewed(row.UploadedAt, p.now) {
				rep.Findings = append(rep.Findings, Finding{Kind: KindUploadClockSkew, Subject: row.Name, Severity: SeverityWarning,
					Detail:   "uploaded_at is in the future; the row is not treated as recent",
					Evidence: fmt.Sprintf("uploaded_at=%s now=%s", row.UploadedAt.UTC().Format(time.RFC3339Nano), p.now.UTC().Format(time.RFC3339Nano))})
			}
			if p.objects == nil {
				s.cov.Object = Check{Method: MethodSkipped, Reason: "no object store"}
				break
			}
			if timeUp {
				s.cov.Object = Check{Method: MethodSkipped, Reason: reasonDeferredTime}
				break
			}
			if err := p.head(s, noDigest); err != nil {
				if cancelled(p.ctx) {
					segs = append(segs, s)
					p.store(segs)
					return nil, false, p.ctx.Err()
				}
				if p.outOfTime(err) {
					timeUp = true
					s.cov.Object = Check{Method: MethodSkipped, Reason: reasonDeferredTime}
					break
				}
				s.cov.Object = Check{Method: MethodError, Reason: err.Error()}
				if rep.ObjectErr == nil {
					rep.ObjectErr = err
				}
			}
		}
		segs = append(segs, s)
		rep.Coverage[row.Name] = s.cov
	}
	return segs, localClean, nil
}

func (p *pass) store(segs []*seg) {
	for _, s := range segs {
		p.rep.Coverage[s.row.Name] = s.cov
	}
}

// head HEADs one object and records what a HEAD alone can tell.
func (p *pass) head(s *seg, noDigest bool) error {
	row := s.row
	key := strings.TrimSpace(row.ObjectKey)
	size, err := p.objects.Head(p.ctx, key)
	if errors.Is(err, os.ErrNotExist) {
		s.cov.Object = Check{Method: MethodMissing, Reason: "HEAD answered not found"}
		p.rep.Findings = append(p.rep.Findings, *objectFinding(KindObjectMissing, row.Name, "uploaded object is missing", "key="+key))
		return nil
	}
	if err != nil {
		return err
	}
	s.objSize = size
	s.cov.Object = Check{Method: MethodHead, Reason: reasonNotScheduled, Found: true, SizeChecked: row.Size > 0}
	if row.Size > 0 && size != row.Size {
		s.cov.Object.Reason = "size differs from the catalog row; body not read"
		p.rep.Findings = append(p.rep.Findings, *objectFinding(KindObjectSizeMismatch, row.Name, "object size differs from the catalog row", fmt.Sprintf("object=%d row=%d", size, row.Size)))
		return nil
	}
	if noDigest && !s.parse {
		s.cov.Object.Method = MethodUnverifiable
		s.cov.Object.Reason = "no digest: catalog sha256 is empty and there is no local file to parse"
		return nil
	}
	s.hash = true
	return nil
}

// tierOf places a segment in the content queue (design §5.2).
func (p *pass) tierOf(s *seg) int {
	if p.forcedBy(s) {
		if p.readThisPeriod(s) {
			s.held = true
			return tierNone
		}
		return tierForced
	}
	if p.named[s.row.Name] {
		return p.fallback(s)
	}
	if p.fresh(s) {
		return tierRecent
	}
	// Verified once, but not since an upload that is now outside the
	// recent window (a backlog the recent share could not take in time):
	// the tail, paid only from what the pass has left (QA r4 G1). A row
	// never verified is the overdue tier's, and stays unread when the
	// rotation is off.
	if t := p.fallback(s); t != tierNone || s.last.IsZero() || !p.stale(s) {
		return t
	}
	return tierTail
}

// readThisPeriod is a forced segment already read in this rotation period
// (Options.LastRead) whose local size, local mtime and object size are what
// that read saw: reading it again would read the same bytes (design §6.3).
func (p *pass) readThisPeriod(s *seg) bool {
	m, ok := p.opt.LastRead[s.row.Name]
	return ok && !m.At.Before(slotStart(p.now, p.opt.Period)) && m.LocalSize == s.size && m.LocalModTime.Equal(s.mtime) && m.ObjectSize == s.objSize
}

// fresh is the recent tier: a row not content-verified since its last
// change (sealed, or uploaded) that is not uploaded yet, or was uploaded
// within the recent window. Once verified it leaves the tier. A sealed_at or
// uploaded_at more than ClockSkewTolerance in the future is clock skew
// (UPLOAD_CLOCK_SKEW) and is not a change.
func (p *pass) fresh(s *seg) bool {
	return p.stale(s) && (!uploaded(s.row) || RecentUpload(s.row.UploadedAt, p.now, p.opt.Recent))
}

// stale is a row not content-verified since its last change: sealed_at or
// uploaded_at, whichever is later and not clock skew.
func (p *pass) stale(s *seg) bool {
	row := s.row
	var changed time.Time
	if !ClockSkewed(row.SealedAt, p.now) {
		changed = row.SealedAt
	}
	if !ClockSkewed(row.UploadedAt, p.now) && row.UploadedAt.After(changed) {
		changed = row.UploadedAt
	}
	return s.last.IsZero() || s.last.Before(changed)
}

// fallback is the tier below recent: the rotation slot once per slot
// period, then overdue by last_verified_at. A segment an open finding names
// gets the slot only.
func (p *pass) fallback(s *seg) int {
	row := s.row
	if InRotation(row.Name, p.now, p.opt.Period, p.opt.RotationPeriods) && s.last.Before(slotStart(p.now, p.opt.Period)) {
		return tierSlot
	}
	if p.named[row.Name] {
		return tierNone
	}
	n := rotationLength(p.opt.RotationPeriods)
	if n > 0 && (s.last.IsZero() || p.now.Sub(s.last) >= time.Duration(n)*periodOf(p.opt.Period)) {
		return tierOverdue
	}
	return tierNone
}

// slotStart is the start of the rotation period that holds now.
func slotStart(now time.Time, period time.Duration) time.Time {
	sec := int64(periodOf(period) / time.Second)
	return time.Unix(now.Unix()/sec*sec, 0)
}

// queue is the content work in the order it is paid for.
func (p *pass) queue(segs []*seg) []work {
	var q []work
	for _, s := range segs {
		s.tier = p.tierOf(s)
		if s.held {
			if s.cov.Local.Method == MethodStat && s.parse {
				s.cov.Local.Reason = reasonHeldForced
			}
			if s.cov.Object.Method == MethodHead && s.hash {
				s.cov.Object.Reason = reasonHeldForced
			}
			p.rep.Coverage[s.row.Name] = s.cov
		}
		if s.tier == tierNone {
			continue
		}
		if s.parse {
			q = append(q, work{s: s, local: true, tier: s.tier})
		}
		if s.hash {
			q = append(q, work{s: s, tier: s.tier})
		}
	}
	sort.SliceStable(q, func(i, j int) bool { return before(q[i], q[j]) })
	return q
}

func before(a, b work) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	if !a.s.last.Equal(b.s.last) {
		return a.s.last.Before(b.s.last)
	}
	if a.s.row.Name != b.s.row.Name {
		return a.s.row.Name < b.s.row.Name
	}
	return a.local && !b.local
}

// content pays for the queue in order. Work that does not fit is deferred
// and the next, smaller work is tried; once the deadline has passed the rest
// is deferred. Recent work past the recent share drops to its fallback tier,
// or to the tail tier when it has none. A cancelled context stops the pass
// with its error.
func (p *pass) content(segs []*seg) error {
	q := p.queue(segs)
	share := p.recentShare(q)
	var recent int64
	for i := 0; i < len(q); i++ {
		w := q[i]
		if cancelled(p.ctx) {
			return p.ctx.Err()
		}
		if p.ctx.Err() != nil {
			for _, rest := range q[i:] {
				p.defer1(rest, reasonDeferredTime)
			}
			return nil
		}
		size := w.s.size
		if !w.local {
			size = w.s.objSize
		}
		if w.tier == tierRecent && (w.s.demoted || share >= 0 && recent+size > share) {
			// The segment's remaining recent work drops together, so an
			// object hash never runs ahead of the local parse it needs.
			w.s.demoted = true
			w.tier = p.fallback(w.s)
			if w.tier == tierNone {
				w.tier = tierTail
			}
			rest := q[i+1:]
			at := i + 1 + sort.Search(len(rest), func(j int) bool { return before(w, rest[j]) })
			q = append(q[:at], append([]work{w}, q[at:]...)...)
			continue
		}
		if !p.fits(size) {
			p.defer1(w, reasonDeferredBudget)
			continue
		}
		paid := p.count.total()
		var err error
		if w.local {
			err = p.parseLocal(w)
		} else {
			err = p.hashObject(w)
		}
		if err != nil {
			return err
		}
		if w.tier == tierRecent {
			recent += p.count.total() - paid
		}
	}
	return nil
}

// recentShare is R, the most the recent tier may spend this pass (design
// §5.3): min(⌊C × share⌋, max(0, C − F − S)), where C is the lesser of
// PassBytes and what the budget can pay before the deadline, F the forced
// work and S the largest other segment that fits in C − F. So whenever
// C − F ≥ S the slot and overdue tiers keep room for one segment. -1 when
// neither PassBytes nor the budget limits the pass.
func (p *pass) recentShare(q []work) int64 {
	c := int64(-1)
	if p.opt.PassBytes > 0 {
		c = p.opt.PassBytes
	}
	if b := p.opt.Budget; b != nil && p.hasDL {
		now := time.Now()
		left := max(p.deadline.Sub(now), 0)
		if bc := int64(b.lim.TokensAt(now) + float64(b.lim.Limit())*left.Seconds()); c < 0 || bc < c {
			c = bc
		}
	}
	if c < 0 {
		return -1
	}
	pct := p.opt.RecentSharePercent
	if pct == 0 {
		pct = DefaultRecentSharePercent
	}
	var f int64
	cost := map[*seg]int64{}
	for _, w := range q {
		size := w.s.size
		if !w.local {
			size = w.s.objSize
		}
		if w.tier == tierForced {
			f += size
		} else {
			cost[w.s] += size
		}
	}
	var s int64
	for _, n := range cost {
		if n <= c-f && n > s {
			s = n
		}
	}
	return min(c*int64(pct)/100, max(0, c-f-s))
}

func (p *pass) fits(size int64) bool {
	if p.opt.PassBytes > 0 && p.count.total()+size > p.opt.PassBytes {
		return false
	}
	return p.opt.Budget.fits(size, p.deadline, p.hasDL)
}

func (p *pass) defer1(w work, reason string) {
	p.rep.Deferred++
	c := &w.s.cov.Object
	if w.local {
		c = &w.s.cov.Local
	}
	c.Method, c.Reason = MethodSkipped, reason
	p.rep.Coverage[w.s.row.Name] = w.s.cov
}

// failed sorts a read error: cancel stops the pass, a deadline defers this
// copy, anything else leaves it ERROR.
func (p *pass) failed(w work, err error) error {
	if cancelled(p.ctx) {
		return p.ctx.Err()
	}
	if p.outOfTime(err) {
		p.defer1(w, reasonDeferredTime)
		return nil
	}
	c := &w.s.cov.Object
	if w.local {
		c = &w.s.cov.Local
		if p.rep.LocalErr == nil {
			p.rep.LocalErr = err
		}
		p.unreadable(w.s.row, err)
	} else if p.rep.ObjectErr == nil {
		p.rep.ObjectErr = err
	}
	c.Method, c.Reason = MethodError, err.Error()
	p.rep.Coverage[w.s.row.Name] = w.s.cov
	return nil
}

// unreadable reports a local file that exists but cannot be read. A file
// that vanished is not unreadable: the next pass sees it absent.
func (p *pass) unreadable(row VerifyRow, err error) {
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	sev := SeverityWarning
	if !uploaded(row) {
		sev = SeverityCritical
	}
	p.rep.Findings = append(p.rep.Findings, Finding{Kind: KindLocalUnreadable, Subject: row.Name, Severity: sev,
		Detail: "local segment file could not be read", Evidence: err.Error()})
}

func (p *pass) parseLocal(w work) error {
	s := w.s
	res, err := checkLocal(p.ctx, s.path, s.row, s.size, p.opt.Budget, &p.count.local)
	if err != nil {
		return p.failed(w, err)
	}
	res.check.Found, res.check.SizeChecked = true, s.row.Size > 0
	s.cov.Local = res.check
	if res.finding != nil {
		p.rep.Findings = append(p.rep.Findings, *res.finding)
	} else {
		s.localSHA = res.sum
	}
	p.rep.Coverage[s.row.Name] = s.cov
	return nil
}

func (p *pass) hashObject(w work) error {
	s := w.s
	row := s.row
	digest, source := s.localSHA, "local"
	if digest == "" {
		digest, source = strings.TrimSpace(row.SHA256), "catalog"
	}
	if digest == "" {
		if s.cov.Local.Method == MethodSkipped {
			p.defer1(w, "deferred: no digest this pass, the local parse was deferred")
			return nil
		}
		s.cov.Object.Method = MethodUnverifiable
		s.cov.Object.Reason = "no digest: catalog sha256 is empty and the local file did not parse cleanly"
		p.rep.UnverifiableNoDigest++
		p.rep.Coverage[row.Name] = s.cov
		return nil
	}
	// An open VERIFY_FAILED with no local file left can only close on the
	// object: parse it while hashing, from the same bytes (design §6.2).
	parse := source == "catalog" && s.cov.Local.Method == MethodAbsent && p.openKind(row.Name, KindVerifyFailed)
	key := strings.TrimSpace(row.ObjectKey)
	body, err := p.objects.Open(p.ctx, key)
	if err == nil {
		var rd segRead
		rd, err = readSegment(p.ctx, body, s.objSize, parse, p.opt.Budget, &p.count.object)
		body.Close()
		if err == nil && rd.n < s.objSize {
			// A body that ends cleanly before the HEAD size is a cut-short
			// read, not different bytes.
			err = fmt.Errorf("object body ended after %d of %d bytes (HEAD): %w", rd.n, s.objSize, io.ErrUnexpectedEOF)
		}
		if err == nil {
			s.cov.Object = Check{Method: MethodHashed, Reason: tierReason(w.tier), Digest: source, Found: true,
				SizeChecked: s.cov.Object.SizeChecked}
			switch {
			case !strings.EqualFold(rd.sum, digest):
				p.rep.Findings = append(p.rep.Findings, *objectFinding(KindObjectCorrupt, row.Name, "object sha256 differs", fmt.Sprintf("object=%s want=%s source=%s", rd.sum, digest, source)))
			case source == "local":
				s.cov.Object.SizeChecked = true
			case parse:
				if detail, evidence := mismatch(row, rd, false); detail != "" {
					p.rep.Findings = append(p.rep.Findings, *localFinding(row.Name, DetailObjectPrefix+detail, evidence))
				} else {
					s.cov.Object.Parsed = true
				}
			}
			p.rep.Coverage[row.Name] = s.cov
			return nil
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		s.cov.Object = Check{Method: MethodMissing, Reason: "GET answered not found"}
		p.rep.Findings = append(p.rep.Findings, *objectFinding(KindObjectMissing, row.Name, "uploaded object is missing", "key="+key))
		p.rep.Coverage[row.Name] = s.cov
		return nil
	}
	return p.failed(w, err)
}

// openKind is whether a finding of kind on subject was open before this pass.
func (p *pass) openKind(subject, kind string) bool {
	for _, k := range p.forced[subject] {
		if k == kind {
			return true
		}
	}
	return false
}

func tierReason(tier int) string {
	switch tier {
	case tierForced:
		return "an open finding forces a recheck"
	case tierRecent:
		return "uploaded within the recent window, or not uploaded yet"
	case tierSlot:
		return "rotation slot"
	case tierTail:
		return reasonTail
	default:
		return "overdue: oldest last_verified_at"
	}
}

// finish sorts and merges findings, totals this pass's bytes, closes what it
// can, and derives VerifiedAt from Coverage. It runs on every return.
func (rep *Report) finish(now time.Time, count *passCounter, open []Finding) {
	rep.Findings = dedupe(rep.Findings)
	rep.LocalBytes = count.local
	rep.ObjectBytes = count.object
	rep.Bytes = count.total()
	rep.Closed = Closures(open, *rep)
	for _, c := range rep.Coverage {
		if c.Object.Found && !c.Object.SizeChecked {
			rep.UnverifiableSize++
		}
	}
	// UPLOAD_CLOCK_SKEW is about timestamps, not bytes: it does not hold
	// back last_verified_at (QA r4 G5).
	named := map[string]bool{}
	for _, f := range rep.Findings {
		if f.Kind != KindUploadClockSkew {
			named[f.Subject] = true
		}
	}
	closed := map[string]bool{}
	for _, c := range rep.Closed {
		closed[c.Finding.Kind+"\x00"+c.Finding.Subject] = true
	}
	for _, f := range open {
		if f.Kind != KindUploadClockSkew && !closed[f.Kind+"\x00"+f.Subject] {
			named[f.Subject] = true
		}
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
// HEAD only, UNVERIFIABLE, STAT, SKIPPED, ERROR and unreached copies never count.
func contentVerified(cov Coverage) bool {
	local := cov.Local.Method == MethodParsed || cov.Local.Method == MethodAbsent
	object := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable
	read := cov.Local.Method == MethodParsed || cov.Object.Method == MethodHashed
	return local && object && read
}

// Disappeared is the findings of Closures.
func Disappeared(previous []Finding, rep Report) []Finding {
	var out []Finding
	for _, c := range Closures(previous, rep) {
		out = append(out, c.Finding)
	}
	return out
}

// Closures is the previous findings this pass is positive evidence against:
// not reported again, and the subject was checked with the method that
// detects that kind, or its row is gone from a complete catalog. A copy that
// was skipped, errored or not reached closes nothing.
func Closures(previous []Finding, rep Report) []Closure {
	have := map[string]bool{}
	for _, f := range rep.Findings {
		have[f.Kind+"\x00"+f.Subject] = true
	}
	var out []Closure
	for _, f := range previous {
		if have[f.Kind+"\x00"+f.Subject] {
			continue
		}
		if reason := closes(f, rep); reason != "" {
			out = append(out, Closure{Finding: f, Reason: reason})
		}
	}
	return out
}

// closes is the per-kind detector table (design §6.2): the reason f closes, or "".
func closes(f Finding, rep Report) string {
	if rep.CatalogComplete && !rep.catalog[f.Subject] && rowScoped(f.Kind) {
		return ReasonRowRemoved
	}
	cov, ok := rep.Coverage[f.Subject]
	switch f.Kind {
	case KindVerifyFailed:
		if ok && cov.Local.Method == MethodParsed {
			return ReasonParsed
		}
		if ok && cov.Local.Method == MethodAbsent && cov.Object.Method == MethodHashed && cov.Object.Digest == "catalog" && cov.Object.Parsed && !reported(rep, KindObjectCorrupt, f.Subject) {
			if strings.HasPrefix(f.Detail, DetailSHA) {
				return ReasonLocalRemoved
			}
			return ReasonObjectParsed
		}
	case KindLocalMissing:
		if ok && cov.Local.Found {
			return ReasonLocalPresent
		}
		if rep.ChainChecked {
			return ReasonChainChecked
		}
	case KindObjectMissing:
		if ok && cov.Object.Found {
			return ReasonObjectFound
		}
		if !ok && rep.ChainChecked {
			return ReasonChainChecked
		}
	case KindObjectSizeMismatch:
		if ok && cov.Object.SizeChecked {
			return ReasonSizeCompared
		}
		if !ok && rep.ChainChecked {
			return ReasonChainChecked
		}
	case KindObjectCorrupt:
		if ok && cov.Object.Method == MethodHashed {
			return ReasonHashed
		}
	case KindUploadClockSkew:
		if ok && cov.ClockChecked {
			return ReasonClockChecked
		}
	case KindLocalUnreadable:
		if ok && cov.Local.Method == MethodParsed {
			return ReasonLocalRead
		}
		if ok && (cov.Local.Method == MethodAbsent || cov.Local.Method == MethodMissing) {
			return ReasonLocalGone
		}
	default:
		if rep.ChainChecked {
			return ReasonChainChecked
		}
	}
	return ""
}

// rowScoped is a kind about one catalog row, which a removed row closes:
// this file's kinds and the chain's I6 kinds except I6_MISSING_ROW. The
// chain's other kinds (I1–I5, I7) describe the chain and close only when the
// chain is checked (design §6.2).
func rowScoped(kind string) bool {
	switch kind {
	case KindVerifyFailed, KindObjectCorrupt, KindUploadClockSkew, KindLocalUnreadable:
		return true
	case string(I6MissingRow):
		return false
	}
	return strings.HasPrefix(kind, "I6_")
}

func reported(rep Report, kind, subject string) bool {
	for _, f := range rep.Findings {
		if f.Kind == kind && f.Subject == subject {
			return true
		}
	}
	return false
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

// openSubjects is the open kinds per subject and every subject an open finding names.
func openSubjects(open []Finding) (forced map[string][]string, named map[string]bool) {
	forced, named = map[string][]string{}, map[string]bool{}
	for _, f := range open {
		if f.Kind != KindUploadClockSkew {
			named[f.Subject] = true
		}
		forced[f.Subject] = append(forced[f.Subject], f.Kind)
	}
	return forced, named
}

// forcedBy is whether an open finding forces s into every pass: OBJECT_CORRUPT
// or VERIFY_FAILED, or I6_OBJECT_SIZE_MISMATCH on a row whose size is unknown
// (only a hash against the local file can recheck it). Only while a local copy
// exists: without one nothing here can repair the segment, and the rotation
// rechecks it once per rotation.
func (p *pass) forcedBy(s *seg) bool {
	if !s.cov.Local.Found {
		return false
	}
	for _, kind := range p.forced[s.row.Name] {
		switch {
		case kind == KindObjectCorrupt, kind == KindVerifyFailed, kind == KindLocalUnreadable:
			return true
		case kind == KindObjectSizeMismatch && s.row.Size <= 0:
			return true
		}
	}
	return false
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

func uploaded(row VerifyRow) bool {
	return strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOADED") && strings.TrimSpace(row.ObjectKey) != ""
}

// openLocal opens a local segment file; tests replace it to inject read errors.
var openLocal = func(path string) (io.ReadCloser, error) { return os.Open(path) }

type localResult struct {
	check   Check
	finding *Finding
	sum     string
}

// checkLocal reads the file once: the parse, the SHA-256 and the event
// span all come from the same stream, so the file is paid for once.
func checkLocal(ctx context.Context, path string, row VerifyRow, size int64, budget *ByteBudget, n *int64) (localResult, error) {
	f, err := openLocal(path)
	if err != nil {
		return localResult{}, err
	}
	defer f.Close()
	rd, err := readSegment(ctx, f, size, true, budget, n)
	if err != nil {
		return localResult{}, err
	}
	parsed := func(reason string, finding *Finding, sum string) (localResult, error) {
		return localResult{check: Check{Method: MethodParsed, Reason: reason}, finding: finding, sum: sum}, nil
	}
	if detail, evidence := mismatch(row, rd, true); detail != "" {
		sum := rd.sum
		if detail == DetailParse {
			sum = ""
		}
		return parsed(reasonFor(detail), localFinding(row.Name, detail, evidence), sum)
	}
	if strings.TrimSpace(row.SHA256) == "" {
		return parsed("parsed; catalog row has no sha256 to compare", nil, rd.sum)
	}
	return parsed("parsed; matches the catalog row", nil, rd.sum)
}

func reasonFor(detail string) string {
	if detail == DetailParse {
		return "did not parse"
	}
	return detail
}

// segRead is one segment's bytes read once: the SHA-256 of everything read,
// and, when parsed, the parse result and the event span.
type segRead struct {
	sum string
	n   int64 // bytes read

	contents binlog.SegmentContents
	parseErr error
	span     *spanTracker
}

// readSegment streams r through the budget into the hash and, when parse is
// set, the parser and the span tracker. A parse failure does not stop the
// read: the rest is drained into the hash so the digest covers every byte.
// The error is a budget, context or read error, never a parse failure.
func readSegment(ctx context.Context, r io.Reader, size int64, parse bool, budget *ByteBudget, n *int64) (segRead, error) {
	br := &budgetReader{ctx: ctx, r: r, b: budget, n: n}
	h := sha256.New()
	rd := segRead{span: &spanTracker{size: size}}
	var err error
	if parse {
		w := io.MultiWriter(h, rd.span)
		rd.contents, rd.parseErr = binlog.ReadSealedSegmentContents(io.TeeReader(br, w))
		_, err = io.Copy(w, br)
	} else {
		_, err = io.Copy(h, br)
	}
	if br.err != nil {
		return segRead{}, br.err
	}
	if err != nil {
		return segRead{}, err
	}
	if err := ctx.Err(); err != nil {
		return segRead{}, err
	}
	rd.sum = hex.EncodeToString(h.Sum(nil))
	rd.n = br.got
	return rd, nil
}

// mismatch is the first way rd differs from row, as a VERIFY_FAILED detail
// and evidence, or "" when it matches. sha compares the digest too.
func mismatch(row VerifyRow, rd segRead, sha bool) (string, string) {
	if rd.parseErr != nil {
		return DetailParse, rd.parseErr.Error()
	}
	if want := strings.TrimSpace(row.SHA256); sha && want != "" && !strings.EqualFold(rd.sum, want) {
		return DetailSHA, fmt.Sprintf("local=%s row=%s", rd.sum, want)
	}
	if row.StartPos > 0 || row.EndPos > 0 {
		start, end, ok := rd.span.result()
		if !ok || (row.StartPos > 0 && start != row.StartPos) || (row.EndPos > 0 && end != row.EndPos) {
			return DetailPositions, fmt.Sprintf("start got=%d row=%d end got=%d row=%d", start, row.StartPos, end, row.EndPos)
		}
	}
	if gtid := strings.TrimSpace(row.GTIDSet); gtid != "" && rd.contents.GTIDs.String() != gtid {
		return DetailGTID, fmt.Sprintf("parsed=%s row=%s", rd.contents.GTIDs.String(), gtid)
	}
	return "", ""
}

func localFinding(name, detail, evidence string) *Finding {
	return &Finding{Kind: KindVerifyFailed, Subject: name, Severity: SeverityCritical, Detail: detail, Evidence: evidence}
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

// budgetReader paces reads on the shared budget and counts, for this pass,
// only the bytes it paid for. One read takes at most one burst, so it is
// paid in one step or not at all.
type budgetReader struct {
	ctx context.Context
	r   io.Reader
	b   *ByteBudget
	n   *int64
	got int64 // bytes read through this reader
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
	if c := b.b.chunk(); c > 0 && len(p) > c {
		p = p[:c]
	}
	n, err := b.r.Read(p)
	if n > 0 {
		if werr := b.b.Wait(b.ctx, int64(n)); werr != nil {
			b.err = werr
			return 0, werr
		}
		*b.n += int64(n)
		b.got += int64(n)
	}
	if err != nil && err != io.EOF {
		b.err = err
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

// spanTracker is binlog.EventSpan computed from the bytes as they stream
// past, so checking positions does not read the file a second time. size is
// the file size EventSpan would stat.
type spanTracker struct {
	size       int64
	off        int64 // bytes seen
	magic      []byte
	next       int64 // offset of the next event header
	hdr        []byte
	done       bool
	start, end uint32
	found      bool
}

const eventHeaderSize = 19

func (t *spanTracker) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !t.done {
		if t.off < 4 {
			k := min(int(4-t.off), len(p))
			t.magic = append(t.magic, p[:k]...)
			t.off += int64(k)
			p = p[k:]
			if t.off == 4 {
				if t.size < 4 || string(t.magic) != "\xfebin" {
					t.done = true
				}
				t.next = 4
			}
			continue
		}
		if t.off < t.next {
			k := int(min(t.next-t.off, int64(len(p))))
			t.off += int64(k)
			p = p[k:]
			continue
		}
		if len(t.hdr) == 0 && t.next+eventHeaderSize > t.size {
			t.done = true
			break
		}
		k := min(eventHeaderSize-len(t.hdr), len(p))
		t.hdr = append(t.hdr, p[:k]...)
		t.off += int64(k)
		p = p[k:]
		if len(t.hdr) < eventHeaderSize {
			continue
		}
		t.event()
	}
	return n, nil
}

func (t *spanTracker) event() {
	eventSize := int64(binary.LittleEndian.Uint32(t.hdr[9:13]))
	logPos := binary.LittleEndian.Uint32(t.hdr[13:17])
	t.hdr = t.hdr[:0]
	if eventSize < eventHeaderSize || t.next+eventSize > t.size {
		t.done = true
		return
	}
	t.next += eventSize
	if logPos == 0 || int64(logPos) < eventSize {
		return
	}
	evStart := logPos - uint32(eventSize)
	if !t.found || evStart > t.end {
		t.start, t.end, t.found = evStart, logPos, true
		return
	}
	if evStart < t.start {
		t.start = evStart
	}
	if logPos > t.end {
		t.end = logPos
	}
}

func (t *spanTracker) result() (start, end uint32, ok bool) {
	if !t.found {
		return 0, 0, false
	}
	return t.start, t.end, true
}
