package server

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
)

// fakeWalks is a walk of the marked blobs whose runs the test counts and
// may hold until it releases them.
type fakeWalks struct {
	calls   atomic.Int64
	running atomic.Int64
	most    atomic.Int64
	hold    chan struct{} // nil: a walk returns at once
	count   atomic.Int64
}

func (f *fakeWalks) walk(ctx context.Context, _, _ string) (markedResult, error) {
	f.calls.Add(1)
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		m := f.most.Load()
		if n <= m || f.most.CompareAndSwap(m, n) {
			break
		}
	}
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return markedResult{}, ctx.Err()
		}
	}
	c := f.count.Load()
	return markedResult{Count: c, Bytes: 10 * c}, nil
}

func newTestCounter(t *testing.T, f *fakeWalks, wait time.Duration) *markedCounter {
	t.Helper()
	c := newMarkedCounter()
	c.walk, c.wait = f.walk, wait
	t.Cleanup(c.close)
	return c
}

// Ruling SR12 (finding F2 of the test host): a request waits a bounded
// time; before the first walk ends it answers counting without a count,
// after it with the count.
func TestAMarkedCountAnswersInBoundedTime(t *testing.T) {
	f := &fakeWalks{hold: make(chan struct{})}
	f.count.Store(3)
	c := newTestCounter(t, f, 30*time.Millisecond)
	start := time.Now()
	got := c.get("demo-big", "/x")
	if took := time.Since(start); took > 2*time.Second || got.have || !got.counting || got.err != nil {
		t.Fatalf("%+v after %v", got, took)
	}
	close(f.hold)
	c.wait = 5 * time.Second
	got = c.get("demo-big", "/x")
	if !got.have || got.counting || got.result.Count != 3 || got.result.Bytes != 30 || got.result.At.IsZero() {
		t.Fatalf("%+v", got)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d walks, want 1", n)
	}
}

// Ruling SR15: without catalog.sqlite the count of a walk is kept and the
// deltas move it; no request walks again.
func TestAMarkedCountMovesByDeltas(t *testing.T) {
	f := &fakeWalks{}
	f.count.Store(3)
	c := newTestCounter(t, f, 5*time.Second)
	for range 3 {
		if got := c.get("demo-a", "/a"); !got.have || got.result.Count != 3 {
			t.Fatalf("%+v", got)
		}
	}
	c.delta("demo-a", 1, 10)
	c.delta("demo-b", 1, 1)
	if got := c.get("demo-a", "/a"); got.result.Count != 4 || got.result.Bytes != 40 {
		t.Fatalf("before the flush: %+v", got)
	}
	c.flush()
	c.delta("demo-a", -2, -20)
	c.flush()
	if got := c.get("demo-a", "/a"); got.result.Count != 2 || got.result.Bytes != 20 || got.counting {
		t.Fatalf("after the flushes: %+v", got)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d walks, want 1", n)
	}
}

// Ruling SR12: at most two walks run at a time, and one per store; the
// walks stop when the counter closes.
func TestMarkedCountsWalkTwoAtATime(t *testing.T) {
	f := &fakeWalks{hold: make(chan struct{})}
	c := newTestCounter(t, f, 10*time.Millisecond)
	var wg sync.WaitGroup
	for _, name := range []string{"demo-a", "demo-b", "demo-c", "demo-a", "demo-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.get(name, "/"+name)
		}()
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)
	if n := f.most.Load(); n != 2 {
		t.Fatalf("%d walks at a time, want 2", n)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("%d walks started, want 2 (the third waits for a slot)", n)
	}
	c.close()
	deadline := time.Now().Add(2 * time.Second)
	for f.running.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.running.Load(); n != 0 {
		t.Fatalf("%d walks run after the close", n)
	}
}

// Review H1: a walk that fails is answered with its error until a new try
// after markedRetry, and no walk starts at every request meanwhile.
func TestAFailedMarkedCountIsAnswered(t *testing.T) {
	f := &fakeWalks{hold: make(chan struct{})}
	c := newTestCounter(t, f, 10*time.Millisecond)
	c.walk = func(ctx context.Context, name, root string) (markedResult, error) {
		_, _ = f.walk(ctx, name, root)
		return markedResult{}, errors.New("demo read error")
	}
	if got := c.get("demo-bad", "/bad"); !got.counting || got.err != nil {
		t.Fatalf("first: %+v", got)
	}
	close(f.hold)
	deadline := time.Now().Add(2 * time.Second)
	for f.running.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for range 3 {
		if got := c.get("demo-bad", "/bad"); got.err == nil || got.counting {
			t.Fatalf("after the failure: %+v", got)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d walks, want 1 within markedRetry", n)
	}
	now := time.Now()
	c.now = func() time.Time { return now.Add(markedRetry + time.Second) }
	c.get("demo-bad", "/bad")
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("%d walks after markedRetry, want 2", n)
	}
}

// Review H6: a new root of a store stops the walk of the old one, so that
// one walk per store runs.
func TestANewRootStopsTheOldWalk(t *testing.T) {
	f := &fakeWalks{hold: make(chan struct{})}
	c := newTestCounter(t, f, 10*time.Millisecond)
	c.get("demo-a", "/old")
	c.get("demo-a", "/new")
	deadline := time.Now().Add(2 * time.Second)
	for f.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := f.running.Load(); n != 1 {
		t.Fatalf("%d walks run for one store", n)
	}
}

// Review H5: a store whose folder is missing has no count; a store whose
// content/ is missing has none marked.
func TestTheWalkOfAMissingStoreFails(t *testing.T) {
	dir := t.TempDir()
	if r, err := walkMarked(context.Background(), "demo-new", dir); err != nil || r.Count != 0 {
		t.Fatalf("a store without content/: %+v %v", r, err)
	}
	if _, err := walkMarked(context.Background(), "demo-gone", filepath.Join(dir, "gone")); err == nil {
		t.Fatal("a missing folder was counted")
	}
}

// Review P4: a delta that comes while the walk of a store without a row
// runs moves the row the walk writes.
func TestADeltaDuringTheWalkMovesItsRow(t *testing.T) {
	cat, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	f := &fakeWalks{hold: make(chan struct{})}
	f.count.Store(3)
	c := newTestCounter(t, f, 10*time.Millisecond)
	c.rows = func() (*catalog.Catalog, error) { return cat, nil }
	if got := c.get("demo-a", "/a"); !got.counting {
		t.Fatalf("%+v", got)
	}
	c.delta("demo-a", 1, 10)
	c.flush()
	close(f.hold)
	c.wait = 5 * time.Second
	if got := c.get("demo-a", "/a"); got.counting || got.result.Count != 4 || got.result.Bytes != 40 {
		t.Fatalf("after the walk %+v", got)
	}
	c.flush()
	if m, found, err := cat.StoreMarks("demo-a"); err != nil || !found || m.Blobs != 4 || m.Bytes != 40 {
		t.Fatalf("row %+v %v %v", m, found, err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d walks, want 1", n)
	}
}
