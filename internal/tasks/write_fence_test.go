package tasks

import (
	"testing"
	"time"
)

func TestLeaseClock_FreshWindowLoseAndEpochs(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewLeaseClock()
	c.now = func() time.Time { return now }
	if c.Fresh("1", 2) {
		t.Fatal("unknown task is fresh")
	}
	c.Confirm("1", 2, now, 10*time.Second)
	if !c.Fresh("1", 2) || c.Fresh("1", 3) {
		t.Fatal("confirmed epoch 2 should be fresh, epoch 3 not")
	}
	now = now.Add(7 * time.Second)
	if !c.Fresh("1", 2) {
		t.Fatal("inside 4/5 of ttl should be fresh")
	}
	now = now.Add(2 * time.Second) // 9s > 8s: frozen past the trusted window
	if c.Fresh("1", 2) {
		t.Fatal("past 4/5 of ttl should be stale")
	}
	c.Confirm("1", 2, now, 10*time.Second)
	if !c.Fresh("1", 2) {
		t.Fatal("renew should refresh")
	}
	c.Lose("1", 2)
	c.Confirm("1", 2, now, 10*time.Second)
	if c.Fresh("1", 2) {
		t.Fatal("a lost epoch must not come back")
	}
	c.Confirm("1", 3, now, 10*time.Second)
	if !c.Fresh("1", 3) {
		t.Fatal("new epoch should be fresh")
	}
	c.Confirm("1", 2, now, 10*time.Second)
	if c.Fresh("1", 2) || !c.Fresh("1", 3) {
		t.Fatal("an older epoch must not replace a newer one")
	}
}
