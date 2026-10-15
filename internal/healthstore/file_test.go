package healthstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/health"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/healthstore/storetest"
)

func TestFileStoreContract(t *testing.T) {
	storetest.Run(t, storetest.Harness{New: func(t *testing.T) healthstore.Store {
		return healthstore.NewFileStore(filepath.Join(t.TempDir(), healthstore.HealthFileName))
	}})
}

func TestFileStoreSurvivesRestartAndKeepsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), healthstore.HealthFileName)
	ctx := context.Background()
	at := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	k := health.Key{TaskID: "1", Kind: health.SegmentOverlap, Subject: "a"}
	tr := health.Transition{Key: k, Op: health.OpOpen, At: at, FactAt: at, Instance: health.Instance{Key: k, Severity: health.Critical}}
	s := healthstore.NewFileStore(path)
	if _, err := s.ApplyTransitions(ctx, "1", health.Fence{Mode: health.FenceStandalone, Node: "n1", Instance: "i1"}, []health.Transition{tr}, health.TaskHealth{TaskID: "1", Status: health.StatusUnhealthy}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePending("1", []health.Transition{tr}); err != nil {
		t.Fatal(err)
	}
	again := healthstore.NewFileStore(path)
	got, err := again.ListOpenFindingsForTask(ctx, "1")
	if err != nil || len(got) != 1 {
		t.Fatalf("after restart %+v %v", got, err)
	}
	p, err := again.LoadPending()
	if err != nil || len(p["1"]) != 1 {
		t.Fatalf("pending after restart %+v %v", p, err)
	}
	if _, err := again.ApplyTransitions(ctx, "1", health.Fence{Mode: health.FenceCluster, TaskEpoch: 1, Node: "n1"}, nil, health.TaskHealth{TaskID: "1"}); !errors.Is(err, healthstore.ErrModeUnsupported) {
		t.Fatalf("cluster fence on the file store: %v", err)
	}
}

func TestCheckFence(t *testing.T) {
	st := func(mode health.FenceMode, epoch int64, node string) *healthstore.Stored {
		return &healthstore.Stored{Mode: mode, Epoch: epoch, Node: node}
	}
	cases := []struct {
		name   string
		f      health.Fence
		lease  *healthstore.Lease
		stored *healthstore.Stored
		want   error
	}{
		{"holder live", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a"}, &healthstore.Lease{Owner: "a", Epoch: 2}, nil, nil},
		{"holder lagging", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a"}, &healthstore.Lease{Owner: "a", Epoch: 2, Expired: true}, nil, healthstore.ErrLeaseLagging},
		{"holder other owner", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a"}, &healthstore.Lease{Owner: "b", Epoch: 3}, nil, healthstore.ErrFenced},
		{"cluster no lease row", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a"}, nil, nil, healthstore.ErrFenced},
		{"stored newer epoch", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a"}, &healthstore.Lease{Owner: "a", Epoch: 2}, st(health.FenceCluster, 3, "b"), healthstore.ErrFenced},
		{"stored same epoch other node", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a"}, &healthstore.Lease{Owner: "a", Epoch: 2}, st(health.FenceCluster, 2, "b"), healthstore.ErrFenced},
		{"proxy expired", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a", Proxy: true}, &healthstore.Lease{Owner: "a", Epoch: 2, Expired: true}, st(health.FenceCluster, 2, "a"), nil},
		{"proxy released", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a", Proxy: true}, &healthstore.Lease{Owner: "", Epoch: 2, Expired: true}, nil, nil},
		{"proxy on a live lease", health.Fence{Mode: health.FenceCluster, TaskEpoch: 2, Node: "a", Proxy: true}, &healthstore.Lease{Owner: "a", Epoch: 2}, nil, healthstore.ErrFenced},
		{"never no row", health.Fence{Mode: health.FenceNeverLeased, Node: "h"}, nil, nil, nil},
		{"never with row", health.Fence{Mode: health.FenceNeverLeased, Node: "h"}, &healthstore.Lease{Owner: "a", Epoch: 1}, nil, healthstore.ErrFenced},
		{"standalone first", health.Fence{Mode: health.FenceStandalone, Node: "n", Instance: "i"}, nil, nil, nil},
		{"standalone restart", health.Fence{Mode: health.FenceStandalone, Node: "n", Instance: "j"}, nil, &healthstore.Stored{Mode: health.FenceStandalone, Node: "n", Instance: "i"}, nil},
		{"standalone other node", health.Fence{Mode: health.FenceStandalone, Node: "m", Instance: "j"}, nil, &healthstore.Stored{Mode: health.FenceStandalone, Node: "n", Instance: "i"}, healthstore.ErrFenced},
		{"standalone after cluster", health.Fence{Mode: health.FenceStandalone, Node: "n"}, nil, st(health.FenceCluster, 4, "a"), healthstore.ErrFenced},
		{"cluster after standalone", health.Fence{Mode: health.FenceCluster, TaskEpoch: 1, Node: "a"}, &healthstore.Lease{Owner: "a", Epoch: 1}, &healthstore.Stored{Mode: health.FenceStandalone, Node: "n"}, nil},
	}
	for _, c := range cases {
		if err := healthstore.CheckFence(c.f, c.lease, c.stored); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}
