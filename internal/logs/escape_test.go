package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Escape writes a text as a field of the classic line is written, for a line
// of text elsewhere: the escape of every byte equals that of appendClassic,
// an empty text stays empty, and an address comes back as it is.
func TestEscape(t *testing.T) {
	for i := range 256 {
		field := string([]byte{byte(i)})
		if got, want := Escape(field), string(appendClassic(nil, field, false)); got != want {
			t.Errorf("byte %#02x: %q, want %q", i, got, want)
		}
	}
	if got := Escape(""); got != "" {
		t.Errorf("an empty text: %q", got)
	}
	for _, address := range []string{"203.0.113.9:0", "2001:db8::1:443", "127.0.0.1:40000", "192.0.2.7"} {
		if got := Escape(address); got != address {
			t.Errorf("%q: %q", address, got)
		}
	}
	if got, want := Escape("203.0.113.9\n\"x\"\\ \x85"), `203.0.113.9\x0a\x22x\x22\x5c\x20\x85`; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

// The classic line of a request whose client a hostile X-Forwarded-For named
// stays one line: requests-classic.log holds printable ASCII and one line
// break per request, so no line boundary of Python (forwarded_test.go of
// internal/server) splits it, and the parser of classic_test.go reads every
// line back with the client as it came. The clients are what the server
// makes of the header: latin-1 text, so the byte 0x85 is U+0085.
func TestClassicLineOfAForwardedClient(t *testing.T) {
	const forged = `198.51.100.1 - demo-admin [19/Nov/2026:10:00:00 +0000] "GET /forged HTTP/1.1" 200 - 0 0 "-" [0000000000000000]`
	clients := []string{"203.0.113.9"}
	for _, sep := range []string{"\n", "\r", "\v", "\f", "\x1b[2K\x1b[1G", "\x00", "\t", "\u0085", "\u00e2\u0080\u00a8", "\u2028"} {
		clients = append(clients, "203.0.113.9"+sep+forged)
	}
	if _, err := parseClassic(forged); err != nil {
		t.Fatalf("the forged line does not parse, so it proves nothing: %v", err)
	}
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	for i, client := range clients {
		r := demoRequest("/api/health")
		r.Client, r.ID = client, fmt.Sprintf("%016x", i)
		f.Request(r)
	}
	if err := f.Close(); err != nil || f.Errors() != 0 {
		t.Fatalf("close %v, %d errors", err, f.Errors())
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests-classic.log"))
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range data {
		if (c < 0x20 || c > 0x7e) && c != '\n' {
			t.Fatalf("byte %#02x at %d of requests-classic.log", c, i)
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != len(clients) {
		t.Fatalf("%d lines for %d requests: %q", len(lines), len(clients), lines)
	}
	for i, line := range lines {
		rec, err := parseClassic(line)
		if err != nil {
			t.Fatalf("line %d: %v: %q", i, err, line)
		}
		if rec.client != clients[i] || rec.target != "/api/health?a=1" {
			t.Errorf("line %d: client %q target %q, want %q", i, rec.client, rec.target, clients[i])
		}
	}
	if !strings.HasPrefix(lines[0], "203.0.113.9 - demo-reader [") {
		t.Errorf("the plain address: %q", lines[0])
	}
}
