package auth_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The credential cache of decision N8 of the phase 3 plan: a successful check
// of Basic credentials is remembered for 300 s, keyed by an HMAC of realm,
// user and password under a secret of the process.

func basicOf(e *env, user, pass string) (*model.Subject, error) {
	return e.svc.Subject(basic(request(http.MethodGet, "/repository/demo-maven/", nil), user, pass))
}

// The proof of leg S1: 100 Basic requests of one user compute 1 hash, and
// after 300 s at the fake clock one more.
func TestCredentialCacheCountsHashes(t *testing.T) {
	e := newEnv(t, fixture(t))
	pass := password(t, "demo-reader")
	want := []string{"nexus:repository-view:maven2:demo-maven:browse", "nexus:repository-view:maven2:demo-maven:read"}
	before := auth.HashRuns()
	for i := range 100 {
		s, err := basicOf(e, "demo-reader", pass)
		if err != nil || s == nil || s.User.ID != "demo-reader" || s.Realm != auth.RealmLocal || s.Session != "" ||
			!slices.Equal(s.Permissions, want) {
			t.Fatalf("request %d: %+v %v", i+1, s, err)
		}
	}
	if runs := auth.HashRuns() - before; runs != 1 {
		t.Fatalf("100 Basic requests of one user computed %d hashes, want 1", runs)
	}
	e.clock.Add(auth.CredentialTime - time.Second)
	if _, err := basicOf(e, "demo-reader", pass); err != nil || auth.HashRuns()-before != 1 {
		t.Fatalf("after 299 s: %v, %d hashes", err, auth.HashRuns()-before)
	}
	e.clock.Add(time.Second)
	if _, err := basicOf(e, "demo-reader", pass); err != nil || auth.HashRuns()-before != 2 {
		t.Fatalf("after 300 s: %v, %d hashes, want 2", err, auth.HashRuns()-before)
	}
	if auth.CredentialTime != 300*time.Second || auth.MaxCredentials != 10_000 || auth.CredentialLimit(e.svc) != auth.MaxCredentials {
		t.Fatalf("bounds %v and %d", auth.CredentialTime, auth.CredentialLimit(e.svc))
	}
}

// Only a success is remembered, and only for the exact password: a wrong
// password beside a remembered right one is refused and runs a hash.
func TestCredentialCacheKeepsRefusals(t *testing.T) {
	e := newEnv(t, fixture(t))
	pass := password(t, "demo-reader")
	if _, err := basicOf(e, "demo-reader", pass); err != nil {
		t.Fatal(err)
	}
	// a remembered subject is a copy
	s, _ := basicOf(e, "demo-reader", pass)
	s.Permissions[0] = "nexus:*"
	s.User.Roles = append(s.User.Roles[:0], "nx-admin")
	again, _ := basicOf(e, "demo-reader", pass)
	if again.Permissions[0] == "nexus:*" || slices.Contains(again.User.Roles, "nx-admin") {
		t.Errorf("a change of one subject reached the cache: %q %q", again.Permissions, again.User.Roles)
	}
	for i, wrong := range []string{pass + "x", pass[:len(pass)-1], "", "DEMO-READER-PASS-1"} {
		before := auth.HashRuns()
		if s, err := basicOf(e, "demo-reader", wrong); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Fatalf("wrong password %d: %+v %v", i, s, err)
		}
		if runs := auth.HashRuns() - before; runs != 1 {
			t.Errorf("wrong password %d ran %d hashes, want 1", i, runs)
		}
	}
	if n := auth.CredentialCount(e.svc); n != 1 {
		t.Errorf("%d remembered credentials, want 1", n)
	}
	// the user name in other letters with the right password is no hit
	// either (the fifth failure of the pair, so still refused with 401)
	if s, err := basicOf(e, "Demo-Reader", pass); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("the name in other letters: %+v %v", s, err)
	}
}

// The hook that empties the cache: a change of a user, of its roles or of
// the realm order calls it (N8), and the next request checks again.
func TestForgetCredentials(t *testing.T) {
	e := newEnv(t, fixture(t))
	pass := password(t, "demo-admin")
	before := auth.HashRuns()
	for range 3 {
		if _, err := basicOf(e, "demo-admin", pass); err != nil {
			t.Fatal(err)
		}
	}
	if runs := auth.HashRuns() - before; runs != 1 || auth.CredentialCount(e.svc) != 1 {
		t.Fatalf("%d hashes, %d remembered", runs, auth.CredentialCount(e.svc))
	}
	e.svc.ForgetCredentials()
	if n := auth.CredentialCount(e.svc); n != 0 {
		t.Fatalf("%d remembered after the hook", n)
	}
	if _, err := basicOf(e, "demo-admin", pass); err != nil || auth.HashRuns()-before != 2 {
		t.Fatalf("after the hook: %v, %d hashes, want 2", err, auth.HashRuns()-before)
	}
}

// Hardening line 9: the cache has a bound and a test that fills past it. A
// full cache drops the expired entries first, then the oldest.
func TestCredentialCacheBound(t *testing.T) {
	e := newEnv(t, fixture(t))
	name := func(i int) string { return fmt.Sprintf("demo-fill-%d", i) }
	for i := range auth.MaxCredentials + 50 {
		auth.RememberCredential(e.svc, auth.RealmLocal, name(i), "demo-fill-pass")
		e.clock.Add(time.Millisecond)
	}
	if n := auth.CredentialCount(e.svc); n != auth.MaxCredentials {
		t.Fatalf("%d remembered after filling past the bound of %d", n, auth.MaxCredentials)
	}
	for _, i := range []int{0, 49} {
		if auth.CredentialHeld(e.svc, auth.RealmLocal, name(i), "demo-fill-pass") {
			t.Errorf("entry %d of the oldest 50 is held", i)
		}
	}
	for _, i := range []int{50, auth.MaxCredentials + 49} {
		if !auth.CredentialHeld(e.svc, auth.RealmLocal, name(i), "demo-fill-pass") {
			t.Errorf("entry %d is gone", i)
		}
	}

	e = newEnv(t, fixture(t))
	auth.SetMaxCredentials(e.svc, 3)
	auth.RememberCredential(e.svc, auth.RealmLocal, "demo-a", "demo-pass")
	e.clock.Add(200 * time.Second)
	auth.RememberCredential(e.svc, auth.RealmLocal, "demo-b", "demo-pass")
	auth.RememberCredential(e.svc, auth.RealmLocal, "demo-c", "demo-pass")
	e.clock.Add(101 * time.Second) // demo-a has expired, demo-b and demo-c have not
	auth.RememberCredential(e.svc, auth.RealmLocal, "demo-d", "demo-pass")
	if auth.CredentialCount(e.svc) != 3 || auth.CredentialHeld(e.svc, auth.RealmLocal, "demo-a", "demo-pass") {
		t.Errorf("the expired entry stays: %d remembered", auth.CredentialCount(e.svc))
	}
	for _, user := range []string{"demo-b", "demo-c", "demo-d"} {
		if !auth.CredentialHeld(e.svc, auth.RealmLocal, user, "demo-pass") {
			t.Errorf("%s is gone although an expired entry made room", user)
		}
	}
	// the realm is part of the key
	if auth.CredentialHeld(e.svc, auth.RealmLDAP, "demo-b", "demo-pass") {
		t.Error("an entry of the local realm answers for LDAP")
	}
}
