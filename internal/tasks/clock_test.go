package tasks

import (
	"slices"
	"testing"
	"time"
)

// Advance calls the timers that come due in the order of their instants,
// and at one instant in the order they were made; the time is the instant of
// each call and at the end the time Advance reached.
func TestFakeClockFiresDueTimersInOrder(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	var calls []string
	note := func(name string) func() {
		return func() { calls = append(calls, name+" "+c.Now().Format("15:04:05")) }
	}
	c.AfterFunc(3*time.Second, note("c"))
	c.AfterFunc(time.Second, note("a"))
	c.AfterFunc(3*time.Second, note("d"))
	c.AfterFunc(2*time.Second, note("b"))
	late := c.AfterFunc(10*time.Second, note("late"))
	stopped := c.AfterFunc(2*time.Second, note("stopped"))
	if !stopped.Stop() || stopped.Stop() {
		t.Fatal("Stop of a waiting timer is true once, then false")
	}
	c.Advance(3 * time.Second)
	want := []string{"a 10:00:01", "b 10:00:02", "c 10:00:03", "d 10:00:03"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	if got := c.Now(); !got.Equal(start.Add(3 * time.Second)) {
		t.Fatalf("time %s after Advance", got)
	}
	c.Advance(2 * time.Second)
	if len(calls) != 4 || !c.Now().Equal(start.Add(5*time.Second)) {
		t.Fatalf("calls %v at %s, want none more at 10:00:05", calls, c.Now())
	}
	if !late.Stop() {
		t.Fatal("the timer of 10:00:10 was not waiting")
	}
	c.Advance(time.Hour)
	if len(calls) != 4 {
		t.Fatalf("a stopped timer was called: %v", calls)
	}
}

// A timer that a call makes fires in the same Advance when it comes due
// before its end; a timer of no duration fires at the next Advance.
func TestFakeClockTimersOfCallsAndOfNoDuration(t *testing.T) {
	c := NewFakeClock(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	var calls []string
	var again func()
	again = func() {
		calls = append(calls, c.Now().Format("15:04"))
		if len(calls) < 3 {
			c.AfterFunc(time.Minute, again)
		}
	}
	c.AfterFunc(time.Minute, again)
	c.Advance(10 * time.Minute)
	if want := []string{"10:01", "10:02", "10:03"}; !slices.Equal(calls, want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	now := false
	c.AfterFunc(0, func() { now = true })
	if now {
		t.Fatal("a timer of no duration fired before Advance")
	}
	c.Advance(0)
	if !now {
		t.Fatal("a timer of no duration did not fire at Advance(0)")
	}
}

// Step moves the time and calls no timer; a timer keeps the time it has
// left and comes due that long after the step.
func TestFakeClockStep(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	for _, step := range []time.Duration{time.Hour, -time.Hour} {
		c := NewFakeClock(start)
		var fired []time.Time
		c.AfterFunc(10*time.Minute, func() { fired = append(fired, c.Now()) })
		c.Step(step)
		if !c.Now().Equal(start.Add(step)) || len(fired) != 0 {
			t.Fatalf("after a step of %s: time %s, calls %v; want %s and none", step, c.Now(), fired, start.Add(step))
		}
		c.Advance(10*time.Minute - time.Second)
		if len(fired) != 0 {
			t.Fatalf("after a step of %s the timer fired %s early", step, time.Second)
		}
		c.Advance(time.Second)
		if want := start.Add(step + 10*time.Minute); len(fired) != 1 || !fired[0].Equal(want) {
			t.Fatalf("after a step of %s the timer fired at %v, want once at %s", step, fired, want)
		}
	}
}

// The real clock reads the time of the machine and its timer calls once.
func TestRealClock(t *testing.T) {
	c := RealClock()
	if d := time.Since(c.Now()); d < 0 || d > time.Minute {
		t.Fatalf("Now is %s off", d)
	}
	fired := make(chan struct{})
	c.AfterFunc(time.Millisecond, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the timer did not fire within 5 s")
	}
	if !c.AfterFunc(time.Hour, func() { t.Error("a stopped timer fired") }).Stop() {
		t.Fatal("Stop of a waiting timer is false")
	}
}
