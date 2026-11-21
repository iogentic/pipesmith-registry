package server

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The own account of leg 6-api (contract S41, rulings AP6, AP7) on a fake
// security service whose password change keeps the form it was given.

const (
	asPerson    = "demo-person:person-pw"
	newPassword = "example-account-new-1"
)

// accountSecurity is the fake security with the password change of
// auth.Service: admin-pw is the current password of every subject.
type accountSecurity struct {
	*fakeSecurity
	mu      sync.Mutex
	changed []string // the forms stored, in order
}

func newAccountSecurity() *accountSecurity {
	f := newFakeSecurity()
	person := subject("demo-person", "")
	person.User.Source = "LDAP"
	f.addBasic(asPerson, person)
	return &accountSecurity{fakeSecurity: f}
}

func (s *accountSecurity) ChangeOwnPassword(_ *http.Request, subject *model.Subject, current, hash string) error {
	if subject.User.ID == "demo-throttled-change" {
		return &model.Error{Status: http.StatusTooManyRequests, Message: model.ErrThrottled.Message, RetryAfter: 7}
	}
	if current != "admin-pw" {
		return auth.ErrWrongPassword
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changed = append(s.changed, hash)
	return nil
}

func (s *accountSecurity) forms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.changed...)
}

func accountApp(t *testing.T) (*secureApp, *accountSecurity) {
	t.Helper()
	sec := newAccountSecurity()
	return &secureApp{app: startSecured(t, sec), sec: sec.fakeSecurity}, sec
}

func TestAccountSaveKeepsRolesAndStatus(t *testing.T) {
	a, sec := accountApp(t)
	r := do(t, a.addr(), "PUT", "/api/account", []byte(`{"firstName":"Dana","emailAddress":"dana@example.com","roles":[],"status":"disabled"}`),
		"basic", asAdmin, "Content-Type", jsonType)
	status(t, r, 204)
	if r.header.Get("Cache-Control") != "no-store" || len(r.body) != 0 {
		t.Fatalf("answer %v %s", r.header, r.body)
	}
	u := sec.Snapshot().Users[0]
	if u.FirstName != "Dana" || u.LastName != "Admin" || u.Email != "dana@example.com" || u.Status != "active" ||
		len(u.Roles) != 1 || u.Roles[0] != "demo-administrators" {
		t.Fatalf("saved %+v", u)
	}
	// Q6: a directory user keeps its names and mail; nothing is written
	updates, _ := sec.writes()
	r = do(t, a.addr(), "PUT", "/api/account", []byte(`{"firstName":"Dana"}`), "basic", asPerson, "Content-Type", jsonType)
	if r.status != 409 || !strings.Contains(r.object(t)["message"].(string), "directory") {
		t.Fatalf("a directory user: %d %s", r.status, r.body)
	}
	if after, _ := sec.writes(); after != updates {
		t.Fatal("the refused save wrote")
	}
	status(t, do(t, a.addr(), "PUT", "/api/account", []byte(`[]`), "basic", asAdmin), 400)
	status(t, do(t, a.addr(), "PUT", "/api/account", []byte(`{}`)), 401)
}

func TestOwnPasswordChange(t *testing.T) {
	a, sec := accountApp(t)
	send := func(body, who string) *answer {
		return do(t, a.addr(), "POST", "/api/account/password", []byte(body), "basic", who, "Content-Type", jsonType)
	}
	field := func(r *answer, want string) {
		t.Helper()
		if r.status != 400 || !strings.Contains(string(r.body), `"id":"`+want+`"`) {
			t.Fatalf("want 400 at %s: %d %s", want, r.status, r.body)
		}
	}
	field(send(`{"currentPassword":"wrong","newPassword":"`+newPassword+`"}`, asAdmin), "currentPassword")
	field(send(`{"currentPassword":"admin-pw","newPassword":""}`, asAdmin), "newPassword")
	field(send(`{"currentPassword":"admin-pw","newPassword":"a\nb"}`, asAdmin), "newPassword")
	field(send(`{"currentPassword":"admin-pw"}`, asAdmin), "newPassword")
	if len(sec.forms()) != 0 {
		t.Fatal("a refused change stored a form")
	}
	r := send(`{"currentPassword":"admin-pw","newPassword":"`+newPassword+`"}`, asAdmin)
	status(t, r, 204)
	if strings.Contains(string(r.body), newPassword) || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("answer %v %s", r.header, r.body)
	}
	forms := sec.forms()
	if len(forms) != 1 || !auth.VerifyPassword(newPassword, forms[0]) || strings.Contains(forms[0], newPassword) {
		t.Fatalf("stored forms %q", forms)
	}
	// Q6: a directory user keeps its password in its directory
	if r := send(`{"currentPassword":"admin-pw","newPassword":"`+newPassword+`"}`, asPerson); r.status != 409 {
		t.Fatalf("a directory user: %d %s", r.status, r.body)
	}
	if len(sec.forms()) != 1 {
		t.Fatal("the change of a directory user stored a form")
	}
	status(t, do(t, a.addr(), "POST", "/api/account/password", []byte(`{}`)), 401)
}

// The 429 of the sign in limits answers as the 429 of a sign in: an empty
// body with Retry-After.
func TestOwnPasswordChangeThrottledAnswersAsASignIn(t *testing.T) {
	a, sec := accountApp(t)
	sec.addBasic("demo-throttled-change:pw", subject("demo-throttled-change", ""))
	r := do(t, a.addr(), "POST", "/api/account/password", []byte(`{"currentPassword":"pw","newPassword":"`+newPassword+`"}`),
		"basic", "demo-throttled-change:pw", "Content-Type", jsonType)
	signIn := a.send("GET", "/api/repositories", "basic", "demo-throttled:x")
	if r.status != 429 || r.header.Get("Retry-After") != "7" || len(r.body) != 0 {
		t.Fatalf("throttled change: %d %v %s", r.status, r.header, r.body)
	}
	if signIn.status != 429 || string(signIn.body) != string(r.body) || signIn.header.Get("Retry-After") != r.header.Get("Retry-After") {
		t.Fatalf("throttled sign in: %d %v %s", signIn.status, signIn.header, signIn.body)
	}
}

// A server whose security service changes no passwords answers 503.
func TestOwnPasswordChangeWithoutTheServiceAnswers503(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	r := do(t, a.addr(), "POST", "/api/account/password", []byte(`{"currentPassword":"admin-pw","newPassword":"`+newPassword+`"}`),
		"basic", asAdmin, "Content-Type", jsonType)
	if r.status != 503 || r.object(t)["message"] != accountRefusal {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

// AP6: the account writes take the request token through a session cookie
// and stay behind freezeGate: while frozen they answer 503 with the reason
// and change nothing.
func TestAccountWritesNeedTheRequestTokenAndStopWhileFrozen(t *testing.T) {
	a, sec := accountApp(t)
	writes := []struct{ method, target, body string }{
		{"PUT", "/api/account", `{"firstName":"Cookie"}`},
		{"POST", "/api/account/password", `{"currentPassword":"admin-pw","newPassword":"` + newPassword + `"}`},
	}
	for _, c := range writes {
		r := do(t, a.addr(), c.method, c.target, []byte(c.body), "Cookie", "fake-session=s1")
		if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
			t.Errorf("%s %s without the request token: %d %s", c.method, c.target, r.status, r.body)
		}
		for _, p := range unfrozen {
			if p.MatchString(c.target) {
				t.Fatalf("an account write is in unfrozen: %s", p)
			}
		}
	}
	if sec.Snapshot().Users[0].FirstName != "Demo" || len(sec.forms()) != 0 {
		t.Fatal("a write without the request token changed the account")
	}
	status(t, do(t, a.addr(), "PUT", "/api/account", []byte(writes[0].body), "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1"), 204)
	status(t, do(t, a.addr(), "POST", "/service/rest/v1/read-only/freeze", nil, "basic", asAdmin), 204)
	reason := a.send("GET", "/service/rest/v1/read-only", "basic", asAdmin).object(t)["summaryReason"]
	for _, c := range writes {
		r := do(t, a.addr(), c.method, c.target, []byte(c.body), "basic", asAdmin, "Content-Type", jsonType)
		if r.status != 503 || r.object(t)["message"] != reason {
			t.Errorf("%s %s while frozen: %d %s", c.method, c.target, r.status, r.body)
		}
	}
	if len(sec.forms()) != 0 {
		t.Fatal("a change while frozen stored a form")
	}
	status(t, do(t, a.addr(), "POST", "/service/rest/v1/read-only/release", nil, "basic", asAdmin), 204)
	status(t, do(t, a.addr(), "POST", "/api/account/password", []byte(writes[1].body), "basic", asAdmin), 204)
}

// Finding F2 of the review: a subject that signed in with a token changes
// neither the account nor the password; a session and the password do.
func TestAccountWritesNeedASessionOrThePassword(t *testing.T) {
	a, sec := accountApp(t)
	sec.addBasic(asAdminToken, tokenSubject())
	save := []byte(`{"firstName":"Dana"}`)
	change := []byte(`{"currentPassword":"admin-pw","newPassword":"` + newPassword + `"}`)
	for _, c := range []struct {
		target string
		body   []byte
	}{{"/api/account", save}, {"/api/account/password", change}} {
		method := map[string]string{"/api/account": "PUT", "/api/account/password": "POST"}[c.target]
		r := do(t, a.addr(), method, c.target, c.body, "basic", asAdminToken, "Content-Type", jsonType)
		status(t, r, 403)
		if m := r.object(t); m["message"] == nil || m["message"] == "" {
			t.Fatalf("%s: body %s", c.target, r.body)
		}
	}
	if u := sec.Snapshot().Users[0]; u.FirstName == "Dana" || len(sec.forms()) != 0 {
		t.Fatalf("a token subject changed the account: %+v, forms %v", u, sec.forms())
	}
	status(t, do(t, a.addr(), "PUT", "/api/account", save, "basic", asAdmin, "Content-Type", jsonType), 204)
	status(t, do(t, a.addr(), "POST", "/api/account/password", change, "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1",
		"Content-Type", jsonType), 204)
	if u := sec.Snapshot().Users[0]; u.FirstName != "Dana" || len(sec.forms()) != 1 {
		t.Fatalf("the password and the session did not change the account: %+v, forms %v", u, sec.forms())
	}
}
