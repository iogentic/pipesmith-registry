package ldapauth

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// places are the places for sign ins of one connection (finding A2 of the
// security assessment): each sign in in flight holds one of
// MaxChecksInFlight. A sign in that finds them all taken waits for one, at
// most limit, which is the connection timeout. The connection gives each
// step of a check, the dial included, that long to answer, so against a
// directory that stops answering a check ends about one timeout after its
// last answer. Waiting longer for a place would hold the request longer
// than the connection lets one step of the directory take. At most
// MaxWaiting sign ins wait (finding T2 of review 3); a sign in beyond them
// gets no place at once.
type places struct {
	taken   chan struct{} // one token per place in use
	limit   time.Duration
	waiting atomic.Int64 // the sign ins that wait for a place now, never more than MaxWaiting
}

func newPlaces(limit time.Duration) *places {
	return &places{taken: make(chan struct{}, MaxChecksInFlight), limit: limit}
}

// take gives a place and the function that gives it back. While every place
// is taken it waits for one, in the order of arrival, at most limit and
// while ctx lasts; the error names which of the two ended the wait. While
// MaxWaiting sign ins wait already, it waits not at all.
func (p *places) take(ctx context.Context) (func(), error) {
	back := func() { <-p.taken }
	select {
	case p.taken <- struct{}{}:
		return back, nil
	default:
	}
	if !p.queue() {
		return nil, fmt.Errorf("%d sign ins wait for a place already", MaxWaiting)
	}
	defer p.waiting.Add(-1)
	limit := time.NewTimer(p.limit)
	defer limit.Stop()
	select {
	case p.taken <- struct{}{}:
		return back, nil
	case <-limit.C:
		return nil, fmt.Errorf("no place came free within %v", p.limit)
	case <-ctx.Done():
		return nil, fmt.Errorf("the request ended while it waited for a place: %w", ctx.Err())
	}
}

// queue counts one more waiting sign in unless MaxWaiting wait already.
func (p *places) queue() bool {
	for {
		n := p.waiting.Load()
		if n >= MaxWaiting {
			return false
		}
		if p.waiting.CompareAndSwap(n, n+1) {
			return true
		}
	}
}
