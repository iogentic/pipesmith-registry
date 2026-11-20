package server

import (
	"encoding/json"
	"errors"
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

// Findings 7b-2, 7b-4 and 7b-5 of review 7b.

// typedSecret is a password typed into the user name field.
const typedSecret = "demo-typed-secret-pass-9"

// holdsText reports whether a line or a request record holds text.
func holdsText(v any, text string) bool {
	data, _ := json.Marshal(v)
	return strings.Contains(string(data), text)
}

// Finding 7b-2: a failed sign in names the user only when the typed name
// is a user of this server; else the subject is unknown. The answer is the
// same for both.
func TestAuditTypedPasswordNotLogged(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	tr := traced(a.app)
	signIn := func(user string) []byte {
		body, _ := json.Marshal(map[string]string{"username": user, "password": "demo-wrong-pass"})
		return body
	}
	w, lines := serveBody(t, a.app, tr, "POST", "/api/session", signIn(typedSecret), "Content-Type", "application/json")
	typed := oneLine(t, "typed password", w, lines, http.StatusUnauthorized)
	wantLine(t, "typed password", typed, "session.sign_in", audit.Unknown, "", "denied")
	typedBody := w.Body.String()
	requests := slices.Clone(tr.requests)

	w, lines = serveBody(t, a.app, tr, "POST", "/api/session", signIn(npmUser), "Content-Type", "application/json")
	known := oneLine(t, "known name", w, lines, http.StatusUnauthorized)
	wantLine(t, "known name", known, "session.sign_in", npmUser, "", "denied")
	if w.Body.String() != typedBody {
		t.Errorf("answers differ: %q and %q", typedBody, w.Body.String())
	}

	w, lines = serveBody(t, a.app, tr, "GET", "/repository/demo-docker/v2/token", nil,
		"Authorization", basicPair(typedSecret, "demo-wrong-pass"))
	docker := oneLine(t, "docker token", w, lines, http.StatusUnauthorized)
	wantLine(t, "docker token", docker, "token.issue", audit.Unknown, "", "denied")
	requests = append(requests, tr.requests...)

	w, lines = serveBody(t, a.app, tr, "PUT", "/repository/demo-npm/-/user/org.couchdb.user:"+typedSecret,
		loginBody(typedSecret, "demo-wrong-pass"), "Content-Type", "application/json")
	npm := oneLine(t, "npm login", w, lines, http.StatusUnauthorized)
	wantLine(t, "npm login", npm, "token.issue", audit.Unknown, "", "denied")

	for _, line := range []logs.AuditEvent{typed, known, docker, npm} {
		if holdsText(line, typedSecret) {
			t.Errorf("audit line %+v holds the typed password", line)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("%d request records", len(requests))
	}
	for _, r := range requests {
		if holdsText(r, typedSecret) {
			t.Errorf("request record %+v holds the typed password", r)
		}
	}
}

// Finding 7b-4: the token route of Docker writes one line per issued token
// and per refused pair, with realm and address and without the token; a
// request without credentials writes none.
func TestAuditDockerTokenLines(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	tr := traced(a.app)
	const target = "/repository/demo-docker/v2/token?service=Pipesmith+Registry"
	w, lines := serveBody(t, a.app, tr, "GET", target, nil, "Authorization", basicPair(pullerUser, pullerPass))
	issued := oneLine(t, "issued", w, lines, http.StatusOK)
	wantLine(t, "issued", issued, "token.issue", pullerUser, auth.RealmLocal, "ok")
	if issued.ObjectType != "repository" || issued.ObjectName != "demo-docker" || issued.RequestID == "" {
		t.Errorf("issued line %+v", issued)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil || tok.Token == "" {
		t.Fatalf("answer %s: %v", w.Body, err)
	}
	parts := strings.Split(tok.Token, ".")
	if holdsText(issued, tok.Token) || holdsText(issued, parts[len(parts)-1]) || holdsText(issued, pullerPass) {
		t.Errorf("issued line %+v holds a secret", issued)
	}

	w, lines = serveBody(t, a.app, tr, "GET", target, nil, "Authorization", basicPair(pullerUser, "demo-wrong-pass"))
	refused := oneLine(t, "refused", w, lines, http.StatusUnauthorized)
	wantLine(t, "refused", refused, "token.issue", pullerUser, "", "denied")

	w, lines = serveBody(t, a.app, tr, "GET", target, nil)
	if len(lines) != 0 {
		t.Errorf("a request without credentials (status %d) wrote %+v", w.Code, lines)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("demo broken body") }

// Finding 7b-5: a scan body that cannot be read gets a line with the result
// of its 400, written before the header.
func TestAuditScanUnreadableBody(t *testing.T) {
	a := startWriting(t, newFakeSecurity(), false)
	tr := traced(a)
	r := httptest.NewRequest("POST", "/api/scan", brokenBody{})
	r.Host = "localhost:" + strconv.Itoa(a.srv.port)
	r.RemoteAddr = "192.0.2.7:40000"
	user, password, _ := strings.Cut(asAdmin, ":")
	r.SetBasicAuth(user, password)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(postHeader, "1")
	w := &orderWriter{ResponseRecorder: httptest.NewRecorder(), trace: tr}
	a.srv.ServeHTTP(w, r)
	tr.mu.Lock()
	lines, events := slices.Clone(tr.lines), slices.Clone(tr.events)
	tr.mu.Unlock()
	if w.Code != http.StatusBadRequest || len(lines) != 1 {
		t.Fatalf("status %d, lines %+v", w.Code, lines)
	}
	if lines[0].Result != audit.Failed || lines[0].Action != "scan.create" {
		t.Errorf("line %+v", lines[0])
	}
	if len(events) < 2 || events[0] != "audit" || events[1] != "header" {
		t.Errorf("order %v", events)
	}
}
