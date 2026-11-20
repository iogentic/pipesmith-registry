package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The line boundaries of str.splitlines of Python, read on 2026-11-19 at
// https://docs.python.org/3/library/stdtypes.html#str.splitlines (served
// from https://docs.python.org/3/builtins/stdtypes.html): \n, \r, \r\n, \v
// (0x0b), \f (0x0c), 0x1c, 0x1d, 0x1e, 0x85, U+2028 and U+2029. Python
// 3.15.0 on this machine split a string at exactly these code points on the
// same day.

// forgedLine is the access line a hostile X-Forwarded-For carries after the
// byte that should start a line of its own.
const forgedLine = `INFO:     198.51.100.1:0 - "GET /forged HTTP/1.1" 200 OK`

// How a connection through the gate carries a hostile value, as observed on
// 2026-11-19.
const (
	connCarried = "carried" // the value reaches the handler as it was sent
	connSplit   = "split"   // the line feed ends the header; the forged text is a header of its own
	connRefused = "refused" // net/http answers 400 before the handler runs
)

// hostileClients are the values of X-Forwarded-For of the fix of finding
// sa-23: an address, a byte that ends a line or starts a sequence of a
// terminal, and a forged access line.
var hostileClients = []struct {
	name, value, conn string
}{
	{"line feed", "203.0.113.9\n" + forgedLine, connSplit},
	{"carriage return", "203.0.113.9\r" + forgedLine, connRefused},
	{"vertical tab", "203.0.113.9\x0b" + forgedLine, connRefused},
	{"form feed", "203.0.113.9\x0c" + forgedLine, connRefused},
	{"escape sequence", "203.0.113.9\x1b[2K\x1b[1G" + forgedLine, connCarried},
	{"nul", "203.0.113.9\x00" + forgedLine, connRefused},
	{"tab", "203.0.113.9\t" + forgedLine, connCarried},
	{"next line 0x85", "203.0.113.9\x85" + forgedLine, connCarried},
	{"line separator e2 80 a8", "203.0.113.9\xe2\x80\xa8" + forgedLine, connCarried},
}

// pyLines splits text where str.splitlines of Python splits it.
func pyLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, text[start:i])
			start = i + size
		case '\r':
			lines = append(lines, text[start:i])
			if strings.HasPrefix(text[i+1:], "\n") {
				size++
			}
			start = i + size
		}
		i += size
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// escapedWant is the rule of the fix, written without the code of the
// server: every byte outside 0x21 to 0x7e, the quote and the backslash as
// \xHH with lower case digits.
func escapedWant(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			fmt.Fprintf(&b, `\x%02x`, c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// forwardApp is the server of jarApp with a second listener behind the gate,
// as Serve opens it, and its request records written to the log files of a
// test directory, as serve writes them.
type forwardApp struct {
	*app
	gated string // the address of the listener behind the gate
	dir   string
	files *logs.Files
	rs    *recordSink
}

func newForwardApp(t *testing.T) *forwardApp {
	t.Helper()
	a, _ := jarApp(t, "jar bytes of the forwarded client test")
	f := &forwardApp{app: a, dir: filepath.Join(t.TempDir(), "logs"), rs: &recordSink{}}
	f.files = logs.Open(logs.Options{Dir: f.dir, Settings: logs.DefaultSettings()})
	t.Cleanup(func() { _ = f.files.Close() })
	a.srv.requests = func(r logs.Request) {
		f.files.Request(r)
		f.rs.add(r)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := newHTTPServer(a.srv)
	go func() { _ = hs.Serve(&gateListener{Listener: l, marker: a.srv.marker, escape: a.srv.escape}) }()
	t.Cleanup(func() { _ = hs.Close() })
	f.gated = l.Addr().String()
	return f
}

// direct hands a request from a loopback peer with the header set to
// ServeHTTP, for the bytes no connection can carry.
func (f *forwardApp) direct(value string) int {
	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Host = "localhost"
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("X-Forwarded-For", value)
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, r)
	return w.Code
}

// read closes the log files and returns the text of one of them.
func (f *forwardApp) read(t *testing.T, name string) string {
	t.Helper()
	if err := f.files.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The client that X-Forwarded-For of a trusted peer names cannot start a
// second line: standard error has one access line per request, with the
// client escaped, at every line boundary of Python; requests.log holds one
// line per request with the client as it came, and requests-classic.log one
// line per request with the client escaped. A plain address stays as it was.
func TestForwardedClientCannotForgeALine(t *testing.T) {
	f := newForwardApp(t)
	var clients []string // the client of each request the handler answered, as it came
	if r := raw(t, f.gated, "GET", "/api/health", "X-Forwarded-For", "203.0.113.9"); r.status != 200 {
		t.Fatalf("plain address: %s", r)
	}
	if rec := f.rs.next(t); rec.Client != "203.0.113.9" {
		t.Fatalf("plain address: client %q", rec.Client)
	}
	clients = append(clients, "203.0.113.9")
	for _, h := range hostileClients {
		r := raw(t, f.gated, "GET", "/api/health", "X-Forwarded-For", h.value)
		switch h.conn {
		case connRefused:
			if r.status != 400 {
				t.Fatalf("%s over a connection: %s, want 400 from net/http", h.name, r)
			}
			continue
		case connSplit:
			clients = append(clients, "203.0.113.9")
		case connCarried:
			clients = append(clients, latin1Text(h.value))
		}
		if r.status != 200 {
			t.Fatalf("%s over a connection: %s", h.name, r)
		}
		if rec := f.rs.next(t); rec.Client != clients[len(clients)-1] {
			t.Fatalf("%s over a connection: client %q, want %q", h.name, rec.Client, clients[len(clients)-1])
		}
	}
	for _, h := range hostileClients {
		if code := f.direct(h.value); code != 200 {
			t.Fatalf("%s set on the request: status %d", h.name, code)
		}
		clients = append(clients, latin1Text(h.value))
		if rec := f.rs.next(t); rec.Client != clients[len(clients)-1] {
			t.Fatalf("%s set on the request: client %q, want %q", h.name, rec.Client, clients[len(clients)-1])
		}
	}
	f.rs.wait(t, len(clients))

	stderr := pyLines(f.access.String())
	if len(stderr) != len(clients) {
		t.Errorf("standard error: %d lines for %d requests: %q", len(stderr), len(clients), stderr)
	}
	for i, line := range stderr {
		if strings.HasPrefix(line, forgedLine) {
			t.Errorf("standard error: line %d is the forged line: %q", i, line)
		}
		if i < len(clients) {
			if want := "INFO:     " + escapedWant(clients[i]+":0") + ` - "GET /api/health HTTP/1.1" 200 OK`; line != want {
				t.Errorf("standard error: line %d\n got %q\nwant %q", i, line, want)
			}
		}
	}
	if want := `INFO:     203.0.113.9:0 - "GET /api/health HTTP/1.1" 200 OK`; len(stderr) == 0 || stderr[0] != want {
		t.Errorf("the plain address: %q, want %q", stderr, want)
	}

	requests := strings.Split(strings.TrimSuffix(f.read(t, "requests.log"), "\n"), "\n")
	if len(requests) != len(clients) {
		t.Fatalf("requests.log: %d lines for %d requests: %q", len(requests), len(clients), requests)
	}
	for i, line := range requests {
		var v struct {
			Client *string `json:"client"`
		}
		if err := json.Unmarshal([]byte(line), &v); err != nil || v.Client == nil {
			t.Fatalf("requests.log: line %d does not parse: %v: %q", i, err, line)
		}
		if *v.Client != clients[i] {
			t.Errorf("requests.log: line %d: client %q, want %q", i, *v.Client, clients[i])
		}
	}

	classic := pyLines(f.read(t, "requests-classic.log"))
	if len(classic) != len(clients) {
		t.Fatalf("requests-classic.log: %d lines for %d requests: %q", len(classic), len(clients), classic)
	}
	for i, line := range classic {
		if strings.HasPrefix(line, forgedLine) || !strings.HasPrefix(line, escapedWant(clients[i])+" - ") {
			t.Errorf("requests-classic.log: line %d: %q, want the client %q escaped", i, line, clients[i])
		}
	}
	if !strings.HasPrefix(classic[0], "203.0.113.9 - ") {
		t.Errorf("requests-classic.log: the plain address: %q", classic[0])
	}
}

// From a peer the server does not trust the header is ignored: the forged
// text is in no stream and no file, and every request names the peer.
func TestUntrustedPeerCannotForgeALine(t *testing.T) {
	t.Setenv("FORWARDED_ALLOW_IPS", "192.0.2.1")
	f := newForwardApp(t)
	handled := 0
	for _, h := range hostileClients {
		r := raw(t, f.gated, "GET", "/api/health", "X-Forwarded-For", h.value)
		if h.conn == connRefused {
			if r.status != 400 {
				t.Fatalf("%s over a connection: %s, want 400 from net/http", h.name, r)
			}
		} else {
			if r.status != 200 {
				t.Fatalf("%s over a connection: %s", h.name, r)
			}
			handled++
			if rec := f.rs.next(t); rec.Client != "127.0.0.1" {
				t.Fatalf("%s over a connection: client %q, want the peer", h.name, rec.Client)
			}
		}
		if code := f.direct(h.value); code != 200 {
			t.Fatalf("%s set on the request: status %d", h.name, code)
		}
		handled++
		if rec := f.rs.next(t); rec.Client != "127.0.0.1" {
			t.Fatalf("%s set on the request: client %q, want the peer", h.name, rec.Client)
		}
	}
	f.rs.wait(t, handled)
	streams := map[string]string{"standard error": f.access.String()}
	if err := f.files.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		streams[e.Name()] = f.read(t, e.Name())
	}
	if len(streams) != 7 {
		t.Fatalf("streams and files %d, want standard error, five files and logs.lock", len(streams))
	}
	for name, text := range streams {
		for _, forged := range []string{"198.51.100.1", "/forged"} {
			if strings.Contains(text, forged) {
				t.Errorf("%s holds %q", name, forged)
			}
		}
	}
	if lines := pyLines(streams["standard error"]); len(lines) != handled || !strings.HasPrefix(lines[0], "INFO:     127.0.0.1:") {
		t.Errorf("standard error: %d lines for %d requests: %q", len(lines), handled, lines)
	}
}
