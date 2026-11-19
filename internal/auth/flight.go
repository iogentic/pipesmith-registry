package auth

import (
	"context"
	"sync"
	"sync/atomic"
)

// flight lets the calls of one key that overlap wait for the first of them
// and share its value, so parallel requests of one client cost one check.
type flight[V any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[V]
	// waiting counts the calls that wait for another call of their key now,
	// for the tests
	waiting atomic.Int64
}

type flightCall[V any] struct {
	done chan struct{}
	val  V
}

func newFlight[V any]() *flight[V] { return &flight[V]{calls: map[string]*flightCall[V]{}} }

// do runs fn for key unless a call of key runs it already; then it waits for
// that call and gives its value with shared true. A call whose ctx ends while
// it waits gives ctx.Err(). A panic of fn passes on in the call that ran it,
// and the calls that wait get the zero value.
func (f *flight[V]) do(ctx context.Context, key string, fn func() V) (val V, shared bool, err error) {
	f.mu.Lock()
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		f.waiting.Add(1)
		defer f.waiting.Add(-1)
		select {
		case <-c.done:
			return c.val, true, nil
		case <-ctx.Done():
			var zero V
			return zero, true, ctx.Err()
		}
	}
	c := &flightCall[V]{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	c.val = fn()
	return c.val, false, nil
}
