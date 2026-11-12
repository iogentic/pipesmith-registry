package auth_test

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/auth"
	"github.com/mirosso25/pipesmith-registry/internal/model"
)

func TestSignInEachForm(t *testing.T) {
	e := newEnv(t, fixture(t))
	for _, u := range fixtureUsers {
		if !u.signsIn {
			continue
		}
		rec := e.signIn(u.id, password(t, u.id), "192.0.2.10")
		if rec.Code != http.StatusOK {
			t.Errorf("%s (%s): %d %s", u.id, u.vector, rec.Code, rec.Body)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type %q", u.id, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q", u.id, cc)
		}
		a := decodeSession(t, rec)
		if a.User == nil || a.User.UserID != u.id || a.User.Source != "default" || a.User.Realm != auth.RealmLocal ||
			a.User.EmailAddress != u.id+"@example.com" || a.User.FirstName != "Demo" {
			t.Errorf("%s: user %+v", u.id, a.User)
		}
		if a.CSRFToken == nil || len(*a.CSRFToken) != 43 || a.ExpiresInSeconds != 1800 {
			t.Errorf("%s: token %v, expires %d", u.id, a.CSRFToken, a.ExpiresInSeconds)
		}
		if a.Permissions == nil {
			t.Errorf("%s: permissions null", u.id)
		}
	}
}

func TestSessionPermissions(t *testing.T) {
	e := newEnv(t, fixture(t))
	cases := map[string][]string{
		"demo-admin": {"nexus:*"},
		"demo-reader": {"nexus:repository-view:maven2:demo-maven:browse",
			"nexus:repository-view:maven2:demo-maven:read"},
		"demo-nobody": {},
	}
	for user, want := range cases {
		_, a := e.mustSignIn(user, password(t, user))
		if !slices.Equal(a.Permissions, want) {
			t.Errorf("%s: %q, want %q", user, a.Permissions, want)
		}
	}
}

func TestCookieFlags(t *testing.T) {
	e := newEnv(t, fixture(t))
	plain := sessionCookie(t, e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.10"))
	if !plain.HttpOnly || plain.SameSite != http.SameSiteStrictMode || plain.Path != "/" || plain.Secure {
		t.Errorf("cookie over plain HTTP: %+v", plain)
	}
	if len(plain.Value) != 43 || plain.MaxAge != 0 || !plain.Expires.IsZero() {
		t.Errorf("cookie value or lifetime: %+v", plain)
	}
	r := signInRequest("demo-admin", password(t, "demo-admin"), "192.0.2.10")
	r.TLS = &tls.ConnectionState{}
	secure := sessionCookie(t, e.do(r))
	if !secure.Secure || !secure.HttpOnly || secure.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie over TLS: %+v", secure)
	}
}

// A wrong password, an unknown user and every user that may not sign in get
// the same answer, and each runs one hash.
func TestRefusedAlike(t *testing.T) {
	e := newEnv(t, fixture(t))
	cases := []struct{ user, pass string }{
		{"demo-admin", "demo-wrong-pass-1"},
		{"demo-no-such-user", "demo-admin-pass-1"},
		{"demo-off", password(t, "demo-off")},
		{"demo-locked", password(t, "demo-locked")},
		{"demo-change", password(t, "demo-change")},
		{noHashUser, "demo-any-pass-1"},
		{"Demo-Admin", password(t, "demo-admin")},
	}
	var first []byte
	for i, c := range cases {
		before := auth.HashRuns()
		rec := e.signIn(c.user, c.pass, "192.0.2.20")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", c.user, rec.Code)
		}
		if runs := auth.HashRuns() - before; runs != 1 {
			t.Errorf("%s: %d digests, want 1", c.user, runs)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: cookie set", c.user)
		}
		if i == 0 {
			first = rec.Body.Bytes()
			if !bytes.Equal(first, []byte(`{"message":"wrong user name or password"}`+"\n")) {
				t.Errorf("body %q", first)
			}
		} else if !bytes.Equal(rec.Body.Bytes(), first) {
			t.Errorf("%s: body %q differs from %q", c.user, rec.Body, first)
		}
	}
	for _, c := range []struct{ user, pass string }{{"", "demo-admin-pass-1"}, {"demo-admin", ""}} {
		rec := e.signIn(c.user, c.pass, "192.0.2.21")
		if rec.Code != http.StatusUnauthorized || !bytes.Equal(rec.Body.Bytes(), first) {
			t.Errorf("empty field %q/%q: %d %q", c.user, c.pass, rec.Code, rec.Body)
		}
	}
}

// refusalFloor is the least time of a refused local check.
const refusalFloor = 5 * time.Millisecond

// Finding 6 of review 6: a refused local check lasts at least the floor for
// every hash form and for an unknown name, so the time tells neither the form
// nor a known name. A successful check is not slowed down.
func TestRefusalFloor(t *testing.T) {
	if auth.RefusalFloor != refusalFloor {
		t.Fatalf("RefusalFloor %v, want %v", auth.RefusalFloor, refusalFloor)
	}
	e := newEnv(t, fixture(t))
	users := []string{"demo-no-such-user", "demo-admin", "demo-reader", "demo-legacy-sha1", "demo-legacy-md5",
		"demo-pbkdf2", "demo-pbkdf2-sha1", "demo-off", noHashUser}
	for i, user := range users {
		addr := fmt.Sprintf("192.0.2.%d", 70+i)
		start := time.Now()
		rec := e.signIn(user, "demo-wrong-pass-1", addr)
		if took := time.Since(start); rec.Code != http.StatusUnauthorized || took < refusalFloor {
			t.Errorf("sign in of %s: %d after %v, want 401 after at least %v", user, rec.Code, took, refusalFloor)
		}
		r := basic(request(http.MethodGet, "/", nil), user, "demo-wrong-pass-1")
		r.RemoteAddr = addr + ":50000"
		start = time.Now()
		_, err := e.svc.Subject(r)
		if took := time.Since(start); !errors.Is(err, model.ErrUnauthorized) || took < refusalFloor {
			t.Errorf("Basic of %s: %v after %v, want 401 after at least %v", user, err, took, refusalFloor)
		}
	}
	fastest := time.Hour
	for range 5 {
		start := time.Now()
		rec := e.signIn("demo-legacy-sha1", password(t, "demo-legacy-sha1"), "192.0.2.79")
		took := time.Since(start)
		if rec.Code != http.StatusOK {
			t.Fatalf("sign in of demo-legacy-sha1: %d", rec.Code)
		}
		fastest = min(fastest, took)
	}
	if fastest >= refusalFloor {
		t.Errorf("the fastest of five successful sign ins took %v, the floor is %v", fastest, refusalFloor)
	}
}

func TestSessionBadRequests(t *testing.T) {
	e := newEnv(t, fixture(t))
	r := signInRequest("demo-admin", password(t, "demo-admin"), "192.0.2.10")
	r.Header.Set("Content-Type", "text/plain")
	if rec := e.do(r); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", rec.Code)
	}
	r = request(http.MethodPost, "/api/session", nil)
	r.Body = http.NoBody
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if rec := e.do(r); rec.Code != http.StatusBadRequest {
		t.Errorf("empty body: %d", rec.Code)
	}
	r = request(http.MethodPost, "/api/session", nil)
	r.Body = io.NopCloser(strings.NewReader(`{"username": "demo-admin", "password": "` + strings.Repeat("x", 9000) + `"}`))
	r.Header.Set("Content-Type", "application/json")
	if rec := e.do(r); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("9000 byte body: %d", rec.Code)
	}
	rec := e.do(request(http.MethodPut, "/api/session", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, POST, DELETE" {
		t.Errorf("PUT: %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestGetSession(t *testing.T) {
	e := newEnv(t, fixture(t))
	rec := e.do(request(http.MethodGet, "/api/session", nil))
	want := `{"user":null,"permissions":[],"csrfToken":null,"expiresInSeconds":0}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("without a cookie: %d %q", rec.Code, rec.Body)
	}
	cookie, signedIn := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	rec = e.do(request(http.MethodGet, "/api/session", cookie))
	a := decodeSession(t, rec)
	if a.User == nil || a.User.UserID != "demo-reader" || *a.CSRFToken != *signedIn.CSRFToken ||
		!slices.Equal(a.Permissions, signedIn.Permissions) {
		t.Errorf("with the cookie: %+v", a)
	}
	stale := &http.Cookie{Name: auth.CookieName, Value: strings.Repeat("A", 43)}
	rec = e.do(request(http.MethodGet, "/api/session", stale))
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("with a stale cookie: %d %q", rec.Code, rec.Body)
	}
	if c := sessionCookie(t, rec); c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("stale cookie not cleared: %+v", c)
	}
}

func TestSignOut(t *testing.T) {
	e := newEnv(t, fixture(t))
	cookie, a := e.mustSignIn("demo-admin", password(t, "demo-admin"))
	for _, token := range []string{"", "demo-wrong-token"} {
		r := request(http.MethodDelete, "/api/session", cookie)
		if token != "" {
			r.Header.Set(auth.CSRFHeader, token)
		}
		rec := e.do(r)
		if rec.Code != http.StatusForbidden || rec.Body.String() != `{"message":"missing or wrong request token"}`+"\n" {
			t.Errorf("sign out with token %q: %d %q", token, rec.Code, rec.Body)
		}
	}
	if s, err := e.svc.Subject(request(http.MethodGet, "/api/browse", cookie)); err != nil || s == nil {
		t.Fatalf("the session ended after a refused sign out: %v", err)
	}
	r := request(http.MethodDelete, "/api/session", cookie)
	r.Header.Set(auth.CSRFHeader, *a.CSRFToken)
	rec := e.do(r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign out: %d %q", rec.Code, rec.Body)
	}
	if c := sessionCookie(t, rec); c.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", c)
	}
	if _, err := e.svc.Subject(request(http.MethodGet, "/api/browse", cookie)); !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("the old cookie after sign out: %v", err)
	}
	if got := decodeSession(t, e.do(request(http.MethodGet, "/api/session", cookie))); got.User != nil {
		t.Errorf("session answer after sign out: %+v", got)
	}
	if rec := e.do(request(http.MethodDelete, "/api/session", nil)); rec.Code != http.StatusNoContent {
		t.Errorf("sign out without a session: %d", rec.Code)
	}
	if auth.SessionCount(e.svc) != 0 {
		t.Errorf("%d sessions left", auth.SessionCount(e.svc))
	}
}

func TestIdleTime(t *testing.T) {
	e := newEnv(t, fixture(t))
	cookie, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	for range 3 {
		e.clock.Add(29 * time.Minute)
		if s, err := e.svc.Subject(request(http.MethodGet, "/api/browse", cookie)); err != nil || s == nil {
			t.Fatalf("after 29 minutes: %v", err)
		}
	}
	e.clock.Add(29 * time.Minute)
	if got := decodeSession(t, e.do(request(http.MethodGet, "/api/session", cookie))); got.User == nil {
		t.Fatal("GET /api/session after 29 minutes ended the session")
	}
	e.clock.Add(30*time.Minute + time.Second)
	if _, err := e.svc.Subject(request(http.MethodGet, "/api/browse", cookie)); !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("after 30 idle minutes: %v", err)
	}
	if auth.SessionCount(e.svc) != 0 {
		t.Errorf("the idle session stays in the store")
	}
}

func TestSessionAge(t *testing.T) {
	e := newEnv(t, fixture(t))
	cookie, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	for range 35 {
		e.clock.Add(20 * time.Minute)
		if _, err := e.svc.Subject(request(http.MethodGet, "/api/browse", cookie)); err != nil {
			t.Fatalf("at %v: %v", e.clock.Now(), err)
		}
	}
	e.clock.Add(21 * time.Minute) // 12 hours and 1 minute after the sign in
	if _, err := e.svc.Subject(request(http.MethodGet, "/api/browse", cookie)); !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("after 12 hours: %v", err)
	}
}

func TestNewIDEachSignIn(t *testing.T) {
	e := newEnv(t, fixture(t))
	first, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	second, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	if first.Value == second.Value {
		t.Fatal("two sign ins gave the same id")
	}
	for _, c := range []*http.Cookie{first, second} {
		if s, err := e.svc.Subject(request(http.MethodGet, "/api/browse", c)); err != nil || s == nil {
			t.Errorf("session %d: %v", len(c.Value), err)
		}
	}
	// a sign in that carries a live cookie ends that session
	r := signInRequest("demo-admin", password(t, "demo-admin"), "192.0.2.10")
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: first.Value})
	third := sessionCookie(t, e.do(r))
	if third.Value == first.Value {
		t.Fatal("the sign in kept the id")
	}
	if _, err := e.svc.Subject(request(http.MethodGet, "/api/browse", first)); !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("the replaced session lives on: %v", err)
	}
	if auth.SessionCount(e.svc) != 2 {
		t.Errorf("%d sessions, want 2", auth.SessionCount(e.svc))
	}
}

func TestSessionBounds(t *testing.T) {
	e := newEnv(t, fixture(t))
	if perUser, total := auth.SessionLimits(e.svc); perUser != 20 || total != 10000 {
		t.Fatalf("bounds %d and %d", perUser, total)
	}
	if err := auth.FillSessions(e.svc, auth.MaxSessions+50); err != nil {
		t.Fatal(err)
	}
	if n := auth.SessionCount(e.svc); n != auth.MaxSessions {
		t.Errorf("%d sessions after filling past the bound", n)
	}
	e = newEnv(t, fixture(t))
	auth.SetSessionLimits(e.svc, 3, 5)
	var cookies []*http.Cookie
	for range 4 {
		c, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
		cookies = append(cookies, c)
		e.clock.Add(time.Second)
	}
	if _, err := e.svc.Subject(request(http.MethodGet, "/", cookies[0])); !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("the oldest of 4 sessions with a bound of 3 lives: %v", err)
	}
	for _, c := range cookies[1:] {
		if _, err := e.svc.Subject(request(http.MethodGet, "/", c)); err != nil {
			t.Errorf("a newer session ended: %v", err)
		}
	}
	for _, u := range []string{"demo-admin", "demo-nobody", "demo-unicode"} {
		e.mustSignIn(u, password(t, u))
		e.clock.Add(time.Second)
	}
	if n := auth.SessionCount(e.svc); n != 5 {
		t.Errorf("%d sessions with a bound of 5", n)
	}
	if _, err := e.svc.Subject(request(http.MethodGet, "/", cookies[1])); !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("the oldest session lives past the total bound: %v", err)
	}
}

func TestCSRF(t *testing.T) {
	e := newEnv(t, fixture(t))
	cookie, a := e.mustSignIn("demo-admin", password(t, "demo-admin"))
	subject, err := e.svc.Subject(request(http.MethodGet, "/", cookie))
	if err != nil || subject == nil || subject.Session == "" {
		t.Fatalf("subject of the cookie: %+v %v", subject, err)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for token, want := range map[string]bool{"": false, "demo-wrong-token": false, *a.CSRFToken: true,
			*a.CSRFToken + "x": false} {
			r := request(method, "/service/rest/v1/repositories", cookie)
			if token != "" {
				r.Header.Set(auth.CSRFHeader, token)
			}
			if got := e.svc.CSRF(r, subject); got != want {
				t.Errorf("%s with token %q: %v", method, token, got)
			}
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if !e.svc.CSRF(request(method, "/", cookie), subject) {
			t.Errorf("%s needs a token", method)
		}
	}
	r := basic(request(http.MethodPost, "/", nil), "demo-admin", password(t, "demo-admin"))
	bs, err := e.svc.Subject(r)
	if err != nil || bs == nil || bs.Session != "" {
		t.Fatalf("Basic subject: %+v %v", bs, err)
	}
	if !e.svc.CSRF(r, bs) || !e.svc.CSRF(request(http.MethodPost, "/", nil), nil) {
		t.Error("Basic or anonymous needs a token")
	}
	gone := *subject
	gone.Session = strings.Repeat("B", 43)
	r = request(http.MethodPost, "/", cookie)
	r.Header.Set(auth.CSRFHeader, *a.CSRFToken)
	if e.svc.CSRF(r, &gone) {
		t.Error("the token of another session passes")
	}
}
