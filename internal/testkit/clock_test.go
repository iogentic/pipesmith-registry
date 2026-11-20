package testkit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

var clockStart = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)

// The fake clock starts where it is told, moves only by Advance and Set,
// and fits a field of the convention of the code base: a func() time.Time.
func TestClockMovesOnlyWhenTold(t *testing.T) {
	c := testkit.NewClock(clockStart)
	var now func() time.Time = c.Now
	if got := now(); !got.Equal(clockStart) {
		t.Fatalf("start %v, want %v", got, clockStart)
	}
	time.Sleep(10 * time.Millisecond)
	if got := now(); !got.Equal(clockStart) {
		t.Fatalf("the clock moved by itself to %v", got)
	}
	if got := c.Advance(90 * time.Second); !got.Equal(clockStart.Add(90 * time.Second)) {
		t.Fatalf("Advance gave %v", got)
	}
	if got := now(); !got.Equal(clockStart.Add(90 * time.Second)) {
		t.Fatalf("after Advance %v", got)
	}
	back := clockStart.Add(-time.Hour)
	c.Set(back)
	if got := c.Now(); !got.Equal(back) {
		t.Fatalf("after Set %v, want %v", got, back)
	}
	if got := c.Advance(-time.Minute); !got.Equal(back.Add(-time.Minute)) {
		t.Fatalf("a negative Advance gave %v", got)
	}
}

// Eight goroutines advance and eight read at once under -race: every
// reader sees the time move forward only, and the end is the sum of all
// steps.
func TestClockIsSafeForParallelUse(t *testing.T) {
	c := testkit.NewClock(clockStart)
	const writers, readers, steps = 8, 8, 500
	var wg sync.WaitGroup
	errs := make(chan string, readers)
	stop := make(chan struct{})
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range steps {
				c.Advance(time.Millisecond)
			}
		}()
	}
	var rg sync.WaitGroup
	for range readers {
		rg.Add(1)
		go func() {
			defer rg.Done()
			last := c.Now()
			for {
				select {
				case <-stop:
					return
				default:
				}
				now := c.Now()
				if now.Before(last) {
					errs <- now.String() + " before " + last.String()
					return
				}
				last = now
			}
		}()
	}
	wg.Wait()
	close(stop)
	rg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if got, want := c.Now(), clockStart.Add(writers*steps*time.Millisecond); !got.Equal(want) {
		t.Fatalf("end %v, want %v", got, want)
	}
}
