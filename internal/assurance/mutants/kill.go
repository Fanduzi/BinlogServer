//go:build ignore

// Package main replays the verifier review mutants against the tests.
// input: the module sources (internal/assurance/verify.go, internal/tasks/sealed_upload.go, internal/meta/mysql_store.go) and the mutant table in this file
// output: a kill table on stdout; exit status 1 when a mutant that is not marked equivalent survives
// pos: the reproducible mutation harness for the phase 3 verifier, excluded from go test; run from the module root with go run ./internal/assurance/mutants/kill.go [id ...]
// note: if this file changes, update this header and module README.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type mutant struct {
	id   string
	desc string
	file string
	old  string
	new  string
	// equiv, when set, is why no test can tell the mutant apart; it is
	// expected to survive and is not counted.
	equiv string
}

const (
	verify = "internal/assurance/verify.go"
	upload = "internal/tasks/sealed_upload.go"
	store  = "internal/meta/mysql_store.go"
)

// packages maps a mutated file to the go test arguments that judge it.
// The tasks and meta packages are large; only the tests for the touched code run.
var packages = map[string][]string{
	verify: {"./internal/assurance/"},
	upload: {"-run", "TestApplySealedUpload", "./internal/tasks/"},
	store:  {"-run", "TestMySQLTaskStore_ListFailedUploadBinlogFiles", "./internal/meta/"},
}

var mutants = []mutant{
	// QA's 25 (PR #480 r1 review), rewritten against the r3 verifier.
	{id: "M01", desc: "object HEAD size compare dropped", file: verify,
		old: "\tif row.Size > 0 && size != row.Size {", new: "\tif false && size != row.Size {"},
	{id: "M02", desc: "OBJECT_CORRUPT merged into VERIFY_FAILED", file: verify,
		old: "*objectFinding(KindObjectCorrupt, row.Name, \"object sha256 differs\"", new: "*objectFinding(KindVerifyFailed, row.Name, \"object sha256 differs\""},
	{id: "M03", desc: "object sha compare dropped", file: verify,
		old: "\t\t\tcase !strings.EqualFold(rd.sum, digest):", new: "\t\t\tcase false:"},
	{id: "M04", desc: "local sha vs catalog dropped", file: verify,
		old: "sha && want != \"\" && !strings.EqualFold(rd.sum, want) {", new: "sha && want != \"\" && false {"},
	{id: "M05", desc: "local size compare dropped", file: verify,
		old: "\t\t\tif row.Size > 0 && s.size != row.Size {", new: "\t\t\tif false {"},
	{id: "M06", desc: "VERIFY_FAILED closes on any coverage", file: verify,
		old: "\t\tif ok && cov.Local.Method == MethodParsed {\n\t\t\treturn ReasonParsed", new: "\t\tif ok {\n\t\t\treturn ReasonParsed"},
	{id: "M07", desc: "OBJECT_CORRUPT closes on any coverage", file: verify,
		old: "\t\tif ok && cov.Object.Method == MethodHashed {\n\t\t\treturn ReasonHashed", new: "\t\tif ok {\n\t\t\treturn ReasonHashed"},
	{id: "M08", desc: "chain kinds close without a chain check", file: verify,
		old: "\tdefault:\n\t\tif rep.ChainChecked {", new: "\tdefault:\n\t\tif true {"},
	{id: "M09", desc: "object HEAD error counted as a good HEAD", file: verify,
		old: "s.cov.Object = Check{Method: MethodError, Reason: err.Error()}", new: "s.cov.Object = Check{Method: MethodHead, Reason: err.Error(), Found: true, SizeChecked: true}"},
	{id: "M10", desc: "24h boundary exclusive", file: verify,
		old: "\treturn now.Sub(uploadedAt) <= recent\n", new: "\treturn now.Sub(uploadedAt) < recent\n"},
	{id: "M11", desc: "future uploaded_at beyond the tolerance treated as recent", file: verify,
		old: "\tif uploadedAt.IsZero() || ClockSkewed(uploadedAt, now) {", new: "\tif uploadedAt.IsZero() {"},
	{id: "M12", desc: "recent window halved", file: verify,
		old: "\treturn now.Sub(uploadedAt) <= recent\n", new: "\treturn now.Sub(uploadedAt) <= recent/2\n"},
	{id: "M13", desc: "rotation twice as long", file: verify,
		old: "\treturn h%uint64(n) == slot%uint64(n)\n", new: "\treturn h%uint64(2*n) == slot%uint64(2*n)\n"},
	{id: "M14", desc: "streamed reads not charged", file: verify,
		old: "\tif n > 0 {\n\t\tif werr := b.b.Wait(", new: "\tif false {\n\t\tif werr := b.b.Wait("},
	{id: "M15", desc: "positions read and charged a second time", file: verify,
		old: "\trd, err := readSegment(ctx, f, size, true, budget, n)\n\tif err != nil {\n\t\treturn localResult{}, err\n\t}\n", new: "\trd, err := readSegment(ctx, f, size, true, budget, n)\n\tif err != nil {\n\t\treturn localResult{}, err\n\t}\n\tif row.StartPos > 0 || row.EndPos > 0 {\n\t\tif err := budget.Wait(ctx, size); err != nil {\n\t\t\treturn localResult{}, err\n\t\t}\n\t\t*n += size\n\t}\n"},
	{id: "M16", desc: "burst = full rate (4 MiB)", file: verify,
		old: "\tif burst > maxBurst {\n\t\tburst = maxBurst\n\t}\n", new: ""},
	{id: "M17", desc: "backfill overwrites stored digest", file: upload,
		old: "\tif strings.TrimSpace(file.SHA256) != \"\" || strings.TrimSpace(file.FilePath) == \"\" {", new: "\tif strings.TrimSpace(file.FilePath) == \"\" {"},
	{id: "M18", desc: "backfill overwrites first_event_at", file: upload,
		old: "\t\tif file.FirstEventAt == nil {", new: "\t\tif true {"},
	{id: "M19", desc: "retry list does not load sha256/times", file: store,
		old: "\t\tquery = listFailedSealedBinlogFilesWithSealFactsSQL\n\t\twithFacts = true\n", new: ""},
	{id: "M20", desc: "no 1054 fallback", file: store,
		old: "\tif withFacts && isMySQLUnknownColumnError(err) {", new: "\tif false && isMySQLUnknownColumnError(err) {"},
	{id: "M21", desc: "VerifiedAt advances for segments a finding names", file: verify,
		old: "\t\tif !named[name] && contentVerified(cov) {", new: "\t\tif contentVerified(cov) {"},
	{id: "M22", desc: "chain checked after a local read error", file: verify,
		old: "\tif chain != nil && localClean && ctx.Err() == nil {", new: "\tif chain != nil && (localClean || true) && ctx.Err() == nil {"},
	{id: "M23", desc: "object compared to catalog only (local digest ignored)", file: verify,
		old: "\tdigest, source := s.localSHA, \"local\"\n", new: "\tdigest, source := \"\", \"local\"\n"},
	{id: "M24", desc: "HEAD 404 treated as an error", file: verify,
		old: "\tif errors.Is(err, os.ErrNotExist) {\n\t\ts.cov.Object = Check{Method: MethodMissing, Reason: \"HEAD answered not found\"}", new: "\tif errors.Is(err, os.ErrNotExist) && false {\n\t\ts.cov.Object = Check{Method: MethodMissing, Reason: \"HEAD answered not found\"}"},
	{id: "M25", desc: "no backfill at all", file: upload,
		old: "\tfile = fillSealDigest(file)\n", new: ""},

	// The r2 coverage model.
	{id: "N01", desc: "OBJECT_CORRUPT closes on a HEAD-only pass", file: verify,
		old: "\t\tif ok && cov.Object.Method == MethodHashed {\n\t\t\treturn ReasonHashed", new: "\t\tif ok && cov.Object.Found {\n\t\t\treturn ReasonHashed"},
	{id: "N02", desc: "open findings do not force a recheck", file: verify,
		old: "\tif p.forcedBy(s) {", new: "\tif p.forcedBy(s) && false {"},
	{id: "N03", desc: "open VERIFY_FAILED does not force a recheck", file: verify,
		old: "\t\tcase kind == KindObjectCorrupt, kind == KindVerifyFailed, kind == KindLocalUnreadable:", new: "\t\tcase kind == KindObjectCorrupt, kind == KindLocalUnreadable:"},
	{id: "N04", desc: "last_verified_at advances on HEAD only", file: verify,
		old: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable\n", new: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable || cov.Object.Method == MethodHead\n"},
	{id: "N06", desc: "last_verified_at advances with no content read at all", file: verify,
		old: "\treturn local && object && read\n", new: "\treturn local && object && (read || true)\n"},
	{id: "N07", desc: "no digest: body downloaded anyway", file: verify,
		old: "\tif digest == \"\" {\n\t\tif s.cov.Local.Method == MethodSkipped {", new: "\tif digest == \"\" && false {\n\t\tif s.cov.Local.Method == MethodSkipped {"},
	{id: "N08", desc: "sealed, not uploaded, local missing raises nothing", file: verify,
		old: "\t\t\tif s.cov.Local.Method == MethodMissing {", new: "\t\t\tif false {"},
	{id: "N09", desc: "retention-removed, not uploaded counted as missing", file: verify,
		old: "\tcase row.RetentionRemoved:\n", new: "\tcase false:\n"},
	{id: "N10", desc: "clock skew raises no finding", file: verify,
		old: "\t\t\tif ClockSkewed(row.UploadedAt, p.now) {", new: "\t\t\tif false {"},
	{id: "N11", desc: "clock skew tolerance zero", file: verify,
		old: "uploadedAt.Sub(now) > ClockSkewTolerance", new: "uploadedAt.Sub(now) > 0"},
	{id: "N12", desc: "rotation is one fixed slot", file: verify,
		old: "\treturn h%uint64(n) == slot%uint64(n)\n", new: "\treturn h%uint64(n) == 0*slot\n"},
	{id: "N13", desc: "rotation ignores the name", file: verify,
		old: "\treturn h%uint64(n) == slot%uint64(n)\n", new: "\treturn h%1 == slot%uint64(n)\n"},
	{id: "N14", desc: "NoDigestLocal not counted", file: verify,
		old: "\t\t\t\trep.NoDigestLocal++\n", new: ""},
	{id: "N15", desc: "UnverifiableNoDigest not counted (local gone)", file: verify,
		old: "\t\t\trep.UnverifiableNoDigest++\n", new: ""},
	{id: "N16", desc: "pass byte counter not incremented", file: verify,
		old: "\t\t*b.n += int64(n)\n", new: ""},
	{id: "N17", desc: "Report.Bytes drops object bytes", file: verify,
		old: "func (c *passCounter) total() int64 { return c.local + c.object }", new: "func (c *passCounter) total() int64 { return c.local }"},
	{id: "N18", desc: "I6_MISSING_OBJECT closes on an object HEAD error", file: verify,
		old: "s.cov.Object = Check{Method: MethodError, Reason: err.Error()}", new: "s.cov.Object = Check{Method: MethodError, Reason: err.Error(), Found: true}"},
	{id: "N19", desc: "UPLOAD_CLOCK_SKEW closes without a clock check", file: verify,
		old: "\t\tif ok && cov.ClockChecked {", new: "\t\tif ok {"},
	{id: "N20", desc: "I6_MISSING_LOCAL closes on any coverage", file: verify,
		old: "\t\tif ok && cov.Local.Found {", new: "\t\tif ok {"},
	{id: "N21", desc: "cancel is treated as one read error and the pass goes on", file: verify,
		old: "\tif cancelled(p.ctx) {\n\t\treturn p.ctx.Err()\n\t}\n\tif p.outOfTime(err) {\n\t\tp.defer1(w, reasonDeferredTime)", new: "\tif p.outOfTime(err) {\n\t\tp.defer1(w, reasonDeferredTime)"},
	{id: "N22", desc: "unlisted directory treated as empty", file: verify,
		old: "\tlisted := listErr == nil\n", new: "\tlisted := true\n\t_ = listErr\n"},
	{id: "N23", desc: "local stat error does not block the chain", file: verify,
		old: "\t\t\t\tlocalClean = false\n", new: ""},
	{id: "N24", desc: "a failed local check still lends its digest to the object", file: verify,
		old: "\t} else {\n\t\ts.localSHA = res.sum\n\t}", new: "\t}\n\ts.localSHA = res.sum"},
	{id: "N25", desc: "budget deadline treated as one read error", file: verify,
		old: " || errors.Is(err, ErrBudgetDeadline)", new: ""},
	{id: "N26", desc: "I6_OBJECT_SIZE_MISMATCH closes on any object coverage", file: verify,
		old: "\t\tif ok && cov.Object.SizeChecked {", new: "\t\tif ok && cov.Object.Method != \"\" {"},
	{id: "N27", desc: "Closures closes findings the pass reported again", file: verify,
		old: "\t\tif have[f.Kind+\"\\x00\"+f.Subject] {\n\t\t\tcontinue\n\t\t}\n\t\tif reason := closes(f, rep)", new: "\t\tif reason := closes(f, rep)"},
	{id: "N29", desc: "object byte mismatch reported as VERIFY_FAILED", file: verify,
		old: "objectFinding(KindObjectCorrupt, row.Name, \"object sha256 differs\"", new: "objectFinding(KindVerifyFailed, row.Name, \"object sha256 differs\""},
	{id: "N30", desc: "last_verified_at advances on UNVERIFIABLE", file: verify,
		old: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable\n", new: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable || cov.Object.Method == MethodUnverifiable\n",
		equiv: "UNVERIFIABLE is set only when this pass has no digest: in head() because the local file cannot be parsed (ABSENT, MISSING, STAT on a size mismatch, ERROR, SKIPPED for an unlisted directory), or in hashObject because the parse raised VERIFY_FAILED or failed with ERROR. contentVerified needs a content read, which with an UNVERIFIABLE object can only be a local PARSED; the first case has none, and in the second a PARSED local carries a finding (the subject is named) or the local is ERROR. contentVerified or the naming check is false either way"},
	{id: "N31", desc: "clock observation not recorded", file: verify,
		old: "\t\ts.cov.ClockChecked = true\n", new: ""},

	// QA's 20 (PR #480 r2 review), against the r3 verifier.
	{id: "X01", desc: "HEAD closes OBJECT_CORRUPT", file: verify,
		old: "\t\tif ok && cov.Object.Method == MethodHashed {\n\t\t\treturn ReasonHashed", new: "\t\tif ok && (cov.Object.Method == MethodHashed || cov.Object.Method == MethodHead) {\n\t\t\treturn ReasonHashed"},
	{id: "X02", desc: "STAT closes VERIFY_FAILED", file: verify,
		old: "\t\tif ok && cov.Local.Method == MethodParsed {\n\t\t\treturn ReasonParsed", new: "\t\tif ok && cov.Local.Found {\n\t\t\treturn ReasonParsed"},
	{id: "X03", desc: "HEAD error counts as object found and sized", file: verify,
		old: "s.cov.Object = Check{Method: MethodError, Reason: err.Error()}", new: "s.cov.Object = Check{Method: MethodError, Reason: err.Error(), Found: true, SizeChecked: true}"},
	{id: "X04", desc: "any coverage closes I6_MISSING_OBJECT", file: verify,
		old: "\t\tif ok && cov.Object.Found {\n\t\t\treturn ReasonObjectFound", new: "\t\tif ok || rep.ChainChecked {\n\t\t\treturn ReasonObjectFound"},
	{id: "X05", desc: "STAT counts as local verified", file: verify,
		old: "\tlocal := cov.Local.Method == MethodParsed || cov.Local.Method == MethodAbsent\n", new: "\tlocal := cov.Local.Method == MethodParsed || cov.Local.Method == MethodAbsent || cov.Local.Method == MethodStat\n",
		equiv: "contentVerified with a STAT local needs the object HASHED. A segment's local and object are queued together, local first, so a hashed object with a local the parse never touched happens only when the local was not parseable: STAT then means its size differs from the row, which raised VERIFY_FAILED and names the subject. A scheduled local that was not paid for is SKIPPED, not STAT. QA reached the same conclusion for r2"},
	{id: "X06", desc: "advance without any content read", file: verify,
		old: "\treturn local && object && read\n", new: "\t_ = read\n\treturn local && object\n"},
	{id: "X07", desc: "open VERIFY_FAILED does not force a recheck (guarded)", file: verify,
		old: "\t\tcase kind == KindObjectCorrupt, kind == KindVerifyFailed, kind == KindLocalUnreadable:", new: "\t\tcase kind == KindObjectCorrupt, kind == KindVerifyFailed && false, kind == KindLocalUnreadable:"},
	{id: "X08", desc: "this pass's findings do not block last_verified_at", file: verify,
		old: "\tfor _, f := range rep.Findings {\n\t\tif f.Kind != KindUploadClockSkew {\n\t\t\tnamed[f.Subject] = true\n\t\t}\n\t}\n", new: ""},
	{id: "X09", desc: "budget wait failure is a per-item ERROR", file: verify,
		old: "\treturn errors.Is(p.ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrBudgetDeadline)", new: "\treturn errors.Is(p.ctx.Err(), context.DeadlineExceeded)"},
	{id: "X10", desc: "UPLOAD_CLOCK_SKEW closes on any coverage", file: verify,
		old: "\t\tif ok && cov.ClockChecked {", new: "\t\tif ok || cov.ClockChecked {"},
	{id: "X11", desc: "skewed future counts as recent", file: verify,
		old: "\tif uploadedAt.IsZero() || ClockSkewed(uploadedAt, now) {", new: "\tif uploadedAt.IsZero() || (ClockSkewed(uploadedAt, now) && false) {"},
	{id: "X12", desc: "rotation gap doubled", file: verify,
		old: "\treturn h%uint64(n) == slot%uint64(n)\n", new: "\treturn h%uint64(n) == (slot/2)%uint64(n)\n"},
	{id: "X13", desc: "chain kinds close on coverage without a chain check", file: verify,
		old: "\tdefault:\n\t\tif rep.ChainChecked {", new: "\tdefault:\n\t\tif rep.ChainChecked || ok {"},
	{id: "X14", desc: "ABSENT closes I6_MISSING_LOCAL", file: verify,
		old: "\t\tif ok && cov.Local.Found {\n\t\t\treturn ReasonLocalPresent", new: "\t\tif ok && (cov.Local.Found || cov.Local.Method == MethodAbsent) {\n\t\t\treturn ReasonLocalPresent"},
	{id: "X15", desc: "chain checked after a local read error (dropped)", file: verify,
		old: "\tif chain != nil && localClean && ctx.Err() == nil {", new: "\t_ = localClean\n\tif chain != nil && ctx.Err() == nil {"},
	{id: "X16", desc: "I6_MISSING_LOCAL not closed by the chain", file: verify,
		old: "\t\tif rep.ChainChecked {\n\t\t\treturn ReasonChainChecked\n\t\t}\n\tcase KindObjectMissing:", new: "\tcase KindObjectMissing:"},
	{id: "X17", desc: "bytes counted before they are paid", file: verify,
		old: "\tif n > 0 {\n\t\tif werr := b.b.Wait(b.ctx, int64(n)); werr != nil {\n\t\t\tb.err = werr\n\t\t\treturn 0, werr\n\t\t}\n\t\t*b.n += int64(n)\n", new: "\t*b.n += int64(n)\n\tif n > 0 {\n\t\tif werr := b.b.Wait(b.ctx, int64(n)); werr != nil {\n\t\t\tb.err = werr\n\t\t\treturn 0, werr\n\t\t}\n"},
	{id: "X18", desc: "NoDigestLocal never counted", file: verify,
		old: "\t\t\tif noDigest {\n\t\t\t\trep.NoDigestLocal++\n\t\t\t}\n", new: ""},
	{id: "X19", desc: "UnverifiableNoDigest never counted for a gone local", file: verify,
		old: "\t\tif uploaded(row) && noDigest && !s.parse {", new: "\t\tif false {"},
	{id: "X20", desc: "retention marker ignored: removed file is MISSING", file: verify,
		old: "\tcase row.RetentionRemoved:\n\t\treturn Check{Method: MethodAbsent, Reason: \"local retention removed the file\"}\n", new: ""},

	// r3: the queue, the deadline and the budget (QA r2 F1, F6).
	{id: "R01", desc: "a passed deadline aborts the pass", file: verify,
		old: "\t\tif p.ctx.Err() != nil {\n\t\t\tfor _, rest := range q[i:] {\n\t\t\t\tp.defer1(rest, reasonDeferredTime)\n\t\t\t}\n\t\t\treturn nil\n\t\t}", new: "\t\tif p.ctx.Err() != nil {\n\t\t\treturn p.ctx.Err()\n\t\t}"},
	{id: "R02", desc: "a deadline counts as cancel", file: verify,
		old: "\treturn errors.Is(ctx.Err(), context.Canceled)", new: "\treturn ctx.Err() != nil"},
	{id: "R03", desc: "queue not sorted (row order)", file: verify,
		old: "\tsort.SliceStable(q, func(i, j int) bool {", new: "\tsort.SliceStable(q[:0], func(i, j int) bool {"},
	{id: "R04", desc: "last_verified_at ignored in the order", file: verify,
		old: "\tif !a.s.last.Equal(b.s.last) {\n\t\treturn a.s.last.Before(b.s.last)\n\t}\n", new: ""},
	{id: "R05", desc: "newest last_verified_at first", file: verify,
		old: "\t\treturn a.s.last.Before(b.s.last)\n", new: "\t\treturn b.s.last.Before(a.s.last)\n"},
	{id: "R06", desc: "tiers ignored in the order", file: verify,
		old: "\tif a.tier != b.tier {\n\t\treturn a.tier < b.tier\n\t}\n", new: ""},
	{id: "R07", desc: "rotation slot ranked before the recent window", file: verify,
		old: "\ttierRecent\n\ttierSlot\n", new: "\ttierSlot\n\ttierRecent\n"},
	{id: "R08", desc: "object hashed before the local parse", file: verify,
		old: "\treturn a.local && !b.local\n", new: "\treturn !a.local && b.local\n"},
	{id: "R09", desc: "work that does not fit stops the queue (no skip-ahead)", file: verify,
		old: "\t\t\tp.defer1(w, reasonDeferredBudget)\n\t\t\tcontinue\n", new: "\t\t\tfor _, rest := range q[i:] {\n\t\t\t\tp.defer1(rest, reasonDeferredBudget)\n\t\t\t}\n\t\t\treturn nil\n"},
	{id: "R10", desc: "PassBytes ignored", file: verify,
		old: "\tif p.opt.PassBytes > 0 && p.count.total()+size > p.opt.PassBytes {", new: "\tif false {"},
	{id: "R11", desc: "no fit check against the deadline", file: verify,
		old: "\treturn p.opt.Budget.fits(size, p.deadline, p.hasDL)", new: "\treturn true"},
	{id: "R12", desc: "budget fit always true", file: verify,
		old: "\treturn !now.Add(wait).After(deadline)", new: "\treturn wait >= 0 || true"},
	{id: "R13", desc: "named segments stay in the overdue queue", file: verify,
		old: "\tif p.named[row.Name] {\n\t\treturn tierNone\n\t}\n", new: ""},
	{id: "R14", desc: "every segment overdue", file: verify,
		old: "(s.last.IsZero() || p.now.Sub(s.last) >= time.Duration(n)*periodOf(p.opt.Period))", new: "(true || p.now.Sub(s.last) >= time.Duration(n)*periodOf(p.opt.Period))"},
	{id: "R15", desc: "never-verified segments not overdue", file: verify,
		old: "(s.last.IsZero() || p.now.Sub(s.last)", new: "(false || p.now.Sub(s.last)",
		equiv: "time.Time.Sub saturates: now.Sub(time.Time{}) returns the maximum Duration (about 292 years), which is >= rotation x period for every rotation and period whose product fits in a Duration. The zero check is kept only to make the rule readable"},
	{id: "R16", desc: "overdue only strictly after rotation x period", file: verify,
		old: "p.now.Sub(s.last) >= time.Duration(n)*periodOf(p.opt.Period)", new: "p.now.Sub(s.last) > time.Duration(n)*periodOf(p.opt.Period)"},
	{id: "R17", desc: "objects not HEAD'd", file: verify,
		old: "\t\t\tif err := p.head(s, noDigest); err != nil {", new: "\t\t\tif err := error(nil); err != nil {"},
	{id: "R18", desc: "deferred work not counted", file: verify,
		old: "\tp.rep.Deferred++\n", new: ""},
	{id: "R19", desc: "deadline leaves the rest as STAT/HEAD instead of SKIPPED", file: verify,
		old: "\t\t\tfor _, rest := range q[i:] {\n\t\t\t\tp.defer1(rest, reasonDeferredTime)\n\t\t\t}\n", new: ""},
	{id: "R20", desc: "a deferred local parse makes the digestless object UNVERIFIABLE", file: verify,
		old: "\t\tif s.cov.Local.Method == MethodSkipped {", new: "\t\tif false {"},
	{id: "R21", desc: "UnverifiableNoDigest not counted (local parse failed)", file: verify,
		old: "\t\tp.rep.UnverifiableNoDigest++\n", new: ""},
	{id: "R22", desc: "reads not cut to one burst", file: verify,
		old: "\tif c := b.b.chunk(); c > 0 && len(p) > c {\n\t\tp = p[:c]\n\t}\n", new: ""},
	{id: "R23", desc: "positions not compared", file: verify,
		old: "\tif row.StartPos > 0 || row.EndPos > 0 {", new: "\tif false {"},
	{id: "R24", desc: "span chain never restarts after a gap", file: verify,
		old: "\tif !t.found || evStart > t.end {", new: "\tif !t.found {"},
	{id: "R25", desc: "span ignores an event past the end of the file", file: verify,
		old: "\tif eventSize < eventHeaderSize || t.next+eventSize > t.size {", new: "\tif eventSize < eventHeaderSize {"},
	{id: "R26", desc: "span ignores the magic", file: verify,
		old: "\t\t\t\tif t.size < 4 || string(t.magic) != \"\\xfebin\" {", new: "\t\t\t\tif t.size < 4 {"},

	// r3: closing (QA r2 F2, F3, F4).
	{id: "R30", desc: "forced recheck without a local copy", file: verify,
		old: "\tif !s.cov.Local.Found {\n\t\treturn false\n\t}\n", new: ""},
	{id: "R31", desc: "local-removed closure dropped", file: verify,
		old: "\t\tif ok && cov.Local.Method == MethodAbsent && cov.Object.Method == MethodHashed", new: "\t\tif false && cov.Local.Method == MethodAbsent && cov.Object.Method == MethodHashed"},
	{id: "R32", desc: "local-removed closure accepts a local digest", file: verify,
		old: " && cov.Object.Digest == \"catalog\"", new: ""},
	{id: "R33", desc: "local-removed closure despite OBJECT_CORRUPT", file: verify,
		old: " && !reported(rep, KindObjectCorrupt, f.Subject)", new: ""},
	{id: "R34", desc: "local-removed closure on a HEAD", file: verify,
		old: "cov.Local.Method == MethodAbsent && cov.Object.Method == MethodHashed", new: "cov.Local.Method == MethodAbsent && cov.Object.Found"},
	{id: "R35", desc: "open findings do not name the segment", file: verify,
		old: "\t\tif f.Kind != KindUploadClockSkew && !closed[f.Kind+\"\\x00\"+f.Subject] {", new: "\t\tif false {"},
	{id: "R36", desc: "open findings closed this pass still name the segment", file: verify,
		old: "\t\tif f.Kind != KindUploadClockSkew && !closed[f.Kind+\"\\x00\"+f.Subject] {", new: "\t\tif f.Kind != KindUploadClockSkew {"},
	{id: "R37", desc: "row removed without a complete catalog", file: verify,
		old: "\tif rep.CatalogComplete && !rep.catalog[f.Subject]", new: "\tif !rep.catalog[f.Subject]"},
	{id: "R38", desc: "row removed closes nothing", file: verify,
		old: "\t\treturn ReasonRowRemoved\n", new: "\t\treturn \"\"\n"},
	{id: "R39", desc: "row removed closes I6_MISSING_ROW", file: verify,
		old: "\tcase string(I6MissingRow):\n\t\treturn false\n", new: ""},
	{id: "R40", desc: "an unsealed row counts as removed", file: verify,
		old: "\tfor _, row := range rows {\n\t\trep.catalog[row.Name] = true\n\t}", new: "\tfor _, row := range sealedRows(rows) {\n\t\trep.catalog[row.Name] = true\n\t}"},
	{id: "R41", desc: "HEAD counts as a size check when the row size is unknown", file: verify,
		old: "Check{Method: MethodHead, Reason: reasonNotScheduled, Found: true, SizeChecked: row.Size > 0}", new: "Check{Method: MethodHead, Reason: reasonNotScheduled, Found: true, SizeChecked: true}"},
	{id: "R42", desc: "a catalog-digest hash counts as a size check", file: verify,
		old: "\t\t\tcase source == \"local\":\n", new: "\t\t\tcase true:\n"},
	{id: "R43", desc: "a matching local hash does not check the size", file: verify,
		old: "\t\t\t\ts.cov.Object.SizeChecked = true\n", new: ""},
	{id: "R44", desc: "open size mismatch on an unknown size not forced", file: verify,
		old: "\t\tcase kind == KindObjectSizeMismatch && s.row.Size <= 0:", new: "\t\tcase kind == KindObjectSizeMismatch && false:"},
	{id: "R45", desc: "open size mismatch forced on a known size", file: verify,
		old: "\t\tcase kind == KindObjectSizeMismatch && s.row.Size <= 0:", new: "\t\tcase kind == KindObjectSizeMismatch:"},
	{id: "R46", desc: "UnverifiableSize not counted", file: verify,
		old: "\t\tif c.Object.Found && !c.Object.SizeChecked {", new: "\t\tif false && c.Object.Found {"},
	{id: "R47", desc: "Closed not filled from the open findings", file: verify,
		old: "\trep.Closed = Closures(open, *rep)\n", new: "\trep.Closed = Closures(nil, *rep)\n"},

	// r4: QA r3 F1 (freshness, recent share), F2 (object parse), F3 (chain kinds), F4 (HEAD deadline).
	{id: "Y10", desc: "QA r3: not-uploaded rows not in the recent tier", file: verify,
		old: "\treturn p.stale(s) && (!uploaded(s.row) || RecentUpload(", new: "\treturn p.stale(s) && (RecentUpload("},
	{id: "Y14", desc: "QA r3: HEAD phase keeps sending HEADs after the deadline", file: verify,
		old: "\t\t\t\t\ttimeUp = true\n", new: ""},
	{id: "S02", desc: "recent tier ignores last_verified_at", file: verify,
		old: "\treturn s.last.IsZero() || s.last.Before(changed)\n", new: "\treturn true\n"},
	{id: "S03", desc: "sealed_at is not a change", file: verify,
		old: "\tif !ClockSkewed(row.SealedAt, p.now) {\n\t\tchanged = row.SealedAt\n\t}\n", new: ""},
	{id: "S04", desc: "an upload is not a change", file: verify,
		old: "\tif !ClockSkewed(row.UploadedAt, p.now) && row.UploadedAt.After(changed) {", new: "\tif false {"},
	{id: "S05", desc: "named segments stay in the recent tier", file: verify,
		old: "\tif p.named[s.row.Name] {\n\t\treturn p.fallback(s)\n\t}\n\tif p.fresh(s) {", new: "\tif p.fresh(s) {"},
	{id: "S06", desc: "the slot reads a segment on every pass of its period", file: verify,
		old: " && s.last.Before(slotStart(p.now, p.opt.Period))", new: ""},
	{id: "S07", desc: "recent share ignored", file: verify,
		old: "\t\tif w.tier == tierRecent && (w.s.demoted || share >= 0 && recent+size > share) {", new: "\t\tif w.tier == tierRecent && (w.s.demoted || share >= 0 && false) {"},
	{id: "S08", desc: "recent spend not counted", file: verify,
		old: "\t\t\trecent += p.count.total() - paid\n", new: "\t\t\trecent += 0 * (p.count.total() - paid)\n"},
	{id: "S09", desc: "a demoted segment's other copy stays recent", file: verify,
		old: "\t\t\tw.s.demoted = true\n", new: ""},
	{id: "S10", desc: "recent work past the share always goes to the tail", file: verify,
		old: "\t\t\tw.tier = p.fallback(w.s)\n\t\t\tif w.tier == tierNone {\n\t\t\t\tw.tier = tierTail\n\t\t\t}\n", new: "\t\t\tw.tier = tierTail\n"},
	{id: "S11", desc: "default recent share is the whole pass", file: verify,
		old: "\t\tpct = DefaultRecentSharePercent\n", new: "\t\tpct = 100\n"},
	{id: "S12", desc: "C ignores the budget and deadline", file: verify,
		old: "\tif b := p.opt.Budget; b != nil && p.hasDL {", new: "\tif b := p.opt.Budget; false && b != nil && p.hasDL {"},
	{id: "S20", desc: "object not parsed for an open VERIFY_FAILED", file: verify,
		old: "\tparse := source == \"catalog\" && s.cov.Local.Method == MethodAbsent && p.openKind(row.Name, KindVerifyFailed)\n", new: "\tparse := false\n"},
	{id: "S21", desc: "local-removed close does not need the object parse", file: verify,
		old: " && cov.Object.Parsed && ", new: " && "},
	{id: "S22", desc: "every VERIFY_FAILED variant closes as local removed; object matches catalog", file: verify,
		old: "\t\t\tif strings.HasPrefix(f.Detail, DetailSHA) {\n\t\t\t\treturn ReasonLocalRemoved\n\t\t\t}\n\t\t\treturn ReasonObjectParsed\n", new: "\t\t\treturn ReasonLocalRemoved\n"},
	{id: "S23", desc: "object-side defect not reported", file: verify,
		old: "\t\t\t\t\tp.rep.Findings = append(p.rep.Findings, *localFinding(row.Name, DetailObjectPrefix+detail, evidence))\n", new: "\t\t\t\t\t_, _ = detail, evidence\n"},
	{id: "S24", desc: "object-side finding not marked as the object", file: verify,
		old: "DetailObjectPrefix+detail", new: "detail"},
	{id: "S25", desc: "a parse failure stops the read (digest of a prefix)", file: verify,
		old: "\t\t_, err = io.Copy(w, br)\n", new: "\t\tif rd.parseErr == nil {\n\t\t\t_, err = io.Copy(w, br)\n\t\t}\n"},
	{id: "S26", desc: "object parsed for any open kind", file: verify,
		old: "\t\tif k == kind {\n", new: "\t\tif len(k) >= 0 {\n"},
	{id: "S30", desc: "row removed closes chain kinds", file: verify,
		old: "\treturn strings.HasPrefix(kind, \"I6_\")\n", new: "\treturn true\n"},
	{id: "S31", desc: "row removed does not close this file's kinds", file: verify,
		old: "\tcase KindVerifyFailed, KindObjectCorrupt, KindUploadClockSkew, KindLocalUnreadable:\n\t\treturn true\n", new: ""},
	{id: "Z08", desc: "QA r4: recent overflow with no fallback SKIPPED instead of paid from the tail", file: verify,
		old: "\t\t\tif w.tier == tierNone {\n\t\t\t\tw.tier = tierTail\n\t\t\t}\n", new: "\t\t\tif w.tier == tierNone {\n\t\t\t\tp.defer1(w, reasonDeferredBudget)\n\t\t\t\tcontinue\n\t\t\t}\n"},
	{id: "Z16", desc: "QA r4: budgetReader does not record a read error", file: verify,
		old: "\tif err != nil && err != io.EOF {\n\t\tb.err = err\n\t}\n\treturn n, err", new: "\treturn n, err"},
	{id: "T01", desc: "recent share not capped at C - F - S", file: verify,
		old: "\treturn min(c*int64(pct)/100, max(0, c-f-s))", new: "\treturn c * int64(pct) / 100"},
	{id: "T02", desc: "forced work not counted in the share cap", file: verify,
		old: "\t\t\tf += size\n", new: ""},
	{id: "T03", desc: "S includes segments that do not fit in C - F", file: verify,
		old: "\t\tif n <= c-f && n > s {", new: "\t\tif n > s {"},
	{id: "T04", desc: "stale uploads outside the window get no tail", file: verify,
		old: "\tif t := p.fallback(s); t != tierNone || s.last.IsZero() || !p.stale(s) {", new: "\tif t := p.fallback(s); true {"},
	{id: "T05", desc: "never-verified rows go to the tail with the rotation off", file: verify,
		old: "\tif t := p.fallback(s); t != tierNone || s.last.IsZero() || !p.stale(s) {", new: "\tif t := p.fallback(s); t != tierNone || !p.stale(s) {"},
	{id: "T06", desc: "LOCAL_UNREADABLE not reported on a read error", file: verify,
		old: "\t\tp.unreadable(w.s.row, err)\n", new: ""},
	{id: "T07", desc: "LOCAL_UNREADABLE not reported on a stat error", file: verify,
		old: "\t\t\t\tp.unreadable(row, statErr)\n", new: ""},
	{id: "T08", desc: "LOCAL_UNREADABLE always WARNING", file: verify,
		old: "\t\tsev = SeverityCritical\n\t}\n\tp.rep.Findings = append(p.rep.Findings, Finding{Kind: KindLocalUnreadable", new: "\t}\n\tp.rep.Findings = append(p.rep.Findings, Finding{Kind: KindLocalUnreadable"},
	{id: "T09", desc: "a vanished local file is LOCAL_UNREADABLE", file: verify,
		old: "\tif errors.Is(err, os.ErrNotExist) {\n\t\treturn\n\t}\n\tsev := SeverityWarning", new: "\tsev := SeverityWarning"},
	{id: "T10", desc: "LOCAL_UNREADABLE does not close on a clean read", file: verify,
		old: "\t\tif ok && cov.Local.Method == MethodParsed {\n\t\t\treturn ReasonLocalRead", new: "\t\tif false {\n\t\t\treturn ReasonLocalRead"},
	{id: "T11", desc: "LOCAL_UNREADABLE does not close when the file is gone", file: verify,
		old: "\t\tif ok && (cov.Local.Method == MethodAbsent || cov.Local.Method == MethodMissing) {", new: "\t\tif false {"},
	{id: "T12", desc: "LOCAL_UNREADABLE does not force a re-read", file: verify,
		old: ", kind == KindLocalUnreadable:", new: ":"},
	{id: "T14", desc: "short object body with a clean EOF not caught", file: verify,
		old: "\t\tif err == nil && rd.n < s.objSize {", new: "\t\tif false {"},
	{id: "T15", desc: "bytes read not counted", file: verify,
		old: "\t\tb.got += int64(n)\n", new: ""},
	{id: "T16", desc: "future sealed_at not reported", file: verify,
		old: "\t\tif ClockSkewed(row.SealedAt, p.now) {\n\t\t\trep.Findings", new: "\t\tif false {\n\t\t\trep.Findings"},
	{id: "T17", desc: "future sealed_at counts as a change", file: verify,
		old: "\tif !ClockSkewed(row.SealedAt, p.now) {\n\t\tchanged", new: "\tif true {\n\t\tchanged"},
	{id: "T18", desc: "UPLOAD_CLOCK_SKEW reported this pass holds back last_verified_at", file: verify,
		old: "\tfor _, f := range rep.Findings {\n\t\tif f.Kind != KindUploadClockSkew {", new: "\tfor _, f := range rep.Findings {\n\t\tif true {"},
	{id: "T19", desc: "open UPLOAD_CLOCK_SKEW names the segment for scheduling", file: verify,
		old: "\t\tif f.Kind != KindUploadClockSkew {\n\t\t\tnamed[f.Subject] = true\n\t\t}\n\t\tforced", new: "\t\tnamed[f.Subject] = true\n\t\tforced"},
	{id: "T20", desc: "open UPLOAD_CLOCK_SKEW holds back last_verified_at", file: verify,
		old: "\t\tif f.Kind != KindUploadClockSkew && !closed[", new: "\t\tif !closed["},
	{id: "T21", desc: "forced segment re-read on every pass", file: verify,
		old: "\t\tif p.readThisPeriod(s) {", new: "\t\tif false {"},
	{id: "T22", desc: "forced segment held across periods", file: verify,
		old: "ok && !m.At.Before(slotStart(p.now, p.opt.Period)) && ", new: "ok && "},
	{id: "T23", desc: "local mtime change does not release a held forced segment", file: verify,
		old: " && m.LocalModTime.Equal(s.mtime)", new: ""},
	{id: "T24", desc: "object size change does not release a held forced segment", file: verify,
		old: " && m.ObjectSize == s.objSize", new: ""},
	{id: "T27", desc: "recent share above 100 accepted", file: verify,
		old: "\tif pct < 0 || pct > 100 {", new: "\tif pct < 0 {"},
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, verify)); err != nil {
		fatal(fmt.Errorf("run from the module root: %w", err))
	}
	only := map[string]bool{}
	for _, a := range os.Args[1:] {
		only[a] = true
	}
	src := map[string]string{}
	for _, m := range mutants {
		if _, ok := src[m.file]; !ok {
			b, err := os.ReadFile(filepath.Join(root, m.file))
			if err != nil {
				fatal(err)
			}
			src[m.file] = string(b)
		}
		if n := strings.Count(src[m.file], m.old); n != 1 {
			fatal(fmt.Errorf("%s: %s matched %d times", m.id, m.file, n))
		}
	}
	var todo []mutant
	for _, m := range mutants {
		if len(only) == 0 || only[m.id] {
			todo = append(todo, m)
		}
	}
	type result struct{ id, desc, status, test string }
	out := make([]result, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, m := range todo {
		wg.Add(1)
		go func(i int, m mutant) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status, test := runOne(root, src[m.file], m)
			if m.equiv != "" && status == "survived" {
				status = "equivalent"
			}
			out[i] = result{m.id, m.desc, status, test}
			fmt.Fprintf(os.Stderr, "%s\t%s\t%s\n", m.id, status, test)
		}(i, m)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })

	fmt.Println("| id | mutant | result | killed by |")
	fmt.Println("|---|---|---|---|")
	killed, survived, equiv := 0, 0, 0
	for _, r := range out {
		fmt.Printf("| %s | %s | %s | %s |\n", r.id, r.desc, r.status, r.test)
		switch r.status {
		case "killed":
			killed++
		case "equivalent":
			equiv++
		default:
			survived++
		}
	}
	fmt.Printf("\nkilled %d, survived %d, equivalent %d, total %d\n", killed, survived, equiv, len(out))
	for _, m := range todo {
		if m.equiv != "" {
			fmt.Printf("equivalent %s: %s\n", m.id, m.equiv)
		}
	}
	if survived > 0 {
		os.Exit(1)
	}
}

// runOne builds the mutant with go test -overlay, so the tree is never edited.
func runOne(root, body string, m mutant) (status, test string) {
	dir, err := os.MkdirTemp("", "verify-mutant-"+m.id+"-")
	if err != nil {
		return "error", err.Error()
	}
	defer os.RemoveAll(dir)
	mutated := filepath.Join(dir, filepath.Base(m.file))
	if err := os.WriteFile(mutated, []byte(strings.Replace(body, m.old, m.new, 1)), 0o644); err != nil {
		return "error", err.Error()
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {filepath.Join(root, m.file): mutated}})
	if err != nil {
		return "error", err.Error()
	}
	ov := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(ov, overlay, 0o644); err != nil {
		return "error", err.Error()
	}
	args := append([]string{"test", "-count=1", "-timeout", "300s", "-overlay", ov}, packages[m.file]...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "BINLOG_SERVER_META_DSN=", "VERIFY_MINIO_ENDPOINT=")
	raw, err := cmd.CombinedOutput()
	if err == nil {
		return "survived", ""
	}
	if name := failedTests(string(raw)); name != "" {
		return "killed", name
	}
	return "killed", "build"
}

func failedTests(log string) string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "--- FAIL: ") {
			continue
		}
		rest := strings.TrimPrefix(line, "--- FAIL: ")
		if i := strings.IndexByte(rest, ' '); i > 0 {
			rest = rest[:i]
		}
		if strings.Contains(rest, "/") || seen[rest] {
			continue
		}
		seen[rest] = true
		names = append(names, rest)
	}
	if len(names) > 3 {
		names = append(names[:3], "...")
	}
	return strings.Join(names, " ")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
