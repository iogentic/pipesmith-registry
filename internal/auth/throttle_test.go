package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// gate is a directory that holds every sign in until the test opens it, then
// refuses it.
type gate struct {
	mu    sync.Mutex
	calls int
	open  chan struct{}
}

func (g *gate) Login(_ context.Context, _, _ string) (model.User, []string, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	<-g.open
	return model.User{}, nil, fmt.Errorf("demo gate: %w", auth.ErrWrongPair)
}

func (g *gate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// panicky is a directory whose sign in panics.
type panicky struct{}

func (panicky) Login(context.Context, string, string) (model.User, []string, error) {
	panic("demo directory panic")
}

// Finding 1 of review 6: parallel wrong pairs that wait for a slow directory
// pass the throttle. At most FreeFailures attempts of a pair are in flight;
// the others get 429 at once.
func TestThrottleParallel(t *testing.T) {
	g := &gate{open: make(chan struct{})}
	e := newEnv(t, fixture(t), g)
	const n, addr = 40, "192.0.2.60"
	answers := make(chan *httptest.ResponseRecorder, n)
	for range n {
		go func() { answers <- e.signIn("demo-admin", "demo-wrong-pass-1", addr) }()
	}
	var early []*httptest.ResponseRecorder
	deadline := time.Now().Add(10 * time.Second)
	for len(early)+g.count() < n {
		if time.Now().After(deadline) {
			close(g.open)
			t.Fatalf("after 10 s: %d answers, %d in the directory", len(early), g.count())
		}
		select {
		case rec := <-answers:
			early = append(early, rec)
		case <-time.After(time.Millisecond):
		}
	}
	inFlight := g.count()
	close(g.open)
	var late []*httptest.ResponseRecorder
	for range n - len(early) {
		late = append(late, <-answers)
	}
	if inFlight != auth.FreeFailures {
		t.Errorf("%d sign ins of one pair reached the directory at once, want %d", inFlight, auth.FreeFailures)
	}
	for _, rec := range early {
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
			t.Errorf("answer while %d were in flight: %d, Retry-After %q", inFlight, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	for _, rec := range late {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("answer of the directory: %d", rec.Code)
		}
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), addr); rec.Code != http.StatusTooManyRequests {
		t.Errorf("right password after %d parallel failures: %d", inFlight, rec.Code)
	}
}

// The Basic path holds the same bound: parallel wrong Basic credentials of
// one pair run FreeFailures hashes in all.
func TestThrottleParallelBasic(t *testing.T) {
	e := newEnv(t, fixture(t))
	const n = 40
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	before := auth.HashRuns()
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := basic(request(http.MethodGet, "/", nil), "demo-admin", "demo-wrong-pass-1")
			r.RemoteAddr = "192.0.2.61:50000"
			<-start
			_, errs[i] = e.svc.Subject(r)
		}()
	}
	close(start)
	wg.Wait()
	runs := auth.HashRuns() - before
	refused, throttled := 0, 0
	for _, err := range errs {
		var me *model.Error
		switch {
		case errors.As(err, &me) && me.Status == http.StatusTooManyRequests:
			throttled++
		case errors.Is(err, model.ErrUnauthorized):
			refused++
		default:
			t.Errorf("answer %v", err)
		}
	}
	if runs != auth.FreeFailures || refused != auth.FreeFailures || throttled != n-auth.FreeFailures {
		t.Errorf("%d parallel wrong Basic credentials: %d hashes, %d refused, %d throttled; want %d, %d, %d",
			n, runs, refused, throttled, auth.FreeFailures, auth.FreeFailures, n-auth.FreeFailures)
	}
}

// A sign in that panics settles its reservation as a failure, so the pair is
// not held in flight for ever.
func TestThrottlePanicSettles(t *testing.T) {
	e := newEnv(t, fixture(t), panicky{})
	const addr = "192.0.2.63"
	attempt := func() (code int, panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		return e.signIn("demo-ldap-person", ldapPassword, addr).Code, false
	}
	for i := range auth.FreeFailures {
		if _, panicked := attempt(); !panicked {
			t.Fatalf("attempt %d did not reach the directory", i+1)
		}
	}
	if code, panicked := attempt(); panicked || code != http.StatusTooManyRequests {
		t.Fatalf("after %d panics: %d, panicked %v; want 429", auth.FreeFailures, code, panicked)
	}
	e.clock.Add(auth.ForgetAfter + time.Second)
	if _, panicked := attempt(); !panicked {
		t.Error("the pair is still held after the window")
	}
}

// Finding 2 of review 6: a full table never drops a pair that is blocked
// now. It drops an unblocked pair first and refuses a new pair while every
// pair is blocked.
func TestThrottleFullTable(t *testing.T) {
	e := newEnv(t, fixture(t))
	auth.SetMaxPairs(e.svc, 3)
	const addr = "192.0.2.62"
	fail := func(user string, times int) {
		t.Helper()
		for i := range times {
			if rec := e.signIn(user, "demo-wrong-pass-1", addr); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s, failure %d: %d", user, i+1, rec.Code)
			}
		}
	}
	right := func(user string) *httptest.ResponseRecorder {
		return e.signIn(user, password(t, user), addr)
	}
	fail("demo-admin", auth.FreeFailures)
	fail("demo-reader", auth.FreeFailures)
	e.clock.Add(time.Second)
	fail("demo-nobody", 1)
	fail("demo-legacy-sha1", 1) // the table is full: demo-nobody leaves
	for _, user := range []string{"demo-admin", "demo-reader"} {
		if rec := right(user); rec.Code != http.StatusTooManyRequests {
			t.Errorf("blocked pair %s after a new pair came in: %d", user, rec.Code)
		}
	}
	if n := auth.PairCount(e.svc); n != 3 {
		t.Errorf("%d pairs, want 3", n)
	}
	fail("demo-legacy-sha1", auth.FreeFailures-1)
	before := auth.HashRuns()
	rec := right("demo-legacy-md5")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("new pair while every pair is blocked: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if auth.HashRuns() != before {
		t.Error("a new pair ran a hash while every pair was blocked")
	}
	if n := auth.PairCount(e.svc); n != 3 {
		t.Errorf("%d pairs, want 3", n)
	}
	e.clock.Add(3 * time.Second)
	if rec := right("demo-legacy-md5"); rec.Code != http.StatusOK {
		t.Errorf("new pair after the blocks ended: %d", rec.Code)
	}
}
