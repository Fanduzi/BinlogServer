// Package tasks provides module-level functionality for tasks.
// input: a start time and durations the test advances by
// output: a clock whose Now and After replace the wall clock for evaluator waits and metadata pings
// pos: test double for EvaluatorConfig.Now and EvaluatorConfig.After
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"sync"
	"time"
)

// manualClock is a test clock. After does not sleep; Advance fires every
// waiter whose deadline has arrived. Production timers stay on time.Timer
// unless a test sets EvaluatorConfig.After to Clock.After.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []manualWaiter
}

type manualWaiter struct {
	at time.Time
	ch chan time.Time
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, manualWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and delivers every waiter that is due.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var left, fire []manualWaiter
	for _, w := range c.waiters {
		if !w.at.After(now) {
			fire = append(fire, w)
		} else {
			left = append(left, w)
		}
	}
	c.waiters = left
	c.mu.Unlock()
	for _, w := range fire {
		w.ch <- now
	}
}

// WaiterCount is how many After calls are still parked.
func (c *manualClock) WaiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// HasDeadline reports whether a parked waiter is due at at.
func (c *manualClock) HasDeadline(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waiters {
		if w.at.Equal(at) {
			return true
		}
	}
	return false
}
