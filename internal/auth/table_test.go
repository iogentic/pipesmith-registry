package auth_test

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
)

// Row 3-8 of review 3, limiter rule B: a table full of blocked pairs lets a
// new pair in. The pair whose last failure is oldest leaves, blocked or not;
// the others keep their waits.
func TestFullTableLetsANewPairIn(t *testing.T) {
	e := newEnv(t, fixture(t))
	auth.SetMaxPairs(e.svc, 3)
	fill := func(i int) (string, string) { return fmt.Sprintf("demo-fill-%d", i), fmt.Sprintf("192.0.2.%d", 70+i) }
	for i := range 3 {
		user, addr := fill(i)
		auth.FailPair(e.svc, user, addr, auth.FreeFailures) // waits FirstDelay
		e.clock.Add(time.Millisecond)
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.80"); rec.Code != http.StatusOK {
		t.Fatalf("new pair with the right password while every pair waits: %d", rec.Code)
	}
	for i, want := range []bool{false, true, true} {
		user, addr := fill(i)
		if _, held := auth.PairFailures(e.svc, user, addr); held != want {
			t.Errorf("pair %d held %v, want %v", i, held, want)
		}
	}
	if rec := e.signIn("demo-reader", "demo-wrong-pass-1", "192.0.2.81"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("new pair with a wrong password: %d", rec.Code)
	}
	if n := auth.PairCount(e.svc); n != 3 {
		t.Errorf("%d pairs, want 3", n)
	}
	user, addr := fill(1)
	if _, held := auth.PairFailures(e.svc, user, addr); !held {
		t.Error("the second pair left before the oldest one was gone")
	}
	if rec := e.signIn("demo-legacy-md5", password(t, "demo-legacy-md5"), "192.0.2.82"); rec.Code != http.StatusOK {
		t.Fatalf("third new pair: %d", rec.Code)
	}
	if _, held := auth.PairFailures(e.svc, user, addr); held {
		t.Error("the oldest pair stayed")
	}
	user, addr = fill(2)
	if rec := e.signIn(user, "demo-guess", addr); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a blocked pair that stayed: %d, want 429", rec.Code)
	}
}

// Row 3-8, rule B: a pair with a check in flight never leaves. A table that
// holds only such pairs refuses a new pair with Retry-After 2.
func TestFullTableKeepsAPairInFlight(t *testing.T) {
	e := newEnv(t, fixture(t))
	auth.SetMaxPairs(e.svc, 2)
	auth.FailPair(e.svc, "demo-flight", "192.0.2.71", 1) // the oldest failure of the table
	if wait := auth.BeginPair(e.svc, "demo-flight", "192.0.2.71"); wait != 0 {
		t.Fatalf("begin: %d", wait)
	}
	e.clock.Add(time.Second)
	auth.FailPair(e.svc, "demo-fill", "192.0.2.72", auth.FreeFailures)
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.73"); rec.Code != http.StatusOK {
		t.Fatalf("new pair: %d", rec.Code)
	}
	if _, held := auth.PairFailures(e.svc, "demo-fill", "192.0.2.72"); held {
		t.Error("the blocked pair stayed and the pair in flight left")
	}
	if auth.PairsInFlight(e.svc) != 1 {
		t.Error("the pair in flight left")
	}
	auth.SetMaxPairs(e.svc, 1)
	before := auth.HashRuns()
	rec := e.signIn("demo-reader", password(t, "demo-reader"), "192.0.2.74")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("new pair while every pair is in flight: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if auth.HashRuns() != before {
		t.Error("a refused new pair ran a hash")
	}
	auth.ReleasePair(e.svc, "demo-flight", "192.0.2.71")
	if rec := e.signIn("demo-reader", password(t, "demo-reader"), "192.0.2.74"); rec.Code != http.StatusOK {
		t.Errorf("new pair after the check in flight ended: %d", rec.Code)
	}
}

// Row 3-8, rule B: one address cannot fill the table. After AddressBudget
// failures within ForgetAfter a new pair of the address gets 429 before any
// check, the right password too, while its existing pairs keep their own
// schedule and other addresses are not touched.
func TestOneAddressCannotFillTheTable(t *testing.T) {
	e := newEnv(t, fixture(t))
	const addr = "192.0.2.90"
	for i := range auth.AddressBudget {
		if rec := e.signIn(fmt.Sprintf("demo-made-up-%d", i), "demo-guess", addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("made up name %d: %d", i, rec.Code)
		}
		if i%10 == 9 {
			e.clock.Add(time.Minute)
		}
	}
	before := auth.HashRuns()
	for i := range 2 * auth.AddressBudget {
		rec := e.signIn(fmt.Sprintf("demo-more-%d", i), "demo-guess", addr)
		wait, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
		if rec.Code != http.StatusTooManyRequests || wait < 1 || wait > int(auth.ForgetAfter/time.Second) {
			t.Fatalf("new pair %d past the budget: %d, Retry-After %q", i, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	rec := e.signIn("demo-admin", password(t, "demo-admin"), addr)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("right password of a new pair past the budget: %d, want 429", rec.Code)
	}
	if auth.HashRuns() != before {
		t.Error("a new pair past the budget ran a hash")
	}
	if n := auth.PairCount(e.svc); n != auth.AddressBudget {
		t.Errorf("%d pairs, want %d", n, auth.AddressBudget)
	}
	if rec := e.signIn("demo-made-up-0", "demo-guess", addr); rec.Code != http.StatusUnauthorized {
		t.Errorf("an existing pair past the budget: %d, want 401 by its own schedule", rec.Code)
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.91"); rec.Code != http.StatusOK {
		t.Errorf("another address: %d", rec.Code)
	}
	e.clock.Add(auth.ForgetAfter)
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), addr); rec.Code != http.StatusOK {
		t.Errorf("new pair of the address after the window: %d", rec.Code)
	}
}

// Row 3-16 of review 3: the addresses of one IPv6 /64 share one budget, so a
// client that rotates inside its /64 cannot fill the table. The pair key
// stays the full address, and IPv4 addresses keep a budget each.
func TestAddressesOfOneSlash64ShareABudget(t *testing.T) {
	e := newEnv(t, fixture(t))
	for i := range auth.AddressBudget {
		addr := fmt.Sprintf("[2001:db8:1:2:%x::%x]", i, i+1)
		if rec := e.signIn(fmt.Sprintf("demo-made-up-%d", i), "demo-guess", addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("made up name %d from %s: %d", i, addr, rec.Code)
		}
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "[2001:db8:1:2:ffff:ffff:ffff:ffff]"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("new pair of another address of the /64 past its budget: %d, want 429", rec.Code)
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "[2001:db8:1:3::1]"); rec.Code != http.StatusOK {
		t.Errorf("an address of the next /64: %d", rec.Code)
	}
	if _, held := auth.PairFailures(e.svc, "demo-made-up-0", "2001:db8:1:2:0::1"); !held {
		t.Error("the pair key lost its full address")
	}
	if _, held := auth.PairFailures(e.svc, "demo-made-up-0", "2001:db8:1:2:1::2"); held {
		t.Error("two addresses of one /64 are one pair")
	}
	for i := range auth.AddressBudget {
		if rec := e.signIn(fmt.Sprintf("demo-made-up-%d", i), "demo-guess", fmt.Sprintf("198.51.100.%d", i)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("made up name %d from an IPv4 address of its own: %d", i, rec.Code)
		}
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "198.51.100.250"); rec.Code != http.StatusOK {
		t.Errorf("an IPv4 address next to others with failures: %d", rec.Code)
	}
}
