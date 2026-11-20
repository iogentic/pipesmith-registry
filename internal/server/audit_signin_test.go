package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
)

// Task 7.16: sign in, a bad password, an unknown user, sign out, the issue
// and the revoke of a token each write one audit line with realm and client
// address, through auth.Service; a GET writes none. A bad password and an
// unknown user write the same line but for the subject: the user name, or
// unknown for a name of no user (finding 7b-2).

// serveBody sends one request with a body through ServeHTTP and gives the
// answer and the audit lines it wrote.
func serveBody(t *testing.T, a *app, tr *auditTrace, method, target string, body []byte, headers ...string) (*httptest.ResponseRecorder, []logs.AuditEvent) {
	t.Helper()
	tr.reset()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.Host = "localhost:" + strconv.Itoa(a.srv.port)
	r.RemoteAddr = "198.51.100.23:51000"
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	a.srv.ServeHTTP(w, r)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return w, slices.Clone(tr.lines)
}

func oneLine(t *testing.T, what string, w *httptest.ResponseRecorder, lines []logs.AuditEvent, wantStatus int) logs.AuditEvent {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("%s: status %d, want %d: %s", what, w.Code, wantStatus, w.Body)
	}
	if len(lines) != 1 {
		t.Fatalf("%s: %d audit lines, want 1: %+v", what, len(lines), lines)
	}
	if lines[0].Client != "198.51.100.23" {
		t.Errorf("%s: client %q", what, lines[0].Client)
	}
	return lines[0]
}

func wantLine(t *testing.T, what string, got logs.AuditEvent, action, subject, realm, result string) {
	t.Helper()
	if got.Action != action || got.Subject != subject || got.Realm != realm || got.Result != result {
		t.Errorf("%s: line %+v, want action %s subject %s realm %q result %s", what, got, action, subject, realm, result)
	}
}

func TestAuditSignInSignOutAndTokens(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	tr := traced(a.app)
	signIn := func(user, pass string) []byte {
		body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		return body
	}
	const jsonHeader = "application/json"

	w, lines := serveBody(t, a.app, tr, "POST", "/api/session", signIn(npmUser, npmPass), "Content-Type", jsonHeader)
	good := oneLine(t, "good sign in", w, lines, http.StatusOK)
	wantLine(t, "good sign in", good, "session.sign_in", npmUser, auth.RealmLocal, "ok")
	var session struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil || session.CSRFToken == "" {
		t.Fatalf("session answer %s: %v", w.Body, err)
	}
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.CookieName {
			cookie = c.Value
		}
	}

	w, lines = serveBody(t, a.app, tr, "POST", "/api/session", signIn(npmUser, "demo-wrong-pass"), "Content-Type", jsonHeader)
	bad := oneLine(t, "bad password", w, lines, http.StatusUnauthorized)
	wantLine(t, "bad password", bad, "session.sign_in", npmUser, "", "denied")
	w, lines = serveBody(t, a.app, tr, "POST", "/api/session", signIn("demo-nobody-known", "demo-wrong-pass"), "Content-Type", jsonHeader)
	unknown := oneLine(t, "unknown user", w, lines, http.StatusUnauthorized)
	wantLine(t, "unknown user", unknown, "session.sign_in", audit.Unknown, "", "denied") // finding 7b-2
	bad.Subject, bad.RequestID, unknown.Subject, unknown.RequestID = "", "", "", ""
	if !equalLines(bad, unknown) {
		t.Errorf("bad password %+v and unknown user %+v differ besides the user name", bad, unknown)
	}

	w, lines = serveBody(t, a.app, tr, "GET", "/api/session", nil, "Cookie", auth.CookieName+"="+cookie)
	if w.Code != http.StatusOK || len(lines) != 0 {
		t.Errorf("GET /api/session: status %d, %d audit lines, want 0", w.Code, len(lines))
	}

	w, lines = serveBody(t, a.app, tr, "DELETE", "/api/session", nil, "Cookie", auth.CookieName+"="+cookie,
		auth.CSRFHeader, session.CSRFToken)
	out := oneLine(t, "sign out", w, lines, http.StatusNoContent)
	wantLine(t, "sign out", out, "session.sign_out", npmUser, auth.RealmLocal, "ok")

	w, lines = serveBody(t, a.app, tr, "PUT", "/repository/demo-npm/-/user/org.couchdb.user:"+npmUser, loginBody(npmUser, npmPass),
		"Content-Type", jsonHeader)
	issue := oneLine(t, "token issue", w, lines, http.StatusCreated)
	wantLine(t, "token issue", issue, "token.issue", npmUser, auth.RealmLocal, "ok")
	var issued struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil || issued.Token == "" {
		t.Fatalf("login answer %s: %v", w.Body, err)
	}
	if issue.ObjectType != "repository" || issue.ObjectName != "demo-npm" {
		t.Errorf("token issue object %s %s", issue.ObjectType, issue.ObjectName)
	}

	w, lines = serveBody(t, a.app, tr, "PUT", "/repository/demo-npm/-/user/org.couchdb.user:"+npmUser, loginBody(npmUser, "demo-wrong-pass"),
		"Content-Type", jsonHeader)
	refused := oneLine(t, "token refused", w, lines, http.StatusUnauthorized)
	wantLine(t, "token refused", refused, "token.issue", npmUser, "", "denied")

	w, lines = serveBody(t, a.app, tr, "GET", "/repository/demo-npm/-/whoami", nil, "Authorization", "Bearer "+issued.Token)
	if w.Code != http.StatusOK || len(lines) != 0 {
		t.Errorf("whoami: status %d, %d audit lines, want 0", w.Code, len(lines))
	}

	w, lines = serveBody(t, a.app, tr, "DELETE", "/repository/demo-npm/-/user/token/"+issued.Token, nil,
		"Authorization", "Bearer "+issued.Token)
	revoke := oneLine(t, "token revoke", w, lines, http.StatusOK)
	wantLine(t, "token revoke", revoke, "token.revoke", npmUser, auth.RealmLocal, "ok") // the realm of the subject of the token

	for _, line := range []logs.AuditEvent{good, out, issue, refused, revoke} {
		text, _ := json.Marshal(line)
		for _, secret := range []string{npmPass, "demo-wrong-pass", issued.Token, strings.Split(issued.Token, ".")[2]} {
			if strings.Contains(string(text), secret) {
				t.Errorf("audit line %s holds a secret", text)
			}
		}
	}
}

func equalLines(a, b logs.AuditEvent) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
