package auth

import (
	"context"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"time"
)

// Sign in limits (decision T4): a pair of user name and client address gets
// FreeFailures failures within ForgetAfter; after that it is refused for
// FirstDelay, doubling with each further failure up to MaxDelay. The pair is
// forgotten ForgetAfter after its last failure. No request sleeps out a
// delay; a Basic request waits up to PlaceWait for a place (reserve).
const (
	FreeFailures = 5
	FirstDelay   = 2 * time.Second
	MaxDelay     = 15 * time.Minute
	ForgetAfter  = 15 * time.Minute
	MaxPairs     = 10_000
)

// PlaceWait bounds the wait of a Basic request for a place of its pair while
// the checks in flight of the pair hold all its free failures. It is
// FirstDelay, the Retry-After of the 429 such a request got before, so no
// client waits longer than with that 429 and its retry. A remembered
// success gives its place back within microseconds and a hash within
// milliseconds (a refusal after RefusalFloor), well within the bound. A
// directory bind can take up to the connection timeout; a request that
// waits longer than PlaceWait for one gets 429 with Retry-After 2.
const PlaceWait = FirstDelay

// attempts is the state of one pair. pending counts the attempts in flight:
// reserved by begin or reserve and not yet settled by end or release.
// settled is made for the requests that wait for a place and closed by the
// next settle.
type attempts struct {
	failures int
	pending  int
	last     time.Time
	until    time.Time
	settled  chan struct{}
}

// full reports a pair whose attempts in flight hold all its free failures.
func full(a *attempts) bool {
	return a.pending > 0 && a.failures+a.pending >= FreeFailures
}

// wake lets the requests that wait for a place of the pair try again.
func (a *attempts) wake() {
	if a.settled != nil {
		close(a.settled)
		a.settled = nil
	}
}

// limiter holds the failures per pair. The key is a SHA-256 of the user name
// folded as a directory folds it (limiterName) and the address, so a long
// name costs no memory.
type limiter struct {
	mu      sync.Mutex
	entries map[[32]byte]*attempts
	now     func() time.Time
	max     int
	// waiting counts the requests that wait for a place now, for the tests
	waiting atomic.Int64
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{entries: map[[32]byte]*attempts{}, now: now, max: MaxPairs}
}

func pairKey(user, address string) [32]byte {
	return sha256.Sum256([]byte(limiterName(user) + "\x00" + address))
}

// forgotten reports a pair with no attempt in flight whose last failure is
// older than ForgetAfter.
func (l *limiter) forgotten(a *attempts, now time.Time) bool {
	return a.pending == 0 && now.Sub(a.last) > ForgetAfter
}

// blocked reports a pair that waits now.
func blocked(a *attempts, now time.Time) bool {
	return now.Before(a.until)
}

func seconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// begin reserves an attempt of the pair under the same lock as the check, so
// at most FreeFailures attempts of a pair are failed or in flight within the
// window, and one at a time once the free failures are spent. It gives 0 when
// the attempt may run and must be settled by end or release, else the
// seconds to wait. A new pair while the table is full of pairs that may not
// leave counts as blocked.
func (l *limiter) begin(user, address string) int {
	wait, _ := l.take(pairKey(user, address), false)
	return wait
}

// reserve is begin for a request that may wait for a place (Service.basic,
// review R1 for a burst): while the attempts in flight of the pair hold all
// its free failures, it waits for one of them to settle and tries again, up
// to PlaceWait in all, and then gives FirstDelay as begin does. A pair that
// waits after its failures and a new pair while the table is full get their
// wait at once. The error is that of ctx, which ended while the request
// waited; then nothing is reserved.
func (l *limiter) reserve(ctx context.Context, user, address string) (int, error) {
	key := pairKey(user, address)
	wait, settled := l.take(key, true)
	if settled == nil {
		return wait, nil
	}
	l.waiting.Add(1)
	defer l.waiting.Add(-1)
	limit := time.NewTimer(PlaceWait)
	defer limit.Stop()
	for settled != nil {
		select {
		case <-settled:
		case <-limit.C:
			return wait, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		wait, settled = l.take(key, true)
	}
	return wait, nil
}

// take is begin for the key of a pair. When a request may wait and the pair
// is full, it also gives the channel the next settle of the pair closes.
func (l *limiter) take(key [32]byte, mayWait bool) (int, chan struct{}) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, held := l.entries[key]
	if held && l.forgotten(a, now) {
		*a = attempts{}
	}
	if !held {
		if len(l.entries) >= l.max && !l.evict(now) {
			return seconds(FirstDelay), nil
		}
		a = &attempts{}
		l.entries[key] = a
	}
	switch {
	case blocked(a, now):
		return seconds(a.until.Sub(now)), nil
	case full(a) && mayWait:
		if a.settled == nil {
			a.settled = make(chan struct{})
		}
		return seconds(FirstDelay), a.settled
	case full(a):
		return seconds(FirstDelay), nil
	}
	a.pending++
	return 0, nil
}

// end settles a reservation of begin or reserve: a success forgets the
// failures of the pair, a refusal counts one. Like release, it lets the
// requests that wait for a place of the pair try again.
func (l *limiter) end(user, address string, ok bool) {
	key := pairKey(user, address)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, held := l.entries[key]
	if !held {
		return // evict and end never drop a pair in flight
	}
	a.pending--
	a.wake()
	if ok {
		a.failures, a.until = 0, time.Time{}
		if a.pending == 0 {
			delete(l.entries, key)
		}
		return
	}
	a.failures++
	a.last = now
	if a.failures >= FreeFailures {
		delay := MaxDelay
		if shift := a.failures - FreeFailures; shift < 20 {
			delay = min(FirstDelay<<shift, MaxDelay)
		}
		a.until = now.Add(delay)
	}
}

// release settles a reservation of begin or reserve without a result: for a
// check a directory had no place for (finding A2 of the security
// assessment), and for a Basic request that answered without a check of its
// own (review R1). The pair keeps its failures and counts none.
func (l *limiter) release(user, address string) {
	key := pairKey(user, address)
	l.mu.Lock()
	defer l.mu.Unlock()
	a, held := l.entries[key]
	if !held {
		return
	}
	a.pending--
	a.wake()
	if a.pending == 0 && a.failures == 0 {
		delete(l.entries, key)
	}
}

// evict makes room in a full table: it drops the forgotten pairs, then the
// pair with the oldest failure among those neither blocked nor in flight. It
// reports false when no pair may leave.
func (l *limiter) evict(now time.Time) bool {
	var oldestKey [32]byte
	var oldest *attempts
	for key, a := range l.entries {
		if l.forgotten(a, now) {
			delete(l.entries, key)
			continue
		}
		if a.pending > 0 || blocked(a, now) {
			continue
		}
		if oldest == nil || a.last.Before(oldest.last) {
			oldestKey, oldest = key, a
		}
	}
	if len(l.entries) < l.max {
		return true
	}
	if oldest == nil {
		return false
	}
	delete(l.entries, oldestKey)
	return true
}

func (l *limiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
