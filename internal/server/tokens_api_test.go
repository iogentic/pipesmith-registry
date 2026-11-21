package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The tokens of leg 6-api (contract S29, rulings AP1 to AP4, AP6) on a
// fake security service that issues tokens in memory.

const fakeSecret = "NpmToken.PUBLICHALF01.example-secret-half-of-the-fake-token-000"

// tokenSecurity is the fake security with a store of tokens.
type tokenSecurity struct {
	*fakeSecurity
	mu     sync.Mutex
	tokens []auth.TokenEntry
	next   int
}

func newTokenSecurity() *tokenSecurity {
	f := newFakeSecurity()
	// demo-keeper holds the tokens permission but not nexus:users:update
	f.addBasic("demo-keeper:keeper-pw", subject("demo-keeper", "", "nexus:apikey:*"))
	return &tokenSecurity{fakeSecurity: f, tokens: []auth.TokenEntry{
		{ID: "readerpublic", Kind: "npm", Name: "ci", UserID: "demo-reader", Source: "default", Public: "readerpublic",
			Created: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)},
	}}
}

func (s *tokenSecurity) IssueToken(subject *model.Subject, kind, name string) (string, model.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind != "npm" && kind != "user" {
		return "", model.Token{}, &auth.TokenKindError{Kind: kind, Reason: "the kind " + kind + " is not issued here"}
	}
	count := 0
	for _, e := range s.tokens {
		if e.UserID == subject.User.ID && e.Kind == kind && e.Name == name {
			return "", model.Token{}, model.ErrTokenName
		}
		if e.UserID == subject.User.ID && e.Kind == kind && e.Name != "" {
			count++
		}
	}
	if count >= auth.MaxTokensPerKind {
		return "", model.Token{}, model.ErrTokenLimit
	}
	s.next++
	public := "PUBLICHALF0" + string(rune('0'+s.next))
	row := model.Token{Public: public, Kind: kind, UserID: subject.User.ID, Source: subject.User.Source, Name: name,
		Created: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	s.tokens = append(s.tokens, auth.TokenEntry{ID: public, Kind: kind, Name: name, UserID: row.UserID, Source: row.Source,
		Public: public, Created: row.Created})
	return fakeSecret, row, nil
}

func (s *tokenSecurity) CheckTokenKind(kind string) error {
	if kind != "npm" && kind != "user" {
		return &auth.TokenKindError{Kind: kind, Reason: "the kind " + kind + " is not issued here"}
	}
	return nil
}

func (s *tokenSecurity) ListTokens(userID, source string) ([]auth.TokenEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []auth.TokenEntry{}
	for _, e := range s.tokens {
		if e.UserID == userID && e.Source == source {
			list = append(list, e)
		}
	}
	return list, nil
}

func (s *tokenSecurity) TokenOwner(id string) (string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.tokens {
		if e.ID == id {
			return e.UserID, e.Source, true, nil
		}
	}
	return "", "", false, nil
}

func (s *tokenSecurity) RemoveTokenID(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.tokens {
		if e.ID == id {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func apiTokenApp(t *testing.T) (*secureApp, *tokenSecurity) {
	t.Helper()
	sec := newTokenSecurity()
	return &secureApp{app: startSecured(t, sec), sec: sec.fakeSecurity}, sec
}

func apiTokenList(t *testing.T, a *answer) []map[string]any {
	t.Helper()
	status(t, a, 200)
	var list []map[string]any
	if err := json.Unmarshal(a.body, &list); err != nil {
		t.Fatalf("%v: %s", err, a.body)
	}
	return list
}

func TestTokensIssueListRevoke(t *testing.T) {
	a, _ := apiTokenApp(t)
	r := do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"laptop"}`), "basic", asAdmin, "Content-Type", jsonType)
	status(t, r, 201)
	created := r.object(t)
	if created["secret"] != fakeSecret || created["name"] != "laptop" || created["kind"] != "npm" || created["userId"] != "demo-admin" ||
		created["publicPart"] != created["id"] || created["created"] != "2026-11-20T09:00:00.000Z" || created["lastUsed"] != nil {
		t.Fatalf("create answer %s", r.body)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create answer without no-store: %v", r.header)
	}
	// A11: the create answer shows the secret at its top, which Leaks allows for a token create only
	if leaks := adminrules.Leaks(created, adminrules.KindToken); len(leaks) != 0 {
		t.Fatalf("the create answer leaks %v", leaks)
	}
	if leaks := adminrules.Leaks(created, ""); len(leaks) != 1 || leaks[0] != "secret" {
		t.Fatalf("the guard does not see the secret of the create answer: %v", leaks)
	}
	list := apiTokenList(t, a.send("GET", "/api/tokens", "basic", asAdmin))
	if len(list) != 1 || list[0]["id"] != created["id"] || list[0]["name"] != "laptop" {
		t.Fatalf("own list %v", list)
	}
	for _, path := range []string{"/api/tokens", "/api/tokens?userId=demo-admin"} {
		r := a.send("GET", path, "basic", asAdmin)
		if body := string(r.body); strings.Contains(body, fakeSecret) || strings.Contains(body, "secret") {
			t.Fatalf("%s holds the secret: %s", path, body)
		}
		if leaks := adminrules.Leaks(apiTokenList(t, r), ""); len(leaks) != 0 || r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s leaks %v, Cache-Control %q", path, leaks, r.header.Get("Cache-Control"))
		}
	}
	// the tokens of another user, with nexus:users:update
	other := apiTokenList(t, a.send("GET", "/api/tokens?userId=demo-reader", "basic", asAdmin))
	if len(other) != 1 || other[0]["id"] != "readerpublic" {
		t.Fatalf("list of demo-reader %v", other)
	}
	status(t, do(t, a.addr(), "DELETE", "/api/tokens/"+created["id"].(string), nil, "basic", asAdmin), 204)
	if list := apiTokenList(t, a.send("GET", "/api/tokens", "basic", asAdmin)); len(list) != 0 {
		t.Fatalf("list after the revoke %v", list)
	}
	status(t, do(t, a.addr(), "DELETE", "/api/tokens/"+created["id"].(string), nil, "basic", asAdmin), 404)
	status(t, do(t, a.addr(), "DELETE", "/api/tokens/readerpublic", nil, "basic", asAdmin), 204)
}

func TestTokensOfAnotherUserNeedUsersUpdate(t *testing.T) {
	a, sec := apiTokenApp(t)
	list := apiTokenList(t, a.send("GET", "/api/tokens", "basic", "demo-keeper:keeper-pw"))
	if len(list) != 0 {
		t.Fatalf("own list of demo-keeper %v", list)
	}
	r := a.send("GET", "/api/tokens?userId=demo-reader", "basic", "demo-keeper:keeper-pw")
	if r.status != 403 || strings.Contains(string(r.body), "readerpublic") {
		t.Fatalf("list of another user without users:update: %d %s", r.status, r.body)
	}
	if r := do(t, a.addr(), "DELETE", "/api/tokens/readerpublic", nil, "basic", "demo-keeper:keeper-pw"); r.status != 404 {
		t.Fatalf("delete of another user's token without users:update: %d %s", r.status, r.body)
	}
	if _, _, found, _ := sec.TokenOwner("readerpublic"); !found {
		t.Fatal("the token of demo-reader was removed")
	}
	// without nexus:apikey:* none of the three passes
	for _, c := range []struct{ method, target string }{{"GET", "/api/tokens"}, {"POST", "/api/tokens"}, {"DELETE", "/api/tokens/readerpublic"}} {
		if r := do(t, a.addr(), c.method, c.target, []byte(`{"kind":"npm","name":"x1"}`), "basic", asReader); r.status != 403 {
			t.Errorf("%s %s as demo-reader: %d", c.method, c.target, r.status)
		}
	}
}

func TestTokenCreateRefusals(t *testing.T) {
	a, _ := apiTokenApp(t)
	for _, c := range []struct {
		body, field string
		status      int
	}{
		{`{"kind":"docker","name":"x1"}`, "kind", 400},
		{`{"kind":"nuget","name":"x1"}`, "kind", 400},
		{`{"kind":"npm","name":""}`, "name", 400},
		{`{"kind":"npm","name":"` + strings.Repeat("a", 65) + `"}`, "name", 400},
		{`{"kind":"npm","name":"-dash"}`, "name", 400},
		{`{"kind":"npm","name":"a b"}`, "name", 400},
		{`[]`, "*", 400},
	} {
		r := do(t, a.addr(), "POST", "/api/tokens", []byte(c.body), "basic", asAdmin, "Content-Type", jsonType)
		if r.status != c.status || !strings.Contains(string(r.body), `"id":"`+c.field+`"`) || strings.Contains(string(r.body), "secret") {
			t.Errorf("%s: %d %s", c.body, r.status, r.body)
		}
	}
	status(t, do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"`+strings.Repeat("a", 64)+`"}`), "basic", asAdmin), 201)
	status(t, do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"twice"}`), "basic", asAdmin), 201)
	if r := do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"twice"}`), "basic", asAdmin); r.status != 409 ||
		strings.Contains(string(r.body), fakeSecret) {
		t.Fatalf("a name taken: %d %s", r.status, r.body)
	}
}

// AP6: the token writes take the request token through a session cookie
// and stay behind freezeGate: while frozen they answer 503 with the reason
// and change nothing; the list is a read and passes.
func TestTokenWritesNeedTheRequestTokenAndStopWhileFrozen(t *testing.T) {
	a, sec := apiTokenApp(t)
	for _, c := range []struct{ method, target string }{{"POST", "/api/tokens"}, {"DELETE", "/api/tokens/readerpublic"}} {
		r := do(t, a.addr(), c.method, c.target, []byte(`{"kind":"npm","name":"x1"}`), "Cookie", "fake-session=s1")
		if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
			t.Errorf("%s %s without the request token: %d %s", c.method, c.target, r.status, r.body)
		}
	}
	status(t, do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"x1"}`), "Cookie", "fake-session=s1",
		"X-Pipesmith-Csrf", "csrf-s1"), 201)
	for _, p := range unfrozen {
		if p.MatchString("/api/tokens") || p.MatchString("/api/tokens/readerpublic") {
			t.Fatalf("a token write is in unfrozen: %s", p)
		}
	}
	status(t, do(t, a.addr(), "POST", "/service/rest/v1/read-only/freeze", nil, "basic", asAdmin), 204)
	reason := readOnlyOf(t, a).SummaryReason
	if reason == "" {
		t.Fatal("a freeze without a reason")
	}
	for _, c := range []struct{ method, target string }{{"POST", "/api/tokens"}, {"DELETE", "/api/tokens/readerpublic"}} {
		r := do(t, a.addr(), c.method, c.target, []byte(`{"kind":"npm","name":"x2"}`), "basic", asAdmin)
		if r.status != 503 || r.object(t)["message"] != reason {
			t.Errorf("%s %s while frozen: %d %s", c.method, c.target, r.status, r.body)
		}
	}
	if _, _, found, _ := sec.TokenOwner("readerpublic"); !found {
		t.Fatal("a delete while frozen removed the token")
	}
	if list := apiTokenList(t, a.send("GET", "/api/tokens", "basic", asAdmin)); len(list) != 1 || list[0]["name"] != "x1" {
		t.Fatalf("the list while frozen %v", list)
	}
	status(t, a.send("POST", "/service/rest/v1/read-only/release", "basic", asAdmin), 204)
	status(t, do(t, a.addr(), "DELETE", "/api/tokens/readerpublic", nil, "basic", asAdmin), 204)
}

func TestTokensWithoutATokenService(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	if r := a.send("GET", "/api/tokens", "basic", asAdmin); r.status != 503 {
		t.Fatalf("GET /api/tokens without a token service: %d %s", r.status, r.body)
	}
}

// Finding F1 of the review: the 21st named token of a kind answers 409
// with a message and the list keeps 20.
func TestTokensTheTwentyFirstNamedAnswers409(t *testing.T) {
	a, _ := apiTokenApp(t)
	for i := range auth.MaxTokensPerKind {
		body := []byte(`{"kind":"user","name":"named-` + string(rune('a'+i)) + `"}`)
		status(t, do(t, a.addr(), "POST", "/api/tokens", body, "basic", asAdmin, "Content-Type", jsonType), 201)
	}
	r := do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"user","name":"named-extra"}`), "basic", asAdmin, "Content-Type", jsonType)
	status(t, r, 409)
	if m := r.object(t); m["message"] == "" || m["message"] == nil || m["secret"] != nil {
		t.Fatalf("the 409 body %s", r.body)
	}
	if list := apiTokenList(t, a.send("GET", "/api/tokens", "basic", asAdmin)); len(list) != auth.MaxTokensPerKind {
		t.Fatalf("%d tokens listed, want %d", len(list), auth.MaxTokensPerKind)
	}
	status(t, do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"named-extra"}`), "basic", asAdmin, "Content-Type", jsonType), 201)
}

// asAdminToken is demo-admin signed in with a token as the Basic password.
const asAdminToken = "demo-admin:UserToken.PUBLICHALF09.example-user-token-secret"

func tokenSubject() *model.Subject {
	s := subject("demo-admin", "", "nexus:*")
	s.ByToken = true
	return s
}

// Finding F2 of the review: a subject that signed in with a token lists
// tokens but issues and revokes none; a session and the password do both.
func TestTokensWritesNeedASessionOrThePassword(t *testing.T) {
	a, sec := apiTokenApp(t)
	sec.addBasic(asAdminToken, tokenSubject())
	apiTokenList(t, a.send("GET", "/api/tokens", "basic", asAdminToken))
	apiTokenList(t, a.send("GET", "/api/tokens?userId=demo-reader", "basic", asAdminToken))
	for _, c := range []struct{ method, target, body string }{
		{"POST", "/api/tokens", `{"kind":"npm","name":"by-token"}`},
		{"DELETE", "/api/tokens/readerpublic", ""},
	} {
		r := do(t, a.addr(), c.method, c.target, []byte(c.body), "basic", asAdminToken, "Content-Type", jsonType)
		status(t, r, 403)
		if m := r.object(t); m["message"] == nil || m["message"] == "" {
			t.Fatalf("%s %s: body %s", c.method, c.target, r.body)
		}
	}
	if list := apiTokenList(t, a.send("GET", "/api/tokens?userId=demo-reader", "basic", asAdmin)); len(list) != 1 {
		t.Fatalf("a token subject changed the tokens: %v", list)
	}
	// the password and the session pass
	status(t, do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"by-password"}`), "basic", asAdmin, "Content-Type", jsonType), 201)
	status(t, do(t, a.addr(), "POST", "/api/tokens", []byte(`{"kind":"npm","name":"by-session"}`), "Cookie", "fake-session=s1",
		"X-Pipesmith-Csrf", "csrf-s1", "Content-Type", jsonType), 201)
	status(t, do(t, a.addr(), "DELETE", "/api/tokens/readerpublic", nil, "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1"), 204)
}

// Finding F6 of the review: the kind is checked before the name, so a
// docker or unknown kind with a bad name answers 400 at kind (AP1); a good
// kind with a bad name answers 400 at name.
func TestTokensKindBeforeName(t *testing.T) {
	a, _ := apiTokenApp(t)
	for body, want := range map[string]string{
		`{"kind":"docker","name":"a b"}`: "kind",
		`{"kind":"docker"}`:              "kind",
		`{"kind":"nuget","name":""}`:     "kind",
		`{"kind":"npm","name":"a b"}`:    "name",
	} {
		r := do(t, a.addr(), "POST", "/api/tokens", []byte(body), "basic", asAdmin, "Content-Type", jsonType)
		status(t, r, 400)
		var fields []map[string]any
		if err := json.Unmarshal(r.body, &fields); err != nil || len(fields) != 1 || fields[0]["id"] != want {
			t.Fatalf("%s: %s, want 400 at %s", body, r.body, want)
		}
	}
}
