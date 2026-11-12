package auth_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/auth"
	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// vector is one entry of testdata/passwords.json (make_passwords.py).
type vector struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Hash     string `json:"hash"`
	Source   string `json:"source"`
	Why      string `json:"why"`
}

type vectorFile struct {
	Verify []vector `json:"verify"`
	Refuse []vector `json:"refuse"`
}

var (
	vectorsOnce sync.Once
	vectors     vectorFile
	vectorErr   error
)

func loadVectors(t testing.TB) vectorFile {
	t.Helper()
	vectorsOnce.Do(func() {
		data, err := os.ReadFile("testdata/passwords.json")
		if err != nil {
			vectorErr = err
			return
		}
		vectorErr = json.Unmarshal(data, &vectors)
	})
	if vectorErr != nil {
		t.Fatal(vectorErr)
	}
	return vectors
}

func vectorNamed(t testing.TB, name string) vector {
	t.Helper()
	for _, v := range loadVectors(t).Verify {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no vector %s", name)
	return vector{}
}

// fixtureUser is an invented user, its password, and what a sign in gives.
type fixtureUser struct {
	id, vector, status string
	roles              []string
	signsIn            bool
}

var fixtureUsers = []fixtureUser{
	{"demo-admin", "shiro1-sha512", "active", []string{"nx-admin"}, true},
	{"demo-reader", "shiro1-sha256", "active", []string{"demo-maven-reader"}, true},
	{"demo-nobody", "shiro1-nobody", "active", nil, true},
	{"demo-off", "shiro1-off", "disabled", []string{"nx-admin"}, false},
	{"demo-locked", "shiro1-locked", "locked", []string{"nx-admin"}, false},
	{"demo-change", "shiro1-change", "changepassword", []string{"nx-admin"}, false},
	{"demo-legacy-sha1", "legacy-sha1", "active", []string{"demo-maven-reader"}, true},
	{"demo-legacy-md5", "legacy-md5", "active", []string{"demo-maven-reader"}, true},
	{"demo-pbkdf2", "pbkdf2-sha256", "active", []string{"demo-maven-reader"}, true},
	{"demo-pbkdf2-sha1", "pbkdf2-sha1", "active", nil, true},
	{"demo-unicode", "shiro1-unicode", "active", nil, true},
	{"anonymous", "shiro1-anonymous", "active", []string{"nx-anonymous"}, true},
}

// noHashUser is active but has no hash, as after an export without
// --with-password-hashes.
const noHashUser = "demo-nohash"

func password(t testing.TB, user string) string {
	t.Helper()
	for _, u := range fixtureUsers {
		if u.id == user {
			return vectorNamed(t, u.vector).Password
		}
	}
	t.Fatalf("no fixture user %s", user)
	return ""
}

// fixture is an invented identity state with every kind of user.
func fixture(t testing.TB) *model.Snapshot {
	t.Helper()
	s := &model.Snapshot{
		Roles: []model.Role{
			{ID: "demo-maven-reader", Source: "default", Name: "Demo maven reader",
				Privileges: []string{"nx-repository-view-maven2-demo-maven-read", "nx-repository-view-maven2-demo-maven-browse"}},
			{ID: "demo-ldap-group", Source: "default", Name: "demo-ldap-group",
				Privileges: []string{"nx-repository-view-raw-demo-raw-read"}},
			{ID: "demo-ops", Source: "default", Name: "Demo operations", Privileges: []string{"nx-settings-read"}},
		},
		RoleMappings: []model.RoleMapping{{UserID: "DEMO-LDAP-PERSON", Source: "LDAP", Roles: []string{"demo-ops"}}},
		Realms:       []string{auth.RealmLocal, "NexusAuthorizingRealm", auth.RealmLDAP, "DockerToken"},
		Anonymous:    model.Anonymous{Enabled: false, UserID: "anonymous", RealmName: "NexusAuthorizingRealm"},
		Repositories: []model.Repository{
			{Name: "demo-maven", Format: "maven2", Type: "hosted", Online: true},
			{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true},
			{Name: "demo-npm", Format: "npm", Type: "proxy", Online: true},
		},
	}
	for _, u := range fixtureUsers {
		v := vectorNamed(t, u.vector)
		s.Users = append(s.Users, model.User{ID: u.id, FirstName: "Demo", LastName: strings.ToUpper(u.id[:1]) + u.id[1:],
			Email: u.id + "@example.com", Status: u.status, Source: "default", Roles: u.roles, PasswordHash: v.Hash})
	}
	s.Users = append(s.Users, model.User{ID: noHashUser, FirstName: "Demo", LastName: "Nohash",
		Email: noHashUser + "@example.com", Status: "active", Source: "default", Roles: []string{"nx-admin"}})
	return s
}

// clock is a fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// directory is a fake LDAP connection with invented people.
type directory struct {
	mu     sync.Mutex
	calls  int
	people map[string]string
	groups []string
	err    error
	noID   bool // accept every pair and answer a user without an id
}

func (d *directory) Login(_ context.Context, username, password string) (model.User, []string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return model.User{}, nil, d.err
	}
	if d.noID {
		return model.User{Source: "LDAP", Status: "active"}, d.groups, nil
	}
	if want, ok := d.people[username]; ok && want == password {
		return model.User{ID: username, FirstName: "Lena", LastName: "Directory", Email: username + "@example.com",
			Source: "LDAP", Status: "active"}, d.groups, nil
	}
	return model.User{}, nil, fmt.Errorf("demo directory: %w", auth.ErrWrongPair)
}

func (d *directory) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

const ldapPassword = "demo-ldap-pass-1"

func ldapDirectory() *directory {
	return &directory{people: map[string]string{"demo-ldap-person": ldapPassword},
		groups: []string{"demo-ldap-group", "demo-unknown-group"}}
}

// env is a service with a fake clock, a log buffer and its handler.
type env struct {
	t       testing.TB
	svc     *auth.Service
	handler http.Handler
	clock   *clock
	logs    *bytes.Buffer
}

func newEnv(t testing.TB, snap *model.Snapshot, dirs ...auth.Directory) *env {
	t.Helper()
	c := newClock()
	logs := &bytes.Buffer{}
	svc, err := auth.New(auth.Options{Snapshot: snap, Directories: dirs, Now: c.Now,
		Logger: slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, svc: svc, handler: svc.SessionHandler(), clock: c, logs: logs}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func signInRequest(user, pass, address string) *http.Request {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	r := httptest.NewRequest(http.MethodPost, "/api/session", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = address + ":40000"
	return r
}

func (e *env) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

func (e *env) signIn(user, pass, address string) *httptest.ResponseRecorder {
	return e.do(signInRequest(user, pass, address))
}

// mustSignIn signs in and returns the cookie and the session answer.
func (e *env) mustSignIn(user, pass string) (*http.Cookie, sessionAnswer) {
	e.t.Helper()
	rec := e.signIn(user, pass, "192.0.2.10")
	if rec.Code != http.StatusOK {
		e.t.Fatalf("sign in of %s: %d %s", user, rec.Code, rec.Body)
	}
	return sessionCookie(e.t, rec), decodeSession(e.t, rec)
}

func sessionCookie(t testing.TB, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatalf("no session cookie in %q", rec.Header().Values("Set-Cookie"))
	return nil
}

type sessionUser struct {
	UserID       string `json:"userId"`
	FirstName    string `json:"firstName"`
	LastName     string `json:"lastName"`
	EmailAddress string `json:"emailAddress"`
	Source       string `json:"source"`
	Realm        string `json:"realm"`
}

type sessionAnswer struct {
	User             *sessionUser `json:"user"`
	Permissions      []string     `json:"permissions"`
	CSRFToken        *string      `json:"csrfToken"`
	ExpiresInSeconds int          `json:"expiresInSeconds"`
}

func decodeSession(t testing.TB, rec *httptest.ResponseRecorder) sessionAnswer {
	t.Helper()
	var a sessionAnswer
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		t.Fatalf("session answer %q: %v", rec.Body, err)
	}
	return a
}

func request(method, target string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "192.0.2.10:40000"
	if cookie != nil {
		r.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	return r
}

func basic(r *http.Request, user, pass string) *http.Request {
	r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	return r
}
