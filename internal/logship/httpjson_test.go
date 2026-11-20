package logship

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The format http-json is our own: one POST per batch to the address of the
// target, Content-Type application/x-ndjson, the body the JSON lines of the
// log files, each followed by a line break. Its rules come from the NDJSON
// specification 1.0.0, https://github.com/ndjson/ndjson-spec (README,
// sections 3.1 to 3.3), read on 2026-11-19: each JSON text is followed by
// \n, which a \r may precede, holds no raw line break, and the data is
// UTF-8. The secret is sent as Authorization: Bearer <secret> (RFC 6750,
// section 2.1) or, with a user, as HTTP Basic (RFC 7617, section 2: a
// user-id holds no colon and no control character), both read at
// https://www.rfc-editor.org on 2026-11-19. Retry-After is seconds or an
// HTTP date (RFC 9110, section 10.2.3, read the same day). The statuses that
// are tried again, 429, 502, 503 and 504, are those of the OTLP
// specification, https://opentelemetry.io/docs/specs/otlp/, section
// Retryable Response Codes, as read for this leg on 2026-11-19.

// ndjsonStub is a receiver of the format http-json. It takes a POST to its
// path with Content-Type application/x-ndjson, the User-Agent of this
// server, the Authorization of its secret or none, no Content-Encoding, and
// a body of JSON objects that each start with the four fields of a log line
// and each end with a line break. It refuses anything else with a 4xx
// answer, and answers a request it took with 204.
type ndjsonStub struct {
	srv    *httptest.Server
	path   string
	auth   string          // the Authorization the stub takes; empty takes none
	status func(n int) int // the status of request n once its body was taken; 0 is 204
	caFile string          // the authority of the certificate of an https stub

	mu       sync.Mutex
	requests int
	lines    []string // the lines of the requests answered with 204
	refused  []string // why each refused request was refused
}

func newNDJSONStub(t *testing.T, path, auth string, https bool) *ndjsonStub {
	t.Helper()
	s := &ndjsonStub{path: path, auth: auth}
	s.srv = httptest.NewUnstartedServer(s)
	if https {
		var cert tls.Certificate
		s.caFile, cert = testCA(t)
		s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		s.srv.StartTLS()
	} else {
		s.srv.Start()
	}
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ndjsonStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	n := s.requests
	s.mu.Unlock()
	lines, code, reason := s.decode(r)
	if code == 0 && s.status != nil {
		code, reason = s.status(n), "the answer of the test"
	}
	if code != 0 {
		s.mu.Lock()
		s.refused = append(s.refused, reason)
		s.mu.Unlock()
		http.Error(w, reason, code)
		return
	}
	s.mu.Lock()
	s.lines = append(s.lines, lines...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// decode applies the rules of the format; a code other than 0 refuses.
func (s *ndjsonStub) decode(r *http.Request) ([]string, int, string) {
	switch {
	case r.Method != http.MethodPost:
		return nil, http.StatusMethodNotAllowed, "method " + r.Method
	case r.URL.Path != s.path:
		return nil, http.StatusNotFound, "path " + r.URL.Path
	case r.Header.Get("Content-Type") != "application/x-ndjson":
		return nil, http.StatusUnsupportedMediaType, "content type " + r.Header.Get("Content-Type")
	case r.Header.Get("Content-Encoding") != "":
		return nil, http.StatusUnsupportedMediaType, "content encoding"
	case r.Header.Get("User-Agent") != "pipesmith-registry/"+testVersion:
		return nil, http.StatusBadRequest, "user agent " + r.Header.Get("User-Agent")
	case r.Header.Get("Authorization") != s.auth:
		return nil, http.StatusUnauthorized, "authorization"
	}
	body, err := io.ReadAll(r.Body)
	switch {
	case err != nil:
		return nil, http.StatusBadRequest, "body not read"
	case len(body) == 0 || body[len(body)-1] != '\n':
		return nil, http.StatusBadRequest, "the last JSON text is not followed by a line break"
	case !utf8.Valid(body):
		return nil, http.StatusBadRequest, "not UTF-8"
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		var fields map[string]any
		switch {
		case line == "":
			return nil, http.StatusBadRequest, "an empty line"
		case !json.Valid([]byte(line)) || line[0] != '{':
			return nil, http.StatusBadRequest, "a line that is not one JSON object"
		case json.Unmarshal([]byte(line), &fields) != nil || !headIn(fields):
			return nil, http.StatusBadRequest, "a line without time, level, logger and msg by these names"
		}
		lines = append(lines, line)
	}
	return lines, 0, ""
}

// headIn reports whether the fields of a line hold the four keys of the head
// of a log line by their exact names, each a string. A struct read by
// json.Unmarshal would take "Time" or "MSG" too: Unmarshal prefers an exact
// match of a key but also accepts one in another case (go doc
// encoding/json Unmarshal, Go 1.27.4, read on 2026-11-19).
func headIn(fields map[string]any) bool {
	for _, key := range []string{"time", "level", "logger", "msg"} {
		if _, ok := fields[key].(string); !ok {
			return false
		}
	}
	return true
}

func (s *ndjsonStub) url() string { return s.srv.URL }

func (s *ndjsonStub) taken() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *ndjsonStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// The 1,000 hostile lines of hostileLines, sent through the manager, arrive
// as 1,000 lines, byte for byte, with the same ids, over http and over https
// with a test authority as ca_file.
func TestHTTPJSONThousandHostileLines(t *testing.T) {
	lines := hostileLines(t)
	want := idSet(t, lines)
	for _, variant := range []string{"http", "https"} {
		t.Run(variant, func(t *testing.T) {
			stub := newNDJSONStub(t, "/ingest", "", variant == "https")
			row := map[string]any{"name": "demo", "kind": "http-json", "address": stub.url() + "/ingest"}
			if variant == "https" {
				row["ca_file"] = stub.caFile
			}
			var set setting
			set.put(t, row)
			var log testLog
			m := startManager(t, newFakeClock(), &set, &log)
			shipAll(t, m, lines)
			waitFor(t, func() bool { return len(stub.taken()) >= len(lines) }, "1,000 lines decoded")
			got := stub.taken()
			if len(got) != len(lines) {
				t.Fatalf("%d lines decoded, %d sent", len(got), len(lines))
			}
			ids := map[string]bool{}
			for i, line := range got {
				if line != string(lines[i].JSON) {
					t.Fatalf("line %d arrived as %q, sent as %q", i, line, lines[i].JSON)
				}
				id, err := lineIDOf([]byte(line))
				if err != nil {
					t.Fatal(err)
				}
				ids[id] = true
			}
			if len(ids) != len(want) || len(ids) != 1000 {
				t.Fatalf("%d ids decoded, %d sent", len(ids), len(want))
			}
			for id := range want {
				if !ids[id] {
					t.Fatalf("the id %s did not arrive", id)
				}
			}
			waitCounted(t, m, "demo", len(lines))
			if st := statsOf(t, m, "demo"); st.Sent != 1000 || st.Failed != 0 || st.Errors != 0 || st.Attempts != 2 {
				t.Fatalf("%+v", st)
			}
		})
	}
}

// The secret reaches the stub as Bearer, and with a user as HTTP Basic over
// https; neither secret, nor the query of the address, is in a log line of
// the manager, its warnings with the class of each failure among them.
func TestHTTPJSONSecretReachesOnlyTheStub(t *testing.T) {
	const token, password, query = "canary-token-6c1f0b2e", "canary-password-4e9d27a1", "canary-query-8b3d5f"
	cases := []struct {
		name, user, secret string
		https              bool
	}{
		{"bearer", "", token, false},
		{"basic", "demo-admin", password, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth := "Bearer " + c.secret
			if c.user != "" {
				auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.secret))
			}
			stub := newNDJSONStub(t, "/ingest", auth, c.https)
			stub.status = func(n int) int {
				if n == 1 {
					return http.StatusServiceUnavailable
				}
				return 0
			}
			row := map[string]any{"name": "demo", "kind": "http-json", "address": stub.url() + "/ingest?token=" + query,
				"secret": "log-target-demo", "batch_lines": 3}
			if c.https {
				row["ca_file"], row["user"] = stub.caFile, c.user
			}
			var set setting
			set.put(t, row)
			set.putSecret("log-target-demo", c.secret)
			clk := newFakeClock()
			var log testLog
			m := startManager(t, clk, &set, &log)
			shipAll(t, m, testLines("server", 0, 3))
			clk.waitPending(t, 500*time.Millisecond)
			clk.Advance(500 * time.Millisecond)
			waitFor(t, func() bool { return len(stub.taken()) == 3 }, "the lines taken with the secret")
			m.Close()
			if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "status 503" {
				t.Fatalf("warnings %v", warns)
			}
			noCanary(t, "the log of the manager", log.text(), c.secret, query, auth[strings.Index(auth, " ")+1:])
		})
	}
}

// Each retry rule once: 429, 502, 503 and 504 send the batch again and it
// counts as sent; every other status outside 2xx, 3xx among them, drops it
// after one attempt; a refused connection is tried 3 times and fails as
// dial. The timeout and Retry-After are hit through this adapter by
// TestSilentTargetCostsAtMostItsTimeout and TestRetryAfterIsHonoured.
func TestHTTPJSONRetryRules(t *testing.T) {
	retried := []int{429, 502, 503, 504}
	dropped := []int{301, 302, 307, 308, 400, 401, 403, 404, 405, 409, 413, 415, 500, 501}
	for _, code := range append(append([]int(nil), retried...), dropped...) {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			stub := newNDJSONStub(t, "/", "", false)
			stub.status = func(n int) int {
				if n == 1 {
					return code
				}
				return 0
			}
			var set setting
			set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
			clk := newFakeClock()
			var log testLog
			m := startManager(t, clk, &set, &log)
			m.Ship(testLine("tasks", 1))
			if slices.Contains(retried, code) {
				clk.waitPending(t, 500*time.Millisecond)
				clk.Advance(500 * time.Millisecond)
				waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent again")
				if st := statsOf(t, m, "demo"); st.Attempts != 2 || st.Errors != 1 || st.Failed != 0 {
					t.Fatalf("%+v", st)
				}
				return
			}
			waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line dropped")
			if st := statsOf(t, m, "demo"); st.Attempts != 1 || st.Sent != 0 || stub.count() != 1 {
				t.Fatalf("%+v after %d requests", st, stub.count())
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := "http://" + ln.Addr().String()
		_ = ln.Close() // the port refuses from here on
		var set setting
		set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": address, "batch_lines": 1})
		clk := newFakeClock()
		var log testLog
		m := startManager(t, clk, &set, &log)
		m.Ship(testLine("tasks", 1))
		clk.waitPending(t, 500*time.Millisecond)
		clk.Advance(500 * time.Millisecond)
		clk.waitPending(t, 2*time.Second)
		clk.Advance(2 * time.Second)
		waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line given up")
		if st := statsOf(t, m, "demo"); st.Attempts != 3 || st.Errors != 3 {
			t.Fatalf("%+v", st)
		}
		m.Close() // the warning of the last attempt is written before the stop line
		if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "dial" {
			t.Fatalf("warnings %v", warns)
		}
	})
}

// An https target without ca_file trusts the roots of the system only, and
// a test authority is not among them: each attempt fails as tls.
func TestHTTPJSONUntrustedCertificate(t *testing.T) {
	stub := newNDJSONStub(t, "/", "", true)
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "http-json", "address": stub.url(), "batch_lines": 1})
	clk := newFakeClock()
	var log testLog
	m := startManager(t, clk, &set, &log)
	m.Ship(testLine("server", 1))
	clk.waitPending(t, 500*time.Millisecond)
	if st := statsOf(t, m, "demo"); st.Errors != 1 || st.Sent != 0 || stub.count() != 0 {
		t.Fatalf("%+v after %d requests", st, stub.count())
	}
	if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "tls" {
		t.Fatalf("warnings %v", warns)
	}
}

// The stub refuses what the format does not allow, one bad request each,
// sent to it directly, and takes the one good request.
func TestHTTPJSONStubRefuses(t *testing.T) {
	stub := newNDJSONStub(t, "/ingest", "Bearer demo-token", false)
	good := string(testLine("server", 1).JSON) + "\n"
	agent := "pipesmith-registry/" + testVersion
	cases := []struct {
		name, method, path, contentType, encoding, agent, auth, body string
		want                                                         int
	}{
		{"good", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", good, 204},
		{"method", "PUT", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", good, 405},
		{"path", "POST", "/other", contentTypeNDJSON, "", agent, "Bearer demo-token", good, 404},
		{"content type", "POST", "/ingest", "application/json", "", agent, "Bearer demo-token", good, 415},
		{"no content type", "POST", "/ingest", "", "", agent, "Bearer demo-token", good, 415},
		{"compressed", "POST", "/ingest", contentTypeNDJSON, "gzip", agent, "Bearer demo-token", good, 415},
		{"user agent", "POST", "/ingest", contentTypeNDJSON, "", "demo-client/1.0", "Bearer demo-token", good, 400},
		{"no secret", "POST", "/ingest", contentTypeNDJSON, "", agent, "", good, 401},
		{"other secret", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer other-token", good, 401},
		{"no last line break", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", strings.TrimSuffix(good, "\n"), 400},
		{"two texts on a line", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", strings.TrimSuffix(good, "\n") + good, 400},
		{"empty line", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", good + "\n", 400},
		{"not JSON", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", "not json\n", 400},
		{"not UTF-8", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", "{\"msg\":\"\xff\"}\n", 400},
		{"a list", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", "[1,2]\n", 400},
		{"no head", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", "{\"line_id\":\"x\"}\n", 400},
		{"the head in another case", "POST", "/ingest", contentTypeNDJSON, "", agent, "Bearer demo-token", `{"Time":"2026-11-19T10:00:00.000Z","LEVEL":"INFO","Logger":"pipesmith_registry.demo","Msg":"line"}` + "\n", 400},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, stub.url()+c.path, bytes.NewReader([]byte(c.body)))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range map[string]string{"Content-Type": c.contentType, "Content-Encoding": c.encoding,
			"User-Agent": c.agent, "Authorization": c.auth} {
			if value != "" {
				req.Header.Set(key, value)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if got := stub.taken(); !slices.Equal(got, []string{strings.TrimSuffix(good, "\n")}) {
		t.Fatalf("the stub took %q", got)
	}
}

// The format http-json writes each line followed by a line break and
// nothing else.
func TestHTTPJSONBody(t *testing.T) {
	stub := newWebStub(t, nil)
	a, err := openHTTPJSON(Target{Name: "demo", Kind: kindHTTPJSON, Address: stub.url()}, "", env{version: testVersion, clock: realClock{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	batch := []logs.Line{testLine("server", 1), testLine("audit", 2)}
	if _, err := a.Send(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	want := string(batch[0].JSON) + "\n" + string(batch[1].JSON) + "\n"
	if got := stub.requests(); len(got) != 1 || string(got[0].body) != want || got[0].header.Get("Authorization") != "" {
		t.Fatalf("requests %+v", got)
	}
}
