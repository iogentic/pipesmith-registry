package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Decision E10 of the execution plan: Basic credentials are checked against
// LDAP as well, behind the credential cache, so the Maven, npm and Docker
// clients of directory users keep working. It replaces decision T7 of the
// thin slice for Basic.
func TestBasicDirectory(t *testing.T) {
	d := ldapDirectory()
	e := newEnv(t, fixture(t), d)
	want := []string{"nexus:repository-view:raw:demo-raw:read", "nexus:settings:read"}
	for i := range 100 {
		s, err := basicOf(e, "demo-ldap-person", ldapPassword)
		if err != nil || s == nil || s.User.ID != "demo-ldap-person" || s.User.Source != auth.SourceLDAP ||
			s.Realm != auth.RealmLDAP || s.Session != "" || !slices.Equal(s.Permissions, want) {
			t.Fatalf("request %d: %+v %v", i+1, s, err)
		}
	}
	if n := d.count(); n != 1 {
		t.Fatalf("100 Basic requests of a directory user asked the directory %d times, want 1", n)
	}
	e.clock.Add(auth.CredentialTime)
	if _, err := basicOf(e, "demo-ldap-person", ldapPassword); err != nil || d.count() != 2 {
		t.Fatalf("after 300 s: %v, %d directory calls, want 2", err, d.count())
	}
	// a wrong password is refused, and each try asks the directory
	calls := d.count()
	for i := range 3 {
		if s, err := basicOf(e, "demo-ldap-person", "demo-wrong-pass-1"); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Fatalf("wrong password %d: %+v %v", i+1, s, err)
		}
	}
	if d.count() != calls+3 {
		t.Errorf("3 wrong passwords made %d directory calls, want 3", d.count()-calls)
	}
	// a local user with the right password does not reach the directory
	calls = d.count()
	if s, err := basicOf(e, "demo-admin", password(t, "demo-admin")); err != nil || s.Realm != auth.RealmLocal || d.count() != calls {
		t.Errorf("local user: %+v %v, %d directory calls", s, err, d.count()-calls)
	}

	// without the LDAP realm in the stored order Basic asks no directory
	snap := fixture(t)
	snap.Realms = []string{auth.RealmLocal, "NexusAuthorizingRealm"}
	d = ldapDirectory()
	e = newEnv(t, snap, d)
	if s, err := basicOf(e, "demo-ldap-person", ldapPassword); s != nil || !errors.Is(err, model.ErrUnauthorized) || d.count() != 0 {
		t.Errorf("LDAP realm off: %+v %v, %d directory calls", s, err, d.count())
	}
}

// hashTime is what one hash takes on the floor clock.
const hashTime = time.Millisecond

// floorClock stands for the real clock of the refusal floor: each hash of
// VerifyPassword takes hashTime on it, a directory moves it on, and the
// floor records its sleeps instead of sleeping.
type floorClock struct {
	mu     sync.Mutex
	t      time.Time
	hashes int64 // auth.HashRuns when the clock started
	sleeps []time.Duration
}

func newFloorClock() *floorClock {
	return &floorClock{t: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), hashes: auth.HashRuns()}
}

func (f *floorClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t.Add(time.Duration(auth.HashRuns()-f.hashes) * hashTime)
}

func (f *floorClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func (f *floorClock) sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps = append(f.sleeps, d)
	f.t = f.t.Add(d)
}

// taken returns the sleeps since the last call.
func (f *floorClock) taken() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sleeps
	f.sleeps = nil
	return out
}

// slowDirectory refuses every pair after it took its time on the floor
// clock.
type slowDirectory struct {
	clock *floorClock
	mu    sync.Mutex
	takes time.Duration
}

func (d *slowDirectory) Login(context.Context, string, string) (model.User, []string, error) {
	d.mu.Lock()
	takes := d.takes
	d.mu.Unlock()
	d.clock.advance(takes)
	return model.User{}, nil, fmt.Errorf("demo slow directory: %w", auth.ErrWrongPair)
}

func (d *slowDirectory) Lookup(context.Context, string) (model.User, []string, error) {
	return model.User{}, nil, fmt.Errorf("demo slow directory: %w", auth.ErrNoUser)
}

func (d *slowDirectory) set(takes time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.takes = takes
}

// The refusal floor covers the whole check, the hash of the local check and
// the directories together, and sleeps once for what is left of it: neither
// the hash form of a user nor a directory that refuses within the floor adds
// time a client can measure.
func TestRefusalFloorWholeCheck(t *testing.T) {
	fc := newFloorClock()
	d := &slowDirectory{clock: fc, takes: 3 * time.Millisecond}
	e := newEnv(t, fixture(t), d)
	auth.SetFloorClock(e.svc, fc.now, fc.sleep)
	rest := []time.Duration{auth.RefusalFloor - hashTime - 3*time.Millisecond}

	if s, err := basicOf(e, "demo-ldap-nobody", "demo-wrong-pass-1"); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("Basic of an unknown name: %+v %v", s, err)
	}
	if got := fc.taken(); !slices.Equal(got, rest) {
		t.Errorf("Basic, a directory of 3 ms: sleeps %v, want %v", got, rest)
	}
	if rec := e.signIn("demo-admin", "demo-wrong-pass-1", "192.0.2.80"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign in: %d", rec.Code)
	}
	if got := fc.taken(); !slices.Equal(got, rest) {
		t.Errorf("sign in, a directory of 3 ms: sleeps %v, want %v", got, rest)
	}
	// a directory slower than the floor adds no sleep
	d.set(7 * time.Millisecond)
	if _, err := basicOf(e, "demo-ldap-nobody", "demo-wrong-pass-2"); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("Basic, a directory of 7 ms: %v", err)
	}
	if got := fc.taken(); len(got) != 0 {
		t.Errorf("Basic, a directory of 7 ms: sleeps %v, want none", got)
	}
	// a success is not slowed down
	if s, err := basicOf(e, "demo-reader", password(t, "demo-reader")); err != nil || s == nil {
		t.Fatalf("Basic of demo-reader: %v", err)
	}
	if got := fc.taken(); len(got) != 0 {
		t.Errorf("a success slept %v", got)
	}
}
