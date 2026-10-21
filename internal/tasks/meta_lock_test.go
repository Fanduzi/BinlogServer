// Package tasks provides module-level functionality for tasks.
// input: two lock-wait observations and later acquisitions of the same row
// output: proof that the first observation stays healthy, the second opens META_LOCK_STALLED, a successful acquisition resets that count, two takes inside lock_wait close it, another node does not, a late take restarts the count, and lock_detail=reduced does not degrade health
// pos: ADR 0011 PR-2 F12 confirm and the close rule
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/ownership"
)

func lockParams() ownership.Params {
	p := ownership.DefaultParams()
	p.LockWait = time.Second
	return p
}

type lockDetailStore struct {
	*fakeStore
	detail string
}

func (s lockDetailStore) LockDetail() string { return s.detail }

func TestReducedLockDetailStaysHealthy(t *testing.T) {
	s := NewScheduler(WithStore(lockDetailStore{fakeStore: newFakeStore(), detail: "reduced"}))
	sum := s.HealthSummary(context.Background())
	if sum.Status != HealthOK {
		t.Fatalf("status %s reasons %v, want ok", sum.Status, sum.Reasons)
	}
	if sum.LockDetail != "reduced" {
		t.Fatalf("lock_detail %q", sum.LockDetail)
	}
}

func TestMetaLockFirstTimeoutIsNotAPage(t *testing.T) {
	s := NewScheduler()
	now := time.Now()
	if opened := s.NoteMetaLockTimeout("t1", "n1", now, lockParams()); opened {
		t.Fatal("first observation opened the finding")
	}
	sum := s.HealthSummary(context.Background())
	if sum.Status != HealthOK {
		t.Fatalf("status %s reasons %v, want ok", sum.Status, sum.Reasons)
	}
}

func TestMetaLockSecondTimeoutDegradesHealth(t *testing.T) {
	s := NewScheduler()
	now := time.Now()
	p := lockParams()
	s.NoteMetaLockTimeout("t1", "n1", now, p)
	if opened := s.NoteMetaLockTimeout("t1", "n1", now.Add(time.Millisecond), p); !opened {
		t.Fatal("second observation did not open")
	}
	sum := s.HealthSummary(context.Background())
	if sum.Status != HealthDegraded {
		t.Fatalf("status %s, want degraded", sum.Status)
	}
	found := false
	for _, r := range sum.Reasons {
		if strings.Contains(r, "META_LOCK_STALLED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons %v, want META_LOCK_STALLED", sum.Reasons)
	}
}

func TestMetaLockSuccessResetsTheCount(t *testing.T) {
	s := NewScheduler()
	now := time.Now()
	p := lockParams()
	if opened := s.NoteMetaLockTimeout("t1", "n1", now, p); opened {
		t.Fatal("first observation opened the finding")
	}
	s.NoteMetaLockHeld("t1", "n1", now, p, false, time.Hour, nil)
	if opened := s.NoteMetaLockTimeout("t1", "n1", now.Add(time.Millisecond), p); opened {
		t.Fatal("a successful acquisition did not reset the count")
	}
	if s.HealthSummary(context.Background()).Status != HealthOK {
		t.Fatal("one timeout after a success degraded health")
	}
}

func TestMetaLockTwoTakesClose(t *testing.T) {
	s := NewScheduler()
	now := time.Now()
	p := lockParams()
	s.NoteMetaLockTimeout("t1", "n1", now, p)
	s.NoteMetaLockTimeout("t1", "n1", now, p)
	s.NoteMetaLockHeld("t1", "n2", now, p, false, time.Hour, nil)
	sum := s.HealthSummary(context.Background())
	if sum.Status != HealthDegraded {
		t.Fatal("another node closed the finding")
	}
	s.NoteMetaLockHeld("t1", "n1", now, p, false, time.Hour, nil)
	if !s.MetaLockAwaitingSecond("t1", "n1") {
		t.Fatal("first take did not start the close count")
	}
	s.NoteMetaLockHeld("t1", "n1", now.Add(10*time.Millisecond), p, false, time.Hour, nil)
	sum = s.HealthSummary(context.Background())
	if sum.Status != HealthOK {
		t.Fatalf("after two takes: %+v", sum)
	}
}

func TestMetaLockWaitRestartsTheCount(t *testing.T) {
	s := NewScheduler()
	now := time.Now()
	p := lockParams()
	s.NoteMetaLockTimeout("t1", "n1", now, p)
	s.NoteMetaLockTimeout("t1", "n1", now, p)
	s.NoteMetaLockHeld("t1", "n1", now, p, false, time.Hour, nil)
	late := now.Add(p.LockWait + time.Millisecond)
	s.NoteMetaLockHeld("t1", "n1", late, p, false, time.Hour, nil)
	if s.HealthSummary(context.Background()).Status != HealthDegraded {
		t.Fatal("a take past lock_wait closed the finding")
	}
	s.NoteMetaLockHeld("t1", "n1", late.Add(time.Millisecond), p, false, time.Hour, nil)
	if s.HealthSummary(context.Background()).Status != HealthOK {
		t.Fatal("the restarted pair did not close")
	}
}

func TestMetaLockHealthHolderClosesAfterObserverGone(t *testing.T) {
	s := NewScheduler()
	now := time.Now()
	p := lockParams()
	p.NodeGoneAfter = time.Minute
	s.NoteMetaLockTimeout("t1", "n1", now, p)
	s.NoteMetaLockTimeout("t1", "n1", now, p)
	seen := func(id string) (time.Time, bool) {
		if id == "n1" {
			return now.Add(-2 * time.Minute), true
		}
		return time.Time{}, false
	}
	s.NoteMetaLockHeld("t1", "n2", now, p, false, time.Minute, seen)
	if s.HealthSummary(context.Background()).Status != HealthDegraded {
		t.Fatal("a non-holder closed the finding")
	}
	s.NoteMetaLockHeld("t1", "n2", now, p, true, time.Minute, seen)
	s.NoteMetaLockHeld("t1", "n2", now.Add(time.Millisecond), p, true, time.Minute, seen)
	if s.HealthSummary(context.Background()).Status != HealthOK {
		t.Fatal("the _health holder did not close after two takes")
	}
}
