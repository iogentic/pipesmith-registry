package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// placeWait is a directory whose first sign in waits for a place until its
// context ends and then gets ErrBusy, as a sign in of package ldapauth whose
// request ended while it waited. Every later sign in gets a place at once.
type placeWait struct {
	*directory
	first   atomic.Bool
	entered chan struct{}
}

func (p *placeWait) Login(ctx context.Context, username, password string) (model.User, []string, error) {
	if p.first.CompareAndSwap(false, true) {
		close(p.entered)
		<-ctx.Done()
		return model.User{}, nil, fmt.Errorf("demo connection: %w: the request ended while it waited for a place: %w",
			auth.ErrBusy, ctx.Err())
	}
	return p.directory.Login(ctx, username, password)
}

// Row 3-49 of review 3: when the client of the first Basic request of a
// user name and password closes while its check waits for a place, the
// requests that wait for its answer run a check of their own and do not get
// the 429 of a busy directory.
func TestWaitersOfAClosedFirstRequestRunTheirOwnCheck(t *testing.T) {
	d := &placeWait{directory: ldapDirectory(), entered: make(chan struct{})}
	e := newEnv(t, fixture(t), d)
	at := func(ctx context.Context) *http.Request {
		r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), "demo-ldap-person", ldapPassword).WithContext(ctx)
		r.RemoteAddr = "192.0.2.96:40000"
		return r
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := e.svc.Subject(at(ctx))
		first <- err
	}()
	<-d.entered
	type answer struct {
		s   *model.Subject
		err error
	}
	waiters := make(chan answer, 3)
	for range 3 {
		go func() {
			s, err := e.svc.Subject(at(context.Background()))
			waiters <- answer{s, err}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for auth.CheckWaiters(e.svc) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d requests wait for the first check, want 3", auth.CheckWaiters(e.svc))
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-first
	for i := range 3 {
		a := <-waiters
		var me *model.Error
		if errors.As(a.err, &me) && me.Status == http.StatusTooManyRequests {
			t.Errorf("waiter %d: 429 with Retry-After %d after the first request ended", i+1, me.RetryAfter)
			continue
		}
		if a.err != nil || a.s == nil || a.s.User.ID != "demo-ldap-person" {
			t.Errorf("waiter %d: %v, %v, want the directory user", i+1, a.s, a.err)
		}
	}
	if n, held := auth.PairFailures(e.svc, "demo-ldap-person", "192.0.2.96"); held && n > 0 {
		t.Errorf("%d failures on record for the right password", n)
	}
}

// Row 3-53 of review 3: a directory that does not answer compared nothing,
// so the right password collects no failure while it is down, on the
// session path and with Basic. A refused bind still counts one.
func TestDirectoryDownCountsNoFailure(t *testing.T) {
	d := ldapDirectory()
	d.fail(fmt.Errorf("demo dial: %w", auth.ErrUnreachable))
	e := newEnv(t, fixture(t), d)
	const address = "192.0.2.97"
	for i := range auth.FreeFailures + 3 {
		if rec := e.signIn("demo-ldap-person", ldapPassword, address); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("sign in %d with the right password while the directory is down: 429", i+1)
		}
		r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), "demo-ldap-person", ldapPassword)
		r.RemoteAddr = address + ":40000"
		var me *model.Error
		if _, err := e.svc.Subject(r); errors.As(err, &me) && me.Status == http.StatusTooManyRequests {
			t.Fatalf("Basic request %d with the right password while the directory is down: 429", i+1)
		}
	}
	if n, _ := auth.PairFailures(e.svc, "demo-ldap-person", address); n != 0 {
		t.Errorf("%d failures on record while the directory was down, want 0", n)
	}
	// a request that ended before the answer may have reached the directory
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), "demo-ldap-person", "demo-guess").WithContext(ended)
	r.RemoteAddr = address + ":40000"
	_, _ = e.svc.Subject(r)
	if n, _ := auth.PairFailures(e.svc, "demo-ldap-person", address); n != 1 {
		t.Errorf("%d failures after a request that ended before the directory answered, want 1", n)
	}
	d.fail(nil)
	if rec := e.signIn("demo-ldap-person", ldapPassword, address); rec.Code != http.StatusOK {
		t.Errorf("right password once the directory answers: %d", rec.Code)
	}
	if rec := e.signIn("demo-ldap-person", "demo-guess", address); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", rec.Code)
	}
	if n, _ := auth.PairFailures(e.svc, "demo-ldap-person", address); n != 1 {
		t.Errorf("%d failures after a success and a refused bind, want 1", n)
	}
}

// Carry Over / Security of docs/plans/0.2.0.md, "Retry-After of a busy
// directory": while every place of the directory is held, the answers for a
// local name with a wrong password and for an unknown name are compared,
// each request sent at once for the first FreeFailures and after the
// Retry-After of the one before from then on. They differ: a wrong local
// password was compared, counts a failure and makes the pair wait with the
// doubling schedule, while the unknown name compared nothing, counts none
// (findings A2 and T5) and keeps Retry-After 2. The test pins that
// difference; the fix waits for the owner (g1 result file of stream
// sec-020).
func TestRetryAfterOfABusyDirectoryTellsALocalName(t *testing.T) {
	answers := func(user string) []string {
		b := &busyDirectory{directory: ldapDirectory()}
		b.busy.Store(true)
		e := newEnv(t, fixture(t), b)
		var got []string
		for i := range auth.FreeFailures + 10 {
			rec := e.signIn(user, fmt.Sprintf("demo-guess-%d", i), "192.0.2.98")
			got = append(got, rec.Header().Get("Retry-After"))
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("%s, request %d: %d, want 429", user, i+1, rec.Code)
			}
			if i >= auth.FreeFailures {
				wait, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
				e.clock.Add(time.Duration(wait) * time.Second)
			}
		}
		return got
	}
	local, unknown := answers("demo-admin"), answers("demo-made-up-name")
	wantLocal := []string{"2", "2", "2", "2", "2", "2", "2", "2", "2", "6", "2", "14", "2", "30", "2"}
	if !slices.Equal(local, wantLocal) {
		t.Errorf("Retry-After for a local name %v, want %v", local, wantLocal)
	}
	if want := slices.Repeat([]string{"2"}, auth.FreeFailures+10); !slices.Equal(unknown, want) {
		t.Errorf("Retry-After for an unknown name %v, want %v", unknown, want)
	}
}
