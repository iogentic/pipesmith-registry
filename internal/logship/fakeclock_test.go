package logship

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock is the clock of a test: its time moves only by Advance, and the
// channels of After fire when it passes their time.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the time by d and fires every channel whose time came.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// pending lists the waits from now, shortest first.
func (c *fakeClock) pending() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, w := range c.waiters {
		out = append(out, w.at.Sub(c.now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// waitPending waits, in real time, until something waits for d from now.
func (c *fakeClock) waitPending(t *testing.T, d time.Duration) {
	t.Helper()
	waitFor(t, func() bool {
		for _, p := range c.pending() {
			if p == d {
				return true
			}
		}
		return false
	}, "a wait of "+d.String()+" on the fake clock")
}
