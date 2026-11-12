package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// served is Serve on a free port over the directory store of the gate
// cases: repository demo-raw with a/100%zz.txt and a/hello.txt.
type served struct {
	addr   string
	access *syncBuffer
	cancel context.CancelFunc
	done   chan error
}

func serveFiles(t *testing.T) *served {
	t.Helper()
	base := t.TempDir()
	store := treeStore(t, base, "tree", map[string][]byte{
		"demo-raw/a/100%zz.txt": []byte("percent"),
		"demo-raw/a/hello.txt":  []byte("hello world"),
	})
	cfg := prepare(t, base, []config.StoreConfig{store}, appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	cfg.Port = 0
	s := &served{access: &syncBuffer{}, done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	ready := make(chan []net.Addr, 1)
	go func() {
		s.done <- Serve(ctx, cfg, ServeOptions{AccessLog: s.access, Ready: func(addrs []net.Addr) { ready <- addrs }})
	}()
	select {
	case addrs := <-ready:
		s.addr = addrs[0].String()
	case err := <-s.done:
		t.Fatalf("serve ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	return s
}

// stop ends Serve and returns how long that took.
func (s *served) stop(t *testing.T) time.Duration {
	t.Helper()
	started := time.Now()
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	s.done <- nil // for the cleanup
	return time.Since(started)
}

// link is one client connection that sends bytes as given.
type link struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func dial(t *testing.T, addr string) *link {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &link{t: t, c: c, r: bufio.NewReader(c)}
}

func (l *link) send(text string) {
	l.t.Helper()
	if _, err := io.WriteString(l.c, text); err != nil {
		l.t.Fatal(err)
	}
}

// answer reads one answer within wait.
func (l *link) answer(method string, wait time.Duration) *response {
	l.t.Helper()
	l.c.SetReadDeadline(time.Now().Add(wait))
	req, _ := http.NewRequest(method, "http://localhost/", nil)
	answer, err := http.ReadResponse(l.r, req)
	if err != nil {
		l.t.Fatalf("no answer within %v: %v", wait, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		l.t.Fatal(err)
	}
	return &response{status: answer.StatusCode, header: answer.Header, body: body}
}

// rest reads until the server closes the connection, at most wait.
func (l *link) rest(wait time.Duration) ([]byte, error) {
	l.c.SetReadDeadline(time.Now().Add(wait))
	return io.ReadAll(l.r)
}

func (s *served) head(target string) string {
	return fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, s.addr)
}

func TestMalformedEscapesReachTheRoutes(t *testing.T) {
	s := serveFiles(t)
	for _, c := range []struct {
		target string
		status int
		body   string
	}{
		{"/repository/demo-raw/a/100%zz.txt", 200, "percent"},
		{"/repository/demo-raw/a/100%25zz.txt", 200, "percent"},
		{"/repository/demo-raw/a/hello.txt%", 404, "Not found\n"},
		{"/repository/demo-raw/a/hello.txt%4", 404, "Not found\n"},
		{"/repository/demo-raw/a/%zz/../hello.txt", 400, "Bad request path: empty, '.' or '..' segment\n"},
		{"/api/health%", 404, `{"detail":"Not Found"}`},
		{"/api/health?x=%zz", 200, `{"status":"ok"}`},
	} {
		r := raw(t, s.addr, "GET", c.target)
		if r.status != c.status || string(r.body) != c.body {
			t.Errorf("%s: %s", c.target, r)
		}
	}
	for _, line := range []string{
		`"GET /repository/demo-raw/a/100%25zz.txt HTTP/1.1" 200 OK`,
		`"GET /repository/demo-raw/a/hello.txt%25 HTTP/1.1" 404 Not Found`,
		`"GET /api/health%25 HTTP/1.1" 404 Not Found`,
	} {
		if !strings.Contains(s.access.String(), line) {
			t.Errorf("no access line %s in %q", line, s.access.String())
		}
	}
	// keep alive: requests one after the other on one connection, and a
	// body that looks like a request line passes unchanged
	l := dial(t, s.addr)
	l.send(s.head("/repository/demo-raw/a/100%zz.txt") + s.head("/repository/demo-raw/a/hello.txt%"))
	if r := l.answer("GET", 5*time.Second); r.status != 200 || string(r.body) != "percent" {
		t.Fatalf("first: %s", r)
	}
	if r := l.answer("GET", 5*time.Second); r.status != 404 {
		t.Fatalf("second: %s", r)
	}
	body := "GET /a%zz HTTP/1.1\r\n"
	l.send(fmt.Sprintf("PUT /repository/demo-raw/a/x HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n%s", s.addr, len(body), body))
	if r := l.answer("PUT", 5*time.Second); r.status != 405 {
		t.Fatalf("put: %s", r)
	}
	l.send(s.head("/repository/demo-raw/a/100%25zz.txt"))
	if r := l.answer("GET", 5*time.Second); r.status != 200 || string(r.body) != "percent" {
		t.Fatalf("after the body: %s", r)
	}
}

func TestAbsoluteTargetIsJudgedByTheHostHeader(t *testing.T) {
	s := serveFiles(t)
	r := raw(t, s.addr, "GET", "http://localhost/api/health", "Host", "evil.example")
	if r.status != 403 || string(r.body) != forbidden+"\n" {
		t.Fatalf("%s", r)
	}
	r = raw(t, s.addr, "GET", "http://evil.example/api/health")
	if r.status != 404 || string(r.body) != `{"detail":"Not Found"}` {
		t.Fatalf("%s", r)
	}
	r = raw(t, s.addr, "GET", "*")
	if r.status != 404 || string(r.body) != `{"detail":"Not Found"}` {
		t.Fatalf("%s", r)
	}
	for _, line := range []string{
		`"GET http%3A//localhost/api/health HTTP/1.1" 403 Forbidden`,
		`"GET http%3A//evil.example/api/health HTTP/1.1" 404 Not Found`,
		`"GET %2A HTTP/1.1" 404 Not Found`,
	} {
		if !strings.Contains(s.access.String(), line) {
			t.Errorf("no access line %s in %q", line, s.access.String())
		}
	}
}

func TestHTTP10IsAnsweredAsHTTP11AndClosed(t *testing.T) {
	s := serveFiles(t)
	for _, c := range []struct {
		head, status, body string
	}{
		{fmt.Sprintf("GET /api/health HTTP/1.0\r\nHost: %s\r\n\r\n", s.addr), "HTTP/1.1 200 OK", `{"status":"ok"}`},
		{"GET /api/health HTTP/1.0\r\n\r\n", "HTTP/1.1 403 Forbidden", forbidden + "\n"},
		{fmt.Sprintf("GET /api/health HTTP/1.0\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", s.addr), "HTTP/1.1 200 OK", `{"status":"ok"}`},
	} {
		l := dial(t, s.addr)
		l.send(c.head)
		data, err := l.rest(5 * time.Second)
		if err != nil {
			t.Fatalf("%q: %v after %q", c.head, err, data)
		}
		head, body, _ := strings.Cut(string(data), "\r\n\r\n")
		lines := strings.Split(head, "\r\n")
		if lines[0] != c.status || body != c.body || !containsLine(lines[1:], "Connection: close") {
			t.Errorf("%q: %q", c.head, data)
		}
	}
	if !strings.Contains(s.access.String(), `"GET /api/health HTTP/1.0" 200 OK`) {
		t.Errorf("access %q", s.access.String())
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestWriteWhoseBodyNeverArrives(t *testing.T) {
	t.Parallel()
	s := serveFiles(t)
	for _, extra := range []string{"", "Expect: 100-continue\r\n"} {
		l := dial(t, s.addr)
		l.send(fmt.Sprintf("PUT /repository/demo-raw/a/x HTTP/1.1\r\nHost: %s\r\nContent-Length: 5\r\n%s\r\n", s.addr, extra))
		r := l.answer("PUT", 2*time.Second)
		if r.status != 405 || string(r.body) != "Method not allowed: this server is read only\n" {
			t.Fatalf("%q: %s", extra, r)
		}
		started := time.Now()
		data, err := l.rest(9 * time.Second)
		took := time.Since(started)
		if err != nil || len(data) != 0 || took < 4*time.Second || took > 7*time.Second {
			t.Fatalf("%q: closed after %v: %q %v", extra, took, data, err)
		}
	}
}

func TestBodyThatArrivesLateKeepsTheConnection(t *testing.T) {
	t.Parallel()
	s := serveFiles(t)
	l := dial(t, s.addr)
	l.send(fmt.Sprintf("PUT /repository/demo-raw/a/x HTTP/1.1\r\nHost: %s\r\nContent-Length: 5\r\n\r\n", s.addr))
	if r := l.answer("PUT", 2*time.Second); r.status != 405 {
		t.Fatalf("%s", r)
	}
	time.Sleep(time.Second)
	l.send("12345" + s.head("/api/health"))
	if r := l.answer("GET", 5*time.Second); r.status != 200 || string(r.body) != `{"status":"ok"}` {
		t.Fatalf("%s", r)
	}
}

func TestStopDoesNotWaitForOpenConnections(t *testing.T) {
	t.Parallel()
	s := serveFiles(t)
	body := dial(t, s.addr)
	body.send(fmt.Sprintf("PUT /repository/demo-raw/a/x HTTP/1.1\r\nHost: %s\r\nContent-Length: 5\r\n\r\n", s.addr))
	partial := dial(t, s.addr)
	partial.send("GET /api/health HTTP/1.1\r\nHost: ")
	idle := dial(t, s.addr)
	idle.send(s.head("/api/health"))
	if r := idle.answer("GET", 5*time.Second); r.status != 200 {
		t.Fatalf("%s", r)
	}
	time.Sleep(200 * time.Millisecond)
	if took := s.stop(t); took > 2*time.Second {
		t.Fatalf("stop took %v", took)
	}
	for _, l := range []*link{body, partial, idle} {
		if _, err := l.rest(3 * time.Second); err != nil && !errors.Is(err, io.EOF) && !isReset(err) {
			t.Fatalf("connection still open: %v", err)
		}
	}
}

func isReset(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection reset")
}

// TestHostHeaderNetHTTPRefuses: a Host header net/http calls malformed
// reaches the host guard, which answers as the Python server did on
// 2026-11-12; a value that str.strip turns into a local name is served.
func TestHostHeaderNetHTTPRefuses(t *testing.T) {
	s := serveFiles(t)
	port := s.addr[strings.LastIndex(s.addr, ":")+1:]
	for _, host := range []string{"user@localhost:" + port, "localhost:" + port + "/x", "local host", "a/b", `a\b`, "x<y",
		`"q"`, "{a}", "a|b", "a^b", "a`b", "h\xe9", "a#b", "a?b", "a\tb", "localhost\x7f", "localhost:" + port + "\x01"} {
		r := raw(t, s.addr, "GET", "/api/health", "Host", host)
		if r.status != 403 || string(r.body) != forbidden+"\n" || r.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("Host %q: %s", host, r)
		}
	}
	for _, host := range []string{"localhost:" + port + "\x85", "\xa0localhost", "\x1clocalhost:" + port} {
		r := raw(t, s.addr, "GET", "/api/health", "Host", host)
		if r.status != 200 || string(r.body) != `{"status":"ok"}` {
			t.Errorf("Host %q: %s", host, r)
		}
	}
	r := raw(t, s.addr, "GET", "/repository/demo-raw/a/hello.txt", "Host", "user@localhost:"+port)
	if r.status != 403 {
		t.Errorf("file route: %s", r)
	}
	// Starlette's URL takes no Host with such a byte and names the server
	for _, host := range []string{"\xa0localhost:" + port, "[::1]:" + port + "\x85", "\x1cLOCALHOST"} {
		r := raw(t, s.addr, "GET", "/api/health/", "Host", host)
		if r.status != 307 || r.header.Get("Location") != "http://"+s.addr+"/api/health" {
			t.Errorf("Host %q: %s", host, r)
		}
	}
}

// TestControlCharactersInHeaderValues: h11 takes a header value with a
// control character other than NUL, CR, LF, VT and FF, net/http does not.
// Such values reach the routes unchanged.
func TestControlCharactersInHeaderValues(t *testing.T) {
	s := serveFiles(t)
	port := s.addr[strings.LastIndex(s.addr, ":")+1:]
	host := "localhost:" + port
	post := func(headers ...string) *response {
		t.Helper()
		l := dial(t, s.addr)
		var b strings.Builder
		b.WriteString("POST /api/scan HTTP/1.1\r\nHost: " + host + "\r\n")
		for i := 0; i+1 < len(headers); i += 2 {
			b.WriteString(headers[i] + ": " + headers[i+1] + "\r\n")
		}
		b.WriteString("Content-Length: 16\r\nConnection: close\r\n\r\n{\"store\":\"nope\"}")
		l.send(b.String())
		return l.answer("POST", 5*time.Second)
	}
	for _, c := range []struct {
		headers []string
		body    string
	}{
		{[]string{"X-Artifact-Repository", "1", "Origin", "it's \"x\" \\ \xe9\x7f"},
			`{"error":"the Origin 'it\\'s \"x\" \\\\ é\\x7f' is not this server"}`},
		{[]string{"X-Artifact-Repository", "1", "Origin", "http://" + host + "\x01"},
			`{"error":"the Origin 'http://` + host + `\\x01' is not this server"}`},
		{[]string{"X-Artifact-Repository", "1", "Origin", "\x1fhttp://" + host},
			`{"error":"the Origin '\\x1fhttp://` + host + `' is not this server"}`},
		{[]string{"X-Artifact-Repository", "1\x01"}, `{"error":"the header X-Artifact-Repository: 1 is missing"}`},
	} {
		r := post(c.headers...)
		if r.status != 403 || string(r.body) != c.body {
			t.Errorf("%q: %s", c.headers, r)
		}
	}
	if r := post("X-Artifact-Repository", "1", "Origin", "http://"+host); r.status != 404 {
		t.Errorf("own origin: %s", r)
	}
	r := raw(t, s.addr, "GET", "/repository/demo-raw/a/hello.txt", "Range", "bytes=0-1\x01", "X-Junk", "a\x01b")
	if r.status != 200 || string(r.body) != "hello world" {
		t.Errorf("range with a control character: %s", r)
	}
}
