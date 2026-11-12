package ldapauth

import (
	"sync"
	"time"
)

// breaker is the incident counter of one connection. After max failures in
// a row the connection rests for delay: sign ins are refused without a
// dial. The first sign in after the rest dials again; a failure there rests
// the connection again, a success clears the count. A max of 0 never rests.
type breaker struct {
	mu        sync.Mutex
	max       int
	delay     time.Duration
	now       func() time.Time
	count     int
	downUntil time.Time
}

func newBreaker(max int, delay time.Duration, now func() time.Time) *breaker {
	return &breaker{max: max, delay: delay, now: now}
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.now().Before(b.downUntil)
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.count++
	if b.max > 0 && b.count >= b.max {
		b.downUntil = b.now().Add(b.delay)
	}
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.count = 0
	b.downUntil = time.Time{}
}

func (b *breaker) failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}
