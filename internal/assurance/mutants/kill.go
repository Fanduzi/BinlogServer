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
	// QA's 25 (PR #480 r1 review), rewritten against the coverage model.
	{id: "M01", desc: "object HEAD size compare dropped", file: verify,
		old: "\tif row.Size > 0 && size != row.Size {", new: "\tif false && size != row.Size {"},
	{id: "M02", desc: "OBJECT_CORRUPT merged into VERIFY_FAILED", file: verify,
		old: "\tKindObjectCorrupt = \"OBJECT_CORRUPT\"", new: "\tKindObjectCorrupt = \"VERIFY_FAILED\""},
	{id: "M03", desc: "object sha compare dropped", file: verify,
		old: "\tif !strings.EqualFold(sum, digest) {", new: "\tif false {"},
	{id: "M04", desc: "local sha vs catalog dropped", file: verify,
		old: "want != \"\" && !strings.EqualFold(sum, want) {", new: "want != \"\" && false {"},
	{id: "M05", desc: "local size compare dropped", file: verify,
		old: "\tif row.Size > 0 && info.Size() != row.Size {", new: "\tif false {"},
	{id: "M06", desc: "Disappeared local guard dropped (VERIFY_FAILED closes on any coverage)", file: verify,
		old: "\t\treturn ok && cov.Local.Method == MethodParsed\n", new: "\t\treturn ok\n"},
	{id: "M07", desc: "Disappeared object guard dropped (OBJECT_CORRUPT closes on any coverage)", file: verify,
		old: "\t\treturn ok && cov.Object.Method == MethodHashed\n", new: "\t\treturn ok\n"},
	{id: "M08", desc: "Disappeared chain guard dropped", file: verify,
		old: "\tdefault:\n\t\treturn rep.ChainChecked\n", new: "\tdefault:\n\t\treturn true\n"},
	{id: "M09", desc: "object error does not leave the object unverified", file: verify,
		old: "cov.Object = Check{Method: MethodError, Reason: objErr.Error()}", new: "cov.Object = Check{Method: MethodHead, Reason: objErr.Error()}"},
	{id: "M10", desc: "24h boundary exclusive", file: verify,
		old: "\treturn now.Sub(uploadedAt) <= recent\n", new: "\treturn now.Sub(uploadedAt) < recent\n"},
	{id: "M11", desc: "future uploaded_at beyond the tolerance treated as recent", file: verify,
		old: "\tif uploadedAt.IsZero() || ClockSkewed(uploadedAt, now) {", new: "\tif uploadedAt.IsZero() {"},
	{id: "M12", desc: "recent window halved", file: verify,
		old: "\treturn now.Sub(uploadedAt) <= recent\n", new: "\treturn now.Sub(uploadedAt) <= recent/2\n"},
	{id: "M13", desc: "sample halved (rotation twice as long)", file: verify,
		old: "\treturn (100 + percent - 1) / percent\n", new: "\treturn 2 * ((100 + percent - 1) / percent)\n"},
	{id: "M14", desc: "streamed reads not charged", file: verify,
		old: "\tif n > 0 {\n\t\tif werr := b.b.Wait(", new: "\tif false {\n\t\tif werr := b.b.Wait("},
	{id: "M15", desc: "EventSpan second read not charged", file: verify,
		old: "\t\tif err := budget.Wait(ctx, info.Size()); err != nil {\n\t\t\treturn localResult{}, err\n\t\t}\n", new: ""},
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
		old: "\tif chain != nil && localClean {", new: "\tif chain != nil && (localClean || true) {"},
	{id: "M23", desc: "object compared to catalog only (local digest ignored)", file: verify,
		old: "\t\tdigest, source := localSHA[row.Name], \"local\"\n", new: "\t\tdigest, source := \"\", \"local\"\n"},
	{id: "M24", desc: "HEAD 404 treated as an error", file: verify,
		old: "\tsize, err := objects.Head(ctx, key)\n\tif errors.Is(err, os.ErrNotExist) {\n\t\treturn missing(\"HEAD\")\n", new: "\tsize, err := objects.Head(ctx, key)\n\tif errors.Is(err, os.ErrNotExist) {\n\t\treturn objectResult{}, err\n"},
	{id: "M25", desc: "no backfill at all", file: upload,
		old: "\tfile = fillSealDigest(file)\n", new: ""},

	// The coverage model (r2).
	{id: "N01", desc: "OBJECT_CORRUPT closes on a HEAD-only pass", file: verify,
		old: "\t\treturn ok && cov.Object.Method == MethodHashed\n", new: "\t\treturn ok && objectFound(cov.Object.Method)\n"},
	{id: "N02", desc: "open findings do not force a recheck", file: verify,
		old: "\tif kind, ok := forced[row.Name]; ok {", new: "\tif kind, ok := forced[row.Name]; ok && false {"},
	{id: "N03", desc: "open VERIFY_FAILED does not force the object hash", file: verify,
		old: "\t\tif f.Kind == KindObjectCorrupt || f.Kind == KindVerifyFailed {", new: "\t\tif f.Kind == KindObjectCorrupt {"},
	{id: "N04", desc: "last_verified_at advances on HEAD only", file: verify,
		old: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable\n", new: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable || cov.Object.Method == MethodHead\n"},
	{id: "N06", desc: "last_verified_at advances with no content read at all", file: verify,
		old: "\treturn local && object && read\n", new: "\treturn local && object && (read || true)\n"},
	{id: "N07", desc: "no digest: body downloaded anyway", file: verify,
		old: "\tif digest == \"\" {\n\t\treturn objectResult{check: Check{Method: MethodUnverifiable", new: "\tif digest == \"\" && false {\n\t\treturn objectResult{check: Check{Method: MethodUnverifiable"},
	{id: "N08", desc: "sealed, not uploaded, local missing raises nothing", file: verify,
		old: "\t\t\tif cov.Local.Method == MethodMissing {", new: "\t\t\tif false {"},
	{id: "N09", desc: "retention-removed, not uploaded counted as missing", file: verify,
		old: "\tcase row.RetentionRemoved:\n", new: "\tcase false:\n"},
	{id: "N10", desc: "clock skew raises no finding", file: verify,
		old: "\t\tif ClockSkewed(row.UploadedAt, now) {", new: "\t\tif false {"},
	{id: "N11", desc: "clock skew tolerance zero", file: verify,
		old: "uploadedAt.Sub(now) > ClockSkewTolerance", new: "uploadedAt.Sub(now) > 0"},
	{id: "N12", desc: "rotation is one fixed slot", file: verify,
		old: "\treturn h%uint64(n) == slot%uint64(n)\n", new: "\treturn h%uint64(n) == 0*slot\n"},
	{id: "N13", desc: "rotation ignores the name", file: verify,
		old: "\treturn h%uint64(n) == slot%uint64(n)\n", new: "\treturn h%1 == slot%uint64(n)\n"},
	{id: "N14", desc: "NoDigestLocal not counted", file: verify,
		old: "\t\t\t\trep.NoDigestLocal++\n", new: ""},
	{id: "N15", desc: "UnverifiableNoDigest not counted", file: verify,
		old: "\t\t\trep.UnverifiableNoDigest++\n", new: ""},
	{id: "N16", desc: "pass byte counter not incremented", file: verify,
		old: "\t*b.n += int64(n)\n", new: ""},
	{id: "N17", desc: "Report.Bytes drops object bytes", file: verify,
		old: "\trep.Bytes = count.local + count.object\n", new: "\trep.Bytes = count.local\n"},
	{id: "N18", desc: "I6_MISSING_OBJECT closes on an object ERROR", file: verify,
		old: "\treturn m == MethodHead || m == MethodUnverifiable || m == MethodHashed\n", new: "\treturn m == MethodHead || m == MethodUnverifiable || m == MethodHashed || m == MethodError\n"},
	{id: "N19", desc: "UPLOAD_CLOCK_SKEW closes without a clock check", file: verify,
		old: "\t\treturn ok && cov.ClockChecked\n", new: "\t\treturn ok\n"},
	{id: "N20", desc: "I6_MISSING_LOCAL closes on any coverage", file: verify,
		old: "\t\treturn (ok && localPresent(cov.Local.Method)) || rep.ChainChecked\n", new: "\t\treturn ok || rep.ChainChecked\n"},
	{id: "N21", desc: "cancel is treated as one read error and the pass goes on", file: verify,
		old: "\tif ctxErr := ctx.Err(); ctxErr != nil {\n\t\treturn ctxErr\n\t}\n\tif errors.Is(err, ErrBudgetDeadline) {", new: "\tif errors.Is(err, ErrBudgetDeadline) {"},
	{id: "N22", desc: "unlisted directory treated as empty", file: verify,
		old: "\tlisted := listErr == nil\n", new: "\tlisted := true\n\t_ = listErr\n"},
	{id: "N23", desc: "local read error does not block the chain", file: verify,
		old: "\t\t\t\tlocalClean = false\n", new: ""},
	{id: "N24", desc: "a failed local check still lends its digest to the object", file: verify,
		old: "\t\t\t} else if res.sum != \"\" {", new: "\t\t\t}\n\t\t\tif res.sum != \"\" {"},
	{id: "N25", desc: "budget deadline treated as one read error", file: verify,
		old: "\tif errors.Is(err, ErrBudgetDeadline) {\n\t\treturn err\n\t}\n", new: ""},
	{id: "N26", desc: "I6_OBJECT_SIZE_MISMATCH closes on GET 404 (object gone)", file: verify,
		old: "\tcase KindObjectSizeMismatch:\n\t\tif ok {\n\t\t\treturn objectFound(cov.Object.Method)\n", new: "\tcase KindObjectSizeMismatch:\n\t\tif ok {\n\t\t\treturn cov.Object.Method != \"\"\n"},
	{id: "N27", desc: "Disappeared closes findings the pass reported again", file: verify,
		old: "\t\tif have[f.Kind+\"\\x00\"+f.Subject] {\n\t\t\tcontinue\n\t\t}\n\t\tif closes(f, rep) {", new: "\t\tif closes(f, rep) {"},
	{id: "N29", desc: "object byte mismatch reported as VERIFY_FAILED at the call site", file: verify,
		old: "\t\tres.finding = objectFinding(KindObjectCorrupt,", new: "\t\tres.finding = objectFinding(KindVerifyFailed,"},
	{id: "N30", desc: "last_verified_at advances on UNVERIFIABLE", file: verify,
		old: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable\n", new: "\tobject := cov.Object.Method == MethodHashed || cov.Object.Method == MethodNotApplicable || cov.Object.Method == MethodUnverifiable\n",
		equiv: "UNVERIFIABLE means this pass had no clean local parse to take a digest from, so the local copy is either ABSENT (then no content was read and the read term is false) or its check raised VERIFY_FAILED (then the subject is named by a finding). contentVerified is false either way; the term is defence in depth"},
	{id: "N31", desc: "clock observation not recorded before the object half", file: verify,
		old: "\t\tcov.ClockChecked = true\n\t\trep.Coverage[row.Name] = cov\n", new: "\t\tdefer func(c *Coverage) { c.ClockChecked = true }(&cov)\n"},
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
