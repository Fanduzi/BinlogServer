// Package replication provides module-level functionality for replication.
// input: a runner bound to a recording finding sink, a failing open-tail truncate and a dump read that times out
// output: proof that S4 (open tail cut failed) and S12 (dump stalled past the read timeout) reach the finding sink
// pos: fault-injection tests for ADR 0009 phase 0, dump and open tail
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

// S4: the unfinished trailing transaction could not be cut.
func TestS4_OpenTailCutFailureIsFinding(t *testing.T) {
	dir := t.TempDir()
	seg := newDiskSegment()
	seg.txn(10)
	seg.gtid(sampleGTID, 11)
	seg.begin()
	seg.rows()
	seg.write(t, filepath.Join(dir, "task-4", "mysql-bin.000001.open.e1"))

	orig := truncateFile
	truncateFile = func(string, int64) error { return errors.New("EROFS") }
	defer func() { truncateFile = orig }()

	r := NewMySQLRunner(dir)
	rec := bindRecorder(r)
	r.reconcileOpenTail(tasks.Task{ID: "task-4"}, newExecutedGTID("mysql", sampleGTID+":1-9"))
	f, ok := rec.has("task-4", tasks.FindingOpenTailCutFailed, "mysql-bin.000001")
	if !ok {
		t.Fatalf("no OPEN_TAIL_CUT_FAILED: %v", rec.open)
	}
	if f.Severity != tasks.FindingCritical {
		t.Fatalf("severity %s, want CRITICAL", f.Severity)
	}
}

type netTimeout struct{}

func (netTimeout) Error() string   { return "read tcp: i/o timeout" }
func (netTimeout) Timeout() bool   { return true }
func (netTimeout) Temporary() bool { return true }

// S12: the dump connection must time out instead of blocking forever.
func TestS12_DumpHasReadTimeout(t *testing.T) {
	cfg := buildSyncerConfig(tasks.Task{ID: "t"})
	if cfg.ReadTimeout != 45*time.Second {
		t.Fatalf("ReadTimeout %s, want 45s", cfg.ReadTimeout)
	}
	if cfg.ReadTimeout <= dumpHeartbeatPeriod {
		t.Fatalf("ReadTimeout %s must exceed heartbeat %s", cfg.ReadTimeout, dumpHeartbeatPeriod)
	}
}

// S12: a read timeout on the dump is a DUMP_STALLED finding; the next event resolves it.
func TestS12_DumpStallIsFinding(t *testing.T) {
	r := NewMySQLRunner(t.TempDir())
	rec := bindRecorder(r)
	st := &fakeStreamer{results: []streamResult{{err: netTimeout{}}, {event: xidAt(200)}}}
	if _, err := r.nextEvent(context.Background(), st, "t12", tasks.SourceConfig{}, "mysql-bin.000003", 100, nil); err == nil {
		t.Fatal("stall must still return an error so the dump reconnects")
	}
	if _, ok := rec.has("t12", tasks.FindingDumpStalled, ""); !ok {
		t.Fatalf("no DUMP_STALLED: %v", rec.open)
	}
	if _, err := r.nextEvent(context.Background(), st, "t12", tasks.SourceConfig{}, "mysql-bin.000003", 100, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.has("t12", tasks.FindingDumpStalled, ""); ok {
		t.Fatal("DUMP_STALLED must resolve once events flow")
	}
}
