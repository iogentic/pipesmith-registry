package auth

import (
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

// Sign in limits (decision T4): a pair of user name and client address gets
// FreeFailures failures within ForgetAfter; after that it is refused for
// FirstDelay, doubling with each further failure up to MaxDelay. The pair is
// forgotten ForgetAfter after its last failure. No request sleeps.
const (
	FreeFailures = 5
	FirstDelay   = 2 * time.Second
	MaxDelay     = 15 * time.Minute
	ForgetAfter  = 15 * time.Minute
	MaxPairs     = 10_000
)

// attempts is the state of one pair. pending counts the attempts in flight:
// reserved by begin and not yet settled by end.
type attempts struct {
	failures int
	pending  int
	last     time.Time
	until    time.Time
}

// limiter holds the failures per pair. The key is a SHA-256 of the lower
// cased user name and the address, so a long name costs no memory.
type limiter struct {
	mu      sync.Mutex
	entries map[[32]byte]*attempts
	now     func() time.Time
	max     int
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{entries: map[[32]byte]*attempts{}, now: now, max: MaxPairs}
}

func pairKey(user, address string) [32]byte {
	return sha256.Sum256([]byte(strings.ToLower(user) + "\x00" + address))
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
// the attempt may run and must be settled by end, else the seconds to wait.
// A new pair while the table is full of pairs that may not leave counts as
// blocked.
func (l *limiter) begin(user, address string) int {
	key := pairKey(user, address)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, held := l.entries[key]
	if held && l.forgotten(a, now) {
		*a = attempts{}
	}
	if !held {
		if len(l.entries) >= l.max && !l.evict(now) {
			return seconds(FirstDelay)
		}
		a = &attempts{}
		l.entries[key] = a
	}
	if blocked(a, now) {
		return seconds(a.until.Sub(now))
	}
	if a.pending > 0 && a.failures+a.pending >= FreeFailures {
		return seconds(FirstDelay)
	}
	a.pending++
	return 0
}

// end settles a reservation of begin: a success forgets the failures of the
// pair, a refusal counts one.
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
