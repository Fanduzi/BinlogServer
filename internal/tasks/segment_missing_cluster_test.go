// Package tasks provides module-level functionality for tasks.
// input: a cluster holder, a catalog of sealed LOCAL_ONLY segments, and the task directory on that holder's data_dir; a second node that has not read the directory; a second holder that has read a directory where the file is present; the recorded owner's directory made unreadable while SEGMENT_MISSING is open; an OPEN catalog row whose .open.eN file is deleted
// output: proof that deleting a sealed segment on the verified owner opens CRITICAL SEGMENT_MISSING after Confirm and Scheduler.HealthSummary becomes degraded (GET /api/health maps any status other than ok to 503), that the finding detail, ListFindings and the health reason name chain_owner, that restoring the file to the same directory closes it with EVAL, that an unverified owner change neither closes it nor writes chain_owner, and that a holder who never read the directory does not write chain_owner, that a recorded owner whose directory read fails keeps SEGMENT_MISSING open and chain_owner unchanged (QA mutant Q02), that deleting the open segment does not open SEGMENT_MISSING (QA mutant Q08), and that an unreadable owner directory is logged once when it starts and once when it ends and is listed on HealthSummary.Detail without closing the finding or changing chain_owner (QA P3 5)
// pos: ADR 0011 PR-3 acceptance for issue #462 (docs/adr/0011-cluster-ownership-and-failover.md rollout row 3 and §3.5); runs in go test
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/health"
)

func segmentMissingNode(env *p262Env, id, data string, files FileStore) (*Scheduler, *Evaluator) {
	env.t.Helper()
	s := NewScheduler(WithDataDir(data), WithStore(env.store), WithFileStore(files), WithClusterLeaseManager(env.leases), WithClusterWorkerID(id), WithFindingStore(env.hs))
	s.mu.Lock()
	s.tasks[env.task.ID] = env.task
	s.mu.Unlock()
	ev := NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorOn, Store: env.log, Cluster: true, NodeID: id, Instance: id + "-i",
		Sweep: time.Minute, Debounce: time.Millisecond,
	})
	return s, ev
}

func sealCatalog(t *testing.T, cat *lfCatalog, taskID, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := cat.UpsertBinlogFile(context.Background(), BinlogFile{
			TaskID: taskID, FileName: name, FilePath: p, Epoch: 0, State: "SEALED", UploadState: "LOCAL_ONLY",
			SizeBytes: info.Size(), CreatedAt: time.Now(), SealedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

type sealedCluster struct {
	env  *p262Env
	cat  *lfCatalog
	data string
	dir  string
	s    *Scheduler
	e    *Evaluator
	body []byte
}

func newSealedCluster(t *testing.T) *sealedCluster {
	t.Helper()
	env := newP262(t)
	data := t.TempDir()
	dir := filepath.Join(data, env.task.ID)
	p262WriteChain(t, dir, false)
	cat := newLFCatalog()
	sealCatalog(t, cat, env.task.ID, dir, "mysql-bin.000001", "mysql-bin.000002", "mysql-bin.000003")
	body, err := os.ReadFile(filepath.Join(dir, "mysql-bin.000002"))
	if err != nil {
		t.Fatal(err)
	}
	s, e := segmentMissingNode(env, "n1", data, cat)
	return &sealedCluster{env: env, cat: cat, data: data, dir: dir, s: s, e: e, body: body}
}

func (c *sealedCluster) missingOpen(t *testing.T) bool {
	t.Helper()
	_, open, _ := c.env.hs.snapshot(c.env.task.ID)
	_, ok := open[health.Key{TaskID: c.env.task.ID, Kind: health.SegmentMissing, Subject: "mysql-bin.000002"}]
	return ok
}

func (c *sealedCluster) deleteSealed(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(c.dir, "mysql-bin.000002")); err != nil {
		t.Fatal(err)
	}
}

func (c *sealedCluster) restoreSealed(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, "mysql-bin.000002"), c.body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClusterVerifiedOwnerOpensSegmentMissing(t *testing.T) {
	c := newSealedCluster(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatalf("clean pass %d: %v", i, err)
		}
	}
	if c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING opened on a complete directory")
	}
	owner, err := c.env.hs.ChainOwner(ctx, c.env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want n1 after the verified read", owner, err)
	}
	if sum := c.s.HealthSummary(ctx); sum.Status != HealthOK {
		t.Fatalf("health before the delete: %+v, want ok", sum)
	}

	c.deleteSealed(t)
	if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	if c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING opened on the first pass; Confirm needs two")
	}
	h, err := c.e.EvaluateNow(ctx, c.env.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !c.missingOpen(t) {
		_, open, _ := c.env.hs.snapshot(c.env.task.ID)
		t.Fatalf("SEGMENT_MISSING not open after Confirm: %+v", open)
	}
	if h.Status != health.StatusUnhealthy {
		t.Fatalf("task health %s, want UNHEALTHY", h.Status)
	}
	// handleAPIHealth maps every HealthSummary status other than ok to 503.
	_, open, _ := c.env.hs.snapshot(c.env.task.ID)
	stored := open[health.Key{TaskID: c.env.task.ID, Kind: health.SegmentMissing, Subject: "mysql-bin.000002"}]
	if !strings.Contains(stored.Detail, "chain_owner=n1") {
		t.Fatalf("stored detail %q, want chain_owner=n1", stored.Detail)
	}
	listed, err := c.s.ListFindings(ctx, c.env.task.ID, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	var listedDetail string
	for _, f := range listed {
		if string(f.Kind) == string(health.SegmentMissing) && f.Subject == "mysql-bin.000002" {
			listedDetail = f.Detail
		}
	}
	if !strings.Contains(listedDetail, "chain_owner=n1") {
		t.Fatalf("ListFindings detail %q, want chain_owner=n1", listedDetail)
	}
	sum := c.s.HealthSummary(ctx)
	reasons := strings.Join(sum.Reasons, " ")
	if sum.Status != HealthDegraded || !strings.Contains(reasons, "SEGMENT_MISSING/mysql-bin.000002") || !strings.Contains(reasons, "chain_owner=n1") {
		t.Fatalf("health %+v, want degraded naming SEGMENT_MISSING and chain_owner=n1", sum)
	}

	c.restoreSealed(t)
	if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	if c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING still open after the file returned")
	}
	reason, found := c.env.log.closeReason(health.SegmentMissing)
	if !found || reason != health.ReasonEval {
		t.Fatalf("close reason %q found=%v, want EVAL", reason, found)
	}
	owner, err = c.env.hs.ChainOwner(ctx, c.env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q after restore (%v), want n1", owner, err)
	}
	if sum := c.s.HealthSummary(ctx); sum.Status != HealthOK {
		t.Fatalf("health after restore: %+v, want ok", sum)
	}
}

func TestHolderWithoutDirectoryDoesNotWriteChainOwner(t *testing.T) {
	env := newP262(t)
	_, e := segmentMissingNode(env, "n1", t.TempDir(), newLFCatalog())
	if _, err := e.EvaluateNow(context.Background(), env.task.ID); err != nil {
		t.Fatal(err)
	}
	owner, err := env.hs.ChainOwner(context.Background(), env.task.ID)
	if err != nil || owner != "" {
		t.Fatalf("chain owner %q (%v), want empty when the holder did not read a directory", owner, err)
	}
}

func TestSegmentMissingUnverifiedOwnerDoesNotClose(t *testing.T) {
	c := newSealedCluster(t)
	ctx := context.Background()
	if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	c.deleteSealed(t)
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if !c.missingOpen(t) {
		t.Fatal("setup did not open SEGMENT_MISSING")
	}
	c.env.release("n1", 1)
	epoch, ok, err := c.env.leases.Acquire(ctx, c.env.task.ID, "n2", time.Hour)
	if err != nil || !ok || epoch <= 1 {
		t.Fatalf("acquire epoch=%d ok=%v err=%v", epoch, ok, err)
	}
	c.env.own("n2", epoch, StateRunning, TaskDesiredRun)
	_, e2 := segmentMissingNode(c.env, "n2", t.TempDir(), c.cat)
	if _, err := e2.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatalf("unverified holder: %v", err)
	}
	owner, err := c.env.hs.ChainOwner(ctx, c.env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want n1", owner, err)
	}
	if !c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING closed on a node that has no task directory")
	}
	if reason, found := c.env.log.closeReason(health.SegmentMissing); found {
		t.Fatalf("closed with %s", reason)
	}
}

func TestSegmentMissingVerifiedSuccessorDoesNotCloseOnFirstPass(t *testing.T) {
	c := newSealedCluster(t)
	ctx := context.Background()
	if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	c.deleteSealed(t)
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	c.env.release("n1", 1)
	epoch, ok, err := c.env.leases.Acquire(ctx, c.env.task.ID, "n2", time.Hour)
	if err != nil || !ok || epoch <= 1 {
		t.Fatalf("acquire epoch=%d ok=%v err=%v", epoch, ok, err)
	}
	c.env.own("n2", epoch, StateRunning, TaskDesiredRun)
	data2 := t.TempDir()
	p262WriteChain(t, filepath.Join(data2, c.env.task.ID), false)
	_, e2 := segmentMissingNode(c.env, "n2", data2, c.cat)
	if _, err := e2.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatalf("verified successor: %v", err)
	}
	owner, err := c.env.hs.ChainOwner(ctx, c.env.task.ID)
	if err != nil || owner != "n2" {
		t.Fatalf("chain owner %q (%v), want n2 after that node read the directory", owner, err)
	}
	if !c.missingOpen(t) {
		t.Fatal("first pass of the new holder closed SEGMENT_MISSING")
	}
	if _, err := e2.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	if c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING stayed open after the new verified directory no longer derived it")
	}
	reason, found := c.env.log.closeReason(health.SegmentMissing)
	if !found || reason != health.ReasonEval {
		t.Fatalf("close reason %q found=%v, want EVAL", reason, found)
	}
}

// TestSegmentMissingRecordedOwnerReadFailureKeepsOpen: the recorded
// chain_owner itself cannot read its directory while the finding is open.
// That pass is incomplete: no close, no chain_owner write (QA Q02).
func TestSegmentMissingRecordedOwnerReadFailureKeepsOpen(t *testing.T) {
	c := newSealedCluster(t)
	ctx := context.Background()
	if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	c.deleteSealed(t)
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if !c.missingOpen(t) {
		t.Fatal("setup did not open SEGMENT_MISSING")
	}
	away := c.dir + ".away"
	if err := os.Rename(c.dir, away); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatalf("unreadable pass %d: %v", i, err)
		}
	}
	if !c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING closed on a pass where the recorded owner could not read its directory")
	}
	if reason, found := c.env.log.closeReason(health.SegmentMissing); found {
		t.Fatalf("closed with %s on an unread directory", reason)
	}
	owner, err := c.env.hs.ChainOwner(ctx, c.env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v), want n1", owner, err)
	}
	if err := os.Rename(away, c.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	if !c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING closed once the directory was readable again but the file is still gone")
	}
}

// TestSegmentMissingNotForOpenSegment: the open segment is the writer's file,
// not a sealed copy. Deleting it fails the runner loudly; it is never
// SEGMENT_MISSING (QA Q08).
func TestSegmentMissingNotForOpenSegment(t *testing.T) {
	c := newSealedCluster(t)
	ctx := context.Background()
	openName := "mysql-bin.000004.open.e1"
	openPath := filepath.Join(c.dir, openName)
	writeAlertSegment(t, openPath, 43)
	if err := c.cat.UpsertBinlogFile(ctx, BinlogFile{
		TaskID: c.env.task.ID, FileName: "mysql-bin.000004", FilePath: openPath, Epoch: 1, State: "OPEN", UploadState: "LOCAL_ONLY",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(openPath); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, open, _ := c.env.hs.snapshot(c.env.task.ID)
	for k := range open {
		if k.Kind == health.SegmentMissing {
			t.Fatalf("deleted open segment opened %v", k)
		}
	}
}

// TestUnreadableOwnerDirectoryIsLogged: an owner directory that exists but
// cannot be read is no longer silent (QA P3 5). One line when it starts, one
// when it ends. HealthSummary.Detail carries the same line and does not
// change status. The open finding and chain_owner stay put. mode 000 is the
// permission failure; a file standing in for the directory is the same
// condition when the process can still read mode 000 (root).
func TestUnreadableOwnerDirectoryIsLogged(t *testing.T) {
	t.Run("mode 000", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode 000 directory")
		}
		assertUnreadableOwnerDirectory(t, func(t *testing.T, dir string) func() {
			t.Helper()
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			var once bool
			return func() {
				if once {
					return
				}
				once = true
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Errorf("restore %s: %v", dir, err)
				}
			}
		})
	})
	t.Run("not a directory", func(t *testing.T) {
		assertUnreadableOwnerDirectory(t, func(t *testing.T, dir string) func() {
			t.Helper()
			kept := dir + ".kept"
			if err := os.Rename(dir, kept); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dir, []byte("not-a-directory"), 0o644); err != nil {
				t.Fatal(err)
			}
			var once bool
			return func() {
				if once {
					return
				}
				once = true
				if err := os.Remove(dir); err != nil {
					t.Errorf("remove stand-in %s: %v", dir, err)
				}
				if err := os.Rename(kept, dir); err != nil {
					t.Errorf("restore %s: %v", dir, err)
				}
			}
		})
	})
}

func assertUnreadableOwnerDirectory(t *testing.T, block func(t *testing.T, dir string) func()) {
	t.Helper()
	c := newSealedCluster(t)
	ctx := context.Background()
	if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
		t.Fatal(err)
	}
	c.deleteSealed(t)
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	restore := block(t, c.dir)
	t.Cleanup(restore)
	for i := 0; i < 3; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "directory "+c.dir+" is unreadable"); n != 1 {
		t.Fatalf("unreadable logged %d times, want 1:\n%s", n, buf.String())
	}
	if !c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING closed while the owner directory was unreadable")
	}
	owner, err := c.env.hs.ChainOwner(ctx, c.env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v) while unreadable, want n1", owner, err)
	}
	sum := c.s.HealthSummary(ctx)
	detail := strings.Join(sum.Detail, "\n")
	if sum.Status != HealthDegraded || !strings.Contains(detail, "directory "+c.dir+" is unreadable") {
		t.Fatalf("health %+v, want degraded with the unreadable directory in detail", sum)
	}
	if strings.Contains(strings.Join(sum.Reasons, " "), "is unreadable") {
		t.Fatalf("unreadable directory changed status via reasons: %+v", sum)
	}
	restore()
	for i := 0; i < 2; i++ {
		if _, err := c.e.EvaluateNow(ctx, c.env.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "directory "+c.dir+" is readable again"); n != 1 {
		t.Fatalf("readable again logged %d times, want 1:\n%s", n, buf.String())
	}
	if !c.missingOpen(t) {
		t.Fatal("SEGMENT_MISSING closed when the directory became readable but the file was still gone")
	}
	owner, err = c.env.hs.ChainOwner(ctx, c.env.task.ID)
	if err != nil || owner != "n1" {
		t.Fatalf("chain owner %q (%v) after the directory was readable again, want n1", owner, err)
	}
	sum = c.s.HealthSummary(ctx)
	if strings.Contains(strings.Join(sum.Detail, "\n"), "is unreadable") {
		t.Fatalf("health detail still names an unreadable directory: %+v", sum)
	}
	if sum.Status != HealthDegraded {
		t.Fatalf("health %+v, want degraded while the segment is still missing", sum)
	}
}
