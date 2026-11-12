package auth_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestBasic(t *testing.T) {
	e := newEnv(t, fixture(t))
	s, err := e.svc.Subject(basic(request(http.MethodGet, "/repository/demo-maven/", nil), "demo-reader", password(t, "demo-reader")))
	if err != nil || s == nil {
		t.Fatalf("Basic of demo-reader: %v", err)
	}
	if s.User.ID != "demo-reader" || s.Realm != auth.RealmLocal || s.Session != "" || s.User.PasswordHash != "" ||
		!slices.Equal(s.Permissions, []string{"nexus:repository-view:maven2:demo-maven:browse",
			"nexus:repository-view:maven2:demo-maven:read"}) {
		t.Errorf("subject %+v, permissions %q", s, s.Permissions)
	}
	refused := []*http.Request{
		basic(request(http.MethodGet, "/", nil), "demo-reader", "demo-wrong-pass-1"),
		basic(request(http.MethodGet, "/", nil), "demo-no-such-user", "demo-wrong-pass-1"),
		basic(request(http.MethodGet, "/", nil), "demo-off", password(t, "demo-off")),
		basic(request(http.MethodGet, "/", nil), noHashUser, ""),
	}
	for _, header := range []string{"Basic !!!", "Basic " + "ZGVtby1yZWFkZXI=", "Bearer NpmToken.0123", "Digest x", "Basic"} {
		r := request(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", header)
		refused = append(refused, r)
	}
	for _, r := range refused {
		if s, err := e.svc.Subject(r); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Errorf("%q: %+v, %v", r.Header.Get("Authorization"), s, err)
		}
	}
	if s, err := e.svc.Subject(request(http.MethodGet, "/", nil)); s != nil || err != nil {
		t.Errorf("no credentials: %+v, %v", s, err)
	}
}

// T7: Basic credentials are checked against local users only.
func TestBasicLocalOnly(t *testing.T) {
	d := ldapDirectory()
	e := newEnv(t, fixture(t), d)
	r := basic(request(http.MethodGet, "/", nil), "demo-ldap-person", ldapPassword)
	if s, err := e.svc.Subject(r); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("Basic of a directory user: %+v, %v", s, err)
	}
	if d.count() != 0 {
		t.Errorf("Basic asked the directory %d times", d.count())
	}
}

func TestStaleCookie(t *testing.T) {
	e := newEnv(t, fixture(t))
	stale := &http.Cookie{Name: auth.CookieName, Value: strings.Repeat("A", 43)}
	if s, err := e.svc.Subject(request(http.MethodGet, "/", stale)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("stale cookie: %+v, %v", s, err)
	}
	r := basic(request(http.MethodGet, "/", stale), "demo-admin", password(t, "demo-admin"))
	if s, err := e.svc.Subject(r); err != nil || s == nil || s.User.ID != "demo-admin" {
		t.Errorf("stale cookie and Basic: %+v, %v", s, err)
	}
	other := request(http.MethodGet, "/", nil)
	other.AddCookie(&http.Cookie{Name: "demo-other-cookie", Value: "1"})
	if s, err := e.svc.Subject(other); s != nil || err != nil {
		t.Errorf("another cookie: %+v, %v", s, err)
	}
}

func TestDirectorySignIn(t *testing.T) {
	d := ldapDirectory()
	e := newEnv(t, fixture(t), d)
	cookie, a := e.mustSignIn("demo-ldap-person", ldapPassword)
	if a.User == nil || a.User.Source != "LDAP" || a.User.Realm != auth.RealmLDAP || a.User.UserID != "demo-ldap-person" {
		t.Errorf("directory user: %+v", a.User)
	}
	want := []string{"nexus:repository-view:raw:demo-raw:read", "nexus:settings:read"}
	if !slices.Equal(a.Permissions, want) {
		t.Errorf("directory permissions %q, want %q", a.Permissions, want)
	}
	s, err := e.svc.Subject(request(http.MethodGet, "/", cookie))
	if err != nil || s.User.Source != "LDAP" || !slices.Equal(s.User.Roles, []string{"demo-ldap-group", "demo-ops"}) {
		t.Errorf("subject of the directory session: %+v, %v", s, err)
	}
	if rec := e.signIn("demo-ldap-person", "demo-wrong-pass-1", "192.0.2.30"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong directory password: %d", rec.Code)
	}
	// a local user with the right password does not reach the directory
	calls := d.count()
	e.mustSignIn("demo-admin", password(t, "demo-admin"))
	if d.count() != calls {
		t.Error("a local sign in asked the directory")
	}
	// a local user with a wrong password does
	if rec := e.signIn("demo-admin", "demo-wrong-pass-1", "192.0.2.31"); rec.Code != http.StatusUnauthorized ||
		d.count() != calls+1 {
		t.Errorf("wrong local password: %d, %d directory calls", rec.Code, d.count()-calls)
	}
}

func TestDirectoryOrder(t *testing.T) {
	down := &directory{err: fmt.Errorf("demo dial: %w", auth.ErrUnreachable)}
	d := ldapDirectory()
	e := newEnv(t, fixture(t), down, d)
	_, a := e.mustSignIn("demo-ldap-person", ldapPassword)
	if a.User == nil || a.User.Source != "LDAP" || down.count() != 1 || d.count() != 1 {
		t.Errorf("second directory after an unreachable first: %+v, calls %d and %d", a.User, down.count(), d.count())
	}
	if !strings.Contains(e.logs.String(), "directory did not answer") {
		t.Errorf("no warning for the unreachable directory: %q", e.logs)
	}
	refusing := &directory{people: map[string]string{}}
	e = newEnv(t, fixture(t), refusing, d)
	e.mustSignIn("demo-ldap-person", ldapPassword)
	if refusing.count() != 1 || d.count() != 2 {
		t.Errorf("calls after a refusing first directory: %d and %d", refusing.count(), d.count())
	}
	e = newEnv(t, fixture(t), down)
	if rec := e.signIn("demo-ldap-person", ldapPassword, "192.0.2.32"); rec.Code != http.StatusUnauthorized ||
		rec.Body.String() != `{"message":"wrong user name or password"}`+"\n" {
		t.Errorf("only an unreachable directory: %d %q", rec.Code, rec.Body)
	}
}

// A directory answer without a user id signs nobody in; the next directory
// still answers.
func TestDirectoryWithoutID(t *testing.T) {
	blank := &directory{noID: true, groups: []string{"demo-ldap-group"}}
	d := ldapDirectory()
	e := newEnv(t, fixture(t), blank, d)
	if rec := e.signIn("demo-unknown-person", "demo-any-pass-1", "192.0.2.34"); rec.Code != http.StatusUnauthorized {
		t.Errorf("answer without a user id: %d %q", rec.Code, rec.Body)
	}
	_, a := e.mustSignIn("demo-ldap-person", ldapPassword)
	if a.User == nil || a.User.UserID != "demo-ldap-person" || blank.count() != 2 {
		t.Errorf("after an answer without an id: %+v, %d calls", a.User, blank.count())
	}
}

// The subject of a session is a copy: changing it changes no later subject.
func TestSubjectIsACopy(t *testing.T) {
	e := newEnv(t, fixture(t))
	cookie, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	s, err := e.svc.Subject(request(http.MethodGet, "/", cookie))
	if err != nil || len(s.Permissions) == 0 || len(s.User.Roles) == 0 {
		t.Fatalf("subject: %+v %v", s, err)
	}
	s.Permissions[0] = "nexus:*"
	s.User.Roles[0] = "nx-admin"
	again, _ := e.svc.Subject(request(http.MethodGet, "/", cookie))
	if again.Permissions[0] == "nexus:*" || again.User.Roles[0] == "nx-admin" || e.svc.Permits(again, "nexus:settings:read") {
		t.Errorf("a change of one subject reached the session: %q %q", again.Permissions, again.User.Roles)
	}
}

// A realm that is not in the stored order signs nobody in.
func TestRealmOrder(t *testing.T) {
	snap := fixture(t)
	snap.Realms = []string{auth.RealmLocal, "NexusAuthorizingRealm"}
	d := ldapDirectory()
	e := newEnv(t, snap, d)
	if rec := e.signIn("demo-ldap-person", ldapPassword, "192.0.2.33"); rec.Code != http.StatusUnauthorized || d.count() != 0 {
		t.Errorf("LDAP realm off: %d, %d calls", rec.Code, d.count())
	}
	snap = fixture(t)
	snap.Realms = []string{auth.RealmLDAP}
	e = newEnv(t, snap, d)
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.33"); rec.Code != http.StatusUnauthorized {
		t.Errorf("local realm off, local sign in: %d", rec.Code)
	}
	if s, err := e.svc.Subject(basic(request(http.MethodGet, "/", nil), "demo-admin", password(t, "demo-admin"))); s != nil ||
		!errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("local realm off, Basic: %+v %v", s, err)
	}
	if !strings.Contains(e.logs.String(), "NexusAuthenticatingRealm") {
		t.Errorf("no warning for the missing local realm: %q", e.logs)
	}
}

func TestPermits(t *testing.T) {
	e := newEnv(t, fixture(t))
	read := "nexus:repository-view:maven2:demo-maven:read"
	if e.svc.AnonymousEnabled() || e.svc.Permits(nil, read) {
		t.Error("anonymous off permits a read")
	}
	reader, _ := e.svc.Subject(basic(request(http.MethodGet, "/", nil), "demo-reader", password(t, "demo-reader")))
	admin, _ := e.svc.Subject(basic(request(http.MethodGet, "/", nil), "demo-admin", password(t, "demo-admin")))
	cases := []struct {
		s    *model.Subject
		perm string
		want bool
	}{
		{reader, read, true},
		{reader, "nexus:repository-view:maven2:demo-maven:browse", true},
		{reader, "nexus:repository-view:raw:demo-raw:read", false},
		{reader, "nexus:repository-view:maven2:demo-maven:add", false},
		{reader, "nexus:settings:read", false},
		{admin, "nexus:settings:update", true},
		{admin, "nexus:repository-admin:npm:demo-npm:edit", true},
	}
	for _, c := range cases {
		if got := e.svc.Permits(c.s, c.perm); got != c.want {
			t.Errorf("%s permits %s: %v", c.s.User.ID, c.perm, got)
		}
	}
	if !e.svc.PermitsSome(reader, "nexus:repository-view:*:*:browse") ||
		e.svc.PermitsSome(reader, "nexus:repository-admin:*:*:read") || e.svc.PermitsSome(nil, "nexus:*") {
		t.Error("PermitsSome")
	}
	snap := fixture(t)
	snap.Anonymous.Enabled = true
	e = newEnv(t, snap)
	if !e.svc.AnonymousEnabled() || !e.svc.Permits(nil, read) ||
		!e.svc.Permits(nil, "nexus:repository-view:raw:demo-raw:browse") ||
		e.svc.Permits(nil, "nexus:repository-view:raw:demo-raw:add") || e.svc.Permits(nil, "nexus:settings:read") {
		t.Error("anonymous on: the rights of nx-anonymous")
	}
	if !e.svc.PermitsSome(nil, "nexus:repository-view:*:*:browse") {
		t.Error("anonymous on: PermitsSome")
	}
	for i := range snap.Users {
		if snap.Users[i].ID == "anonymous" {
			snap.Users[i].Status = "disabled"
		}
	}
	e = newEnv(t, snap)
	if e.svc.Permits(nil, read) {
		t.Error("a disabled anonymous user holds rights")
	}
}

func TestThrottle(t *testing.T) {
	e := newEnv(t, fixture(t))
	const addr = "192.0.2.40"
	for i := range auth.FreeFailures {
		if rec := e.signIn("demo-admin", "demo-wrong-pass-1", addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, rec.Code)
		}
	}
	before := auth.HashRuns()
	rec := e.signIn("demo-admin", password(t, "demo-admin"), addr)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" || rec.Body.Len() != 0 {
		t.Fatalf("after 5 failures: %d, Retry-After %q, body %q", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if auth.HashRuns() != before {
		t.Error("a throttled sign in ran a hash")
	}
	if rec := e.signIn("DEMO-Admin", "demo-wrong-pass-1", addr); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the user name in other letter case: %d", rec.Code)
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.41"); rec.Code != http.StatusOK {
		t.Errorf("another address: %d", rec.Code)
	}
	if rec := e.signIn("demo-reader", password(t, "demo-reader"), addr); rec.Code != http.StatusOK {
		t.Errorf("another user from the same address: %d", rec.Code)
	}
	r := basic(request(http.MethodGet, "/", nil), "demo-admin", password(t, "demo-admin"))
	r.RemoteAddr = addr + ":50000"
	_, err := e.svc.Subject(r)
	var me *model.Error
	if !errors.As(err, &me) || me.Status != http.StatusTooManyRequests || me.RetryAfter != 2 {
		t.Errorf("Basic while throttled: %v", err)
	}
	// the delay doubles with each further failure, up to 15 minutes
	want := []int{4, 8, 16, 32, 64, 128, 256, 512, 900, 900}
	for _, w := range want {
		wait, _ := strconv.Atoi(e.signIn("demo-admin", "x", addr).Header().Get("Retry-After"))
		e.clock.Add(time.Duration(wait) * time.Second)
		if rec := e.signIn("demo-admin", "demo-wrong-pass-1", addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure after the wait: %d", rec.Code)
		}
		if got := e.signIn("demo-admin", "demo-wrong-pass-1", addr).Header().Get("Retry-After"); got != strconv.Itoa(w) {
			t.Errorf("Retry-After %s, want %d", got, w)
		}
	}
	// a success forgets the pair
	e.clock.Add(MaxWait())
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), addr); rec.Code != http.StatusOK {
		t.Fatalf("after the last wait: %d", rec.Code)
	}
	for i := range auth.FreeFailures {
		if rec := e.signIn("demo-admin", "demo-wrong-pass-1", addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d after a success: %d", i+1, rec.Code)
		}
	}
}

// MaxWait is the longest wait of the schedule.
func MaxWait() time.Duration { return auth.MaxDelay }

func TestThrottleForgets(t *testing.T) {
	e := newEnv(t, fixture(t))
	const addr = "192.0.2.42"
	for range auth.FreeFailures - 1 {
		e.signIn("demo-admin", "demo-wrong-pass-1", addr)
	}
	e.clock.Add(auth.ForgetAfter + time.Second)
	for i := range auth.FreeFailures {
		if rec := e.signIn("demo-admin", "demo-wrong-pass-1", addr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d after the pair was forgotten: %d", i+1, rec.Code)
		}
	}
	if rec := e.signIn("demo-admin", "demo-wrong-pass-1", addr); rec.Code != http.StatusTooManyRequests {
		t.Errorf("sixth failure: %d", rec.Code)
	}
	if auth.PairCount(e.svc) != 1 {
		t.Errorf("%d pairs", auth.PairCount(e.svc))
	}
	auth.FillPairs(e.svc, auth.MaxPairs+500)
	if n := auth.PairCount(e.svc); n != auth.MaxPairs {
		t.Errorf("%d pairs after filling past the bound", n)
	}
}

// No password and no hash in any answer, header, error, log line or printed
// subject.
func TestNoSecretInOutput(t *testing.T) {
	snap := fixture(t)
	d := ldapDirectory()
	e := newEnv(t, snap, &directory{err: fmt.Errorf("demo dial: %w", auth.ErrUnreachable)}, d)
	var out strings.Builder
	record := func(rec interface {
		Header() http.Header
	}, body string) {
		fmt.Fprintf(&out, "%v\n%s\n", rec.Header(), body)
	}
	for _, u := range fixtureUsers {
		pass := password(t, u.id)
		for _, p := range []string{pass, pass + "-wrong"} {
			rec := e.signIn(u.id, p, "192.0.2.50")
			record(rec, rec.Body.String())
			s, err := e.svc.Subject(basic(request(http.MethodGet, "/", nil), u.id, p))
			fmt.Fprintf(&out, "%v %+v %#v %v\n", s, s, s, err)
			if s != nil {
				data, _ := json.Marshal(s)
				out.Write(data)
				fmt.Fprintf(&out, "%v %+v %#v\n", *s, *s, *s)
			}
		}
	}
	rec := e.signIn("demo-ldap-person", ldapPassword, "192.0.2.51")
	record(rec, rec.Body.String())
	cookie := sessionCookie(t, rec)
	rec = e.do(request(http.MethodGet, "/api/session", cookie))
	record(rec, rec.Body.String())
	out.WriteString(e.logs.String())
	text := out.String()
	secrets := []string{ldapPassword}
	for _, u := range snap.Users {
		if u.PasswordHash != "" {
			secrets = append(secrets, u.PasswordHash)
			if cut := strings.LastIndex(u.PasswordHash, "$"); cut >= 0 {
				secrets = append(secrets, u.PasswordHash[cut+1:])
			}
		}
	}
	for _, u := range fixtureUsers {
		secrets = append(secrets, password(t, u.id))
	}
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("output holds %q", secret)
		}
	}
	if !strings.Contains(text, "demo-admin") || len(text) < 5000 {
		t.Fatalf("the output was not captured: %d bytes", len(text))
	}
}

func TestConcurrent(t *testing.T) {
	e := newEnv(t, fixture(t), ldapDirectory())
	users := []string{"demo-admin", "demo-reader", "demo-legacy-sha1", "demo-legacy-md5", "demo-nobody"}
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := users[i%len(users)]
			addr := fmt.Sprintf("192.0.2.%d", 100+i)
			rec := e.signIn(user, password(t, user), addr)
			if rec.Code != http.StatusOK {
				t.Errorf("%s: %d", user, rec.Code)
				return
			}
			cookie := sessionCookie(t, rec)
			var a sessionAnswer
			_ = json.Unmarshal(rec.Body.Bytes(), &a)
			s, err := e.svc.Subject(request(http.MethodGet, "/", cookie))
			if err != nil || s.User.ID != user {
				t.Errorf("%s: subject %v", user, err)
				return
			}
			e.svc.Permits(s, "nexus:settings:read")
			e.signIn(user, "demo-wrong-pass-1", addr)
			e.do(request(http.MethodGet, "/api/session", cookie))
			r := request(http.MethodDelete, "/api/session", cookie)
			r.Header.Set(auth.CSRFHeader, *a.CSRFToken)
			if rec := e.do(r); rec.Code != http.StatusNoContent {
				t.Errorf("%s: sign out %d", user, rec.Code)
			}
		}()
	}
	wg.Wait()
	if n := auth.SessionCount(e.svc); n != 0 {
		t.Errorf("%d sessions left", n)
	}
}
