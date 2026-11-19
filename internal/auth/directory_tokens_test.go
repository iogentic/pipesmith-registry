package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Review B1: an npm or user token of a directory user signs in while a
// directory of the stored order knows its user. The user and its groups come
// from a lookup under the service bind, remembered for auth.CredentialTime
// and emptied by a reload like the credential cache, so the role of a group
// holds with a token as with Basic, and a user that left the directory is
// refused once the answer ends.

// personRights are the rights of demo-ldap-person: its group demo-ldap-group
// and its role mapping demo-ops.
var personRights = []string{"nexus:repository-view:raw:demo-raw:read", "nexus:settings:read"}

// ldapToken writes a token row of a directory user, as npm login or the
// token command does, and gives its wire form.
func ldapToken(t testing.TB, e *env, store auth.Store, kind, id string) string {
	t.Helper()
	text, row, err := auth.NewToken(kind, id, auth.SourceLDAP, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddToken(row, auth.MaxTokensPerKind); err != nil {
		t.Fatal(err)
	}
	return text
}

func TestTokenOfADirectoryUser(t *testing.T) {
	d := ldapDirectory()
	e, store := newStoreEnv(t, tokenSnapshot(t), d)
	npm := npmLogin(t, e, "demo-ldap-person", ldapPassword).Text
	user := ldapToken(t, e, store, auth.KindUser, "demo-ldap-person")
	uses := map[string]func() *http.Request{
		"npm token":              func() *http.Request { return bearer(npm) },
		"user token":             func() *http.Request { return bearer(user) },
		"user token as password": func() *http.Request { return basic(request(http.MethodGet, "/", nil), "demo-ldap-person", user) },
	}
	accepted := func(when string) {
		t.Helper()
		for name, r := range uses {
			s, err := subjectOf(e, r())
			if err != nil || s == nil || s.User.ID != "demo-ldap-person" || s.User.Source != auth.SourceLDAP ||
				!slices.Equal(s.Permissions, personRights) {
				t.Fatalf("%s, %s: %+v %v", when, name, s, err)
			}
		}
	}
	refused := func(when string) {
		t.Helper()
		for name, r := range uses {
			if s, err := subjectOf(e, r()); s != nil || !errors.Is(err, model.ErrUnauthorized) {
				t.Errorf("%s, %s: %+v %v, want a refusal", when, name, s, err)
			}
		}
	}
	e.clock.Add(auth.ReloadInterval) // the issues raised the generation

	// the groups of the directory grant their roles, as with Basic, and one
	// lookup answers every use of the person
	accepted("first use")
	if n := d.lookupCount(); n != 1 {
		t.Errorf("%d lookups for the first uses, want 1", n)
	}
	// a person removed from the directory: the answer holds for
	// CredentialTime, then every token of the person is refused
	d.set("demo-ldap-person", "")
	e.clock.Add(auth.CredentialTime - time.Second)
	accepted("removed, before the answer ended")
	e.clock.Add(time.Second)
	refused("removed, after the answer ended")
	// the refusal is an answer as well; a reload empties the answers
	d.set("demo-ldap-person", ldapPassword)
	refused("back, before the refusal ended")
	rewrite(t, store, func(*model.Snapshot) {})
	e.clock.Add(auth.ReloadInterval)
	accepted("back, after a reload")
	// a directory that does not answer gives no answer to keep
	d.fail(fmt.Errorf("demo dial: %w", auth.ErrUnreachable))
	e.clock.Add(auth.CredentialTime)
	refused("directory down")
	d.fail(nil)
	accepted("directory up again")
}

// The first directory of the stored order that knows the name answers. A
// directory before it that does not answer leaves the token without an
// answer, and a name no directory knows is refused.
func TestTokenOfADirectoryUserAcrossDirectories(t *testing.T) {
	empty, down, d := &directory{people: map[string]string{}}, &directory{people: map[string]string{}}, ldapDirectory()
	e, store := newStoreEnv(t, tokenSnapshot(t), empty, down, d)
	person := ldapToken(t, e, store, auth.KindNpm, "demo-ldap-person")
	ghost := ldapToken(t, e, store, auth.KindNpm, "demo-ldap-ghost")
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(person)); err != nil || s == nil || !slices.Equal(s.Permissions, personRights) {
		t.Fatalf("person of the third directory: %+v %v", s, err)
	}
	if s, err := subjectOf(e, bearer(ghost)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("a name no directory knows: %+v %v", s, err)
	}
	down.fail(fmt.Errorf("demo dial: %w", auth.ErrUnreachable))
	rewrite(t, store, func(*model.Snapshot) {})
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(person)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("a directory before the one that knows the name is down: %+v %v", s, err)
	}
	if n := empty.count() + down.count() + d.count(); n != 0 {
		t.Errorf("%d sign ins with a password for a token", n)
	}
}

// Review R7 of the second security review: one bound of CredentialTime for a
// removed directory user on every credential path, counted from the last
// answer of its directory. A user token as the Basic password is kept no
// longer than the lookup that let it in, and a Docker token issued from a
// remembered check asks the directory again once that check has ended; a
// user the directory still holds keeps its Docker token.
func TestRemovedDirectoryUserWithinOneBound(t *testing.T) {
	d := ldapDirectory()
	e, store := newStoreEnv(t, tokenSnapshot(t), d)
	user := ldapToken(t, e, store, auth.KindUser, "demo-ldap-person")
	e.clock.Add(auth.ReloadInterval)
	asPassword := func() *http.Request {
		return basic(request(http.MethodGet, "/", nil), "demo-ldap-person", user)
	}
	docker := func() string {
		t.Helper()
		tok, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), "demo-ldap-person", ldapPassword), "demo-docker")
		if err != nil {
			t.Fatalf("Docker token: %v", err)
		}
		return tok.Text
	}
	signsIn := func(when string, r *http.Request, want bool) {
		t.Helper()
		s, err := subjectOf(e, r)
		if got := err == nil && s != nil && slices.Equal(s.Permissions, personRights); got != want {
			t.Errorf("%s: %+v %v, signs in %v, want %v", when, s, err, got, want)
		}
	}
	// the directory answers for the person at the same moment for both paths
	signsIn("bearer use of the user token", bearer(user), true)
	signsIn("Basic with the directory password", basic(request(http.MethodGet, "/", nil), "demo-ldap-person", ldapPassword), true)
	e.clock.Add(auth.CredentialTime - time.Second)
	signsIn("the user token as the password, 1 s before the answer ends", asPassword(), true)
	late := docker()
	signsIn("a Docker token from the remembered check", bearer(late), true)
	// the person leaves the directory; the next second the answers end
	d.set("demo-ldap-person", "")
	e.clock.Add(time.Second)
	signsIn("the user token as the password once the answer ended", asPassword(), false)
	signsIn("the Docker token once the remembered check ended", bearer(late), false)

	// a person the directory still holds keeps the Docker token, through one
	// lookup after the remembered check ended
	d.set("demo-ldap-person", ldapPassword)
	rewrite(t, store, func(*model.Snapshot) {})
	e.clock.Add(auth.ReloadInterval)
	signsIn("Basic once the person is back", basic(request(http.MethodGet, "/", nil), "demo-ldap-person", ldapPassword), true)
	e.clock.Add(auth.CredentialTime - time.Second)
	kept := docker()
	lookups := d.lookupCount()
	e.clock.Add(time.Second)
	signsIn("a Docker token after the remembered check ended", bearer(kept), true)
	signsIn("the same Docker token again", bearer(kept), true)
	if n := d.lookupCount() - lookups; n != 1 {
		t.Errorf("%d lookups for two uses of a Docker token, want 1", n)
	}
}

// Review R10 of the second security review: a missing answer is not kept,
// so while a directory does not answer every use of a token of a directory
// user asks it again. The warning of the lookup comes at most once per
// connection in 60 s.
func TestLookupWarningOncePerConnectionInAMinute(t *testing.T) {
	a, b := ldapDirectory(), &directory{people: map[string]string{}}
	e, store := newStoreEnv(t, tokenSnapshot(t), a, b)
	text := ldapToken(t, e, store, auth.KindNpm, "demo-ldap-person")
	e.clock.Add(auth.ReloadInterval)
	down := fmt.Errorf("demo dial: %w", auth.ErrUnreachable)
	warned := func(connection string) int {
		n := 0
		for _, line := range strings.Split(e.logs.String(), "\n") {
			if strings.Contains(line, "did not answer the lookup") && strings.Contains(line, "connection="+connection+" ") {
				n++
			}
		}
		return n
	}
	refused := func(when string) {
		t.Helper()
		if s, err := subjectOf(e, bearer(text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Fatalf("%s: %+v %v", when, s, err)
		}
	}
	a.fail(down)
	for i := range 20 {
		refused(fmt.Sprintf("use %d while connection 1 is down", i+1))
		e.clock.Add(time.Second)
	}
	if n := warned("1"); n != 1 {
		t.Errorf("%d warnings for connection 1 in 20 s, want 1", n)
	}
	// another connection that does not answer warns at once
	a.fail(nil)
	a.set("demo-ldap-person", "")
	b.fail(down)
	refused("connection 2 is down")
	if n := warned("2"); n != 1 {
		t.Errorf("%d warnings for connection 2, want 1", n)
	}
	// connection 1 again: no line before 60 s have passed, one after
	b.fail(nil)
	a.fail(down)
	refused("connection 1 down again after 20 s")
	e.clock.Add(39 * time.Second)
	refused("connection 1 down again after 59 s")
	if n := warned("1"); n != 1 {
		t.Errorf("%d warnings for connection 1 within 60 s, want 1", n)
	}
	e.clock.Add(time.Second)
	refused("connection 1 down again after 60 s")
	if n := warned("1"); n != 2 {
		t.Errorf("%d warnings for connection 1 after 60 s, want 2", n)
	}
}

// heldLookup is a directory whose lookups wait until the test opens them and
// then answer as the fake directory does; a lookup whose context ends first
// answers that the directory did not answer, as package ldapauth does.
type heldLookup struct {
	*directory
	open    chan struct{}
	entered atomic.Int64
}

func (h *heldLookup) Lookup(ctx context.Context, name string) (model.User, []string, error) {
	h.entered.Add(1)
	select {
	case <-h.open:
		return h.directory.Lookup(ctx, name)
	case <-ctx.Done():
		return model.User{}, nil, fmt.Errorf("demo lookup: %w: %w", auth.ErrUnreachable, ctx.Err())
	}
}

// Review R4 of the second security review: the lookup of the user of a token
// answers every request that waits for it, so the request that started it
// does not end it. Its client leaves during the lookup; the waiting uses of
// the token still sign in, one lookup answers them, and no line says that
// the directory did not answer.
func TestLookupOutlivesTheRequestThatStartedIt(t *testing.T) {
	h := &heldLookup{directory: ldapDirectory(), open: make(chan struct{})}
	e, store := newStoreEnv(t, tokenSnapshot(t), h)
	text := ldapToken(t, e, store, auth.KindNpm, "demo-ldap-person")
	e.clock.Add(auth.ReloadInterval)
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	first := make(chan error, 1)
	go func() {
		_, err := subjectOf(e, bearer(text).WithContext(ctx))
		first <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for h.entered.Load() == 0 {
		if time.Now().After(deadline) {
			close(h.open)
			t.Fatal("after 10 s the lookup has not started")
		}
		time.Sleep(time.Millisecond)
	}
	const n = 10
	var wg sync.WaitGroup
	subjects, errs := make([]*model.Subject, n), make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subjects[i], errs[i] = subjectOf(e, bearer(text))
		}()
	}
	time.Sleep(50 * time.Millisecond) // the waiting uses reach the lookup in flight
	leave()
	time.Sleep(50 * time.Millisecond) // a lookup under the context of the first request ends now
	close(h.open)
	wg.Wait()
	<-first
	for i := range n {
		if errs[i] != nil || subjects[i] == nil || !slices.Equal(subjects[i].Permissions, personRights) {
			t.Errorf("waiting use %d: %+v %v", i+1, subjects[i], errs[i])
		}
	}
	if got := h.entered.Load(); got != 1 {
		t.Errorf("%d lookups, want 1", got)
	}
	if strings.Contains(e.logs.String(), "did not answer") {
		t.Errorf("a client that left is logged as a directory that did not answer: %s", e.logs)
	}
}

// Review R2 of the second security review: one name in two directories. The
// row of a token keeps the user id and the source only, so while more than
// one directory of the stored order holds the name the token is refused,
// with one log line that names the user and the connections and holds no
// secret. A directory that does not answer may hold the name as well, so it
// refuses the token wherever it stands in the order.
func TestTokenOfANameInTwoDirectories(t *testing.T) {
	a := ldapDirectory()
	b := &directory{people: map[string]string{"demo-ldap-person": "demo-ldap-pass-2"}, groups: []string{"demo-ops"}}
	e, store := newStoreEnv(t, tokenSnapshot(t), a, b)
	text := ldapToken(t, e, store, auth.KindNpm, "demo-ldap-person")
	e.clock.Add(auth.ReloadInterval)
	for i := range 3 {
		if s, err := subjectOf(e, bearer(text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Fatalf("use %d of a token whose name two directories hold: %+v %v", i+1, s, err)
		}
	}
	var lines []string
	for _, line := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(line, "more than one directory") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "user=demo-ldap-person") || !strings.Contains(lines[0], "connections=1,2") {
		t.Errorf("log lines %q, want one that names the user and the connections 1,2", lines)
	}
	_, secret := halves(t, text)
	for _, s := range []string{secret, ldapPassword, "demo-ldap-pass-2"} {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("the log holds the secret %q", s)
		}
	}
	// one directory holds the name: the token signs in after a reload
	b.set("demo-ldap-person", "")
	rewrite(t, store, func(*model.Snapshot) {})
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(text)); err != nil || s == nil || !slices.Equal(s.Permissions, personRights) {
		t.Fatalf("one directory holds the name: %+v %v", s, err)
	}
	// a directory after the one that holds the name does not answer
	b.fail(fmt.Errorf("demo dial: %w", auth.ErrUnreachable))
	rewrite(t, store, func(*model.Snapshot) {})
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("a directory after the one that holds the name does not answer: %+v %v", s, err)
	}
}
