package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
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

func (g *gate) Lookup(context.Context, string) (model.User, []string, error) {
	return model.User{}, nil, fmt.Errorf("demo gate: %w", auth.ErrNoUser)
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

func (panicky) Lookup(context.Context, string) (model.User, []string, error) {
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

// busyDirectory has no place for a sign in while busy is set; otherwise it
// is the fake directory.
type busyDirectory struct {
	*directory
	busy atomic.Bool
}

func (b *busyDirectory) Login(ctx context.Context, username, password string) (model.User, []string, error) {
	if b.busy.Load() {
		return model.User{}, nil, fmt.Errorf("demo connection: %w", auth.ErrBusy)
	}
	return b.directory.Login(ctx, username, password)
}

// waitsTwo fails t unless err is 429 with Retry-After 2.
func waitsTwo(t *testing.T, when string, err error) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != 2 {
		t.Errorf("%s: %v, want 429 with Retry-After 2", when, err)
	}
}

// Finding A2 of the security assessment, for a check that compared nothing:
// a sign in that a directory has no place for, of a name that has no local
// user and no remembered success, is answered as a waiting pair is, 429
// with Retry-After 2, on every path, and puts no failure on record for the
// name, so the right password passes once a place is free. Until finding T1
// of review 3 this test held the case of a check that compared as well; that
// case is TestBusyDirectoryAfterAComparisonCountsAFailure now.
func TestBusyDirectoryWithoutAComparisonPutsNoFailureOnRecord(t *testing.T) {
	b := &busyDirectory{directory: ldapDirectory()}
	b.busy.Store(true)
	e := newEnv(t, fixture(t), b)
	const address = "192.0.2.93"
	at := func(r *http.Request) *http.Request {
		r.RemoteAddr = address + ":40000"
		return r
	}
	for i := range 3 * auth.FreeFailures {
		if rec := e.signIn("demo-ldap-person", fmt.Sprintf("demo-guess-%d", i), address); rec.Code != http.StatusTooManyRequests ||
			rec.Header().Get("Retry-After") != "2" {
			t.Fatalf("sign in %d while the directory is busy: %d, Retry-After %q", i+1, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	_, err := e.svc.Subject(at(basic(request(http.MethodGet, "/", nil), "demo-ldap-person", ldapPassword)))
	waitsTwo(t, "Basic while the directory is busy", err)
	_, err = e.svc.NpmLogin(at(request(http.MethodPut, "/", nil)), "demo-ldap-person", ldapPassword)
	waitsTwo(t, "npm login while the directory is busy", err)
	if n := auth.PairCount(e.svc); n != 0 {
		t.Errorf("%d pairs held after sign ins the directory had no place for, want 0", n)
	}
	b.busy.Store(false)
	if rec := e.signIn("demo-ldap-person", ldapPassword, address); rec.Code != http.StatusOK {
		t.Errorf("right password once a place is free: %d", rec.Code)
	}
}

// Finding T1 of review 3: a check that compared the password with anything
// counts a failure when a directory then has no place for it, so the bound
// of the directory opens no way around the limits. Compared are a stored
// password of a local user, which the local realm asks before the
// directory, and a remembered success of the name, which a Basic guess
// missed. Five such guesses make the pair wait, and the right password gets
// 429 until the wait is over, also once the directory has a place again.
func TestBusyDirectoryAfterAComparisonCountsAFailure(t *testing.T) {
	b := &busyDirectory{directory: ldapDirectory()}
	e := newEnv(t, fixture(t), b)
	const address = "192.0.2.94"
	// the local realm answers before the directory is asked
	b.busy.Store(true)
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.95"); rec.Code != http.StatusOK {
		t.Errorf("right local password while the directory is busy: %d", rec.Code)
	}
	for i := range auth.FreeFailures {
		if rec := e.signIn("demo-admin", fmt.Sprintf("demo-guess-%d", i), address); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("wrong local password %d while the directory is busy: %d", i+1, rec.Code)
		}
	}
	b.busy.Store(false)
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), address); rec.Code != http.StatusTooManyRequests {
		t.Errorf("right local password after %d wrong ones while the directory was busy: %d, want 429", auth.FreeFailures, rec.Code)
	}
	// a Basic guess that missed a remembered success of a directory user
	from := func(pass string) (*model.Subject, error) {
		r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), "demo-ldap-person", pass)
		r.RemoteAddr = address + ":40000"
		return e.svc.Subject(r)
	}
	if s, err := from(ldapPassword); err != nil || s == nil {
		t.Fatalf("first Basic request of the directory user: %v", err)
	}
	b.busy.Store(true)
	for i := range auth.FreeFailures {
		_, err := from(fmt.Sprintf("demo-guess-%d", i))
		waitsTwo(t, fmt.Sprintf("Basic guess %d while the directory is busy", i+1), err)
	}
	if n, _ := auth.PairFailures(e.svc, "demo-ldap-person", address); n != auth.FreeFailures {
		t.Errorf("%d failures on record after %d guesses that missed a remembered success, want %d", n, auth.FreeFailures, auth.FreeFailures)
	}
	b.busy.Store(false)
	asks, logins := auth.CacheAsks(e.svc), b.count()
	_, err := from(ldapPassword)
	waitsTwo(t, "right password after the guesses", err)
	if auth.CacheAsks(e.svc) != asks || b.count() != logins {
		t.Error("the right password of a waiting pair was compared")
	}
	e.clock.Add(auth.FirstDelay)
	if s, err := from(ldapPassword); err != nil || s == nil {
		t.Errorf("right password after the wait: %v", err)
	}
	if b.count() != logins {
		t.Error("the remembered success ran a check after the wait")
	}
}

// Finding T3 of review 3: a directory without a place ended the check, so
// the realms after it were not asked, and with the LDAP realm before the
// local realm full places kept local administrators out. Now the realms
// after a directory without a place are asked: while Basic requests for
// made up names hold every place of the connection, the local administrator
// signs in with a session and with Basic credentials. A pair that no realm
// accepts gets the answer of the busy directory, 429 with Retry-After 2,
// and the rule of settle decides its record: a wrong local password counts
// a failure, an unknown name that no realm compared none.
func TestLocalAdministratorSignsInWhileEveryPlaceIsHeld(t *testing.T) {
	snap := fixture(t)
	snap.Realms = []string{auth.RealmLDAP, auth.RealmLocal}
	conn := newConnection(50 * time.Millisecond)
	e := newEnv(t, snap, conn)
	conn.stallSignIns()
	t.Cleanup(conn.letGo)
	holders := make(chan error, ldapauth.MaxChecksInFlight)
	for i := range ldapauth.MaxChecksInFlight {
		go func() {
			_, err := basicFrom(context.Background(), e, "192.0.2.83", fmt.Sprintf("demo-made-up-%d", i), "demo-guess")
			holders <- err
		}()
	}
	waitUntil(t, "every place of the connection held", func() bool { return conn.stalled.Load() == ldapauth.MaxChecksInFlight })
	const address = "192.0.2.82"
	admin := password(t, "demo-admin")
	if rec := e.signIn("demo-admin", admin, address); rec.Code != http.StatusOK {
		t.Errorf("session sign in of the local administrator while every place is held: %d", rec.Code)
	}
	if s, err := basicFrom(context.Background(), e, address, "demo-admin", admin); err != nil || s == nil || s.User.ID != "demo-admin" {
		t.Errorf("Basic credentials of the local administrator while every place is held: %+v %v", s, err)
	}
	if rec := e.signIn("demo-admin", "demo-guess-1", address); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("wrong local password while every place is held: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if n, _ := auth.PairFailures(e.svc, "demo-admin", address); n != 1 {
		t.Errorf("%d failures on record after a wrong local password, want 1", n)
	}
	if rec := e.signIn("demo-unknown-person", "demo-guess-1", address); rec.Code != http.StatusTooManyRequests ||
		rec.Header().Get("Retry-After") != "2" {
		t.Errorf("unknown name while every place is held: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if _, held := auth.PairFailures(e.svc, "demo-unknown-person", address); held {
		t.Error("an unknown name that no realm compared is on record")
	}
	conn.letGo()
	for range ldapauth.MaxChecksInFlight {
		if err := <-holders; !errors.Is(err, model.ErrUnauthorized) {
			t.Errorf("a made up name that held a place: %v", err)
		}
	}
}

// Finding T1 of review 3, in the order the review gives: a directory user
// with a remembered success; the 16 places of its LDAP connection held by
// Basic requests for made up names; 200 wrong passwords for the user from
// one address, every second one closed by its client after 3 ms, the others
// left to end as busy when the wait for a place ends at its bound. Each
// guess that the cache compares ends as a failure of its pair, whichever way
// it leaves, so the cache compares at most FreeFailures of them, counted by
// the counter of the cache. The pair waits after that, no guess reaches the
// directory, and the right password gets 429 until the wait is over. The
// same holds for a local user while the LDAP realm comes before the local
// realm.
func TestGuessesAtARememberedSuccessWhileTheDirectoryIsBusy(t *testing.T) {
	for name, c := range map[string]struct {
		user, pass string
		realms     []string
	}{
		"directory user":                   {"demo-ldap-person", ldapPassword, nil},
		"local user, the LDAP realm first": {"demo-admin", password(t, "demo-admin"), []string{auth.RealmLDAP, auth.RealmLocal}},
	} {
		t.Run(name, func(t *testing.T) {
			snap := fixture(t)
			if c.realms != nil {
				snap.Realms = c.realms
			}
			conn := newConnection(50 * time.Millisecond)
			e := newEnv(t, snap, conn)
			const address, n = "192.0.2.77", 200
			from := func(ctx context.Context, user, pass string) (*model.Subject, error) {
				r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), user, pass)
				r.RemoteAddr = address + ":40000"
				return e.svc.Subject(r.WithContext(ctx))
			}
			if s, err := from(context.Background(), c.user, c.pass); err != nil || s == nil {
				t.Fatalf("first request: %v", err)
			}
			conn.stallSignIns()
			t.Cleanup(conn.letGo)
			holders := make(chan error, ldapauth.MaxChecksInFlight)
			for i := range ldapauth.MaxChecksInFlight {
				go func() {
					_, err := from(context.Background(), fmt.Sprintf("demo-made-up-%d", i), "demo-guess")
					holders <- err
				}()
			}
			waitUntil(t, "every place of the connection held", func() bool { return conn.stalled.Load() == ldapauth.MaxChecksInFlight })
			asks, logins := auth.CacheAsks(e.svc), conn.count()
			out := make(chan sent, n)
			for i := range n {
				ctx, cancel := context.WithCancel(context.Background())
				if i%2 == 0 {
					time.AfterFunc(3*time.Millisecond, cancel) // the client closes the connection
				}
				go func() {
					defer cancel()
					s, err := from(ctx, c.user, fmt.Sprintf("demo-guess-%d", i))
					out <- sent{fmt.Sprintf("demo-guess-%d", i), s, err}
				}()
			}
			deadline := time.After(30 * time.Second)
			for range n {
				var a sent
				select {
				case a = <-out:
				case <-deadline:
					t.Fatal("after 30 s not every guess has its answer")
				}
				var me *model.Error
				switch {
				case errors.Is(a.err, model.ErrUnauthorized):
				case errors.As(a.err, &me) && me.Status == http.StatusTooManyRequests && me.RetryAfter == 2:
				default:
					t.Errorf("answer to %q: %+v %v", a.pass, a.subject, a.err)
				}
			}
			if compared := auth.CacheAsks(e.svc) - asks; compared > auth.FreeFailures {
				t.Errorf("the cache compared %d of %d guesses while the directory had no place, want at most %d", compared, n, auth.FreeFailures)
			}
			if conn.count() != logins {
				t.Errorf("%d guesses reached the directory", conn.count()-logins)
			}
			_, err := from(context.Background(), c.user, c.pass)
			waitsTwo(t, "right password after the guesses", err)
			e.clock.Add(auth.FirstDelay - time.Second)
			_, err = from(context.Background(), c.user, c.pass)
			var me *model.Error
			if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != 1 {
				t.Errorf("right password 1 s before the wait is over: %v, want 429 with Retry-After 1", err)
			}
			e.clock.Add(time.Second)
			hashes := auth.HashRuns()
			if s, err := from(context.Background(), c.user, c.pass); err != nil || s == nil || s.User.ID != c.user {
				t.Errorf("right password after the wait: %+v %v", s, err)
			}
			if conn.count() != logins || auth.HashRuns() != hashes {
				t.Error("the remembered success ran a check after the wait")
			}
			conn.letGo()
			for range ldapauth.MaxChecksInFlight {
				if err := <-holders; !errors.Is(err, model.ErrUnauthorized) {
					t.Errorf("a made up name that held a place: %v", err)
				}
			}
		})
	}
}

// Finding 2 of review 6 kept every blocked pair in a full table and refused
// a new pair while every pair was blocked. Limiter rule B (row 3-8 of
// review 3) replaces that: the pair whose last failure is oldest leaves,
// blocked or not, so a new pair always gets in unless every pair has a
// check in flight. The budget per address keeps one address from cycling
// the table (TestOneAddressCannotFillTheTable).
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
	e.clock.Add(time.Millisecond)
	fail("demo-reader", auth.FreeFailures)
	e.clock.Add(time.Second)
	fail("demo-nobody", 1)
	fail("demo-legacy-sha1", 1) // the table is full: demo-admin, the oldest, leaves
	if _, held := auth.PairFailures(e.svc, "demo-admin", addr); held {
		t.Error("the oldest blocked pair stayed")
	}
	if rec := right("demo-reader"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("blocked pair demo-reader after a new pair came in: %d", rec.Code)
	}
	if n := auth.PairCount(e.svc); n != 3 {
		t.Errorf("%d pairs, want 3", n)
	}
	if rec := right("demo-legacy-md5"); rec.Code != http.StatusOK {
		t.Errorf("new pair in a full table: %d", rec.Code)
	}
	if _, held := auth.PairFailures(e.svc, "demo-reader", addr); held {
		t.Error("the oldest blocked pair stayed")
	}
	if n := auth.PairCount(e.svc); n != 2 {
		t.Errorf("%d pairs after a success, want 2", n)
	}
}

// heldClock is a floor clock whose reads wait until the test opens it, so
// the check of a pair holds while the other requests of the pair arrive.
type heldClock struct {
	open    chan struct{}
	entered atomic.Int64
	once    sync.Once
}

func (h *heldClock) now() time.Time {
	h.entered.Add(1)
	<-h.open
	return time.Now()
}

// release opens the clock; a second call does nothing.
func (h *heldClock) release() { h.once.Do(func() { close(h.open) }) }

// Review S2: parallel Basic requests with the right password and an empty
// cache wait for the first check and share its success; none of them gets
// 429 from the reservations of the others.
func TestParallelRightBasicShareOneCheck(t *testing.T) {
	for name, c := range map[string]struct {
		user, pass string
		dirs       []auth.Directory
	}{
		"local user":     {"demo-reader", password(t, "demo-reader"), nil},
		"directory user": {"demo-ldap-person", ldapPassword, []auth.Directory{ldapDirectory()}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, fixture(t), c.dirs...)
			h := &heldClock{open: make(chan struct{})}
			auth.SetFloorClock(e.svc, h.now, time.Sleep)
			const n = 20
			var arrived atomic.Int64
			errs, subjects := make([]error, n), make([]*model.Subject, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := basic(request(http.MethodGet, "/", nil), c.user, c.pass)
					arrived.Add(1)
					subjects[i], errs[i] = e.svc.Subject(r)
				}()
			}
			deadline := time.Now().Add(10 * time.Second)
			for arrived.Load() < n || h.entered.Load() == 0 {
				if time.Now().After(deadline) {
					close(h.open)
					t.Fatalf("after 10 s: %d requests, %d checks", arrived.Load(), h.entered.Load())
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond) // the last requests reach the check or the wait
			held := h.entered.Load()
			close(h.open)
			wg.Wait()
			for i := range n {
				if errs[i] != nil || subjects[i] == nil || subjects[i].User.ID != c.user {
					t.Errorf("request %d: %+v %v", i+1, subjects[i], errs[i])
				}
			}
			if held != 1 {
				t.Errorf("%d checks of one pair at once, want 1", held)
			}
		})
	}
}

// Review R1 of the second security review: a pair that waits after wrong
// passwords is refused with 429 before the cache is asked, whatever password
// it sends, so a remembered success answers no guess while the pair waits
// and no guess reaches a hash or the directory. From another address the
// right password passes, and once the wait is over the remembered success
// answers again without a check. Until R1 this test pinned the opposite
// order of review S2: the remembered password of a waiting pair passed.
func TestWaitingPairBeforeTheCache(t *testing.T) {
	for name, c := range map[string]struct {
		user, pass string
		dir        *directory
	}{
		"local user":     {"demo-reader", password(t, "demo-reader"), nil},
		"directory user": {"demo-ldap-person", ldapPassword, ldapDirectory()},
	} {
		t.Run(name, func(t *testing.T) {
			var dirs []auth.Directory
			if c.dir != nil {
				dirs = append(dirs, c.dir)
			}
			e := newEnv(t, fixture(t), dirs...)
			from := func(address, pass string) (*model.Subject, error) {
				r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), c.user, pass)
				r.RemoteAddr = address + ":40000"
				return e.svc.Subject(r)
			}
			logins := func() int {
				if c.dir == nil {
					return 0
				}
				return c.dir.count()
			}
			waits := func(when string, err error, retry int) {
				t.Helper()
				var me *model.Error
				if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != retry {
					t.Errorf("%s: %v, want 429 with Retry-After %d", when, err, retry)
				}
			}
			const address, other = "192.0.2.64", "192.0.2.65"
			if s, err := from(address, c.pass); err != nil || s == nil {
				t.Fatalf("first request: %v", err)
			}
			for i := range auth.FreeFailures {
				if _, err := from(address, fmt.Sprintf("demo-wrong-pass-%d", i+1)); !errors.Is(err, model.ErrUnauthorized) {
					t.Fatalf("wrong password %d: %v, want 401", i+1, err)
				}
			}
			hashes, calls := auth.HashRuns(), logins()
			for i := range 20 {
				if i%2 == 0 {
					_, err := from(address, c.pass)
					waits(fmt.Sprintf("right password %d of a waiting pair", i/2+1), err, 2)
				} else {
					_, err := from(address, fmt.Sprintf("demo-guess-%d", i))
					waits(fmt.Sprintf("wrong guess %d after the fifth", i/2+1), err, 2)
				}
			}
			if auth.HashRuns() != hashes || logins() != calls {
				t.Errorf("a waiting pair ran %d hashes and %d directory sign ins", auth.HashRuns()-hashes, logins()-calls)
			}
			if s, err := from(other, c.pass); err != nil || s == nil {
				t.Errorf("right password from another address: %v", err)
			}
			e.clock.Add(auth.FirstDelay - time.Second)
			_, err := from(address, c.pass)
			waits("right password 1 s before the wait is over", err, 1)
			e.clock.Add(time.Second)
			if s, err := from(address, c.pass); err != nil || s == nil {
				t.Errorf("right password after the wait: %v", err)
			}
			if auth.HashRuns() != hashes || logins() != calls {
				t.Error("the remembered success ran a check after the wait")
			}
		})
	}
}

// basicUser is a user whose Basic credentials a test sends: a local user,
// or a directory user behind the fake directory.
type basicUser struct {
	user, pass string
	ldap       bool
}

func basicUsers(t *testing.T) map[string]basicUser {
	return map[string]basicUser{
		"local user":     {"demo-reader", password(t, "demo-reader"), false},
		"directory user": {"demo-ldap-person", ldapPassword, true},
	}
}

// basicEnv is a service with the realms of one basicUser.
type basicEnv struct {
	*env
	u   basicUser
	dir *directory
}

func newBasicEnv(t *testing.T, u basicUser) *basicEnv {
	t.Helper()
	b := &basicEnv{u: u}
	if u.ldap {
		b.dir = ldapDirectory()
		b.env = newEnv(t, fixture(t), b.dir)
	} else {
		b.env = newEnv(t, fixture(t))
	}
	return b
}

// checks counts the checks of the realms so far: the sign ins of the
// directory for a directory user, the hashes for a local user.
func (b *basicEnv) checks() int64 {
	if b.dir != nil {
		return int64(b.dir.count())
	}
	return auth.HashRuns()
}

// from sends the Basic credentials of the user with pass from address.
func (b *basicEnv) from(ctx context.Context, address, pass string) (*model.Subject, error) {
	r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), b.u.user, pass)
	r.RemoteAddr = address + ":40000"
	return b.svc.Subject(r.WithContext(ctx))
}

// sent is the answer to one Basic request of send.
type sent struct {
	pass    string
	subject *model.Subject
	err     error
}

// send sends the Basic credentials with each password from address, each
// from a goroutine of its own once start is closed, and puts the answers
// into out.
func (b *basicEnv) send(out chan<- sent, start <-chan struct{}, address string, passwords ...string) {
	for _, pass := range passwords {
		go func() {
			<-start
			s, err := b.from(context.Background(), address, pass)
			out <- sent{pass, s, err}
		}()
	}
}

// burstTally sorts the answers of a burst: the passwords that passed, the
// requests refused with 401 and those throttled with 429 and Retry-After 2.
type burstTally struct {
	passed             []string
	refused, throttled int
}

// tally reads n answers from out; an answer of another kind fails t.
func (b *basicEnv) tally(t *testing.T, out <-chan sent, n int) burstTally {
	t.Helper()
	var c burstTally
	deadline := time.After(30 * time.Second)
	for range n {
		var a sent
		select {
		case a = <-out:
		case <-deadline:
			t.Fatalf("after 30 s: %+v of %d answers", c, n)
		}
		var me *model.Error
		switch {
		case a.err == nil && a.subject != nil && a.subject.User.ID == b.u.user:
			c.passed = append(c.passed, a.pass)
		case errors.Is(a.err, model.ErrUnauthorized):
			c.refused++
		case errors.As(a.err, &me) && me.Status == http.StatusTooManyRequests && me.RetryAfter == 2:
			c.throttled++
		default:
			t.Errorf("answer to %q: %+v %v", a.pass, a.subject, a.err)
		}
	}
	return c
}

// guesses are n wrong passwords, numbered from first.
func guesses(first, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("demo-guess-%d", first+i)
	}
	return out
}

// waitUntil polls cond until it holds, for at most 10 s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("after 10 s still not: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Review R1 of the second security review left a burst open: a pair that
// did not wait was compared with the cache of Basic successes before it
// held a reservation of the limiter, so guesses sent at one moment were all
// compared. Every Basic request now holds a reservation of its pair before
// the cache is asked. 200 wrong passwords at once for a user with a
// remembered success, from one address: FreeFailures of them are compared
// and checked and get 401, every other one waits for a place and gets 429
// with Retry-After 2 once the pair waits.
func TestBurstOfWrongBasicPasswords(t *testing.T) {
	for name, u := range basicUsers(t) {
		t.Run(name, func(t *testing.T) {
			b := newBasicEnv(t, u)
			const address, n = "192.0.2.70", 200
			if s, err := b.from(context.Background(), address, u.pass); err != nil || s == nil {
				t.Fatalf("first request: %v", err)
			}
			asks, checks := auth.CacheAsks(b.svc), b.checks()
			out, start := make(chan sent, n), make(chan struct{})
			b.send(out, start, address, guesses(0, n)...)
			close(start)
			got := b.tally(t, out, n)
			if len(got.passed) != 0 || got.refused != auth.FreeFailures || got.throttled != n-auth.FreeFailures {
				t.Errorf("%d guesses: %d passed, %d refused, %d throttled; want 0, %d, %d",
					n, len(got.passed), got.refused, got.throttled, auth.FreeFailures, n-auth.FreeFailures)
			}
			compared, checked := auth.CacheAsks(b.svc)-asks, b.checks()-checks
			if compared != auth.FreeFailures || checked != auth.FreeFailures {
				t.Errorf("the cache compared %d guesses and the realms checked %d, want %d each", compared, checked, auth.FreeFailures)
			}
		})
	}
}

// The burst of TestBurstOfWrongBasicPasswords with the right password as
// one of the 200. The right password passes only from a place of its own
// pair, after its own comparison with the cache. Sent while five guesses
// hold the places, it waits and gets 429 once the pair waits, and the cache
// compared five passwords in all. Holding one of the five places, it passes
// from the cache and gives its place back without a failure; one more guess
// takes that place, so the cache compared it and five guesses, and the pair
// waits after them as without it. In a burst sent at once the cache
// compares the right password only when it passes and at most FreeFailures
// guesses, and the pair waits afterwards.
func TestBurstWithTheRightBasicPassword(t *testing.T) {
	const address, n = "192.0.2.71", 200
	opened := make(chan struct{})
	close(opened)
	remembered := func(t *testing.T, u basicUser) *basicEnv {
		t.Helper()
		b := newBasicEnv(t, u)
		if s, err := b.from(context.Background(), address, u.pass); err != nil || s == nil {
			t.Fatalf("first request: %v", err)
		}
		return b
	}
	held := func(t *testing.T, b *basicEnv) *heldClock {
		h := &heldClock{open: make(chan struct{})}
		t.Cleanup(h.release)
		auth.SetFloorClock(b.svc, h.now, time.Sleep)
		return h
	}
	waits := func(t *testing.T, b *basicEnv) {
		t.Helper()
		_, err := b.from(context.Background(), address, b.u.pass)
		var me *model.Error
		if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != 2 {
			t.Errorf("right password after the burst: %v, want 429 with Retry-After 2", err)
		}
	}
	for name, u := range basicUsers(t) {
		t.Run(name+", after five guesses took the places", func(t *testing.T) {
			b := remembered(t, u)
			h := held(t, b)
			asks := auth.CacheAsks(b.svc)
			out := make(chan sent, n)
			b.send(out, opened, address, guesses(0, auth.FreeFailures)...)
			waitUntil(t, "five guesses in their checks", func() bool { return h.entered.Load() == auth.FreeFailures })
			b.send(out, opened, address, append([]string{u.pass}, guesses(auth.FreeFailures, n-auth.FreeFailures-1)...)...)
			waitUntil(t, "the other requests wait for a place", func() bool { return auth.PlaceWaiters(b.svc) == n-auth.FreeFailures })
			if got := h.entered.Load(); got != auth.FreeFailures {
				t.Errorf("%d checks ran while the five places were taken", got)
			}
			h.release()
			got := b.tally(t, out, n)
			if len(got.passed) != 0 || got.refused != auth.FreeFailures || got.throttled != n-auth.FreeFailures {
				t.Errorf("%d passed, %d refused, %d throttled; want 0, %d, %d",
					len(got.passed), got.refused, got.throttled, auth.FreeFailures, n-auth.FreeFailures)
			}
			if compared := auth.CacheAsks(b.svc) - asks; compared != auth.FreeFailures {
				t.Errorf("the cache compared %d passwords, want %d", compared, auth.FreeFailures)
			}
			waits(t, b)
		})
		t.Run(name+", in one of the five places", func(t *testing.T) {
			b := remembered(t, u)
			h := held(t, b)
			asks := auth.CacheAsks(b.svc)
			out := make(chan sent, n)
			b.send(out, opened, address, guesses(0, auth.FreeFailures-1)...)
			waitUntil(t, "four guesses in their checks", func() bool { return h.entered.Load() == auth.FreeFailures-1 })
			right := make(chan sent, 1)
			b.send(right, opened, address, u.pass)
			if a := <-right; a.err != nil || a.subject == nil || a.subject.User.ID != u.user {
				t.Fatalf("right password in the fifth place: %+v %v", a.subject, a.err)
			}
			b.send(out, opened, address, guesses(auth.FreeFailures-1, n-auth.FreeFailures)...)
			waitUntil(t, "a guess in the place given back, the others waiting", func() bool {
				return h.entered.Load() == auth.FreeFailures && auth.PlaceWaiters(b.svc) == n-auth.FreeFailures-1
			})
			h.release()
			got := b.tally(t, out, n-1)
			if got.refused != auth.FreeFailures || got.throttled != n-auth.FreeFailures-1 {
				t.Errorf("%d refused, %d throttled; want %d, %d", got.refused, got.throttled, auth.FreeFailures, n-auth.FreeFailures-1)
			}
			if compared := auth.CacheAsks(b.svc) - asks; compared != auth.FreeFailures+1 {
				t.Errorf("the cache compared %d passwords, want the right one and %d guesses", compared, auth.FreeFailures)
			}
			waits(t, b)
		})
		for _, at := range []int{0, n - 1} {
			t.Run(fmt.Sprintf("%s, at once, right password at %d", name, at), func(t *testing.T) {
				b := remembered(t, u)
				asks := auth.CacheAsks(b.svc)
				passwords := slices.Insert(guesses(0, n-1), at, u.pass)
				out, start := make(chan sent, n), make(chan struct{})
				b.send(out, start, address, passwords...)
				close(start)
				got := b.tally(t, out, n)
				compared := auth.CacheAsks(b.svc) - asks
				if len(got.passed) > 1 || (len(got.passed) == 1 && got.passed[0] != u.pass) || got.refused != auth.FreeFailures ||
					len(got.passed)+got.refused+got.throttled != n {
					t.Errorf("passed %q, %d refused, %d throttled; want at most the right password, %d refused",
						got.passed, got.refused, got.throttled, auth.FreeFailures)
				}
				if compared != int64(got.refused+len(got.passed)) {
					t.Errorf("the cache compared %d passwords for %d answers of a check", compared, got.refused+len(got.passed))
				}
				waits(t, b)
			})
		}
	}
}

// Review S2 of the first security review stays closed under the
// reservations of review R1: 20 and 100 parallel Basic requests with the
// right password, with its success remembered and without, for a local
// user and through the fake directory, all pass. FreeFailures of them hold
// the places of the pair; the first check is shared and the others answer
// from the cache, each gives its place back, and the requests beyond the
// places wait for one instead of getting 429. Without a remembered success
// the realms check the password once, with one not at all, and no pair is
// held afterwards.
func TestParallelRightBasicAllPass(t *testing.T) {
	for name, u := range basicUsers(t) {
		for _, n := range []int{20, 100} {
			for _, warm := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s, %d requests, remembered %v", name, n, warm), func(t *testing.T) {
					b := newBasicEnv(t, u)
					const address = "192.0.2.72"
					if warm {
						if s, err := b.from(context.Background(), address, u.pass); err != nil || s == nil {
							t.Fatalf("first request: %v", err)
						}
					}
					checks := b.checks()
					out, start := make(chan sent, n), make(chan struct{})
					b.send(out, start, address, slices.Repeat([]string{u.pass}, n)...)
					close(start)
					got := b.tally(t, out, n)
					if len(got.passed) != n {
						t.Errorf("%d of %d passed, %d refused, %d throttled", len(got.passed), n, got.refused, got.throttled)
					}
					want := int64(1)
					if warm {
						want = 0
					}
					if c := b.checks() - checks; c != want {
						t.Errorf("the realms checked the password %d times, want %d", c, want)
					}
					if p := auth.PairCount(b.svc); p != 0 {
						t.Errorf("%d pairs held after the requests, want 0", p)
					}
				})
			}
		}
	}
}

// A Basic request that finds every place of its pair taken waits for one,
// at most PlaceWait, then gets 429 with Retry-After 2 as a pair whose free
// failures are in flight, and the cache compared nothing for it. A request
// whose context ends while it waits leaves at once, without a comparison
// and without a reservation.
func TestBasicWaitsForAPlaceAtMostPlaceWait(t *testing.T) {
	b := newBasicEnv(t, basicUsers(t)["local user"])
	const address = "192.0.2.73"
	h := &heldClock{open: make(chan struct{})}
	t.Cleanup(h.release)
	auth.SetFloorClock(b.svc, h.now, time.Sleep)
	opened := make(chan struct{})
	close(opened)
	out := make(chan sent, auth.FreeFailures)
	b.send(out, opened, address, guesses(0, auth.FreeFailures)...)
	waitUntil(t, "five guesses in their checks", func() bool { return h.entered.Load() == auth.FreeFailures })
	asks := auth.CacheAsks(b.svc)
	start := time.Now()
	_, err := b.from(context.Background(), address, "demo-guess-waits")
	took := time.Since(start)
	var me *model.Error
	if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != 2 ||
		took < auth.PlaceWait || took > auth.PlaceWait+time.Second {
		t.Errorf("a request beyond the places: %v after %v, want 429 with Retry-After 2 after %v", err, took, auth.PlaceWait)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gone := make(chan error, 1)
	go func() {
		_, err := b.from(ctx, address, "demo-guess-gone")
		gone <- err
	}()
	waitUntil(t, "a request waits for a place", func() bool { return auth.PlaceWaiters(b.svc) == 1 })
	cancel()
	select {
	case err := <-gone:
		if !errors.Is(err, model.ErrUnauthorized) {
			t.Errorf("a request whose context ended while it waited: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a request whose context ended still waits after 1 s")
	}
	if w, compared := auth.PlaceWaiters(b.svc), auth.CacheAsks(b.svc)-asks; w != 0 || compared != 0 {
		t.Errorf("%d requests wait, the cache compared %d passwords beyond the places; want 0 and 0", w, compared)
	}
	h.release()
	if got := b.tally(t, out, auth.FreeFailures); got.refused != auth.FreeFailures {
		t.Errorf("the five guesses in the places: %+v", got)
	}
	_, err = b.from(context.Background(), address, b.u.pass)
	if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != 2 {
		t.Errorf("right password after five failures: %v, want 429", err)
	}
}

// A remembered success answers without a check of its own and gives its
// reservation back without a result (review R1): it forgets none of the
// failures of its pair. Otherwise the hits of a client with the right
// password, behind the same address as guesses at the same name (a tunnel
// gives every client 127.0.0.1), would clear their failures again and
// again. Four guesses between remembered successes and a fifth make the
// pair wait, and a sixth guess gets 429 without a check.
func TestRememberedSuccessForgetsNoFailure(t *testing.T) {
	for name, u := range basicUsers(t) {
		t.Run(name, func(t *testing.T) {
			b := newBasicEnv(t, u)
			const address = "192.0.2.74"
			ctx := context.Background()
			if s, err := b.from(ctx, address, u.pass); err != nil || s == nil {
				t.Fatalf("first request: %v", err)
			}
			for i := range auth.FreeFailures - 1 {
				if _, err := b.from(ctx, address, fmt.Sprintf("demo-guess-%d", i+1)); !errors.Is(err, model.ErrUnauthorized) {
					t.Fatalf("guess %d: %v, want 401", i+1, err)
				}
				if s, err := b.from(ctx, address, u.pass); err != nil || s == nil {
					t.Fatalf("remembered password after guess %d: %v", i+1, err)
				}
			}
			if _, err := b.from(ctx, address, "demo-guess-5"); !errors.Is(err, model.ErrUnauthorized) {
				t.Fatalf("guess 5: %v, want 401", err)
			}
			checks := b.checks()
			_, err := b.from(ctx, address, "demo-guess-6")
			var me *model.Error
			if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != 2 {
				t.Errorf("guess 6 after %d remembered successes between the guesses: %v, want 429 with Retry-After 2",
					auth.FreeFailures-1, err)
			}
			if c := b.checks() - checks; c != 0 {
				t.Errorf("guess 6 ran %d checks, want 0", c)
			}
		})
	}
}
