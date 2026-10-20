// Package assurance provides module-level functionality for assurance.
// input: catalog rows, a task directory of sealed segments, an object store that can HEAD and GET, an optional Chain, and a byte budget
// output: the findings a verification pass should have open (structure, object HEAD, sampled SHA-256, chain) and the segments whose last_verified_at may advance; Disappeared is the subset of a previous set that this complete pass no longer reports
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
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/binlog"

	"golang.org/x/time/rate"
)

const (
	// DefaultSamplePercent is the share of older uploaded objects whose
	// bodies are hashed. Recent uploads are always hashed.
	DefaultSamplePercent = 5
	// DefaultRecent is the window in which every upload is hashed.
	DefaultRecent = 24 * time.Hour
	// DefaultPeriod is how long one sample draw stays stable.
	DefaultPeriod = 24 * time.Hour
	// DefaultBudgetBytesPerSec is the process-wide read budget.
	DefaultBudgetBytesPerSec int64 = 4 << 20

	// KindVerifyFailed is a sealed segment that does not parse or does not
	// match its catalog row.
	KindVerifyFailed = "VERIFY_FAILED"
	// KindObjectMissing is an UPLOADED object HEAD did not find.
	KindObjectMissing = "I6_MISSING_OBJECT"
	// KindObjectSizeMismatch is an UPLOADED object whose HEAD size is not the row size.
	KindObjectSizeMismatch = "I6_OBJECT_SIZE_MISMATCH"
	// KindObjectCorrupt is a sampled object whose SHA-256 is not the local file or the recorded digest.
	KindObjectCorrupt = "OBJECT_CORRUPT"

	SeverityCritical = "CRITICAL"
	SeverityInfo     = "INFO"
)

// ObjectStore is the object half of one pass. A missing object is os.ErrNotExist.
// Any other error means this pass must not close object findings.
type ObjectStore interface {
	Head(ctx context.Context, key string) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// VerifyRow is one catalog row. Name is the on-disk segment name.
// GTIDSet is optional: the catalog has no such column, and an empty value is not compared.
// StartPos and EndPos of 0 are unknown and are not compared.
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
}

// Finding is one conclusion a pass wants open. It is not a stored row.
type Finding struct {
	Kind     string
	Subject  string
	Severity string
	Detail   string
	Evidence string
}

// Report is one pass. A scope that is incomplete must not close findings of that scope.
type Report struct {
	Findings         []Finding
	VerifiedAt       map[string]time.Time
	Bytes            int64
	LocalIncomplete  bool
	ObjectIncomplete bool
	ChainChecked     bool
	LocalErr         error
	ObjectErr        error
}

// Options selects the sample and the budget. A nil Budget is unlimited.
// SamplePercent 0 means DefaultSamplePercent. Negative means older objects
// are not downloaded. Recent and Period of 0 mean the defaults.
type Options struct {
	Now           time.Time
	SamplePercent int
	Recent        time.Duration
	Period        time.Duration
	Budget        *ByteBudget
}

// ByteBudget paces reads. A nil budget or a non-positive rate does not pace.
type ByteBudget struct {
	lim     *rate.Limiter
	charged int64
}

// NewByteBudget paces at bytesPerSec. bytesPerSec <= 0 returns nil (unlimited).
func NewByteBudget(bytesPerSec int64) *ByteBudget {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := int(bytesPerSec)
	if burst > 64<<10 {
		burst = 64 << 10
	}
	if burst < 1 {
		burst = 1
	}
	return &ByteBudget{lim: rate.NewLimiter(rate.Limit(bytesPerSec), burst)}
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
			return err
		}
		b.charged += step
		n -= step
	}
	return nil
}

// Charged is the number of bytes Wait has allowed.
func (b *ByteBudget) Charged() int64 {
	if b == nil {
		return 0
	}
	return b.charged
}

// SampleObject reports whether this pass downloads the object body.
func SampleObject(name string, uploadedAt, now time.Time, recent, period time.Duration, percent int) bool {
	if recent <= 0 {
		recent = DefaultRecent
	}
	if !uploadedAt.IsZero() && (uploadedAt.After(now) || now.Sub(uploadedAt) <= recent) {
		return true
	}
	if percent == 0 {
		percent = DefaultSamplePercent
	}
	if percent >= 100 {
		return true
	}
	if percent <= 0 {
		return false
	}
	if period <= 0 {
		period = DefaultPeriod
	}
	slot := now.UTC().Truncate(period).Unix()
	sum := sha256.Sum256([]byte(name + "|" + strconv.FormatInt(slot, 10)))
	n := binary.BigEndian.Uint64(sum[:8])
	threshold := uint64(percent) * (math.MaxUint64 / 100)
	return n < threshold
}

// Verify checks sealed segments under dir against rows, objects and chain.
// The caller must already be the owner of this task. A store of nil leaves
// the object scope incomplete. A chain of nil leaves the chain scope incomplete.
func Verify(ctx context.Context, dir string, rows []VerifyRow, objects ObjectStore, chain *Chain, opt Options) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rep := Report{VerifiedAt: map[string]time.Time{}, ChainChecked: chain != nil}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	byName := map[string]VerifyRow{}
	for _, row := range rows {
		if row.Name != "" {
			byName[row.Name] = row
		}
	}
	localSHA := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		rep.LocalIncomplete = true
		rep.LocalErr = err
	} else {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			named, ok := binlog.ClassifySegment(e.Name())
			if !ok || named.Open {
				continue
			}
			row, has := byName[e.Name()]
			if !has || !row.Sealed {
				continue
			}
			sum, finding, readErr := checkLocal(ctx, filepath.Join(dir, e.Name()), row, opt.Budget)
			if readErr != nil {
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				rep.LocalIncomplete = true
				rep.LocalErr = readErr
				continue
			}
			if finding == nil && sum != "" {
				localSHA[e.Name()] = sum
			}
			if finding != nil {
				rep.Findings = append(rep.Findings, *finding)
			}
		}
	}
	if objects == nil {
		rep.ObjectIncomplete = true
	} else {
		for _, row := range rows {
			if !row.Sealed || !strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOADED") || strings.TrimSpace(row.ObjectKey) == "" {
				continue
			}
			finding, objErr := checkObject(ctx, row, localSHA[row.Name], objects, opt, now)
			if objErr != nil {
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				rep.ObjectIncomplete = true
				if rep.ObjectErr == nil {
					rep.ObjectErr = objErr
				}
				continue
			}
			if finding != nil {
				rep.Findings = append(rep.Findings, *finding)
			}
		}
	}
	if chain != nil && !rep.LocalIncomplete {
		for _, v := range chain.Check(now) {
			rep.Findings = append(rep.Findings, findingFromViolation(v))
		}
	} else if chain != nil && rep.LocalIncomplete {
		rep.ChainChecked = false
	}
	rep.Findings = dedupe(rep.Findings)
	if opt.Budget != nil {
		rep.Bytes = opt.Budget.Charged()
	}
	failed := map[string]bool{}
	for _, f := range rep.Findings {
		if f.Subject != "" {
			failed[f.Subject] = true
		}
	}
	for _, row := range rows {
		if !row.Sealed || row.Name == "" || failed[row.Name] || rep.LocalIncomplete {
			continue
		}
		if uploaded(row) && rep.ObjectIncomplete {
			continue
		}
		rep.VerifiedAt[row.Name] = now
	}
	return rep, nil
}

// Disappeared is the previous findings this complete pass no longer reports.
// An incomplete scope contributes nothing: a missing observation does not close.
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
		switch findingScope(f.Kind) {
		case "local":
			if rep.LocalIncomplete {
				continue
			}
		case "object":
			if rep.ObjectIncomplete {
				continue
			}
		default:
			if !rep.ChainChecked || rep.LocalIncomplete {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

func findingScope(kind string) string {
	switch kind {
	case KindVerifyFailed:
		return "local"
	case KindObjectMissing, KindObjectSizeMismatch, KindObjectCorrupt:
		return "object"
	default:
		return "chain"
	}
}

func uploaded(row VerifyRow) bool {
	return strings.EqualFold(strings.TrimSpace(row.UploadState), "UPLOADED") && strings.TrimSpace(row.ObjectKey) != ""
}

func checkLocal(ctx context.Context, path string, row VerifyRow, budget *ByteBudget) (string, *Finding, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if row.Size > 0 && info.Size() != row.Size {
		return "", localFinding(row.Name, "size differs from the catalog row", fmt.Sprintf("local=%d row=%d", info.Size(), row.Size)), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	br := &budgetReader{ctx: ctx, r: f, b: budget}
	h := sha256.New()
	contents, parseErr := binlog.ReadSealedSegmentContents(io.TeeReader(br, h))
	if parseErr == nil {
		_, parseErr = io.Copy(io.Discard, io.TeeReader(br, h))
		if errors.Is(parseErr, io.EOF) {
			parseErr = nil
		}
	}
	if br.err != nil {
		return "", nil, br.err
	}
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if parseErr != nil {
		return "", localFinding(row.Name, "sealed segment did not parse", parseErr.Error()), nil
	}
	if want := strings.TrimSpace(row.SHA256); want != "" && !strings.EqualFold(sum, want) {
		return sum, localFinding(row.Name, "sha256 differs from the catalog row", fmt.Sprintf("local=%s row=%s", sum, want)), nil
	}
	if row.StartPos > 0 || row.EndPos > 0 {
		// EventSpan seeks, so the second pass is paid up front instead of paced.
		if err := budget.Wait(ctx, info.Size()); err != nil {
			return "", nil, err
		}
		start, end, ok := binlog.EventSpan(path)
		if !ok || (row.StartPos > 0 && start != row.StartPos) || (row.EndPos > 0 && end != row.EndPos) {
			return sum, localFinding(row.Name, "event positions differ from the catalog row", fmt.Sprintf("start local=%d row=%d end local=%d row=%d", start, row.StartPos, end, row.EndPos)), nil
		}
	}
	if want := strings.TrimSpace(row.GTIDSet); want != "" && contents.GTIDs.String() != want {
		return sum, localFinding(row.Name, "gtid set differs from the catalog row", fmt.Sprintf("parsed=%s row=%s", contents.GTIDs.String(), want)), nil
	}
	return sum, nil, nil
}

func localFinding(name, detail, evidence string) *Finding {
	return &Finding{Kind: KindVerifyFailed, Subject: name, Severity: SeverityCritical, Detail: detail, Evidence: evidence}
}

func checkObject(ctx context.Context, row VerifyRow, localSHA string, objects ObjectStore, opt Options, now time.Time) (*Finding, error) {
	key := strings.TrimSpace(row.ObjectKey)
	size, err := objects.Head(ctx, key)
	if errors.Is(err, os.ErrNotExist) {
		return objectFinding(KindObjectMissing, row.Name, "uploaded object is missing", "key="+key), nil
	}
	if err != nil {
		return nil, err
	}
	if row.Size > 0 && size != row.Size {
		return objectFinding(KindObjectSizeMismatch, row.Name, "object size differs from the catalog row", fmt.Sprintf("object=%d row=%d", size, row.Size)), nil
	}
	if !SampleObject(row.Name, row.UploadedAt, now, opt.Recent, opt.Period, opt.SamplePercent) {
		return nil, nil
	}
	body, err := objects.Open(ctx, key)
	if errors.Is(err, os.ErrNotExist) {
		return objectFinding(KindObjectMissing, row.Name, "uploaded object is missing", "key="+key), nil
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	sum, err := hashReader(ctx, body, opt.Budget)
	if errors.Is(err, os.ErrNotExist) {
		return objectFinding(KindObjectMissing, row.Name, "uploaded object is missing", "key="+key), nil
	}
	if err != nil {
		return nil, err
	}
	want, source := localSHA, "local"
	if want == "" {
		want, source = strings.TrimSpace(row.SHA256), "catalog"
	}
	if want == "" {
		return nil, nil
	}
	if !strings.EqualFold(sum, want) {
		return objectFinding(KindObjectCorrupt, row.Name, "object sha256 differs", fmt.Sprintf("object=%s want=%s source=%s", sum, want, source)), nil
	}
	return nil, nil
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

type budgetReader struct {
	ctx context.Context
	r   io.Reader
	b   *ByteBudget
	n   int64
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
	if n > 0 && b.b != nil {
		if werr := b.b.Wait(b.ctx, int64(n)); werr != nil {
			b.err = werr
			return n, werr
		}
	}
	b.n += int64(n)
	return n, err
}

func hashReader(ctx context.Context, r io.Reader, budget *ByteBudget) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, &budgetReader{ctx: ctx, r: r, b: budget}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
