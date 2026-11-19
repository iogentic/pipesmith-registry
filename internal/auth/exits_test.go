package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// scripted is a directory whose answer to a sign in each row of
// TestEveryExitSettles writes. waits counts the sign ins that entered a
// script that waits.
type scripted struct {
	mu     sync.Mutex
	answer script
	waits  atomic.Int64
}

// script is the answer of a scripted directory to one sign in.
type script func(ctx context.Context, d *scripted, user, pass string) (model.User, []string, error)

func (d *scripted) Login(ctx context.Context, user, pass string) (model.User, []string, error) {
	d.mu.Lock()
	answer := d.answer
	d.mu.Unlock()
	return answer(ctx, d, user, pass)
}

func (d *scripted) Lookup(context.Context, string) (model.User, []string, error) {
	return model.User{}, nil, fmt.Errorf("demo script: %w", auth.ErrNoUser)
}

// play makes s the answer to the next sign ins.
func (d *scripted) play(s script) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answer = s
}

// answers is a script that answers every sign in with err.
func answers(err error) script {
	return func(context.Context, *scripted, string, string) (model.User, []string, error) {
		return model.User{}, nil, err
	}
}

// untilTheEnd is a script that waits for the end of the request and then
// answers with kind, as a directory does whose step stops with the context.
func untilTheEnd(kind error, step string) script {
	return func(ctx context.Context, d *scripted, _, _ string) (model.User, []string, error) {
		d.waits.Add(1)
		<-ctx.Done()
		return model.User{}, nil, fmt.Errorf("demo script: %w: %s: %w", kind, step, ctx.Err())
	}
}

// The answers a directory gives a sign in, as package ldapauth gives them
// through the adapter of package cli.
var (
	accepts script = func(_ context.Context, _ *scripted, user, pass string) (model.User, []string, error) {
		if user == "demo-ldap-person" && pass == ldapPassword {
			return model.User{ID: user, FirstName: "Lena", Source: "LDAP", Status: "active"}, nil, nil
		}
		return model.User{}, nil, fmt.Errorf("demo script: %w", auth.ErrWrongPair)
	}
	refuses = answers(fmt.Errorf("demo script: %w", auth.ErrWrongPair))
	silent  = answers(fmt.Errorf("demo script: %w: connect: connection refused", auth.ErrUnreachable))
	odd     = answers(errors.New("demo script: an answer of no known kind"))
	noID    = func(context.Context, *scripted, string, string) (model.User, []string, error) {
		return model.User{Source: "LDAP", Status: "active"}, nil, nil
	}
	panics script = func(context.Context, *scripted, string, string) (model.User, []string, error) {
		panic("demo script panic")
	}
	noPlace       = answers(fmt.Errorf("demo script: %w: every place is taken", auth.ErrBusy))
	noPlaceInTime = func(context.Context, *scripted, string, string) (model.User, []string, error) {
		time.Sleep(20 * time.Millisecond)
		return model.User{}, nil, fmt.Errorf("demo script: %w: no place came free within 20ms", auth.ErrBusy)
	}
	endsInTheWait = untilTheEnd(auth.ErrBusy, "the request ended while it waited for a place")
	endsInTheBind = untilTheEnd(auth.ErrUnreachable, "user bind")
)

// record is what the settlement of a check put on record for its pair, which
// held one failure before.
type record string

const (
	failedOnce record = "one failure more"
	forgiven   record = "the failures forgotten"
	kept       record = "the failures kept"
)

// recordOf reads the record of the pair of a user name and an address that
// held one failure before the check.
func recordOf(t *testing.T, s *auth.Service, user, address string) record {
	t.Helper()
	switch n, held := auth.PairFailures(s, user, address); {
	case !held:
		return forgiven
	case n == 1:
		return kept
	case n == 2:
		return failedOnce
	default:
		t.Errorf("the pair holds %d failures", n)
		return ""
	}
}

// passwordOf is the password of a user of the rows; any other name has none.
func passwordOf(t *testing.T, user string) string {
	switch user {
	case "demo-ldap-person":
		return ldapPassword
	case "demo-reader", "demo-admin":
		return password(t, user)
	}
	return "demo-any-pass"
}

// basicFrom sends Basic credentials from address under ctx.
func basicFrom(ctx context.Context, e *env, address, user, pass string) (*model.Subject, error) {
	r := basic(request(http.MethodGet, "/repository/demo-maven/", nil), user, pass)
	r.RemoteAddr = address + ":40000"
	return e.svc.Subject(r.WithContext(ctx))
}

// codeOf is the status of the answer to a Basic request: 200, 401, 429 with
// Retry-After 2, else -1.
func codeOf(s *model.Subject, err error) int {
	var me *model.Error
	switch {
	case err == nil && s != nil:
		return http.StatusOK
	case errors.Is(err, model.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.As(err, &me) && me.Status == http.StatusTooManyRequests && me.RetryAfter == 2:
		return http.StatusTooManyRequests
	}
	return -1
}

// exitRow is one way out of a check, from the reservation of its pair to the
// settlement, and what the settlement puts on record (finding T1 of review
// 3): a check that compared the password with anything ends as a success or
// a failure, and only a check that compared nothing keeps the record as it
// was. A remembered success is a success that forgets no failure (review R1).
type exitRow struct {
	name       string
	user       string   // demo-ldap-person is a directory user, demo-reader a local user
	realms     []string // the stored order; nil is the one of the fixture, the local realm first
	remembered bool     // a Basic success with the password of the user before the row
	session    bool     // POST /api/session, which asks no cache, in place of Basic
	right      bool     // the password of the user, else a guess
	answer     script   // the answer of the directory
	second     script   // the answer of a second directory after it; nil for none
	ends       bool     // the client ends the request while the directory waits
	code       int      // the answer to the client; 0 for a panic, which passes on
	record     record
}

// ldapFirst is a stored order with the LDAP realm before the local realm.
var ldapFirst = []string{auth.RealmLDAP, auth.RealmLocal}

var exitRows = []exitRow{
	{name: "a remembered success", user: "demo-ldap-person", remembered: true, right: true, answer: refuses,
		code: http.StatusOK, record: kept},
	{name: "the local realm accepts", user: "demo-reader", right: true, answer: refuses, code: http.StatusOK, record: forgiven},
	{name: "the local realm refuses", user: "demo-reader", answer: refuses, code: http.StatusUnauthorized, record: failedOnce},
	{name: "the directory accepts", user: "demo-ldap-person", right: true, answer: accepts, code: http.StatusOK, record: forgiven},
	{name: "the directory refuses", user: "demo-ldap-person", answer: refuses, code: http.StatusUnauthorized, record: failedOnce},
	{name: "the directory refuses a guess at a remembered success", user: "demo-ldap-person", remembered: true, answer: refuses,
		code: http.StatusUnauthorized, record: failedOnce},
	{name: "the directory does not answer", user: "demo-ldap-person", remembered: true, answer: silent,
		code: http.StatusUnauthorized, record: failedOnce},
	{name: "the request ends during the bind", user: "demo-ldap-person", remembered: true, answer: endsInTheBind, ends: true,
		code: http.StatusUnauthorized, record: failedOnce},
	{name: "an error of no known kind", user: "demo-ldap-person", remembered: true, answer: odd,
		code: http.StatusUnauthorized, record: failedOnce},
	{name: "an answer without a user id", user: "demo-ldap-person", answer: noID, code: http.StatusUnauthorized, record: failedOnce},
	{name: "a panic of the directory", user: "demo-ldap-person", remembered: true, answer: panics, code: 0, record: failedOnce},
	{name: "an unknown name", user: "demo-unknown-person", answer: refuses, code: http.StatusUnauthorized, record: failedOnce},
	{name: "no place, nothing compared", user: "demo-ldap-person", answer: noPlace, code: http.StatusTooManyRequests, record: kept},
	{name: "no place after a guess at a remembered success", user: "demo-ldap-person", remembered: true, answer: noPlace,
		code: http.StatusTooManyRequests, record: failedOnce},
	{name: "no place after the local realm compared", user: "demo-reader", answer: noPlace,
		code: http.StatusTooManyRequests, record: failedOnce},
	{name: "the wait for a place ends at its bound, nothing compared", user: "demo-ldap-person", answer: noPlaceInTime,
		code: http.StatusTooManyRequests, record: kept},
	{name: "the wait for a place ends at its bound after a guess at a remembered success", user: "demo-ldap-person",
		remembered: true, answer: noPlaceInTime, code: http.StatusTooManyRequests, record: failedOnce},
	{name: "the request ends while it waits for a place, nothing compared", user: "demo-ldap-person", answer: endsInTheWait,
		ends: true, code: http.StatusTooManyRequests, record: kept},
	{name: "the request ends while it waits for a place after a guess at a remembered success", user: "demo-ldap-person",
		remembered: true, answer: endsInTheWait, ends: true, code: http.StatusTooManyRequests, record: failedOnce},
	{name: "session, the directory accepts", user: "demo-ldap-person", session: true, right: true, answer: accepts,
		code: http.StatusOK, record: forgiven},
	{name: "session, a panic of the directory", user: "demo-ldap-person", session: true, answer: panics, code: 0, record: failedOnce},
	{name: "session, no place, nothing compared", user: "demo-ldap-person", session: true, answer: noPlace,
		code: http.StatusTooManyRequests, record: kept},
	{name: "session, no place for a name with a remembered success, which no session compares", user: "demo-ldap-person",
		remembered: true, session: true, answer: noPlace, code: http.StatusTooManyRequests, record: kept},
	{name: "session, no place after the local realm compared", user: "demo-reader", session: true, answer: noPlace,
		code: http.StatusTooManyRequests, record: failedOnce},
	{name: "the LDAP realm first, no place, then the local realm accepts", user: "demo-reader", realms: ldapFirst, right: true,
		answer: noPlace, code: http.StatusOK, record: forgiven},
	{name: "the LDAP realm first, no place, then the local realm refuses", user: "demo-reader", realms: ldapFirst,
		answer: noPlace, code: http.StatusTooManyRequests, record: failedOnce},
	{name: "the LDAP realm first, no place, an unknown name", user: "demo-unknown-person", realms: ldapFirst,
		answer: noPlace, code: http.StatusTooManyRequests, record: kept},
	{name: "session, the LDAP realm first, no place, then the local realm accepts", user: "demo-reader", realms: ldapFirst,
		session: true, right: true, answer: noPlace, code: http.StatusOK, record: forgiven},
	{name: "two directories, the first has no place, the second accepts", user: "demo-ldap-person", right: true,
		answer: noPlace, second: accepts, code: http.StatusOK, record: forgiven},
	{name: "two directories, the first has no place, the second refuses", user: "demo-ldap-person",
		answer: noPlace, second: refuses, code: http.StatusTooManyRequests, record: failedOnce},
	{name: "two directories, the first refuses, the second has no place", user: "demo-ldap-person",
		answer: refuses, second: noPlace, code: http.StatusTooManyRequests, record: failedOnce},
}

// sharedRow is a way out of a Basic request that waits for the check of an
// earlier request with the same user name and password. It compared nothing
// of its own unless it runs a check of its own after a refusal.
type sharedRow struct {
	name   string
	first  script // the answer of the directory to the first request, once the test lets it go
	ends   bool   // the second request ends while it waits for the first
	code   int    // the answer to the second request
	record record // of the pair of the second request
}

var sharedRows = []sharedRow{
	{name: "the request ends while it waits for the check of another request", first: refuses, ends: true,
		code: http.StatusUnauthorized, record: kept},
	{name: "the success of the check of another request", first: accepts, code: http.StatusOK, record: kept},
	{name: "no place for the check of another request", first: noPlace, code: http.StatusTooManyRequests, record: kept},
	{name: "a refusal of the check of another request, then a check of its own", first: refuses,
		code: http.StatusUnauthorized, record: failedOnce},
}

// Every way out of a check settles its pair by the one rule of finding T1 of
// review 3. Each row starts with one failure on record for its pair and
// sends one request; the table names what the settlement put on record.
func TestEveryExitSettles(t *testing.T) {
	for _, row := range exitRows {
		t.Run(row.name, func(t *testing.T) {
			const address = "192.0.2.78"
			d := &scripted{answer: accepts}
			dirs := []auth.Directory{d}
			if row.second != nil {
				dirs = append(dirs, &scripted{answer: row.second})
			}
			snap := fixture(t)
			if row.realms != nil {
				snap.Realms = row.realms
			}
			e := newEnv(t, snap, dirs...)
			if row.remembered {
				if s, err := basicFrom(context.Background(), e, address, row.user, passwordOf(t, row.user)); err != nil || s == nil {
					t.Fatalf("first request: %v", err)
				}
			}
			auth.FailPair(e.svc, row.user, address, 1)
			pass := "demo-guess-1"
			if row.right {
				pass = passwordOf(t, row.user)
			}
			d.play(row.answer)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			codes := make(chan int, 1)
			go func() {
				defer func() {
					if recover() != nil {
						codes <- 0
					}
				}()
				if !row.session {
					codes <- codeOf(basicFrom(ctx, e, address, row.user, pass))
					return
				}
				rec := e.do(signInRequest(row.user, pass, address).WithContext(ctx))
				if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "2" {
					codes <- -1
					return
				}
				codes <- rec.Code
			}()
			if row.ends {
				waitUntil(t, "the directory waits for the end of the request", func() bool { return d.waits.Load() == 1 })
				cancel()
			}
			select {
			case code := <-codes:
				if code != row.code {
					t.Errorf("answer %d, want %d", code, row.code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no answer after 10 s")
			}
			if got := recordOf(t, e.svc, row.user, address); got != row.record {
				t.Errorf("on record: %s, want %s", got, row.record)
			}
			if n := auth.PairsInFlight(e.svc); n != 0 {
				t.Errorf("%d pairs still in flight", n)
			}
		})
	}
	for _, row := range sharedRows {
		t.Run(row.name, func(t *testing.T) {
			const first, second = "192.0.2.79", "192.0.2.80"
			d := &scripted{}
			hold := make(chan struct{})
			d.play(func(ctx context.Context, d *scripted, user, pass string) (model.User, []string, error) {
				d.waits.Add(1)
				<-hold
				return row.first(ctx, d, user, pass)
			})
			e := newEnv(t, fixture(t), d)
			auth.FailPair(e.svc, "demo-ldap-person", second, 1)
			firstCode, secondCode := make(chan int, 1), make(chan int, 1)
			go func() {
				firstCode <- codeOf(basicFrom(context.Background(), e, first, "demo-ldap-person", ldapPassword))
			}()
			waitUntil(t, "the check of the first request waits", func() bool { return d.waits.Load() == 1 })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { secondCode <- codeOf(basicFrom(ctx, e, second, "demo-ldap-person", ldapPassword)) }()
			waitUntil(t, "the second request waits for the first", func() bool { return auth.CheckWaiters(e.svc) == 1 })
			if row.ends {
				cancel()
				if code := <-secondCode; code != row.code {
					t.Errorf("answer %d, want %d", code, row.code)
				}
				close(hold)
			} else {
				close(hold)
				if code := <-secondCode; code != row.code {
					t.Errorf("answer %d, want %d", code, row.code)
				}
			}
			<-firstCode
			if got := recordOf(t, e.svc, "demo-ldap-person", second); got != row.record {
				t.Errorf("on record: %s, want %s", got, row.record)
			}
			if n := auth.PairsInFlight(e.svc); n != 0 {
				t.Errorf("%d pairs still in flight", n)
			}
		})
	}
}
