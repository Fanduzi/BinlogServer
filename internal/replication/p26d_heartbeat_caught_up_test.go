// Package replication provides module-level functionality for replication.
// input: a heartbeat from the dump, with a progress reporter that also takes caught-up reports
// output: proof that a heartbeat is reported as "caught up with the source" (MySQL and MariaDB send one only when the dump thread has sent the whole binlog and is waiting at its end), so an idle source is not TASK_NO_PROGRESS
// pos: ADR 0009 P2-6d (docs/design/0009-phase2-health-evaluator.md §24.9)
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"testing"
	"time"

	"binlog_server/internal/tasks"
)

type caughtUpReporter struct {
	fakeRunnerProgressReporter
	caughtUp []string
}

func (c *caughtUpReporter) ReportSourceCaughtUp(taskID string) {
	c.caughtUp = append(c.caughtUp, taskID)
}

func TestHeartbeatReportsSourceCaughtUp(t *testing.T) {
	rep := &caughtUpReporter{}
	r := NewMySQLRunner(t.TempDir(), WithProgressReporter(rep))
	l := &runLoop{r: r, ctx: context.Background(), task: tasks.Task{ID: "7"}, runStart: &runStart{}, seg: &openSegment{name: "mysql-bin.000004", pos: 1200}}
	if err := l.onHeartbeat(time.Time{}); err != nil {
		t.Fatalf("onHeartbeat: %v", err)
	}
	if len(rep.caughtUp) != 1 || rep.caughtUp[0] != "7" {
		t.Fatalf("caught-up reports %v, want one for task 7", rep.caughtUp)
	}
	if len(rep.reports) != 1 || rep.reports[0].file != "mysql-bin.000004" || rep.reports[0].pos != 1200 {
		t.Fatalf("progress reports %+v, want the heartbeat's progress report kept", rep.reports)
	}
}
