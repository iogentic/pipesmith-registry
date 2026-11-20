package auth

import (
	"container/heap"
	"container/list"
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
// delay; a Basic request waits up to PlaceWait for a place (reserve). A full
// table of MaxPairs makes room for a new pair (evict), and a client address
// with AddressBudget failures within ForgetAfter gets no new pair (budgets).
const (
	FreeFailures = 5
	FirstDelay   = 2 * time.Second
	MaxDelay     = 15 * time.Minute
	ForgetAfter  = 15 * time.Minute
	MaxPairs     = 10_000
	// AddressBudget bounds the failures of one client address within
	// ForgetAfter (row 3-8 of review 3, limiter rule B)
	AddressBudget = 100
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
// at is the place of the pair in limiter.idle while no attempt is in flight,
// else -1.
type attempts struct {
	key      [32]byte
	failures int
	pending  int
	last     time.Time
	until    time.Time
	settled  chan struct{}
	at       int
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

// idlePairs is a heap of the pairs without an attempt in flight, the oldest
// last failure first. A pair changes its last failure only while an attempt
// is in flight, so out of the heap.
type idlePairs []*attempts

func (h idlePairs) Len() int           { return len(h) }
func (h idlePairs) Less(i, j int) bool { return h[i].last.Before(h[j].last) }
func (h idlePairs) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].at, h[j].at = i, j
}
func (h *idlePairs) Push(x any) {
	a := x.(*attempts)
	a.at = len(*h)
	*h = append(*h, a)
}
func (h *idlePairs) Pop() any {
	old := *h
	a := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	a.at = -1
	return a
}

// limiter holds the failures per pair. The key is a SHA-256 of the user name
// folded as a directory folds it (limiterName) and the address, so a long
// name costs no memory.
type limiter struct {
	mu      sync.Mutex
	entries map[[32]byte]*attempts
	idle    idlePairs
	budgets *budgets
	now     func() time.Time
	max     int
	// waiting counts the requests that wait for a place now, for the tests
	waiting atomic.Int64
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{entries: map[[32]byte]*attempts{}, budgets: newBudgets(MaxPairs), now: now, max: MaxPairs}
}

// park puts a pair without an attempt in flight into the heap of idle pairs.
func (l *limiter) park(a *attempts) {
	if a.pending == 0 && a.at < 0 {
		heap.Push(&l.idle, a)
	}
}

// unpark takes a pair out of the heap of idle pairs.
func (l *limiter) unpark(a *attempts) {
	if a.at >= 0 {
		heap.Remove(&l.idle, a.at)
	}
}

// drop removes a pair from the table.
func (l *limiter) drop(a *attempts) {
	l.unpark(a)
	delete(l.entries, a.key)
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
// seconds to wait. A new pair while every pair of the table has an attempt
// in flight counts as blocked; a new pair of an address past its budget
// waits until the budget has room.
func (l *limiter) begin(user, address string) int {
	wait, _ := l.take(pairKey(user, address), address, false)
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
	wait, settled := l.take(key, address, true)
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
		wait, settled = l.take(key, address, true)
	}
	return wait, nil
}

// take is begin for the key of a pair. When a request may wait and the pair
// is full, it also gives the channel the next settle of the pair closes.
func (l *limiter) take(key [32]byte, address string, mayWait bool) (int, chan struct{}) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, held := l.entries[key]
	if held && l.forgotten(a, now) {
		l.drop(a)
		held = false
	}
	if !held {
		if wait := l.budgets.wait(budgetKey(address), now); wait > 0 {
			return wait, nil
		}
		if !l.evict() {
			return seconds(FirstDelay), nil
		}
		a = &attempts{key: key, at: -1}
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
	l.unpark(a)
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
			l.drop(a)
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
	l.budgets.add(budgetKey(address), now)
	l.park(a)
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
	switch {
	case a.pending > 0:
	case a.failures == 0:
		l.drop(a)
	default:
		l.park(a)
	}
}

// evict makes room in a full table (limiter rule B, row 3-8 of review 3):
// the pair without an attempt in flight whose last failure is oldest
// leaves, a forgotten one, a blocked one or one that waits no more alike. A
// pair with an attempt in flight never leaves. It reports false when every
// pair has an attempt in flight. The heap makes this O(log n) under the
// lock.
func (l *limiter) evict() bool {
	for len(l.entries) >= l.max {
		if len(l.idle) == 0 {
			return false
		}
		a := heap.Pop(&l.idle).(*attempts)
		delete(l.entries, a.key)
	}
	return true
}

// budgetSlots are the minutes of ForgetAfter; a budget counts the failures
// of an address per minute.
const budgetSlots = int64(ForgetAfter / time.Minute)

// budget holds the failures of one client address (budgetKey) in the last
// budgetSlots minutes: count[i] failures in the minute minute[i].
type budget struct {
	key    string
	minute [budgetSlots]int64
	count  [budgetSlots]int
	el     *list.Element
}

// budgets are the failures per client address (limiter rule B, row 3-8 of
// review 3), at most max addresses; a new address past max makes the one
// whose last failure is oldest leave. order holds the addresses by their
// last failure, the oldest first.
type budgets struct {
	by    map[string]*budget
	order *list.List
	max   int
}

func newBudgets(limit int) *budgets {
	return &budgets{by: map[string]*budget{}, order: list.New(), max: limit}
}

// inWindow sums the failures of b within ForgetAfter of minute m and gives
// the oldest minute that holds one.
func (b *budget) inWindow(m int64) (total int, oldest int64) {
	oldest = m
	for i := range budgetSlots {
		if b.count[i] > 0 && m-b.minute[i] < budgetSlots {
			total += b.count[i]
			oldest = min(oldest, b.minute[i])
		}
	}
	return total, oldest
}

// add counts a failure of the address key at now.
func (bs *budgets) add(key string, now time.Time) {
	m := now.Unix() / 60
	if front := bs.order.Front(); front != nil {
		if old := front.Value.(*budget); old.key != key {
			if total, _ := old.inWindow(m); total == 0 {
				bs.remove(old)
			}
		}
	}
	b, held := bs.by[key]
	if held {
		bs.order.MoveToBack(b.el)
	} else {
		if len(bs.by) >= bs.max {
			bs.remove(bs.order.Front().Value.(*budget))
		}
		b = &budget{key: key}
		b.el = bs.order.PushBack(b)
		bs.by[key] = b
	}
	i := m % budgetSlots
	if b.minute[i] != m {
		b.minute[i], b.count[i] = m, 0
	}
	b.count[i]++
}

func (bs *budgets) remove(b *budget) {
	bs.order.Remove(b.el)
	delete(bs.by, b.key)
}

// wait gives 0 while the address key holds fewer than AddressBudget
// failures within ForgetAfter, else the seconds until its oldest minute
// leaves the window.
func (bs *budgets) wait(key string, now time.Time) int {
	b, held := bs.by[key]
	if !held {
		return 0
	}
	m := now.Unix() / 60
	total, oldest := b.inWindow(m)
	if total < AddressBudget {
		return 0
	}
	return max(1, seconds(time.Unix((oldest+budgetSlots)*60, 0).Sub(now)))
}

func (l *limiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
