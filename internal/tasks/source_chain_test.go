// Package tasks provides module-level functionality for tasks.
// input: source-chain lines and SOURCE_SWITCHOVER event text
// output: assertions for continued and stopped chains, file ownership, and the scheduler read
// pos: unit coverage for the VIP source-chain view
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAssembleSourceChainContinuedAndStopped(t *testing.T) {
	oldID := "11111111-1111-1111-1111-111111111111"
	newID := "22222222-2222-2222-2222-222222222222"
	gtid := oldID + ":1-4, " + newID + ":1-2"
	when := time.Date(2026, 10, 8, 1, 2, 3, 0, time.UTC)
	continued := TaskEvent{
		Type:    "SOURCE_SWITCHOVER",
		Time:    when,
		Message: "source switched from " + oldID + " to " + newID + " at mysql-bin.000003:1516 gtid_set=" + gtid + "; continuing from the executed GTID set",
		Detail:  "old=" + oldID + " new=" + newID + " gtid_set=" + gtid + " file=mysql-bin.000003 pos=1516",
	}
	got := AssembleSourceChain([]string{oldID, newID}, []TaskEvent{continued, {Type: "TASK_RUNNING", Message: "runner is running"}})
	if got.Outcome != SwitchOutcomeContinued || got.Current != newID {
		t.Fatalf("chain %+v", got)
	}
	if len(got.Servers) != 2 || got.Servers[0].Identity != oldID || got.Servers[0].Current || !got.Servers[1].Current {
		t.Fatalf("servers %+v", got.Servers)
	}
	if len(got.Switches) != 1 || !got.Switches[0].Continued || got.Switches[0].Reason != "" {
		t.Fatalf("switch %+v", got.Switches)
	}
	sw := got.Switches[0]
	if sw.Old != oldID || sw.New != newID || sw.File != "mysql-bin.000003" || sw.Pos != 1516 || sw.GTIDSet != gtid || !sw.Time.Equal(when) {
		t.Fatalf("parsed %+v", sw)
	}
	files := AnnotateFileSources([]BinlogFile{
		{FileName: "mysql-bin.000003", FilePath: "/data/1/mysql-bin.000003"},
		{FileName: newID + ".mysql-bin.000001", FilePath: "/data/1/" + newID + ".mysql-bin.000001.open.e2"},
	}, got)
	if files[0].SourceIdentity != oldID || files[1].SourceIdentity != newID {
		t.Fatalf("identities %q %q", files[0].SourceIdentity, files[1].SourceIdentity)
	}

	stopped := TaskEvent{
		Type:    "SOURCE_SWITCHOVER",
		Message: "source switched from " + newID + " to " + oldID + ". The new primary is missing transactions this backup already has. Start a new task against the new primary and keep this backup. This task will not mix the two servers.",
		Detail:  "old=" + newID + " new=" + oldID + " gtid_set=" + gtid + " file=mysql-bin.000004 pos=200",
	}
	back := AssembleSourceChain([]string{oldID, newID}, []TaskEvent{continued, stopped})
	if back.Outcome != SwitchOutcomeStopped || back.Current != newID || len(back.Switches) != 2 {
		t.Fatalf("failback %+v", back)
	}
	if back.Switches[1].Continued || back.Switches[1].Reason != SwitchReasonMissing || back.Switches[1].Old != newID || back.Switches[1].New != oldID {
		t.Fatalf("failback switch %+v", back.Switches[1])
	}
}

func TestAssembleSourceChainStopReasons(t *testing.T) {
	cases := []struct {
		why    string
		reason string
	}{
		{"This backup has no GTID set, so the old source file and position cannot be applied to the new source", SwitchReasonNoGTID},
		{"The new primary is missing transactions this backup already has", SwitchReasonMissing},
		{"The new primary has purged transactions this backup does not have yet, so they cannot be copied", SwitchReasonPurged},
		{"MariaDB has no usable GTID path for a source switch", SwitchReasonMariaDB},
		{"The new source GTID state could not be read", SwitchReasonUnreadable},
	}
	for _, tc := range cases {
		event := TaskEvent{
			Type:    "SOURCE_SWITCHOVER",
			Message: "source switched from old-a to new-b. " + tc.why + ". Start a new task against the new primary and keep this backup. This task will not mix the two servers.",
			Detail:  "old=old-a new=new-b gtid_set= file=mysql-bin.000001 pos=4",
		}
		got := AssembleSourceChain(nil, []TaskEvent{event})
		if got.Outcome != SwitchOutcomeStopped || len(got.Switches) != 1 || got.Switches[0].Reason != tc.reason {
			t.Fatalf("%s: %+v", tc.reason, got)
		}
		if len(got.Servers) != 1 || got.Servers[0].Identity != "old-a" || !got.Servers[0].Current || got.Current != "old-a" {
			t.Fatalf("%s servers %+v", tc.reason, got.Servers)
		}
	}
	adopt := AssembleSourceChain(nil, []TaskEvent{{
		Type:    "SOURCE_SWITCHOVER",
		Message: "source switched from mariadb:1:0 to mariadb:2:0 before any transaction was stored; continuing with the configured start",
		Detail:  "old=mariadb:1:0 new=mariadb:2:0 gtid_set= file= pos=0",
	}})
	if adopt.Outcome != SwitchOutcomeContinued || adopt.Current != "mariadb:2:0" {
		t.Fatalf("adopt %+v", adopt)
	}
	name := FileSourceIdentity("mariadb:2:0.mysql-bin.000001", []string{"mariadb:1:0", "mariadb:2:0"})
	if name != "mariadb:2:0" {
		t.Fatalf("mariadb file identity %q", name)
	}
}

func TestSchedulerSourceChainReadsDiskAndEvents(t *testing.T) {
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
	body := "old-server\nnew-server\n"
	if err := os.WriteFile(filepath.Join(dir, task.ID, sourceChainFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, task.ID, "mysql-bin.000001"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(t.Context(), TaskEvent{
		TaskID:  task.ID,
		Type:    "SOURCE_SWITCHOVER",
		Message: "source switched from old-server to new-server at mysql-bin.000001:10 gtid_set=old-server:1-2; continuing from the executed GTID set",
		Detail:  "old=old-server new=new-server gtid_set=old-server:1-2 file=mysql-bin.000001 pos=10",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := scheduler.SourceChain(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != SwitchOutcomeContinued || got.Current != "new-server" || len(got.Switches) != 1 {
		t.Fatalf("%+v", got)
	}
	empty, err := scheduler.SourceChain("missing")
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Empty() {
		t.Fatalf("missing task chain %+v", empty)
	}
}
