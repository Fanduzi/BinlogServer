// Package lintguard provides module-level functionality for lintguard.
// input: the health registry's Managed kinds and Go sources under internal/{tasks,replication,api,app,binlog,upload}
// output: a failure when a Managed kind is named by Report or Resolve, unless it is a tasks.Finding constant or listed in managedWithoutConstant
// pos: P2-4 guard that a managed kind has one writer, the evaluator (design 0009 §5.6)
// note: if this file changes, update this header and module README.md.
package lintguard

import (
	"strconv"
	"strings"
	"testing"

	"binlog_server/internal/health"
)

// managedWithoutConstant are the Managed kinds that have no tasks.FindingX
// constant because only the evaluator derives them (no legacy writer ever
// existed): the chain-derived P2-5.1 kinds (design 0009 §20.1),
// SEGMENT_EMPTY_LEFTOVER (P2-3), TASK_UNADOPTED (#385, §19), UPLOAD_LATE
// (P2-5.2, §21.7), GTID_HOLE / CHECKPOINT_AHEAD (P2-5.3, §22.1) and
// TASK_NO_PROGRESS (P2-6d, §24.9), ORPHAN_ROW / SEAL_FACTS_ABSENT
// (P2-5.4, §26: derived, no legacy writer) and HEALTH_STALE
// (P2-6.1, §28: derived onto _cluster, no legacy writer).
// A new Managed kind must be added here or given a constant.
var managedWithoutConstant = map[health.Kind]bool{
	health.SegmentDamaged: true, health.ChainIndexGap: true, health.LegacyFailback: true, health.SegmentMissing: true,
	health.SegmentEmptyLeftover: true, health.TaskUnadopted: true,
	health.UploadLate: true,
	health.GTIDHole:   true, health.CheckpointAhead: true,
	health.TaskNoProgress: true, // P2-6d, §24.9
	health.OrphanRow:      true, health.SealFactsAbsent: true,
	health.HealthStale: true, // P2-6.1, §28
}

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
	have := map[string]bool{}
	for name, kind := range consts {
		have[kind] = true
		if reg[health.Kind(kind)].Managed {
			out[name] = true
			out["\x00"+kind] = true
		}
	}
	// Every Managed kind must either have a tasks.FindingX constant or be
	// declared in managedWithoutConstant; no count slack, no stale entries.
	for k := range health.ManagedKinds() {
		if !reg[k].Managed {
			t.Errorf("ManagedKinds lists %s but DefaultRegistry does not mark it Managed", k)
		}
		if have[string(k)] {
			if managedWithoutConstant[k] {
				t.Errorf("%s has a tasks constant now; drop it from managedWithoutConstant", k)
			}
			continue
		}
		if !managedWithoutConstant[k] {
			t.Errorf("managed kind %s has no tasks.FindingX constant and is not declared in managedWithoutConstant", k)
		}
		// Still guard its string literal: Report(..., "KIND", ...) is a write.
		out["\x00"+string(k)] = true
	}
	for k := range managedWithoutConstant {
		if !health.ManagedKinds()[k] {
			t.Errorf("managedWithoutConstant lists %s, which is not Managed", k)
		}
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
