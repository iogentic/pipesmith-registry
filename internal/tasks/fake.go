package tasks

import (
	"sync"
	"time"
)

// FakeClock is a Clock for tests. Its time moves only by Advance, and
// Advance calls the function of each timer that comes due, in the order of
// their instants and at one instant in the order the timers were made. The
// function runs in the goroutine of Advance; the time is then the instant of
// its timer. A FakeClock is safe for use by several goroutines.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	made   int
}

type fakeTimer struct {
	clock *FakeClock
	when  time.Time
	order int
	f     func()
}

// NewFakeClock is a fake clock at now.
func NewFakeClock(now time.Time) *FakeClock { return &FakeClock{now: now} }

// Now is the time of the clock.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc makes a timer that comes due d after the time of the clock.
func (c *FakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.made++
	t := &fakeTimer{clock: c, when: c.now.Add(max(d, 0)), order: c.made, f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, waiting := range c.timers {
		if waiting == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			return true
		}
	}
	return false
}

// Step moves the time of the clock by d, as a change of the system time
// does: it calls no timer, and each timer keeps the time it has left, so it
// comes due d later, or earlier for a negative d, than before.
func (c *FakeClock) Step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		t.when = t.when.Add(d)
	}
}

// Advance moves the time of the clock by d and calls the timers that come
// due on the way, a timer made by such a call among them.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(max(d, 0))
	c.mu.Unlock()
	for {
		c.mu.Lock()
		due := -1
		for i, t := range c.timers {
			if t.when.After(end) {
				continue
			}
			if due < 0 || t.when.Before(c.timers[due].when) || t.when.Equal(c.timers[due].when) && t.order < c.timers[due].order {
				due = i
			}
		}
		if due < 0 {
			c.now = end
			c.mu.Unlock()
			return
		}
		t := c.timers[due]
		c.timers = append(c.timers[:due], c.timers[due+1:]...)
		if t.when.After(c.now) {
			c.now = t.when
		}
		c.mu.Unlock()
		t.f()
	}
}
