package logship

import (
	"compress/gzip"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The rules of the push API of Loki, read on 2026-11-19. The page
// https://grafana.com/docs/loki/latest/reference/loki-http-api/ (Loki
// v3.7.x), section Ingest logs: POST /loki/api/v1/push; with Content-Type
// application/json the body {"streams":[{"stream":{<label>:<value>},
// "values":[["<unix epoch in nanoseconds>","<log line>"]]}]}, a value with an
// optional third element of structured metadata; a time stamp sent as a
// number gets 400; a field name outside this schema gets 400 with "found
// unknown field"; Content-Encoding gzip is taken. The page
// docs/sources/operations/troubleshooting/troubleshoot-ingest.md of
// github.com/grafana/loki, branch main: an empty list of streams gets 422; a
// stream without labels and a label name outside [a-zA-Z_][a-zA-Z0-9_]* or
// starting with __ get 400; 429, 503 and 504 are retryable, 400, 413 and 422
// are not. The push handler and its parser in the same repository
// (pkg/distributor/http.go, pkg/loghttp/push/push.go,
// pkg/util/unmarshal/unmarshal.go, pkg/loghttp/entry.go): a push that was
// taken answers 204, the status the page does not print; a Content-Encoding
// other than gzip, snappy or deflate, a body that does not decode, and a
// Content-Type other than application/json, which is read as snappy
// protobuf, get 400 by http.Error; a value holds 2 or 3 elements. The
// choices of this server on top: one stream per pair of kind and level with
// the labels kind, level and instance, the values of a stream in time order,
// no structured metadata, gzip always. The stub reads every key by its exact
// name. unverified: the answer of a gateway in front of Loki to a missing or
// wrong Authorization (the stub answers 401).

// The helpers of the tests of the HTTP kinds loki, otlp, splunk-hec and
// elasticsearch: the base of their stubs, the head every request carries,
// the requests a refusal test sends, and the checks of the status rules and
// of the secret.

// strictWeb is what the stubs of the HTTP kinds share: an HTTP server, or an
// HTTPS one with a certificate of testCA, that counts the requests, keeps the
// reason of each refusal, and answers with the status a test asks for once a
// request passed every rule.
type strictWeb struct {
	srv    *httptest.Server
	caFile string

	mu         sync.Mutex
	requests   int
	refused    []string
	status     func(n int) int // the status of request n once it passed every rule; 0 lets the stub answer
	retryAfter string          // the Retry-After of an answer of status
}

func (b *strictWeb) start(t *testing.T, h http.Handler, https bool) {
	t.Helper()
	b.srv = httptest.NewUnstartedServer(h)
	if https {
		var cert tls.Certificate
		b.caFile, cert = testCA(t)
		b.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		b.srv.StartTLS()
	} else {
		b.srv.Start()
	}
	t.Cleanup(b.srv.Close)
}

func (b *strictWeb) url() string { return b.srv.URL }
func (b *strictWeb) ca() string  { return b.caFile }

// begin counts a request and returns its number, from 1.
func (b *strictWeb) begin() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests++
	return b.requests
}

func (b *strictWeb) note(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refused = append(b.refused, reason)
}

func (b *strictWeb) setStatus(status func(n int) int, retryAfter string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status, b.retryAfter = status, retryAfter
}

// asked is the status the test asks for request n, 0 for none, and the
// Retry-After of that answer.
func (b *strictWeb) asked(n int) (int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status == nil {
		return 0, ""
	}
	return b.status(n), b.retryAfter
}

func (b *strictWeb) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests
}

func (b *strictWeb) refusals() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.refused...)
}

// webHead checks what every request of this server carries: the method
// POST, the path of the protocol and the User-Agent of this server. The
// statuses are those of the stubs.
func webHead(r *http.Request, path string) (int, string) {
	switch {
	case r.Method != http.MethodPost:
		return http.StatusMethodNotAllowed, "method " + r.Method
	case r.URL.Path != path:
		return http.StatusNotFound, "path " + r.URL.Path
	case r.Header.Get("User-Agent") != "pipesmith-registry/"+testVersion:
		return http.StatusBadRequest, "user agent " + r.Header.Get("User-Agent")
	}
	return 0, ""
}

// kindStub is a stub of an HTTP kind as the shared checks use it.
type kindStub interface {
	url() string
	ca() string
	setStatus(status func(n int) int, retryAfter string)
	count() int // the requests it got
	took() int  // the lines it took
	refusals() []string
}

// directRequest is one request a refusal test sends to a stub itself. An
// empty method or path is POST to the path of the protocol; header changes
// the headers of a good request, an empty value removes one.
type directRequest struct {
	name, method, path string
	header             map[string]string
	body               string
	want               int    // the status of the answer
	reason             string // a part of the reason the stub gives for a refusal; empty for a request it takes
}

// sendDirect sends each request to the stub at base and fails the test on a
// status other than the one it wants. It returns the reasons of the
// requests the stub should have refused, in their order.
func sendDirect(t *testing.T, base, path string, headers map[string]string, cases []directRequest) []string {
	t.Helper()
	var wants []string
	for _, c := range cases {
		method, at := c.method, c.path
		if method == "" {
			method = http.MethodPost
		}
		if at == "" {
			at = path
		}
		req, err := http.NewRequest(method, base+at, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		merged := maps.Clone(headers)
		maps.Copy(merged, c.header)
		for key, value := range merged {
			if value != "" {
				req.Header.Set(key, value)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d: %s", c.name, resp.StatusCode, c.want, answer)
		}
		if c.reason != "" {
			wants = append(wants, c.reason)
		}
	}
	return wants
}

// shipThousand ships lines to a target of kind at the address of stub, over
// https with its ca_file when it has one, with the further keys of row, and
// waits until the stub took as many lines or refused a request and the
// sender counted them.
func shipThousand(t *testing.T, kind string, stub kindStub, lines []logs.Line, row map[string]any) *Manager {
	t.Helper()
	full := map[string]any{"name": "demo", "kind": kind, "address": stub.url()}
	if stub.ca() != "" {
		full["ca_file"] = stub.ca()
	}
	maps.Copy(full, row)
	var set setting
	set.put(t, full)
	set.putSecret("log-target-demo", "demo-token")
	var log testLog
	m := startManager(t, newFakeClock(), &set, &log)
	statsOf(t, m, "demo")
	shipAll(t, m, lines)
	waitFor(t, func() bool { return stub.took() >= len(lines) || len(stub.refusals()) > 0 }, "the lines taken")
	if refused := stub.refusals(); len(refused) > 0 {
		t.Fatalf("the stub refused %d requests, the first: %s", len(refused), refused[0])
	}
	waitCounted(t, m, "demo", len(lines))
	return m
}

// checkStatusRules hits each rule of the status of an answer once for kind:
// 429, 502, 503 and 504 send the line again and it counts as sent; every
// other status outside 2xx, 3xx and those of extra among them, drops it
// after one attempt; a Retry-After of 2 s replaces the pause of 500 ms; a
// refused connection is tried 3 times and fails as dial. newStub starts a
// stub that takes requests without an Authorization.
func checkStatusRules(t *testing.T, kind string, extra []int, newStub func(t *testing.T) kindStub) {
	t.Helper()
	retried := []int{429, 502, 503, 504}
	dropped := append([]int{301, 302, 307, 308, 400, 401, 403, 404, 405, 409, 413, 415, 500, 501}, extra...)
	start := func(t *testing.T, stub kindStub) (*Manager, *fakeClock) {
		t.Helper()
		var set setting
		set.put(t, map[string]any{"name": "demo", "kind": kind, "address": stub.url(), "batch_lines": 1})
		clk := newFakeClock()
		var log testLog
		m := startManager(t, clk, &set, &log)
		statsOf(t, m, "demo")
		return m, clk
	}
	for _, code := range append(append([]int(nil), retried...), dropped...) {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			stub := newStub(t)
			stub.setStatus(func(n int) int {
				if n == 1 {
					return code
				}
				return 0
			}, "")
			m, clk := start(t, stub)
			m.Ship(testLine("tasks", 1))
			if slices.Contains(retried, code) {
				clk.waitPending(t, 500*time.Millisecond)
				clk.Advance(500 * time.Millisecond)
				waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent again")
				if st := statsOf(t, m, "demo"); st.Attempts != 2 || st.Errors != 1 || st.Failed != 0 || stub.took() != 1 {
					t.Fatalf("%+v with %d lines taken", st, stub.took())
				}
				return
			}
			waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line dropped")
			if st := statsOf(t, m, "demo"); st.Attempts != 1 || st.Sent != 0 || stub.count() != 1 {
				t.Fatalf("%+v after %d requests", st, stub.count())
			}
		})
	}
	t.Run("Retry-After", func(t *testing.T) {
		stub := newStub(t)
		stub.setStatus(func(n int) int {
			if n == 1 {
				return http.StatusTooManyRequests
			}
			return 0
		}, "2")
		m, clk := start(t, stub)
		m.Ship(testLine("server", 1))
		clk.waitPending(t, 2*time.Second)
		if slices.Contains(clk.pending(), 500*time.Millisecond) {
			t.Fatalf("the pause of 500 ms is asked for: %v", clk.pending())
		}
		clk.Advance(2 * time.Second)
		waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent after Retry-After")
	})
	t.Run("refused", func(t *testing.T) { checkRefusedDial(t, kind, "http") })
}

// checkSecret ships 3 lines to a target of kind at path whose stub takes only
// the Authorization of its secret: scheme and the token over http to
// loopback, and HTTP Basic with a user over https. The first answer is 503.
// The secret, the query of the address and the encoded credentials are in
// no line of the log of the manager, its warning with the class of the
// failure among them.
func checkSecret(t *testing.T, kind, scheme, path string, newStub func(t *testing.T, auth string, https bool) kindStub) {
	t.Helper()
	const token, password, query = "canary-token-71d2c0a9", "canary-password-e04b6f3d", "canary-query-2f8a51"
	cases := []struct {
		name, user, secret string
		https              bool
	}{
		{"token", "", token, false},
		{"basic", "demo-admin", password, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth := scheme + " " + c.secret
			if c.user != "" {
				auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.secret))
			}
			stub := newStub(t, auth, c.https)
			stub.setStatus(func(n int) int {
				if n == 1 {
					return http.StatusServiceUnavailable
				}
				return 0
			}, "")
			row := map[string]any{"name": "demo", "kind": kind, "address": stub.url() + path + "?token=" + query,
				"secret": "log-target-demo", "batch_lines": 3}
			if c.https {
				row["ca_file"], row["user"] = stub.ca(), c.user
			}
			var set setting
			set.put(t, row)
			set.putSecret("log-target-demo", c.secret)
			clk := newFakeClock()
			var log testLog
			m := startManager(t, clk, &set, &log)
			statsOf(t, m, "demo")
			shipAll(t, m, testLines("server", 0, 3))
			clk.waitPending(t, 500*time.Millisecond)
			clk.Advance(500 * time.Millisecond)
			waitFor(t, func() bool { return stub.took() == 3 }, "the lines taken with the secret")
			m.Close()
			if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "status 503" {
				t.Fatalf("warnings %v", warns)
			}
			noCanary(t, "the log of the manager", log.text(), c.secret, query, auth[strings.Index(auth, " ")+1:])
		})
	}
}

// textOf is a JSON value when it is a string.
func textOf(raw json.RawMessage) (string, bool) {
	var v string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return v, true
}

// headOfLine reads a line as a JSON object with the head of a log line by
// its exact names, and the time of its head.
func headOfLine(line string) (map[string]any, time.Time, bool) {
	var head map[string]any
	if json.Unmarshal([]byte(line), &head) != nil || !headIn(head) {
		return nil, time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, head["time"].(string))
	return head, at, err == nil
}

// levelNames is the name of each level of a line, as a line writes it.
var levelNames = map[slog.Level]string{slog.LevelDebug: "DEBUG", slog.LevelInfo: "INFO", slog.LevelWarn: "WARN",
	slog.LevelError: "ERROR"}

// lokiPath is the path of a push.
const lokiPath = "/loki/api/v1/push"

// maxLokiBody is the most bytes of a push the stub reads after gzip.
const maxLokiBody = 16 << 20

// lokiLabelName is a label name as the troubleshooting page of Loki gives it.
var lokiLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// lokiStub is a receiver of the push API of Loki in JSON. It reads a request
// with decode, takes every value of a request that keeps the rules and
// answers 204, and refuses any other request with the status of the rule it
// breaks, by http.Error as Loki does.
type lokiStub struct {
	strictWeb
	auth    string // the Authorization it takes; empty takes none
	entries []lokiEntry
}

// lokiEntry is one value the stub took, with the labels of its stream.
type lokiEntry struct {
	kind, level, instance string
	stamp                 string // the time stamp as it came
	line, id              string
	request, stream       int // the request, from 1, and the stream in it, from 0
}

func newLokiStub(t *testing.T, auth string, https bool) *lokiStub {
	t.Helper()
	s := &lokiStub{auth: auth}
	s.start(t, s, https)
	return s
}

func (s *lokiStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.begin()
	entries, code, reason := s.decode(r, n)
	if code == 0 {
		var retryAfter string
		if code, retryAfter = s.asked(n); code != 0 {
			reason = "the answer of the test"
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
		}
	}
	if code != 0 {
		s.note(reason)
		http.Error(w, reason, code)
		return
	}
	s.mu.Lock()
	s.entries = append(s.entries, entries...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// decode applies the rules of the push API and of this server to request n;
// a code other than 0 refuses it.
func (s *lokiStub) decode(r *http.Request, n int) ([]lokiEntry, int, string) {
	if code, reason := webHead(r, lokiPath); code != 0 {
		return nil, code, reason
	}
	if r.Header.Get("Authorization") != s.auth {
		return nil, http.StatusUnauthorized, "authorization"
	}
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return nil, http.StatusBadRequest, "content type " + r.Header.Get("Content-Type") + ", read as snappy protobuf"
	}
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "gzip":
	case "", "snappy", "deflate":
		return nil, http.StatusBadRequest, "content encoding " + enc + ": this server always sends gzip"
	default:
		return nil, http.StatusBadRequest, fmt.Sprintf("Content-Encoding %q not supported", enc)
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, http.StatusBadRequest, "the body is not gzip"
	}
	body, err := io.ReadAll(io.LimitReader(zr, maxLokiBody+1))
	switch {
	case err != nil:
		return nil, http.StatusBadRequest, "the body is not gzip"
	case len(body) > maxLokiBody:
		return nil, http.StatusRequestEntityTooLarge, "request body too large"
	case !utf8.Valid(body) || !json.Valid(body):
		return nil, http.StatusBadRequest, "the body is not JSON"
	}
	top, err := jsonFields(body)
	if err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	for _, key := range slices.Sorted(maps.Keys(top)) {
		if key != "streams" {
			return nil, http.StatusBadRequest, "found unknown field: " + key
		}
	}
	var streams []json.RawMessage
	if raw, ok := top["streams"]; ok && json.Unmarshal(raw, &streams) != nil {
		return nil, http.StatusBadRequest, "streams is not a list"
	}
	if len(streams) == 0 {
		return nil, http.StatusUnprocessableEntity, "error at least one valid stream is required for ingestion"
	}
	var out []lokiEntry
	seen := map[string]bool{}
	for i, raw := range streams {
		entries, code, reason := s.stream(raw, n, i, seen)
		if code != 0 {
			return nil, code, reason
		}
		out = append(out, entries...)
	}
	return out, 0, ""
}

// stream reads stream i of request n; seen holds the kinds and levels of the
// streams before it.
func (s *lokiStub) stream(raw json.RawMessage, n, i int, seen map[string]bool) ([]lokiEntry, int, string) {
	fields, err := jsonFields(raw)
	if err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if key != "stream" && key != "values" {
			return nil, http.StatusBadRequest, "found unknown field: " + key
		}
	}
	labels := map[string]string{}
	if raw, ok := fields["stream"]; ok {
		pairs, err := jsonFields(raw)
		if err != nil {
			return nil, http.StatusBadRequest, "error parsing labels: " + err.Error()
		}
		for name := range pairs {
			value, ok := jsonText(pairs, name)
			if !ok {
				return nil, http.StatusBadRequest, "error parsing labels: the value of " + name + " is not a string"
			}
			labels[name] = value
		}
	}
	if len(labels) == 0 {
		return nil, http.StatusBadRequest, "error at least one label pair is required per stream"
	}
	names := slices.Sorted(maps.Keys(labels))
	for _, name := range names {
		if !lokiLabelName.MatchString(name) || strings.HasPrefix(name, "__") {
			return nil, http.StatusBadRequest, fmt.Sprintf("error parsing labels: the label name %q", name)
		}
	}
	// The labels of this server.
	kind, level := labels["kind"], labels["level"]
	switch {
	case !slices.Equal(names, []string{"instance", "kind", "level"}) || !lineKinds[kind] || severityByName[level] == 0 ||
		labels["instance"] != stubHost:
		return nil, http.StatusBadRequest, fmt.Sprintf("the labels %v, not those of this server", labels)
	case seen[kind+" "+level]:
		return nil, http.StatusBadRequest, "a second stream of the kind " + kind + " and the level " + level
	}
	seen[kind+" "+level] = true
	var values []json.RawMessage
	if json.Unmarshal(fields["values"], &values) != nil || len(values) == 0 {
		return nil, http.StatusBadRequest, "values is not a list of values"
	}
	var out []lokiEntry
	var last int64
	for _, v := range values {
		var parts []json.RawMessage
		if json.Unmarshal(v, &parts) != nil {
			return nil, http.StatusBadRequest, "a value that is not a list"
		}
		if len(parts) < 2 || len(parts) > 3 {
			return nil, http.StatusBadRequest, "error reading entry: array must have at least 2 and up to 3 values"
		}
		stamp, ok := textOf(parts[0])
		ns, err := strconv.ParseInt(stamp, 10, 64)
		if !ok || err != nil {
			return nil, http.StatusBadRequest, "error reading entry timestamp: not a string of nanoseconds"
		}
		line, ok := textOf(parts[1])
		if !ok {
			return nil, http.StatusBadRequest, "the line is not a string"
		}
		// The values of this server.
		if len(parts) == 3 {
			return nil, http.StatusBadRequest, "structured metadata, which this server does not send"
		}
		head, at, ok := headOfLine(line)
		switch {
		case !ok:
			return nil, http.StatusBadRequest, "a line that is not a JSON object with time, level, logger and msg by these names"
		case head["level"] != level:
			return nil, http.StatusBadRequest, fmt.Sprintf("a line of the level %v in the stream of the level %s", head["level"], level)
		case at.UnixNano() != ns:
			return nil, http.StatusBadRequest, "the time stamp " + stamp + " is not the time of its line"
		case ns < last:
			return nil, http.StatusBadRequest, "values out of time order"
		}
		last = ns
		id, _ := lineIDOf([]byte(line))
		out = append(out, lokiEntry{kind: kind, level: level, instance: labels["instance"], stamp: stamp, line: line, id: id,
			request: n, stream: i})
	}
	return out, 0, ""
}

func (s *lokiStub) taken() []lokiEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]lokiEntry(nil), s.entries...)
}

func (s *lokiStub) took() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// The 1,000 hostile lines arrive over http and over https, the address
// without a path, as 1,000 values with the ids that were sent: each value the
// line byte for byte and the time of the line in nanoseconds, in a stream
// whose labels are the kind and the level of the line and the host. The
// stub took two pushes, one per batch of 500, each with one stream per kind
// and level and the values of a stream in time order.
func TestLokiThousandHostileLines(t *testing.T) {
	lines := hostileLines(t)
	want := idSet(t, lines)
	sent := map[string]logs.Line{}
	for _, l := range lines {
		id, _ := lineIDOf(l.JSON)
		sent[id] = l
	}
	for _, variant := range []string{"http", "https"} {
		t.Run(variant, func(t *testing.T) {
			stub := newLokiStub(t, "", variant == "https")
			m := shipThousand(t, "loki", stub, lines, nil)
			got := stub.taken()
			if len(got) != len(lines) {
				t.Fatalf("%d values decoded, %d lines sent", len(got), len(lines))
			}
			ids := map[string]bool{}
			streams := map[string]bool{}
			for _, e := range got {
				line, ok := sent[e.id]
				switch {
				case !ok:
					t.Fatalf("a value with the id %q, which was not sent", e.id)
				case e.line != string(line.JSON):
					t.Fatalf("%s arrived as %q, sent as %q", e.id, e.line, line.JSON)
				case e.kind != line.Kind || e.level != levelNames[line.Level] || e.instance != stubHost:
					t.Fatalf("%s in the stream of %s, %s, %s for a %s line at %v", e.id, e.kind, e.level, e.instance, line.Kind, line.Level)
				case e.stamp != strconv.FormatInt(line.Time.UnixNano(), 10):
					t.Fatalf("%s: the time stamp %s for a line of %v", e.id, e.stamp, line.Time)
				}
				ids[e.id] = true
				streams[fmt.Sprintf("%d %d", e.request, e.stream)] = true
			}
			if len(ids) != len(want) {
				t.Fatalf("%d ids decoded, %d sent", len(ids), len(want))
			}
			st := statsOf(t, m, "demo")
			if st.Sent != 1000 || st.Failed != 0 || st.Errors != 0 || st.Attempts != 2 || stub.count() != 2 {
				t.Fatalf("%+v in %d requests", st, stub.count())
			}
			t.Logf("%d values in %d requests and %d streams", len(got), stub.count(), len(streams))
		})
	}
}

// The token reaches the stub as Bearer, and with a user as HTTP Basic over
// https; neither secret is in a log line of the manager.
func TestLokiSecretReachesOnlyTheStub(t *testing.T) {
	checkSecret(t, "loki", "Bearer", lokiPath, func(t *testing.T, auth string, https bool) kindStub {
		return newLokiStub(t, auth, https)
	})
}

// Each status rule once, 422 among the statuses that drop the line.
func TestLokiRetryRules(t *testing.T) {
	checkStatusRules(t, "loki", []int{http.StatusUnprocessableEntity}, func(t *testing.T) kindStub {
		return newLokiStub(t, "", false)
	})
}

// lokiText is a push of one stream written by hand: the labels and the
// values as they are given.
func lokiText(labels string, values ...string) string {
	return `{"streams":[{"stream":` + labels + `,"values":[` + strings.Join(values, ",") + `]}]}`
}

// lokiValue is the value of line written by hand: its time in nanoseconds
// as a string and the line as a string.
func lokiValue(line logs.Line) string {
	quoted, _ := json.Marshal(string(line.JSON))
	return `["` + strconv.FormatInt(line.Time.UnixNano(), 10) + `",` + string(quoted) + `]`
}

// The stub refuses what the push API and this server do not allow, one bad
// request each, sent to it directly, each with the status of its rule and a
// reason that names it; it takes the good request.
func TestLokiStubRefuses(t *testing.T) {
	stub := newLokiStub(t, "Bearer demo-token", false)
	line, next := testLine("server", 1), testLine("server", 2)
	labels := `{"instance":"demo-host","kind":"server","level":"INFO"}`
	good := lokiText(labels, lokiValue(line))
	gz := func(text string) string { return compress(t, "GZIP", text) }
	other := func(from, to string) string { return gz(strings.Replace(good, from, to, 1)) }
	stamp := strconv.FormatInt(line.Time.UnixNano(), 10)
	headers := map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip",
		"User-Agent": "pipesmith-registry/" + testVersion, "Authorization": "Bearer demo-token"}
	cases := []directRequest{
		{name: "good", body: gz(good), want: 204},
		{name: "method", method: "PUT", body: gz(good), want: 405, reason: "method PUT"},
		{name: "path", path: "/loki/api/v1/other", body: gz(good), want: 404, reason: "path"},
		{name: "content type", header: map[string]string{"Content-Type": "text/plain"}, body: gz(good), want: 400, reason: "snappy protobuf"},
		{name: "no content type", header: map[string]string{"Content-Type": ""}, body: gz(good), want: 400, reason: "snappy protobuf"},
		{name: "no compression", header: map[string]string{"Content-Encoding": ""}, body: good, want: 400, reason: "always sends gzip"},
		{name: "deflate", header: map[string]string{"Content-Encoding": "deflate"}, body: gz(good), want: 400, reason: "always sends gzip"},
		{name: "br", header: map[string]string{"Content-Encoding": "br"}, body: gz(good), want: 400, reason: "not supported"},
		{name: "not gzip", body: good, want: 400, reason: "not gzip"},
		{name: "user agent", header: map[string]string{"User-Agent": "demo-client/1.0"}, body: gz(good), want: 400, reason: "user agent"},
		{name: "no secret", header: map[string]string{"Authorization": ""}, body: gz(good), want: 401, reason: "authorization"},
		{name: "other secret", header: map[string]string{"Authorization": "Bearer other-token"}, body: gz(good), want: 401, reason: "authorization"},
		{name: "not JSON", body: gz(strings.TrimSuffix(good, "}")), want: 400, reason: "not JSON"},
		{name: "an unknown field", body: gz(strings.TrimSuffix(good, "}") + `,"extra":1}`), want: 400, reason: "found unknown field: extra"},
		{name: "keys in another case", body: other(`"streams"`, `"Streams"`), want: 400, reason: "found unknown field: Streams"},
		{name: "a value key in another case", body: other(`"values"`, `"Values"`), want: 400, reason: "found unknown field: Values"},
		{name: "no streams", body: gz(`{"streams":[]}`), want: 422, reason: "at least one valid stream"},
		{name: "no labels", body: gz(lokiText(`{}`, lokiValue(line))), want: 400, reason: "at least one label pair"},
		{name: "a label name with a dash", body: other(`"instance"`, `"bad-name"`), want: 400, reason: `"bad-name"`},
		{name: "a label name with two underscores", body: other(`"instance"`, `"__instance"`), want: 400, reason: `"__instance"`},
		{name: "a time stamp as a number", body: other(`"`+stamp+`"`, stamp), want: 400, reason: "timestamp"},
		{name: "a value of one element", body: gz(lokiText(labels, `["`+stamp+`"]`)), want: 400, reason: "at least 2"},
		{name: "structured metadata", body: other(`"]]}]}`, `",{"trace_id":"1"}]]}]}`), want: 400, reason: "structured metadata"},
		{name: "labels of another set", body: other(`"instance":"demo-host",`, ""), want: 400, reason: "not those of this server"},
		{name: "the level of another line", body: other(`"level":"INFO"`, `"level":"WARN"`), want: 400, reason: "level"},
		{name: "a line that is not JSON", body: gz(lokiText(labels, `["`+stamp+`","plain text"]`)), want: 400, reason: "not a JSON object"},
		{name: "the head of a line in another case", body: other(`\"msg\"`, `\"Msg\"`), want: 400, reason: "not a JSON object"},
		{name: "a time stamp of another time", body: other(`"`+stamp+`"`, `"1"`), want: 400, reason: "not the time of its line"},
		{name: "values out of time order", body: gz(lokiText(labels, lokiValue(next), lokiValue(line))), want: 400, reason: "time order"},
		{name: "two streams of one kind and level", body: gz(`{"streams":[{"stream":` + labels + `,"values":[` + lokiValue(line) +
			`]},{"stream":` + labels + `,"values":[` + lokiValue(next) + `]}]}`), want: 400, reason: "a second stream"},
	}
	wants := sendDirect(t, stub.url(), lokiPath, headers, cases)
	checkRefusals(t, stub.refusals(), wants)
	if got := stub.taken(); len(got) != 1 || got[0].id != "server-0001" || got[0].line != string(line.JSON) {
		t.Fatalf("the stub took %+v, want the good value", got)
	}
}
