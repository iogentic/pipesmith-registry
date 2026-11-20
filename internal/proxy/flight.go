package proxy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
)

// flights let one fetch of a repository and a path run at a time (decision
// 5.8 of the phase 5 plan): a later caller of the same key waits for the
// running fetch and takes its result. A fetch runs in a goroutine of its own,
// so it goes on when every caller left.
type flights struct {
	mu      sync.Mutex
	running map[flightKey]*flight
	// limit bounds the fetches that run or wait for a slot; 0 is
	// MaxFlights
	limit int
	// stopped takes the panic of a fetch, once, before its callers get 502;
	// nil takes none
	stopped func(key flightKey, v any)
}

type flightKey struct{ repo, path string }

// MaxFlights bounds the fetches of a process that run or wait for a slot
// (finding 5-2 of review 5): a query of a new path past it starts nothing
// and fails with ErrBusy, so the read path serves the cache or answers 502.
const MaxFlights = 4096

// ErrBusy is the refusal of a fetch past MaxFlights.
var ErrBusy = errors.New("too many fetches of remotes wait; try again later")

// count is the number of fetches that run or wait.
func (g *flights) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.running)
}

type flight struct {
	done chan struct{}
	res  result
}

// do runs fn for key unless a fetch of key runs already, and waits for the
// result or for the end of ctx, whichever comes first.
func (g *flights) do(ctx context.Context, key flightKey, fn func() result) (result, error) {
	g.mu.Lock()
	if g.running == nil {
		g.running = map[flightKey]*flight{}
	}
	f := g.running[key]
	if f == nil && len(g.running) >= cmp.Or(g.limit, MaxFlights) {
		g.mu.Unlock()
		return result{kind: resultFailed, err: ErrBusy}, nil
	}
	if f == nil {
		f = &flight{done: make(chan struct{})}
		g.running[key] = f
		go g.run(key, f, fn)
	}
	g.mu.Unlock()
	select {
	case <-f.done:
		return f.res, nil
	case <-ctx.Done():
		return result{}, ctx.Err()
	}
}

// run is one fetch. The key is free again before the callers see the
// result, so a caller that comes after it starts a fetch of its own, which
// finds the stored copy first.
func (g *flights) run(key flightKey, f *flight, fn func() result) {
	defer func() {
		if v := recover(); v != nil {
			f.res = result{kind: resultFailed, err: fmt.Errorf("the fetch of %q stopped: %v", key.path, v)}
			if g.stopped != nil {
				g.stopped(key, v)
			}
		}
		g.mu.Lock()
		delete(g.running, key)
		g.mu.Unlock()
		close(f.done)
	}()
	f.res = fn()
}
