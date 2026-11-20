package testkit

import (
	"sync"
	"time"
)

// Clock is a fake clock for code that takes the time from a func() time.Time
// field, where nil means time.Now: pass Clock.Now. It moves only by Advance
// and Set and is safe for use by several goroutines.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock that stands at start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now is the time of the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock by d, which may be negative, and returns the new
// time.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set puts the clock at t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
