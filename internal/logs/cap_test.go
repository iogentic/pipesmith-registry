package logs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A client sends 100 requests with a user agent of 16 KiB of control bytes
// within one flush, then another client sends one request (finding 7a-3, the
// case of the reader). The user agent is cut at 1 KiB, so no line is
// dropped and the line of the other client is in requests.log.
func TestLargeHeadersDropNoOtherLine(t *testing.T) {
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Settings: DefaultSettings()})
	ua := strings.Repeat("\x01", 16<<10)
	for i := 0; i < 100; i++ {
		f.Request(Request{ID: "0000000000000001", Method: "GET", Path: "/flood", Status: 404, UserAgent: ua})
	}
	f.Request(Request{ID: "00000000000000ff", Method: "GET", Path: "/victim", Status: 200})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if f.Errors() != 0 {
		t.Fatalf("%d lines dropped", f.Errors())
	}
	if paths := requestPaths(t, dir); paths["/flood"] != 100 || paths["/victim"] != 1 {
		t.Fatalf("lines %v", paths)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests-classic.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		if len(line) > 8<<10 {
			t.Fatalf("a classic line of %d bytes", len(line))
		}
	}
}

// Every free text field of a request line is cut at its cap, at a character
// boundary, and ends with the mark; path and query share one cap. The JSON
// line and the classic line both get the cut values.
func TestRequestFieldsAreCapped(t *testing.T) {
	long := func(unit string, n int) string { return strings.Repeat(unit, n) }
	r := Request{
		ID: long("a", 300), Client: long("c", 300), Subject: long("s", 300), Realm: long("r", 300),
		Method: long("M", 300), Path: "/" + long("p", 8000), Query: long("q", 1000), Protocol: long("P", 300),
		ContentLength: long("9", 300), UserAgent: long("ä", 1000), Repository: long("d", 300), Format: long("f", 300),
	}
	var got struct {
		RequestID                                             string `json:"request_id"`
		Client, Subject, Realm, Method, Path, Query, Protocol string
		UserAgent                                             string `json:"user_agent"`
		Repository, Format                                    string
	}
	line := r.line(r.Time)
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		value string
		limit int
	}{
		"id": {got.RequestID, smallCap}, "client": {got.Client, smallCap}, "subject": {got.Subject, smallCap},
		"realm": {got.Realm, smallCap}, "method": {got.Method, smallCap}, "protocol": {got.Protocol, smallCap},
		"repository": {got.Repository, smallCap}, "format": {got.Format, smallCap},
		"user agent": {got.UserAgent, agentCap}, "path and query": {got.Path + "?" + got.Query, targetCap + 1},
	} {
		if len(c.value) > c.limit || !strings.HasSuffix(c.value, cutMark) || !utf8.ValidString(c.value) {
			t.Errorf("%s: %d bytes, mark %v, valid %v", name, len(c.value), strings.HasSuffix(c.value, cutMark), utf8.ValidString(c.value))
		}
	}
	if !strings.HasPrefix(got.Path, "/ppp") || len(got.Path) != 8001 || len(got.Query) != targetCap-len(got.Path) {
		t.Errorf("path of %d bytes, query of %d", len(got.Path), len(got.Query))
	}
	if len(line) > 24<<10 {
		t.Errorf("a JSON line of %d bytes", len(line))
	}
	classic := string(r.classicLine(r.Time))
	if !strings.Contains(classic, cutMark) || len(classic) > 24<<10 {
		t.Errorf("classic line of %d bytes", len(classic))
	}
	short := Request{Path: "/demo", Query: "a=1", UserAgent: "demo-agent/1.0", Method: "GET"}
	if short.capped() != short {
		t.Errorf("a short request changed: %+v", short.capped())
	}
}
