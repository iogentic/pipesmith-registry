package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/auth"
	"github.com/mirosso25/pipesmith-registry/internal/ldapauth"
	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// The LDAP connection of the fixture export and the variable of its bind
// password. The password below is invented for these tests.
const (
	fixtureLDAPID   = "6f1c2a52-0000-4000-8000-000000000001"
	fixtureLDAPPass = "example-bind-secret-7"
)

// importedFixture writes state.sqlite of the identity fixture into a fresh
// data directory.
func importedFixture(t *testing.T) string {
	t.Helper()
	dir := dataDir(t)
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), "")
	if code, _, errOut := run(t, "import", fixtureExport); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	return dir
}

func logTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(fieldHandler{newLogHandler(buf, false)})
}

func basicRequest(user, pass string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://localhost/service/rest/v1/repositories", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.SetBasicAuth(user, pass)
	return r
}

func TestSecurityOffWithoutState(t *testing.T) {
	dir := dataDir(t)
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, logTo(&log))
	if err != nil || sec != nil {
		t.Fatalf("%v %v", sec, err)
	}
	if log.Len() != 0 {
		t.Fatalf("log %q", log.String())
	}
}

func TestSecurityFromFixtureState(t *testing.T) {
	dir := importedFixture(t)
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, logTo(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	subject, err := sec.Subject(basicRequest("admin", "example-admin-1"))
	if err != nil || subject == nil || subject.User.ID != "admin" || !sec.Permits(subject, "nexus:users:read") {
		t.Fatalf("admin: %v %v", subject, err)
	}
	if _, err := sec.Subject(basicRequest("admin", "example-admin-2")); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("wrong password: %v", err)
	}
	viewer, err := sec.Subject(basicRequest("viewer", "example-viewer-1"))
	if err != nil || viewer == nil || sec.Permits(viewer, "nexus:users:read") {
		t.Fatalf("viewer: %v %v", viewer, err)
	}
	if sec.AnonymousEnabled() {
		t.Fatal("anonymous access is off in the fixture")
	}
	// The connection of the fixture has no bind password here: it is left
	// out with a warning that names the variable, and the server still starts.
	text := log.String()
	if !strings.Contains(text, "LDAP connection demo-directory left out") ||
		!strings.Contains(text, ldapauth.PasswordVariable(fixtureLDAPID)) {
		t.Fatalf("log %q", text)
	}
}

func TestSecurityLDAPPasswordFromEnvironment(t *testing.T) {
	dir := importedFixture(t)
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), fixtureLDAPPass)
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, logTo(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	text := log.String()
	if strings.Contains(text, "left out") {
		t.Fatalf("log %q", text)
	}
	// protocol ldap with the simple scheme: the bind password crosses the
	// network in clear, and the start says so.
	if !strings.Contains(text, "LDAP connection demo-directory sends its bind password in clear") {
		t.Fatalf("log %q", text)
	}
	if strings.Contains(text, fixtureLDAPPass) {
		t.Fatal("the bind password is in the log")
	}
}

func TestSecurityLDAPPasswordAsked(t *testing.T) {
	dir := importedFixture(t)
	var asked []string
	ask := func(prompt string) (string, error) {
		asked = append(asked, prompt)
		return fixtureLDAPPass, nil
	}
	var log bytes.Buffer
	sec, err := buildSecurity(dir, ask, logTo(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	if len(asked) != 1 || asked[0] != "bind password of LDAP connection demo-directory: " {
		t.Fatalf("asked %q", asked)
	}
	if strings.Contains(log.String(), "left out") || strings.Contains(log.String(), fixtureLDAPPass) {
		t.Fatalf("log %q", log.String())
	}
	// A value in the environment is taken without asking.
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), fixtureLDAPPass)
	asked = nil
	if _, err := buildSecurity(dir, ask, logTo(&log)); err != nil || len(asked) != 0 {
		t.Fatalf("%v asked %q", err, asked)
	}
	// A failed prompt stops the start.
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), "")
	failing := func(string) (string, error) { return "", errors.New("no terminal") }
	if _, err := buildSecurity(dir, failing, logTo(&log)); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("%v", err)
	}
}

type fakeLogin struct{ err error }

func (f fakeLogin) Login(context.Context, string, string) (model.User, []string, error) {
	return model.User{}, nil, f.err
}

func TestDirectoryErrorsMapped(t *testing.T) {
	for _, c := range []struct {
		in   error
		want error
	}{
		{ldapauth.ErrInvalidCredentials, auth.ErrWrongPair},
		{fmt.Errorf("bind: %w", ldapauth.ErrUnavailable), auth.ErrUnreachable},
		{errors.Join(fmt.Errorf("one: %w", ldapauth.ErrUnavailable), fmt.Errorf("two: %w", ldapauth.ErrUnavailable)), auth.ErrUnreachable},
	} {
		_, _, err := directory{fakeLogin{c.in}}.Login(context.Background(), "someone", "pw")
		if !errors.Is(err, c.want) {
			t.Errorf("%v gives %v, want %v", c.in, err, c.want)
		}
	}
	if _, _, err := (directory{fakeLogin{nil}}).Login(context.Background(), "someone", "pw"); err != nil {
		t.Fatal(err)
	}
}

func TestFieldHandlerKeepsValues(t *testing.T) {
	var buf bytes.Buffer
	logTo(&buf).Warn("stored privileges that grant nothing", "type", "script", "count", 2)
	if got := buf.String(); got != "stored privileges that grant nothing: type=script count=2\n" {
		t.Fatalf("%q", got)
	}
}

// serveFixture starts serve on the fixture state and returns its base URL
// and a stop function that returns the log lines.
func serveFixture(t *testing.T) (string, func() []string) {
	t.Helper()
	importedFixture(t)
	port := freePort(t)
	r, w, _ := os.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"serve", "--port", fmt.Sprint(port)}, os.Stdout, w)
		w.Close()
	}()
	lines := make(chan string, 1000)
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(base + "/api/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return base, func() []string {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
		case <-time.After(7 * time.Second):
			t.Fatal("serve did not stop")
		}
		var log []string
		for line := range lines {
			log = append(log, line)
		}
		return log
	}
}

func call(t *testing.T, method, url string, body io.Reader, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestServeWithFixtureState(t *testing.T) {
	base, stop := serveFixture(t)

	// Without a session the interface page loads and the reads refuse.
	resp, page := call(t, "GET", base+"/", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") ||
		!bytes.Contains(page, []byte("<script")) {
		t.Fatalf("page: %d %q", resp.StatusCode, resp.Header)
	}
	if resp, _ := call(t, "GET", base+"/api/repositories", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("repositories without session: %d", resp.StatusCode)
	}

	// A wrong pair and a good pair.
	login := func(user, pass string) *http.Response {
		resp, _ := call(t, "POST", base+"/api/session",
			strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)),
			map[string]string{"Content-Type": "application/json"})
		return resp
	}
	if resp := login("admin", "example-admin-2"); resp.StatusCode != 401 {
		t.Fatalf("wrong pair: %d", resp.StatusCode)
	}
	resp = login("admin", "example-admin-1")
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			cookie = c
		}
	}
	if resp.StatusCode != 200 || cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("sign in: %d %v", resp.StatusCode, cookie)
	}
	withCookie := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}

	resp, data := call(t, "GET", base+"/api/session", nil, withCookie)
	var session struct {
		User        struct{ UserID string } `json:"user"`
		Permissions []string                `json:"permissions"`
		CSRFToken   string                  `json:"csrfToken"`
	}
	if err := json.Unmarshal(data, &session); err != nil || resp.StatusCode != 200 || session.User.UserID != "admin" ||
		session.CSRFToken == "" || len(session.Permissions) == 0 {
		t.Fatalf("session: %d %s", resp.StatusCode, data)
	}
	if resp, _ := call(t, "GET", base+"/service/rest/v1/security/users", nil, withCookie); resp.StatusCode != 200 {
		t.Fatalf("users as admin: %d", resp.StatusCode)
	}

	// Basic for a client, and the rights of a reader.
	viewer := map[string]string{"Authorization": "Basic " + basicValue("viewer", "example-viewer-1")}
	if resp, _ := call(t, "GET", base+"/service/rest/v1/security/users", nil, viewer); resp.StatusCode != 403 {
		t.Fatalf("users as viewer: %d", resp.StatusCode)
	}

	// Sign out needs the request token, then the cookie is dead.
	if resp, _ := call(t, "DELETE", base+"/api/session", nil, withCookie); resp.StatusCode != 403 {
		t.Fatalf("sign out without token: %d", resp.StatusCode)
	}
	withToken := map[string]string{"Cookie": withCookie["Cookie"], auth.CSRFHeader: session.CSRFToken}
	if resp, _ := call(t, "DELETE", base+"/api/session", nil, withToken); resp.StatusCode/100 != 2 {
		t.Fatalf("sign out: %d", resp.StatusCode)
	}
	if resp, _ := call(t, "GET", base+"/api/repositories", nil, withCookie); resp.StatusCode != 401 {
		t.Fatalf("after sign out: %d", resp.StatusCode)
	}

	log := stop()
	joined := strings.Join(log, "\n")
	summary, listening := -1, -1
	for i, line := range log {
		if strings.Contains(line, "INFO pipesmith_registry.security: security on: 4 local users, 0 of 1 LDAP connections, anonymous access off") {
			summary = i
		}
		if listening < 0 && strings.Contains(line, "listening on") {
			listening = i
		}
	}
	if summary < 0 || listening < summary {
		t.Fatalf("log %q", log)
	}
	for _, secret := range []string{"example-admin-1", "example-admin-2", "example-viewer-1", "$shiro1$", cookie.Value, session.CSRFToken} {
		if strings.Contains(joined, secret) {
			t.Fatalf("the log holds %q", secret)
		}
	}
}

func basicValue(user, pass string) string {
	r := basicRequest(user, pass)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}
