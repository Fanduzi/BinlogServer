// Package tasks provides module-level functionality for tasks.
// input: a failback chain (A, B, A), repeated SOURCE_SWITCHOVER stops, a task that runs again after a stop, and segments on both sides of a server switch
// output: assertions that a repeated stop counts once, a stop the task has recovered from is no longer the outcome and stays resolved after a normal Stop and a process restart, the window does not report a hole where the new server's header lacks transactions the backup already stored or names transactions of the task's start set, and failback files order after the middle server
// pos: unit coverage for the #292 failback-chain findings
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
)

const (
	fbA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	fbB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	fbC = "cccccccc-cccc-cccc-cccc-cccccccccccc"
)

func fbStopEvent(when time.Time) TaskEvent {
	return TaskEvent{
		Type:    "SOURCE_SWITCHOVER",
		Time:    when,
		Message: "source switched from " + fbA + " to " + fbB + ". This backup has no GTID set, so the old source file and position cannot be applied to the new source. Start a new task against the new primary and keep this backup. This task will not mix the two servers.",
		Detail:  "old=" + fbA + " new=" + fbB + " gtid_set= file=mysql-bin.000003 pos=1516",
	}
}

func TestAssembleSourceChain_RepeatedStopCountsOnce(t *testing.T) {
	at := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	got := AssembleSourceChain([]string{fbA}, []TaskEvent{fbStopEvent(at), fbStopEvent(at.Add(time.Minute)), fbStopEvent(at.Add(2 * time.Minute))})
	if len(got.Switches) != 1 {
		t.Fatalf("one VIP move listed %d times: %+v", len(got.Switches), got.Switches)
	}
	if !got.Switches[0].Time.Equal(at) || got.Outcome != SwitchOutcomeStopped {
		t.Fatalf("switch %+v outcome %s", got.Switches[0], got.Outcome)
	}
}

func TestSchedulerSourceChain_RunningAgainClearsStop(t *testing.T) {
	dir := t.TempDir()
	store := newFakeEventStore()
	scheduler := NewScheduler(WithDataDir(dir), WithEventStore(store))
	task, err := scheduler.CreateTask("chain", "chain-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, task.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, task.ID, sourceChainFileName), []byte(fbA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := fbStopEvent(time.Now())
	stop.TaskID = task.ID
	if err := store.AppendEvent(t.Context(), stop); err != nil {
		t.Fatal(err)
	}
	setState := func(state State, lastErr string) {
		scheduler.mu.Lock()
		cur := scheduler.tasks[task.ID]
		cur.State = state
		cur.LastError = lastErr
		scheduler.tasks[task.ID] = cur
		scheduler.mu.Unlock()
	}
	setState(StateFailed, "SOURCE_SWITCHOVER: "+stop.Message)
	held, err := scheduler.SourceChain(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Outcome != SwitchOutcomeStopped {
		t.Fatalf("held outcome %q", held.Outcome)
	}
	setState(StateRunning, "")
	back, err := scheduler.SourceChain(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Outcome == SwitchOutcomeStopped {
		t.Fatalf("task is RUNNING again but outcome is still stopped: %+v", back)
	}
	if len(back.Switches) != 1 || back.Switches[0].Continued {
		t.Fatalf("the stop itself stays in the history: %+v", back.Switches)
	}
}

func fbView(name string, at time.Time, previous string, events ...binlog.GTIDEventRef) segmentView {
	return segmentView{
		File:     BinlogFile{FileName: name, FilePath: "/data/1/" + name, State: "SEALED", CreatedAt: at},
		Readable: true,
		Log:      binlog.SegmentGTIDLog{Previous: previous, HasPrevious: previous != "", Events: events},
	}
}

func fbEvents(uuid string, seqs ...int64) []binlog.GTIDEventRef {
	out := make([]binlog.GTIDEventRef, len(seqs))
	for i, seq := range seqs {
		out[i] = binlog.GTIDEventRef{UUID: uuid, Seq: seq}
	}
	return out
}

func TestRecoveryWindow_SwitchHeaderIsNotAHole(t *testing.T) {
	at := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	// A's last file stored A:3-5. B's current file started before A:4-5
	// replicated into it, so its header is A:1-3. The GTID dump skipped A:4-5
	// on B because this backup already has them.
	views := []segmentView{
		fbView("mysql-bin.000004", at, fbA+":1-2", fbEvents(fbA, 3, 4, 5)...),
		fbView(fbB+".mysql-bin.000002", at.Add(time.Hour), fbA+":1-3", fbEvents(fbB, 1, 2)...),
	}
	got, err := assessRecovery(views, "mysql", false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Continuous || len(got.Breaks) != 0 {
		t.Fatalf("false hole at the switch: %+v", got)
	}
	// B's header names A:6 and C:1, which no stored segment has: a real hole.
	views[1] = fbView(fbB+".mysql-bin.000002", at.Add(time.Hour), fbA+":1-6,"+fbC+":1", fbEvents(fbB, 1, 2)...)
	real, err := assessRecovery(views, "mysql", false)
	if err != nil {
		t.Fatal(err)
	}
	if real.Continuous || !breakMentions(real, "gtid hole between mysql-bin.000004 and "+fbB+".mysql-bin.000002") {
		t.Fatalf("missing transactions across the switch not reported: %+v", real)
	}
}

func TestSelectReplayFiles_FailbackStintAfterMiddleServer(t *testing.T) {
	at := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	files := []BinlogFile{
		{FileName: fbA + "~2.mysql-bin.000004", FilePath: "/d/" + fbA + "~2.mysql-bin.000004.open.e6", State: "OPEN", CreatedAt: at.Add(2 * time.Hour)},
		{FileName: fbB + ".mysql-bin.000006", FilePath: "/d/" + fbB + ".mysql-bin.000006.sealed.e5", State: "SEALED", CreatedAt: at.Add(time.Hour)},
		{FileName: "mysql-bin.000004", FilePath: "/d/mysql-bin.000004.sealed.e1", State: "SEALED", CreatedAt: at},
	}
	got := SelectReplayFiles(files)
	var order []string
	for _, f := range got {
		order = append(order, filepath.Base(f.FilePath))
	}
	want := "mysql-bin.000004.sealed.e1," + fbB + ".mysql-bin.000006.sealed.e5," + fbA + "~2.mysql-bin.000004.open.e6"
	if strings.Join(order, ",") != want {
		t.Fatalf("replay order %v", order)
	}
}

func TestAssembleSourceChain_FailbackStints(t *testing.T) {
	got := AssembleSourceChain([]string{fbA, fbB, fbA}, nil)
	if len(got.Servers) != 3 || got.Current != fbA || !got.Servers[2].Current || got.Servers[0].Current {
		t.Fatalf("servers %+v", got.Servers)
	}
	if got.Servers[0].Prefix != "" || got.Servers[1].Prefix != fbB || got.Servers[2].Prefix != fbA+"~2" {
		t.Fatalf("prefixes %+v", got.Servers)
	}
	files := AnnotateFileSources([]BinlogFile{
		{FileName: "mysql-bin.000004", FilePath: "/d/mysql-bin.000004.sealed.e1"},
		{FileName: fbB + ".mysql-bin.000006", FilePath: "/d/" + fbB + ".mysql-bin.000006.sealed.e5"},
		{FileName: fbA + "~2.mysql-bin.000004", FilePath: "/d/" + fbA + "~2.mysql-bin.000004.open.e6"},
	}, got)
	want := []struct {
		id     string
		server int
	}{{fbA, 1}, {fbB, 2}, {fbA, 3}}
	for i, w := range want {
		if files[i].SourceIdentity != w.id || files[i].SourceServer != w.server {
			t.Fatalf("file %d: %s server %d, want %s server %d", i, files[i].SourceIdentity, files[i].SourceServer, w.id, w.server)
		}
	}
	// Without the chain file, continued switches rebuild the same stints.
	at := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	cont := func(old, neu string, when time.Time) TaskEvent {
		return TaskEvent{Type: "SOURCE_SWITCHOVER", Time: when,
			Message: "source switched from " + old + " to " + neu + " at mysql-bin.000004:4 gtid_set=x; continuing from the executed GTID set",
			Detail:  "old=" + old + " new=" + neu + " gtid_set=x file=mysql-bin.000004 pos=4"}
	}
	rebuilt := AssembleSourceChain(nil, []TaskEvent{cont(fbA, fbB, at), cont(fbB, fbA, at.Add(time.Hour))})
	if len(rebuilt.Servers) != 3 || rebuilt.Current != fbA || rebuilt.Servers[2].Prefix != fbA+"~2" {
		t.Fatalf("rebuilt %+v", rebuilt.Servers)
	}
	if c, s := rebuilt.OutcomeCounts(); c != 2 || s != 0 {
		t.Fatalf("counts continued=%d stopped=%d", c, s)
	}
}

// QA 6c88ad5b P2: a GTID task seeded with A:1-8 whose first file header is
// A:1-5 never stores A:6-8 (the dump skips them). After a failback, A's next
// header names A:6-8. That is the seed, not a hole.
func TestRecoveryWindow_FailbackHeaderInsideStartSetIsNotAHole(t *testing.T) {
	at := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	views := []segmentView{
		fbView("mysql-bin.000003", at, fbA+":1-5", fbEvents(fbA, 9, 10)...),
		fbView(fbB+".mysql-bin.000002", at.Add(time.Hour), "", fbEvents(fbB, 1, 2)...),
		fbView(fbA+"~2.mysql-bin.000004", at.Add(2*time.Hour), fbA+":1-10", fbEvents(fbA, 11, 12)...),
	}
	without, err := assessRecovery(views, "mysql", false)
	if err != nil {
		t.Fatal(err)
	}
	if without.Continuous {
		t.Fatalf("fixture: without the start set A:6-8 should look missing: %+v", without)
	}
	got, err := assessRecoveryFrom(views, "mysql", false, fbA+":1-8")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Continuous || len(got.Breaks) != 0 {
		t.Fatalf("false hole at the failback with start set A:1-8: %+v", got)
	}
	// A header that names A:11-13 before the stint that stores A:11 is still a hole.
	views[2] = fbView(fbA+"~2.mysql-bin.000004", at.Add(2*time.Hour), fbA+":1-13", fbEvents(fbA, 14)...)
	real, err := assessRecoveryFrom(views, "mysql", false, fbA+":1-8")
	if err != nil {
		t.Fatal(err)
	}
	if real.Continuous {
		t.Fatalf("A:11-13 missing across the failback not reported: %+v", real)
	}
}

// QA 6c88ad5b P2: a stop the task ran past stays resolved after a normal
// Stop and after a process restart. A fresh stop after that run is held again.
func TestSchedulerSourceChain_ResumedStaysResolvedAfterStopAndRestart(t *testing.T) {
	dir := t.TempDir()
	store := newFakeEventStore()
	scheduler := NewScheduler(WithDataDir(dir), WithEventStore(store))
	task, err := scheduler.CreateTask("chain", "chain-key")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	stop := fbStopEvent(at)
	stop.TaskID = task.ID
	running := TaskEvent{TaskID: task.ID, Type: "TASK_RUNNING", Message: "runner is running", Time: at.Add(time.Minute)}
	for _, event := range []TaskEvent{stop, running} {
		if err := store.AppendEvent(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	setState := func(s *Scheduler, state State, lastErr string) {
		s.mu.Lock()
		cur := s.tasks[task.ID]
		cur.State = state
		cur.LastError = lastErr
		s.tasks[task.ID] = cur
		s.mu.Unlock()
	}
	check := func(s *Scheduler, label, want string, wantStopped int) {
		t.Helper()
		chain, err := s.SourceChain(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, stopped := chain.OutcomeCounts()
		if chain.Outcome != want || stopped != wantStopped {
			t.Fatalf("%s: outcome %q stopped=%d, want %q stopped=%d (%+v)", label, chain.Outcome, stopped, want, wantStopped, chain.Switches)
		}
	}
	// The user stops the resumed task normally.
	setState(scheduler, StateStopped, "")
	check(scheduler, "resume then Stop", SwitchOutcomeResumed, 0)

	// A new process reads the same events; the task comes back STOPPED.
	restarted := NewScheduler(WithDataDir(dir), WithEventStore(store))
	restarted.mu.Lock()
	restarted.tasks[task.ID] = scheduler.tasks[task.ID]
	restarted.mu.Unlock()
	check(restarted, "resume then restart", SwitchOutcomeResumed, 0)

	// A stop recorded after that run holds the task again.
	later := fbStopEvent(at.Add(2 * time.Minute))
	later.TaskID = task.ID
	later.Detail = strings.Replace(later.Detail, "pos=1516", "pos=9999", 1)
	if err := store.AppendEvent(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	setState(restarted, StateFailed, "SOURCE_SWITCHOVER: "+later.Message)
	check(restarted, "new stop after the run", SwitchOutcomeStopped, 1)

	// A stop the task never ran past (Start checks it again, still no run).
	again := fbStopEvent(at.Add(3 * time.Minute))
	again.TaskID = task.ID
	again.Detail = later.Detail
	if err := store.AppendEvent(t.Context(), again); err != nil {
		t.Fatal(err)
	}
	setState(restarted, StateStopped, "")
	check(restarted, "stopped again without a run", SwitchOutcomeStopped, 1)
}
