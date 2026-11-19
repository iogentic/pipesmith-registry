package auth_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Task 6 of the phase 3 plan: the stored realm order decides which realm
// answers a user name and password; a failure in one realm lets the next one
// answer; a subject of a realm that left the order is refused at its next
// request, with a live session and with a live token (lab research
// 05-identity.md, section Realms: the first realm in the stored order that
// gives a result signs the user in, and a principal of a realm that is not
// active is refused).

const localPassOfTwin = "demo-twin-local-pass-1"

// twinSnapshot holds a local user with the id of the directory person and
// another password, so that the realm that answers shows in the subject.
func twinSnapshot(t testing.TB, realms ...string) *model.Snapshot {
	t.Helper()
	s := tokenSnapshot(t)
	hash, err := auth.HashPassword(localPassOfTwin)
	if err != nil {
		t.Fatal(err)
	}
	s.Users = append(s.Users, model.User{ID: "demo-ldap-person", FirstName: "Demo", LastName: "Twin", Email: "twin@example.com",
		Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-maven-reader"}, PasswordHash: hash})
	s.Realms = realms
	return s
}

func TestStoredOrderDecidesWhichRealmAnswers(t *testing.T) {
	// the directory accepts both passwords of the twin
	both := func() *directory {
		return &directory{people: map[string]string{"demo-ldap-person": ldapPassword}, groups: []string{"demo-ldap-group"}}
	}
	for _, c := range []struct {
		name   string
		realms []string
		pass   string
		realm  string
		calls  int
	}{
		{"local first, local password", []string{auth.RealmLocal, auth.RealmLDAP}, localPassOfTwin, auth.RealmLocal, 0},
		{"local first, directory password", []string{auth.RealmLocal, auth.RealmLDAP}, ldapPassword, auth.RealmLDAP, 1},
		{"directory first, directory password", []string{auth.RealmLDAP, auth.RealmLocal}, ldapPassword, auth.RealmLDAP, 1},
		{"directory first, local password", []string{auth.RealmLDAP, auth.RealmLocal}, localPassOfTwin, auth.RealmLocal, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := both()
			e := newEnv(t, twinSnapshot(t, c.realms...), d)
			_, a := e.mustSignIn("demo-ldap-person", c.pass)
			if a.User == nil || a.User.Realm != c.realm || d.count() != c.calls {
				t.Fatalf("session: %+v, %d directory calls, want realm %s and %d calls", a.User, d.count(), c.realm, c.calls)
			}
			// Basic follows the same order
			d = both()
			e = newEnv(t, twinSnapshot(t, c.realms...), d)
			s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-ldap-person", c.pass))
			if err != nil || s == nil || s.Realm != c.realm || d.count() != c.calls {
				t.Fatalf("Basic: %+v %v, %d directory calls", s, err, d.count())
			}
		})
	}
}

// A user token is a password of the realm of user tokens, at its place in
// the stored order: the user name must be the one of the token.
func TestUserTokenAsBasicPassword(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	text, row, err := auth.NewToken(auth.KindUser, "demo-admin", auth.SourceLocal, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddToken(row, auth.MaxTokensPerKind); err != nil {
		t.Fatal(err)
	}
	s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-admin", text))
	if err != nil || s == nil || s.User.ID != "demo-admin" || !e.svc.Permits(s, "nexus:*") {
		t.Fatalf("Basic with a user token: %+v %v", s, err)
	}
	for _, other := range []string{"demo-reader", "Demo-Admin", ""} {
		if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), other, text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Errorf("the token of demo-admin as the password of %q: %+v %v", other, s, err)
		}
	}
	// the interface sign in takes it as well, and a token of npm does not count
	_, a := e.mustSignIn("demo-admin", text)
	if a.User == nil || a.User.UserID != "demo-admin" {
		t.Fatalf("sign in with a user token: %+v", a.User)
	}
	npm := npmLogin(t, e, "demo-reader", password(t, "demo-reader"))
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", npm.Text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("an npm token as a password: %+v %v", s, err)
	}
	// without the realm of user tokens in the order it is no password
	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmUserToken })
	})
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-admin", text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("user token without its realm: %+v %v", s, err)
	}
	if rec := e.signIn("demo-admin", text, "192.0.2.80"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign in with a user token without its realm: %d", rec.Code)
	}
}

// A realm that leaves the order ends the sessions and the tokens of its
// users at their next request after the reload.
func TestRealmThatLeftTheOrder(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t), ldapDirectory())
	adminCookie, _ := e.mustSignIn("demo-admin", password(t, "demo-admin"))
	adminNpm := npmLogin(t, e, "demo-admin", password(t, "demo-admin"))
	adminDocker, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), "demo-admin", password(t, "demo-admin")), "demo-docker")
	if err != nil {
		t.Fatal(err)
	}
	personCookie, _ := e.mustSignIn("demo-ldap-person", ldapPassword)
	personNpm := npmLogin(t, e, "demo-ldap-person", ldapPassword)
	personDocker, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), "demo-ldap-person", ldapPassword), "demo-docker")
	if err != nil {
		t.Fatal(err)
	}
	live := func(name string, r *http.Request, want bool) {
		t.Helper()
		s, err := subjectOf(e, r)
		if want && (err != nil || s == nil) {
			t.Errorf("%s: refused: %v", name, err)
		}
		if !want && (s != nil || !errors.Is(err, model.ErrUnauthorized)) {
			t.Errorf("%s: %+v %v, want a refusal", name, s, err)
		}
	}
	// the token of a directory user holds the roles of its groups and its
	// role mappings, as with Basic (review B1)
	if s, err := subjectOf(e, bearer(personNpm.Text)); err != nil || s == nil || s.User.Source != auth.SourceLDAP ||
		!slices.Equal(s.Permissions, []string{"nexus:repository-view:raw:demo-raw:read", "nexus:settings:read"}) {
		t.Fatalf("npm token of the directory user: %+v %v", s, err)
	}
	if s, err := subjectOf(e, bearer(personDocker.Text)); err != nil || s == nil ||
		!slices.Equal(s.Permissions, []string{"nexus:repository-view:raw:demo-raw:read", "nexus:settings:read"}) {
		t.Fatalf("Docker token of the directory user: %+v %v", s, err)
	}

	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmLocal })
	})
	e.clock.Add(auth.ReloadInterval)
	live("local session", request(http.MethodGet, "/", adminCookie), false)
	live("npm token of a local user", bearer(adminNpm.Text), false)
	live("Docker token of a local user", bearer(adminDocker.Text), false)
	live("directory session", request(http.MethodGet, "/", personCookie), true)
	live("npm token of a directory user", bearer(personNpm.Text), true)
	live("Docker token of a directory user", bearer(personDocker.Text), true)

	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmLDAP })
	})
	e.clock.Add(auth.ReloadInterval)
	live("directory session", request(http.MethodGet, "/", personCookie), false)
	live("npm token of a directory user", bearer(personNpm.Text), false)
	live("Docker token of a directory user", bearer(personDocker.Text), false)
}

// The default role reaches every signed in subject and not the anonymous one
// while the realm DefaultRole is in the stored order (task 6 of the phase 3
// plan; lab research 05-identity.md, section Roles: the DefaultRole realm
// adds one configured role to every authenticated, non anonymous user). It
// holds for a sign in of the interface, Basic credentials of a local and of
// a directory user, user, npm and Docker tokens. The user keeps its own roles.
func TestDefaultRole(t *testing.T) {
	const settings = "nexus:settings:read" // the right of demo-ops
	other := &directory{people: map[string]string{"demo-ldap-other": ldapPassword}}
	snap := tokenSnapshot(t)
	snap.Anonymous.Enabled = true
	snap.Realms = append(snap.Realms, auth.RealmDefaultRole)
	snap.DefaultRole = "demo-ops"
	e, store := newStoreEnv(t, snap, other)
	nobody := password(t, "demo-nobody")
	subject := func(r *http.Request) *model.Subject {
		t.Helper()
		s, err := subjectOf(e, r)
		if err != nil || s == nil {
			t.Fatalf("subject: %v %v", s, err)
		}
		return s
	}
	basicNobody := subject(basic(request(http.MethodGet, "/", nil), "demo-nobody", nobody))
	if !e.svc.Permits(basicNobody, settings) || len(basicNobody.User.Roles) != 0 {
		t.Fatalf("Basic of a local user without roles: %v, roles %v", basicNobody.Permissions, basicNobody.User.Roles)
	}
	if _, a := e.mustSignIn("demo-nobody", nobody); !slices.Contains(a.Permissions, settings) {
		t.Fatalf("session: %v", a.Permissions)
	}
	if s := subject(basic(request(http.MethodGet, "/", nil), "demo-ldap-other", ldapPassword)); !e.svc.Permits(s, settings) ||
		s.Realm != auth.RealmLDAP {
		t.Fatalf("Basic of a directory user: %+v", s)
	}
	userToken, row, err := auth.NewToken(auth.KindUser, "demo-nobody", auth.SourceLocal, e.clock.Now())
	if err == nil {
		_, err = store.AddToken(row, auth.MaxTokensPerKind)
	}
	if err != nil {
		t.Fatal(err)
	}
	if s := subject(bearer(userToken)); !e.svc.Permits(s, settings) {
		t.Fatalf("user token: %v", s.Permissions)
	}
	if s := subject(bearer(npmLogin(t, e, "demo-nobody", nobody).Text)); !e.svc.Permits(s, settings) {
		t.Fatalf("npm token: %v", s.Permissions)
	}
	docker, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), "demo-nobody", nobody), "demo-maven")
	if err != nil {
		t.Fatal(err)
	}
	if s := subject(bearer(docker.Text)); !e.svc.Permits(s, settings) {
		t.Fatalf("Docker token: %v", s.Permissions)
	}
	if e.svc.Permits(nil, settings) {
		t.Fatal("the anonymous subject holds the default role")
	}
	// also an anonymous user without a local row, whose roles come from a
	// role mapping of the local source
	guest := tokenSnapshot(t)
	guest.Anonymous = model.Anonymous{Enabled: true, UserID: "demo-guest", RealmName: "NexusAuthorizingRealm"}
	guest.RoleMappings = append(guest.RoleMappings, model.RoleMapping{UserID: "demo-guest", Source: auth.SourceLocal,
		Roles: []string{"nx-anonymous"}})
	guest.Realms = append(guest.Realms, auth.RealmDefaultRole)
	guest.DefaultRole = "demo-ops"
	g := newEnv(t, guest)
	if !g.svc.Permits(nil, "nexus:repository-view:maven2:demo-maven:read") || g.svc.Permits(nil, settings) {
		t.Fatal("the anonymous user of a role mapping holds the default role, or not its own roles")
	}

	// without the realm in the order the role is no one's
	snap.Realms = slices.DeleteFunc(slices.Clone(snap.Realms), func(r string) bool { return r == auth.RealmDefaultRole })
	off := newEnv(t, snap, other)
	if s, err := subjectOf(off, basic(request(http.MethodGet, "/", nil), "demo-nobody", nobody)); err != nil || s == nil ||
		off.svc.Permits(s, settings) {
		t.Fatalf("realm off: %+v %v", s, err)
	}
	// the realm without a role, or with a role that does not exist, grants
	// nothing and says so
	for role, warning := range map[string]string{"": "names no role", "no-such-role": "no-such-role does not exist"} {
		snap.Realms = append(slices.Clone(snap.Realms), auth.RealmDefaultRole)
		snap.DefaultRole = role
		w := newEnv(t, snap, other)
		if s, err := subjectOf(w, basic(request(http.MethodGet, "/", nil), "demo-nobody", nobody)); err != nil || s == nil ||
			len(s.Permissions) != 0 {
			t.Fatalf("default role %q: %+v %v", role, s, err)
		}
		if !strings.Contains(w.logs.String(), warning) {
			t.Fatalf("default role %q: log %q", role, w.logs.String())
		}
	}
}

// The realms of the token kinds are built now; any other id of the stored
// order is named once at each load (decision N16).
func TestRealmsThisServerBuilds(t *testing.T) {
	snap := tokenSnapshot(t)
	snap.Realms = append(snap.Realms, "demo-other-realm", auth.RealmDocker)
	e := newEnv(t, snap)
	logs := e.logs.String()
	if strings.Count(logs, "realms this server does not build") != 1 || !strings.Contains(logs, "realms=demo-other-realm\n") {
		t.Fatalf("log %q", logs)
	}
	for _, built := range []string{auth.RealmNpm, auth.RealmDocker, auth.RealmUserToken} {
		if strings.Contains(logs, built) {
			t.Errorf("the log names the realm %s, which this server builds", built)
		}
	}
}
