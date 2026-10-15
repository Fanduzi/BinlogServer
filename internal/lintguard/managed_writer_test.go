package lintguard

import (
	"strconv"
	"strings"
	"testing"

	"binlog_server/internal/health"
)

// managedConstants is every tasks.FindingX constant whose kind the health
// evaluator manages, plus each kind's string literal.
func managedConstants(t *testing.T) map[string]bool {
	t.Helper()
	consts, err := FindingConstants("../../internal/tasks")
	if err != nil {
		t.Fatal(err)
	}
	reg := health.DefaultRegistry()
	out := map[string]bool{}
	for name, kind := range consts {
		if reg[health.Kind(kind)].Managed {
			out[name] = true
			out["\x00"+kind] = true
		}
	}
	if len(out) < 2*len(health.ManagedKinds())-4 { // SEGMENT_EMPTY_LEFTOVER etc. may have no tasks constant
		t.Fatalf("only %d managed constants found: %v", len(out)/2, out)
	}
	return out
}

// TestNoReportOfManagedKinds (P2-4, #342): a managed kind has one writer,
// the evaluator. Hot paths call SignalFailure/SignalSuccess; Report and
// Resolve (and any logger or helper method of that name) must not name a
// managed kind.
func TestNoReportOfManagedKinds(t *testing.T) {
	managed := managedConstants(t)
	var bad []string
	for _, dir := range []string{"internal/tasks", "internal/replication", "internal/api", "internal/app", "internal/binlog", "internal/upload"} {
		sites, err := ScanManagedWrites("../..", "../../"+dir, managed)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sites {
			bad = append(bad, s.File+":"+s.Func+" line "+strconv.Itoa(s.Line))
		}
	}
	if len(bad) > 0 {
		t.Fatalf("Report/Resolve of a managed kind (use SignalFailure/SignalSuccess):\n%s", strings.Join(bad, "\n"))
	}
}

func TestScanManagedWritesFindsEveryShape(t *testing.T) {
	sites, err := ScanManagedWrites("testdata", "testdata/managed", map[string]bool{"FindingDumpStalled": true, "\x00DUMP_STALLED": true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range sites {
		got[s.Func] = true
	}
	for _, want := range []string{"methodReport", "selectorConst", "loggerMethod", "plainFunc", "resolveLiteral"} {
		if !got[want] {
			t.Errorf("missed %s: %+v", want, sites)
		}
	}
	for _, clean := range []string{"signalOK", "unmanaged"} {
		if got[clean] {
			t.Errorf("flagged %s", clean)
		}
	}
}
