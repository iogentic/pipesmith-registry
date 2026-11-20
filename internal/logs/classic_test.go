package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The field order and the time layout of requests-classic.log were read on
// 2026-11-19 with curl:
//
//   - https://javadoc.jetty.org/jetty-12/org/eclipse/jetty/server/CustomRequestLog.html:
//     %{format|timeZone|locale}t defaults to the form [18/Sep/2011:19:18:28 -0400]
//     in GMT; %{CLF}O is the size of the response without its headers; %{ms}T is
//     the time taken in milliseconds.
//   - https://jetty.org/docs/jetty/12/operations-guide/modules/standard.html,
//     module requestlog: %{client}a - %u %{dd/MMM/yyyy:HH:mm:ss ZZZ|GMT}t "%r" %s %O ...
//   - https://logback.qos.ch/manual/layouts.html, the access pattern layout: a date
//     without a pattern is %t{dd/MMM/yyyy:HH:mm:ss Z} of the common log format.
//   - the public help page on logging of the old product: its request.log holds
//     date and time, client address, user id, user agent, status, bytes sent and
//     the response time; the page shows no pattern.
//   - etc/jetty/jetty-requestlog.xml of the public source repository of the old
//     product, branch main: %{client}a - <user id> [<response time>] "%r" %s
//     %{Content-Length}i %O %{ms}T "%{User-Agent}i" [<thread name>], and its request
//     log class writes the time as dd/MMM/yyyy:HH:mm:ss Z in the zone of the JVM.
//
// This file writes the time in UTC and the request id in the place of the
// thread name.

const vectorDir = "../../testdata/vectors/requestlog"

// vectorRequest is one request of classic.json; duration is read by
// time.ParseDuration.
type vectorRequest struct {
	ID            string    `json:"id"`
	Time          time.Time `json:"time"`
	Client        string    `json:"client"`
	Subject       string    `json:"subject"`
	Realm         string    `json:"realm"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Query         string    `json:"query"`
	Protocol      string    `json:"protocol"`
	Status        int       `json:"status"`
	ContentLength string    `json:"content_length"`
	RequestBytes  int64     `json:"request_bytes"`
	ResponseBytes int64     `json:"response_bytes"`
	Duration      string    `json:"duration"`
	UserAgent     string    `json:"user_agent"`
	Repository    string    `json:"repository"`
	Format        string    `json:"format"`
	Port          int       `json:"port"`
}

func (v vectorRequest) request(t *testing.T) Request {
	t.Helper()
	var d time.Duration
	if v.Duration != "" {
		var err error
		if d, err = time.ParseDuration(v.Duration); err != nil {
			t.Fatal(err)
		}
	}
	return Request{ID: v.ID, Time: v.Time, Client: v.Client, Subject: v.Subject, Realm: v.Realm, Method: v.Method, Path: v.Path,
		Query: v.Query, Protocol: v.Protocol, Status: v.Status, ContentLength: v.ContentLength, RequestBytes: v.RequestBytes,
		ResponseBytes: v.ResponseBytes, Duration: d, UserAgent: v.UserAgent, Repository: v.Repository, Format: v.Format, Port: v.Port}
}

type classicVectors struct {
	Note  string    `json:"note"`
	Now   time.Time `json:"now"`
	Cases []struct {
		Name    string        `json:"name"`
		Request vectorRequest `json:"request"`
	} `json:"cases"`
}

// loadClassic reads classic.json and the lines of classic.txt.
func loadClassic(t *testing.T) (classicVectors, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vectorDir, "classic.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v classicVectors
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(vectorDir, "classic.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return v, want
}

// The line of each vector request equals its line in classic.txt byte for
// byte, and requests-classic.log of the same requests equals classic.txt.
func TestClassicVectors(t *testing.T) {
	v, want := loadClassic(t)
	lines := strings.SplitAfter(string(want), "\n")
	if lines[len(lines)-1] != "" {
		t.Fatal("classic.txt does not end with a line break")
	}
	lines = lines[:len(lines)-1]
	if len(lines) != len(v.Cases) || len(lines) == 0 {
		t.Fatalf("%d lines for %d cases", len(lines), len(v.Cases))
	}
	for i, c := range v.Cases {
		if got := string(c.Request.request(t).classicLine(v.Now)); got != lines[i] {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, lines[i])
		}
	}
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: func() time.Time { return v.Now }})
	for _, c := range v.Cases {
		f.Request(c.Request.request(t))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "requests-classic.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("requests-classic.log:\n%s\nwant\n%s", got, want)
	}
}

// Every byte value: 0x21 to 0x7e stay except the quote and the backslash,
// any other byte is \xHH with lower case digits; the space stays in the user
// agent only. An empty field is -.
func TestClassicEscapesEveryByte(t *testing.T) {
	kept := 0
	for i := range 256 {
		c := byte(i)
		want := string([]byte{c})
		if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			want = fmt.Sprintf(`\x%02x`, c)
		} else {
			kept++
		}
		if got := string(appendClassic(nil, string([]byte{c}), false)); got != want {
			t.Errorf("byte %#02x: %q, want %q", c, got, want)
		}
		if c == ' ' {
			want = " "
		}
		if got := string(appendClassic(nil, string([]byte{c}), true)); got != want {
			t.Errorf("byte %#02x in the user agent: %q, want %q", c, got, want)
		}
	}
	if kept != 92 {
		t.Errorf("%d bytes kept, want 92: 0x21 to 0x7e without the quote and the backslash", kept)
	}
	for _, space := range []bool{false, true} {
		if got := string(appendClassic(nil, "", space)); got != "-" {
			t.Errorf("an empty field: %q", got)
		}
	}
}

// classicFields is the line of requests-classic.log as its field list
// describes it, written without the code of the writer:
//
//	<client> - <subject> [<time>] "<method> <path and query> <protocol>" <status> <content length> <bytes sent> <duration in ms> "<user agent>" [<request id>]
var classicFields = regexp.MustCompile(`^(\S+) - (\S+) \[(\d\d/[A-Z][a-z]{2}/\d{4}:\d\d:\d\d:\d\d \+0000)\] "(\S+) (\S+) (\S+)" (\d+) (\S+) (\d+) (\d+) "([^"]*)" \[(\S+)\]$`)

// classicRecord is what a line says about its request.
type classicRecord struct {
	client, subject          string
	unix                     int64
	method, target, protocol string
	status                   int
	contentLength            string
	bytesSent, durationMS    int64
	userAgent, id            string
}

// readField undoes the escaping of one field: \xHH is the byte HH and a lone
// - is empty. A backslash without x and two lower case hex digits, and a raw
// byte that should have been escaped, are errors.
func readField(field string, space bool) (string, error) {
	if field == "-" {
		return "", nil
	}
	hex := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }
	var out []byte
	for i := 0; i < len(field); i++ {
		c := field[i]
		switch {
		case c == '\\':
			if len(field)-i < 4 || field[i+1] != 'x' || !hex(field[i+2]) || !hex(field[i+3]) {
				return "", fmt.Errorf("a backslash without \\xHH at %d", i)
			}
			n, _ := strconv.ParseUint(field[i+2:i+4], 16, 8)
			out = append(out, byte(n))
			i += 3
		case c == ' ' && space:
			out = append(out, c)
		case c < 0x21 || c > 0x7e || c == '"':
			return "", fmt.Errorf("raw byte %#02x at %d", c, i)
		default:
			out = append(out, c)
		}
	}
	return string(out), nil
}

// parseClassic reads one line by the field list.
func parseClassic(line string) (classicRecord, error) {
	m := classicFields.FindStringSubmatch(line)
	if m == nil {
		return classicRecord{}, fmt.Errorf("the line does not match the field list")
	}
	var rec classicRecord
	text := []*string{&rec.client, &rec.subject, &rec.method, &rec.target, &rec.protocol, &rec.contentLength, &rec.userAgent, &rec.id}
	for i, group := range []int{1, 2, 4, 5, 6, 8, 11, 12} {
		v, err := readField(m[group], group == 11)
		if err != nil {
			return classicRecord{}, fmt.Errorf("field %d: %w", group, err)
		}
		*text[i] = v
	}
	at, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[3])
	if err != nil {
		return classicRecord{}, err
	}
	rec.unix = at.Unix()
	if rec.status, err = strconv.Atoi(m[7]); err != nil {
		return classicRecord{}, err
	}
	if rec.bytesSent, err = strconv.ParseInt(m[9], 10, 64); err != nil {
		return classicRecord{}, err
	}
	if rec.durationMS, err = strconv.ParseInt(m[10], 10, 64); err != nil {
		return classicRecord{}, err
	}
	return rec, nil
}

// expected is what the line of r must say: a value of a lone - reads as
// empty, as an empty value does.
func expected(r Request) classicRecord {
	plain := func(s string) string {
		if s == "-" {
			return ""
		}
		return s
	}
	target := r.Path
	if r.Query != "" {
		target += "?" + r.Query
	}
	return classicRecord{client: plain(r.Client), subject: plain(r.Subject), unix: r.Time.Unix(), method: plain(r.Method),
		target: plain(target), protocol: plain(r.Protocol), status: r.Status, contentLength: plain(r.ContentLength),
		bytesSent: r.ResponseBytes, durationMS: int64(r.Duration / time.Millisecond), userAgent: plain(r.UserAgent), id: plain(r.ID)}
}

// hostile is up to 24 random bytes, many of them bytes that end a field, a
// line or an escape.
func hostile(rng *rand.Rand) string {
	const special = " \"\\\r\n\t\x00\x7f\x80\xff[]-x%?"
	b := make([]byte, rng.IntN(25))
	for i := range b {
		switch rng.IntN(3) {
		case 0:
			b[i] = special[rng.IntN(len(special))]
		case 1:
			b[i] = byte(rng.IntN(256))
		default:
			b[i] = byte('a' + rng.IntN(26))
		}
	}
	return string(b)
}

// either is plain or, one time in two, a hostile value.
func either(rng *rand.Rand, plain string) string {
	if rng.IntN(2) == 0 {
		return hostile(rng)
	}
	return plain
}

var zones = []*time.Location{time.UTC, time.FixedZone("demo-east", 5*3600+1800), time.FixedZone("demo-west", -8*3600)}

func randomRequest(rng *rand.Rand) Request {
	client := fmt.Sprintf("192.0.2.%d", rng.IntN(256))
	if rng.IntN(2) == 0 {
		client = fmt.Sprintf("2001:db8::%x", rng.IntN(1<<16))
	}
	return Request{
		ID:            either(rng, fmt.Sprintf("%016x", rng.Uint64())),
		Time:          time.Unix(rng.Int64N(4_102_444_800), rng.Int64N(1e9)).In(zones[rng.IntN(len(zones))]),
		Client:        either(rng, client),
		Subject:       either(rng, "demo-reader"),
		Realm:         either(rng, "demo-realm"),
		Method:        either(rng, "GET"),
		Path:          either(rng, "/repository/demo-raw/a%20b.txt"),
		Query:         either(rng, "x=1&token=***;flag"),
		Protocol:      either(rng, "HTTP/1.1"),
		Status:        rng.IntN(1000),
		ContentLength: either(rng, strconv.Itoa(rng.IntN(1<<20))),
		RequestBytes:  rng.Int64N(1 << 20),
		ResponseBytes: rng.Int64N(1 << 40),
		Duration:      time.Duration(rng.Int64N(int64(time.Hour))),
		UserAgent:     either(rng, "demo-agent/1.0 (demo)"),
		Repository:    either(rng, "demo-raw"),
		Format:        "raw",
		Port:          14100,
	}
}

// 10,000 requests from a fixed seed with hostile values: a parser written
// from the field list reads every line of requests-classic.log, 0 unparsed,
// and each field equals what was sent.
func TestClassicParserRun(t *testing.T) {
	const sent = 10_000
	rng := rand.New(rand.NewPCG(7, 2026))
	dir := t.TempDir()
	f := Open(Options{Dir: dir, Settings: DefaultSettings(), Now: clock})
	requests := make([]Request, sent)
	for i := range requests {
		requests[i] = randomRequest(rng)
		f.Request(requests[i])
		if i%500 == 499 {
			f.Tick()
		}
	}
	if err := f.Close(); err != nil || f.Errors() != 0 {
		t.Fatalf("close %v, %d errors", err, f.Errors())
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests-classic.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != sent {
		t.Fatalf("%d lines for %d requests", len(lines), sent)
	}
	unparsed, differ := 0, 0
	for i, line := range lines {
		got, err := parseClassic(line)
		if err != nil {
			unparsed++
			if unparsed <= 5 {
				t.Errorf("line %d: %v: %q", i, err, line)
			}
			continue
		}
		if want := expected(requests[i]); got != want {
			differ++
			if differ <= 5 {
				t.Errorf("line %d: read %+v, sent %+v", i, got, want)
			}
		}
	}
	t.Logf("%d lines for %d requests: %d unparsed, %d with a field that differs from the request", len(lines), sent, unparsed, differ)
	if unparsed != 0 || differ != 0 {
		t.Fatalf("%d unparsed, %d differ", unparsed, differ)
	}
}

// requests-classic.log is written while Settings.Classic is true and is not
// made while it is false; its lines wait for the flush like those of
// requests.log.
func TestClassicFileFollowsTheSetting(t *testing.T) {
	for _, on := range []bool{true, false} {
		dir := t.TempDir()
		settings := DefaultSettings()
		settings.Classic = on
		f := Open(Options{Dir: dir, Settings: settings, Now: clock})
		f.Request(demoRequest("/repository/demo-releases/a.jar"))
		if got := lines(t, dir, "requests-classic.log"); len(got) != 0 {
			t.Fatalf("classic %v: a line before the flush: %q", on, got)
		}
		f.Tick()
		const want = `192.0.2.7 - demo-reader [19/Nov/2026:10:00:00 +0000] "GET /repository/demo-releases/a.jar?a=1 HTTP/1.1" 200 3 2048 1 "demo-agent/1.0" [0123456789abcdef]`
		if classic := lines(t, dir, "requests-classic.log"); on && (len(classic) != 1 || classic[0] != want) {
			t.Errorf("classic on: %q, want %q", classic, want)
		}
		if err := f.Close(); err != nil || f.Errors() != 0 {
			t.Fatalf("classic %v: close %v, %d errors", on, err, f.Errors())
		}
		info, err := os.Stat(filepath.Join(dir, "requests-classic.log"))
		switch {
		case on && (err != nil || info.Mode().Perm() != 0o600):
			t.Errorf("classic on: %v %v", info, err)
		case !on && !errors.Is(err, os.ErrNotExist):
			t.Errorf("classic off: requests-classic.log %v %v", info, err)
		}
		if got := lines(t, dir, "requests.log"); len(got) != 1 {
			t.Errorf("classic %v: requests.log %q", on, got)
		}
	}
}
