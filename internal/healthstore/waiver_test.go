package healthstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/health"
)

func TestFileStore_WaiverRoundTrip(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), ".health.json"))
	fence := health.Fence{Mode: health.FenceStandalone, Node: "n", Instance: "i"}
	ctx := context.Background()
	// Seed an open finding via ApplyTransitions so the row exists.
	_, err := s.ApplyTransitions(ctx, "1", fence, []health.Transition{{
		Key: health.Key{TaskID: "1", Kind: health.GTIDHole, Subject: "mysql-bin.000004"},
		Op:  health.OpOpen, Reason: health.ReasonEval, At: time.Now().UTC(),
		Instance: health.Instance{Key: health.Key{TaskID: "1", Kind: health.GTIDHole, Subject: "mysql-bin.000004"},
			Severity: health.Critical, Evidence: "aaaaaaaa-0000-0000-0000-000000000001:10-12", Detail: "hole"},
	}}, health.TaskHealth{Status: health.StatusUnhealthy})
	if err != nil {
		t.Fatal(err)
	}
	w := health.Waiver{Key: health.Key{TaskID: "1", Kind: health.GTIDHole, Subject: "mysql-bin.000004"},
		Evidence: "aaaaaaaa-0000-0000-0000-000000000001:10-12", Reason: "gtid_next skip", Actor: "dba", At: time.Now().UTC()}
	if err := s.PutWaiver(ctx, "1", fence, w); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListWaivers(ctx, "1")
	if err != nil || len(list) != 1 || list[0].Evidence != w.Evidence || list[0].Actor != "dba" {
		t.Fatalf("list %+v err %v", list, err)
	}
	// Close the finding; waiver must still list.
	_, err = s.ApplyTransitions(ctx, "1", fence, []health.Transition{{
		Key: w.Key, Op: health.OpClose, Reason: health.ReasonWaived, At: time.Now().UTC(),
		Instance: health.Instance{Key: w.Key, Severity: health.Critical, Evidence: w.Evidence},
	}}, health.TaskHealth{Status: health.StatusHealthy})
	if err != nil {
		t.Fatal(err)
	}
	list, err = s.ListWaivers(ctx, "1")
	if err != nil || len(list) != 1 {
		t.Fatalf("after close list %+v err %v", list, err)
	}
	if err := s.DeleteWaiver(ctx, "1", fence, w.Key); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListWaivers(ctx, "1")
	if err != nil || len(list) != 0 {
		t.Fatalf("after delete list %+v err %v", list, err)
	}
}
